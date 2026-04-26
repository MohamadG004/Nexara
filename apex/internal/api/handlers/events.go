package handlers

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"

	"github.com/yourusername/apex/internal/models"
	"github.com/yourusername/apex/internal/services"
	"github.com/yourusername/apex/pkg/response"
	"github.com/yourusername/apex/pkg/validator"
)

// EventHandler handles all event ingestion and query operations.
// The thin handler pattern keeps handlers as pure HTTP adapters —
// validation, business logic, and persistence all live in the service layer.
type EventHandler struct {
	svc      services.EventService
	upgrader websocket.Upgrader
}

func NewEventHandler(svc services.EventService) *EventHandler {
	return &EventHandler{
		svc: svc,
		upgrader: websocket.Upgrader{
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,
			// In production, validate origin against allowlist
			CheckOrigin: func(r *http.Request) bool { return true },
		},
	}
}

// IngestEvent represents a single captured API request event from an SDK.
// The SDK is a thin wrapper around the user's HTTP server — it captures
// request/response metadata without touching the body (privacy-first).
type IngestEvent struct {
	TraceID      string            `json:"trace_id"`
	Method       string            `json:"method" validate:"required,oneof=GET POST PUT PATCH DELETE OPTIONS HEAD"`
	Path         string            `json:"path" validate:"required,max=2048"`
	StatusCode   int               `json:"status_code" validate:"required,min=100,max=599"`
	LatencyMs    float64           `json:"latency_ms" validate:"required,min=0"`
	RequestSize  int64             `json:"request_size_bytes"`
	ResponseSize int64             `json:"response_size_bytes"`
	UserAgent    string            `json:"user_agent" validate:"max=512"`
	IP           string            `json:"ip"`
	Tags         map[string]string `json:"tags"`      // arbitrary key/value metadata
	OccurredAt   time.Time         `json:"occurred_at"` // when the request happened (client time)
}

// IngestBatchRequest wraps multiple events for efficient bulk ingestion.
type IngestBatchRequest struct {
	Events []IngestEvent `json:"events" validate:"required,min=1,max=1000,dive"`
}

// Ingest accepts a single event from an SDK.
// POST /api/v1/ingest/events
func (h *EventHandler) Ingest(w http.ResponseWriter, r *http.Request) {
	projectID := r.Context().Value(middleware.ProjectIDKey).(string) // set by APIKeyAuth

	var req IngestEvent
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.BadRequest(w, "invalid JSON: "+err.Error())
		return
	}

	if errs := validator.Validate(req); len(errs) > 0 {
		response.ValidationError(w, errs)
		return
	}

	event, err := h.svc.Ingest(r.Context(), projectID, req.toModel())
	if err != nil {
		response.InternalError(w, r, err)
		return
	}

	response.Created(w, event)
}

// IngestBatch accepts up to 1000 events in a single request.
// POST /api/v1/ingest/batch
// SDKs should batch events to reduce network overhead; the default
// SDK batch interval is 5 seconds or 100 events, whichever comes first.
func (h *EventHandler) IngestBatch(w http.ResponseWriter, r *http.Request) {
	projectID := r.Context().Value(middleware.ProjectIDKey).(string)

	var req IngestBatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.BadRequest(w, "invalid JSON: "+err.Error())
		return
	}

	if errs := validator.Validate(req); len(errs) > 0 {
		response.ValidationError(w, errs)
		return
	}

	events := make([]models.Event, len(req.Events))
	for i, e := range req.Events {
		events[i] = e.toModel()
		events[i].ProjectID = projectID
	}

	result, err := h.svc.IngestBatch(r.Context(), projectID, events)
	if err != nil {
		response.InternalError(w, r, err)
		return
	}

	response.JSON(w, http.StatusMultiStatus, map[string]any{
		"accepted": result.Accepted,
		"rejected": result.Rejected,
		"errors":   result.Errors,
	})
}

// List returns a paginated, filterable list of events for a project.
// GET /api/v1/projects/{projectID}/events
func (h *EventHandler) List(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	q := r.URL.Query()

	filter := services.EventFilter{
		Method:        q.Get("method"),
		PathPattern:   q.Get("path"),
		MinStatus:     parseIntQuery(q.Get("status_min"), 0),
		MaxStatus:     parseIntQuery(q.Get("status_max"), 0),
		MinLatencyMs:  parseFloatQuery(q.Get("latency_min"), 0),
		Page:          parseIntQuery(q.Get("page"), 1),
		PageSize:      parseIntQuery(q.Get("page_size"), 50),
	}

	// Parse time range — default to last 24 hours
	if from := q.Get("from"); from != "" {
		if t, err := time.Parse(time.RFC3339, from); err == nil {
			filter.From = t
		}
	} else {
		filter.From = time.Now().Add(-24 * time.Hour)
	}
	if to := q.Get("to"); to != "" {
		if t, err := time.Parse(time.RFC3339, to); err == nil {
			filter.To = t
		}
	} else {
		filter.To = time.Now()
	}

	result, err := h.svc.List(r.Context(), projectID, filter)
	if err != nil {
		response.InternalError(w, r, err)
		return
	}

	response.JSON(w, http.StatusOK, result)
}

// Get returns a single event by ID.
// GET /api/v1/projects/{projectID}/events/{eventID}
func (h *EventHandler) Get(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	eventID := chi.URLParam(r, "eventID")

	event, err := h.svc.GetByID(r.Context(), projectID, eventID)
	if err != nil {
		if err == services.ErrNotFound {
			response.NotFound(w, "event not found")
			return
		}
		response.InternalError(w, r, err)
		return
	}

	response.JSON(w, http.StatusOK, event)
}

// Stream upgrades to WebSocket and pushes live events to the client.
// GET /ws/projects/{projectID}/stream
// Authentication is handled via query param token (WebSocket can't set headers).
// Real-time events are broadcast via Redis pub/sub — this handler is stateless.
func (h *EventHandler) Stream(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	token := r.URL.Query().Get("token")
	if token == "" {
		http.Error(w, "missing token", http.StatusUnauthorized)
		return
	}

	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		// upgrader already wrote error response
		return
	}
	defer conn.Close()

	// Subscribe to Redis channel for this project's events
	ctx := r.Context()
	sub := h.svc.Subscribe(ctx, projectID)
	defer sub.Close()

	// Send a connected confirmation
	conn.WriteJSON(map[string]string{"type": "connected", "project_id": projectID})

	// Ping client every 30s to keep connection alive through proxies
	pingTicker := time.NewTicker(30 * time.Second)
	defer pingTicker.Stop()

	for {
		select {
		case event, ok := <-sub.Channel():
			if !ok {
				return // subscription closed
			}
			if err := conn.WriteJSON(map[string]any{"type": "event", "data": event}); err != nil {
				return // client disconnected
			}
		case <-pingTicker.C:
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		case <-ctx.Done():
			conn.WriteMessage(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseGoingAway, "server shutdown"))
			return
		}
	}
}

// toModel converts the HTTP request DTO to the internal domain model.
// This separation means the API contract can evolve independently of the domain.
func (e IngestEvent) toModel() models.Event {
	return models.Event{
		TraceID:      e.TraceID,
		Method:       e.Method,
		Path:         e.Path,
		StatusCode:   e.StatusCode,
		LatencyMs:    e.LatencyMs,
		RequestSize:  e.RequestSize,
		ResponseSize: e.ResponseSize,
		UserAgent:    e.UserAgent,
		IP:           e.IP,
		Tags:         e.Tags,
		OccurredAt:   e.OccurredAt,
	}
}

func parseIntQuery(s string, fallback int) int {
	if n, err := strconv.Atoi(s); err == nil && n > 0 {
		return n
	}
	return fallback
}

func parseFloatQuery(s string, fallback float64) float64 {
	if n, err := strconv.ParseFloat(s, 64); err == nil && n >= 0 {
		return n
	}
	return fallback
}
