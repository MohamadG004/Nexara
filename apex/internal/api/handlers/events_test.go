package handlers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/yourusername/apex/internal/api/handlers"
	"github.com/yourusername/apex/internal/api/middleware"
	"github.com/yourusername/apex/internal/models"
	"github.com/yourusername/apex/internal/services"
)

// mockEventService is a hand-rolled fake — we avoid mockery/gomock here
// because the interface is simple enough that a struct with fields is
// clearer and faster to write. The downside is you have to update the fake
// when the interface changes; the upside is no framework magic.
type mockEventService struct {
	ingestFn      func(ctx context.Context, projectID string, event models.Event) (*models.Event, error)
	ingestBatchFn func(ctx context.Context, projectID string, events []models.Event) (*services.BatchResult, error)
	listFn        func(ctx context.Context, projectID string, filter services.EventFilter) (*services.EventPage, error)
	getByIDFn     func(ctx context.Context, projectID, eventID string) (*models.Event, error)
}

func (m *mockEventService) Ingest(ctx context.Context, projectID string, event models.Event) (*models.Event, error) {
	return m.ingestFn(ctx, projectID, event)
}

func (m *mockEventService) IngestBatch(ctx context.Context, projectID string, events []models.Event) (*services.BatchResult, error) {
	return m.ingestBatchFn(ctx, projectID, events)
}

func (m *mockEventService) List(ctx context.Context, projectID string, filter services.EventFilter) (*services.EventPage, error) {
	return m.listFn(ctx, projectID, filter)
}

func (m *mockEventService) GetByID(ctx context.Context, projectID, eventID string) (*models.Event, error) {
	return m.getByIDFn(ctx, projectID, eventID)
}

func (m *mockEventService) Subscribe(ctx context.Context, projectID string) services.Subscription {
	return nil
}

// ─── Test Helpers ────────────────────────────────────────────────────────────

func setupEventHandler(svc services.EventService) http.Handler {
	h := handlers.NewEventHandler(svc)
	r := chi.NewRouter()
	// Inject projectID as middleware would normally do it
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := context.WithValue(r.Context(), middleware.ProjectIDKey, "proj_test123")
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	})
	r.Post("/ingest/events", h.Ingest)
	r.Get("/projects/{projectID}/events", h.List)
	r.Get("/projects/{projectID}/events/{eventID}", h.Get)
	return r
}

func postJSON(t *testing.T, handler http.Handler, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr
}

// ─── Tests ───────────────────────────────────────────────────────────────────

func TestIngest_Success(t *testing.T) {
	now := time.Now().UTC()
	expectedEvent := &models.Event{
		ID:         "evt_abc123",
		ProjectID:  "proj_test123",
		Method:     "GET",
		Path:       "/api/users",
		StatusCode: 200,
		LatencyMs:  45.2,
		OccurredAt: now,
		CreatedAt:  now,
	}

	svc := &mockEventService{
		ingestFn: func(ctx context.Context, projectID string, event models.Event) (*models.Event, error) {
			// Verify the service receives the correct project ID
			if projectID != "proj_test123" {
				t.Errorf("expected projectID proj_test123, got %s", projectID)
			}
			return expectedEvent, nil
		},
	}

	handler := setupEventHandler(svc)

	body := map[string]any{
		"method":      "GET",
		"path":        "/api/users",
		"status_code": 200,
		"latency_ms":  45.2,
		"occurred_at": now.Format(time.RFC3339),
	}

	rr := postJSON(t, handler, "/ingest/events", body)

	if rr.Code != http.StatusCreated {
		t.Errorf("expected 201, got %d; body: %s", rr.Code, rr.Body.String())
	}

	var resp struct {
		Success bool         `json:"success"`
		Data    models.Event `json:"data"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.Success {
		t.Errorf("expected success:true")
	}
	if resp.Data.ID != expectedEvent.ID {
		t.Errorf("expected event ID %s, got %s", expectedEvent.ID, resp.Data.ID)
	}
}

func TestIngest_ValidationError_MissingMethod(t *testing.T) {
	svc := &mockEventService{} // ingestFn should never be called
	handler := setupEventHandler(svc)

	body := map[string]any{
		// "method" intentionally omitted
		"path":        "/api/users",
		"status_code": 200,
		"latency_ms":  45.2,
	}

	rr := postJSON(t, handler, "/ingest/events", body)

	if rr.Code != http.StatusUnprocessableEntity {
		t.Errorf("expected 422, got %d", rr.Code)
	}
}

func TestIngest_ValidationError_InvalidStatusCode(t *testing.T) {
	svc := &mockEventService{}
	handler := setupEventHandler(svc)

	body := map[string]any{
		"method":      "GET",
		"path":        "/api/users",
		"status_code": 999, // invalid: max is 599
		"latency_ms":  45.2,
	}

	rr := postJSON(t, handler, "/ingest/events", body)

	if rr.Code != http.StatusUnprocessableEntity {
		t.Errorf("expected 422, got %d", rr.Code)
	}
}

func TestIngest_InvalidJSON(t *testing.T) {
	svc := &mockEventService{}
	handler := setupEventHandler(svc)

	req := httptest.NewRequest(http.MethodPost, "/ingest/events", bytes.NewBufferString("not json{{"))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rr.Code)
	}
}

func TestGetEvent_NotFound(t *testing.T) {
	svc := &mockEventService{
		getByIDFn: func(ctx context.Context, projectID, eventID string) (*models.Event, error) {
			return nil, services.ErrNotFound
		},
	}
	handler := setupEventHandler(svc)

	req := httptest.NewRequest(http.MethodGet, "/projects/proj_test123/events/nonexistent", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rr.Code)
	}
}

// Table-driven test — preferred for testing multiple scenarios of the same function.
// The test name format "TestIngest/valid_GET" makes it easy to run a single case:
// go test -run TestIngest/valid_GET ./...
func TestIngest_ValidMethods(t *testing.T) {
	validMethods := []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "HEAD"}

	for _, method := range validMethods {
		t.Run("valid_"+method, func(t *testing.T) {
			svc := &mockEventService{
				ingestFn: func(ctx context.Context, projectID string, event models.Event) (*models.Event, error) {
					return &models.Event{ID: "evt_1", Method: event.Method}, nil
				},
			}
			handler := setupEventHandler(svc)

			body := map[string]any{
				"method":      method,
				"path":        "/test",
				"status_code": 200,
				"latency_ms":  10.0,
				"occurred_at": time.Now().Format(time.RFC3339),
			}

			rr := postJSON(t, handler, "/ingest/events", body)
			if rr.Code != http.StatusCreated {
				t.Errorf("method %s: expected 201, got %d", method, rr.Code)
			}
		})
	}
}
