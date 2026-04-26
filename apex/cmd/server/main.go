package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"math/big"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// ─────────────────────────────────────────────────────────────────────────────
// Configuration
// ─────────────────────────────────────────────────────────────────────────────

var (
	jwtSecret = []byte(getenv("JWT_SECRET", "apex-dev-secret-change-in-production"))
	port      = getenv("PORT", "8080")
)

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// ─────────────────────────────────────────────────────────────────────────────
// Domain Models
// ─────────────────────────────────────────────────────────────────────────────

type User struct {
	ID           string    `json:"id"`
	Email        string    `json:"email"`
	Name         string    `json:"name"`
	PasswordHash string    `json:"-"`
	CreatedAt    time.Time `json:"created_at"`
}

type Project struct {
	ID          string    `json:"id"`
	OwnerID     string    `json:"owner_id"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

type APIKey struct {
	ID        string    `json:"id"`
	ProjectID string    `json:"project_id"`
	Name      string    `json:"name"`
	KeyPrefix string    `json:"prefix"`
	KeyHash   string    `json:"-"`
	Plaintext string    `json:"key,omitempty"` // returned once on creation
	CreatedAt time.Time `json:"created_at"`
}

type Event struct {
	ID           string            `json:"id"`
	ProjectID    string            `json:"project_id"`
	TraceID      string            `json:"trace_id,omitempty"`
	Method       string            `json:"method"`
	Path         string            `json:"path"`
	StatusCode   int               `json:"status_code"`
	LatencyMs    float64           `json:"latency_ms"`
	RequestSize  int64             `json:"request_size_bytes"`
	ResponseSize int64             `json:"response_size_bytes"`
	UserAgent    string            `json:"user_agent,omitempty"`
	IP           string            `json:"ip,omitempty"`
	Tags         map[string]string `json:"tags,omitempty"`
	OccurredAt   time.Time         `json:"occurred_at"`
}

// ─────────────────────────────────────────────────────────────────────────────
// In-Memory Store (simulates PostgreSQL + Redis)
// ─────────────────────────────────────────────────────────────────────────────

type Store struct {
	mu       sync.RWMutex
	users    map[string]*User    // id → user
	byEmail  map[string]*User    // email → user
	projects map[string]*Project // id → project
	apiKeys  map[string]*APIKey  // hash → apiKey
	events   []*Event            // append-only time-series
}

func NewStore() *Store {
	return &Store{
		users:    make(map[string]*User),
		byEmail:  make(map[string]*User),
		projects: make(map[string]*Project),
		apiKeys:  make(map[string]*APIKey),
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

func hashPassword(pw string) string {
	// SHA-256 + salt — good enough for a local demo.
	// Production: use bcrypt cost 12 (golang.org/x/crypto/bcrypt).
	salt := "apex-salt-2025"
	h := sha256.Sum256([]byte(salt + pw))
	return hex.EncodeToString(h[:])
}

func checkPassword(hash, pw string) bool {
	return hash == hashPassword(pw)
}

func hashKey(key string) string {
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:])
}

func generateAPIKey() string {
	// Format: apx_live_<32 random hex chars>
	b := make([]byte, 16)
	rand.Read(b)
	return "apx_live_" + hex.EncodeToString(b)
}

func newID() string {
	return uuid.New().String()
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"success": status < 400,
		"data":    data,
	})
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"success": false,
		"error": map[string]string{
			"code":    code,
			"message": msg,
		},
	})
}

func decodeJSON(r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, 1<<20) // 1MB limit
	return json.NewDecoder(r.Body).Decode(dst)
}

// ─────────────────────────────────────────────────────────────────────────────
// JWT
// ─────────────────────────────────────────────────────────────────────────────

type Claims struct {
	UserID string `json:"uid"`
	Email  string `json:"email"`
	jwt.RegisteredClaims
}

func signToken(user *User) (string, error) {
	claims := Claims{
		UserID: user.ID,
		Email:  user.Email,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(24 * time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			Issuer:    "apex",
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(jwtSecret)
}

func parseToken(tokenStr string) (*Claims, error) {
	claims := &Claims{}
	token, err := jwt.ParseWithClaims(tokenStr, claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method")
		}
		return jwtSecret, nil
	})
	if err != nil || !token.Valid {
		return nil, fmt.Errorf("invalid token")
	}
	return claims, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Middleware
// ─────────────────────────────────────────────────────────────────────────────

type ctxKey string

const (
	ctxUserID    ctxKey = "user_id"
	ctxProjectID ctxKey = "project_id"
)

func jwtAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			writeError(w, 401, "UNAUTHORIZED", "missing or invalid Authorization header")
			return
		}
		claims, err := parseToken(strings.TrimPrefix(auth, "Bearer "))
		if err != nil {
			writeError(w, 401, "UNAUTHORIZED", "invalid or expired token")
			return
		}
		ctx := context.WithValue(r.Context(), ctxUserID, claims.UserID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func apiKeyAuth(store *Store) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := r.Header.Get("X-API-Key")
			if key == "" {
				key = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			}
			if key == "" {
				writeError(w, 401, "UNAUTHORIZED", "missing X-API-Key header")
				return
			}
			hash := hashKey(key)
			store.mu.RLock()
			apiKey, ok := store.apiKeys[hash]
			store.mu.RUnlock()
			if !ok {
				writeError(w, 401, "UNAUTHORIZED", "invalid API key")
				return
			}
			ctx := context.WithValue(r.Context(), ctxProjectID, apiKey.ProjectID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func projectAccess(store *Store) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			userID := r.Context().Value(ctxUserID).(string)
			projectID := chi.URLParam(r, "projectID")
			store.mu.RLock()
			proj, ok := store.projects[projectID]
			store.mu.RUnlock()
			if !ok {
				writeError(w, 404, "NOT_FOUND", "project not found")
				return
			}
			if proj.OwnerID != userID {
				writeError(w, 403, "FORBIDDEN", "you don't have access to this project")
				return
			}
			ctx := context.WithValue(r.Context(), ctxProjectID, projectID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Handlers
// ─────────────────────────────────────────────────────────────────────────────

// --- Health ---

func handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]string{
		"status":  "ok",
		"version": "1.0.0-demo",
		"storage": "in-memory (demo mode)",
	})
}

// --- Auth ---

func handleRegister(store *Store) http.HandlerFunc {
	type req struct {
		Email    string `json:"email"`
		Name     string `json:"name"`
		Password string `json:"password"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		var body req
		if err := decodeJSON(r, &body); err != nil {
			writeError(w, 400, "BAD_REQUEST", "invalid JSON")
			return
		}
		if body.Email == "" || body.Password == "" || body.Name == "" {
			writeError(w, 422, "VALIDATION_ERROR", "email, name, and password are required")
			return
		}
		store.mu.Lock()
		defer store.mu.Unlock()
		if _, exists := store.byEmail[body.Email]; exists {
			writeError(w, 409, "CONFLICT", "email already registered")
			return
		}
		user := &User{
			ID:           newID(),
			Email:        body.Email,
			Name:         body.Name,
			PasswordHash: hashPassword(body.Password),
			CreatedAt:    time.Now(),
		}
		store.users[user.ID] = user
		store.byEmail[user.Email] = user

		token, _ := signToken(user)
		writeJSON(w, 201, map[string]any{"user": user, "token": token})
	}
}

func handleLogin(store *Store) http.HandlerFunc {
	type req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		var body req
		if err := decodeJSON(r, &body); err != nil {
			writeError(w, 400, "BAD_REQUEST", "invalid JSON")
			return
		}
		store.mu.RLock()
		user, ok := store.byEmail[body.Email]
		store.mu.RUnlock()
		if !ok || !checkPassword(user.PasswordHash, body.Password) {
			writeError(w, 401, "UNAUTHORIZED", "invalid email or password")
			return
		}
		token, _ := signToken(user)
		writeJSON(w, 200, map[string]any{"user": user, "token": token})
	}
}

func handleMe(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid := r.Context().Value(ctxUserID).(string)
		store.mu.RLock()
		user := store.users[uid]
		store.mu.RUnlock()
		writeJSON(w, 200, user)
	}
}

// --- Projects ---

func handleListProjects(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid := r.Context().Value(ctxUserID).(string)
		store.mu.RLock()
		defer store.mu.RUnlock()
		var projects []*Project
		for _, p := range store.projects {
			if p.OwnerID == uid {
				projects = append(projects, p)
			}
		}
		sort.Slice(projects, func(i, j int) bool {
			return projects[i].CreatedAt.After(projects[j].CreatedAt)
		})
		writeJSON(w, 200, projects)
	}
}

func handleCreateProject(store *Store) http.HandlerFunc {
	type req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		uid := r.Context().Value(ctxUserID).(string)
		var body req
		if err := decodeJSON(r, &body); err != nil {
			writeError(w, 400, "BAD_REQUEST", "invalid JSON")
			return
		}
		if body.Name == "" {
			writeError(w, 422, "VALIDATION_ERROR", "name is required")
			return
		}
		proj := &Project{
			ID:          newID(),
			OwnerID:     uid,
			Name:        body.Name,
			Description: body.Description,
			CreatedAt:   time.Now(),
		}
		store.mu.Lock()
		store.projects[proj.ID] = proj
		store.mu.Unlock()
		writeJSON(w, 201, proj)
	}
}

func handleGetProject(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pid := r.Context().Value(ctxProjectID).(string)
		store.mu.RLock()
		proj := store.projects[pid]
		store.mu.RUnlock()
		writeJSON(w, 200, proj)
	}
}

// --- API Keys ---

func handleCreateAPIKey(store *Store) http.HandlerFunc {
	type req struct{ Name string `json:"name"` }
	return func(w http.ResponseWriter, r *http.Request) {
		pid := r.Context().Value(ctxProjectID).(string)
		var body req
		decodeJSON(r, &body)
		if body.Name == "" {
			body.Name = "Default"
		}
		plaintext := generateAPIKey()
		hash := hashKey(plaintext)
		key := &APIKey{
			ID:        newID(),
			ProjectID: pid,
			Name:      body.Name,
			KeyPrefix: plaintext[:12],
			KeyHash:   hash,
			Plaintext: plaintext, // only returned once
			CreatedAt: time.Now(),
		}
		store.mu.Lock()
		store.apiKeys[hash] = key
		store.mu.Unlock()
		writeJSON(w, 201, key)
	}
}

func handleListAPIKeys(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pid := r.Context().Value(ctxProjectID).(string)
		store.mu.RLock()
		defer store.mu.RUnlock()
		var keys []*APIKey
		for _, k := range store.apiKeys {
			if k.ProjectID == pid {
				// Don't expose hash or plaintext in list
				keys = append(keys, &APIKey{
					ID:        k.ID,
					ProjectID: k.ProjectID,
					Name:      k.Name,
					KeyPrefix: k.KeyPrefix,
					CreatedAt: k.CreatedAt,
				})
			}
		}
		writeJSON(w, 200, keys)
	}
}

// --- Event Ingestion ---

func handleIngest(store *Store) http.HandlerFunc {
	type req struct {
		Method       string            `json:"method"`
		Path         string            `json:"path"`
		StatusCode   int               `json:"status_code"`
		LatencyMs    float64           `json:"latency_ms"`
		RequestSize  int64             `json:"request_size_bytes"`
		ResponseSize int64             `json:"response_size_bytes"`
		UserAgent    string            `json:"user_agent"`
		IP           string            `json:"ip"`
		TraceID      string            `json:"trace_id"`
		Tags         map[string]string `json:"tags"`
		OccurredAt   time.Time         `json:"occurred_at"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		pid := r.Context().Value(ctxProjectID).(string)
		var body req
		if err := decodeJSON(r, &body); err != nil {
			writeError(w, 400, "BAD_REQUEST", "invalid JSON")
			return
		}
		if body.Method == "" || body.Path == "" || body.StatusCode == 0 {
			writeError(w, 422, "VALIDATION_ERROR", "method, path, and status_code are required")
			return
		}
		if body.OccurredAt.IsZero() {
			body.OccurredAt = time.Now()
		}
		event := &Event{
			ID:           newID(),
			ProjectID:    pid,
			TraceID:      body.TraceID,
			Method:       body.Method,
			Path:         body.Path,
			StatusCode:   body.StatusCode,
			LatencyMs:    body.LatencyMs,
			RequestSize:  body.RequestSize,
			ResponseSize: body.ResponseSize,
			UserAgent:    body.UserAgent,
			IP:           body.IP,
			Tags:         body.Tags,
			OccurredAt:   body.OccurredAt,
		}
		store.mu.Lock()
		store.events = append(store.events, event)
		store.mu.Unlock()
		slog.Info("event ingested", "project", pid, "method", event.Method, "path", event.Path, "status", event.StatusCode, "latency_ms", event.LatencyMs)
		writeJSON(w, 201, event)
	}
}

func handleIngestBatch(store *Store) http.HandlerFunc {
	type singleEvent struct {
		Method       string            `json:"method"`
		Path         string            `json:"path"`
		StatusCode   int               `json:"status_code"`
		LatencyMs    float64           `json:"latency_ms"`
		RequestSize  int64             `json:"request_size_bytes"`
		ResponseSize int64             `json:"response_size_bytes"`
		UserAgent    string            `json:"user_agent"`
		IP           string            `json:"ip"`
		TraceID      string            `json:"trace_id"`
		Tags         map[string]string `json:"tags"`
		OccurredAt   time.Time         `json:"occurred_at"`
	}
	type req struct {
		Events []singleEvent `json:"events"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		pid := r.Context().Value(ctxProjectID).(string)
		var body req
		if err := decodeJSON(r, &body); err != nil {
			writeError(w, 400, "BAD_REQUEST", "invalid JSON")
			return
		}
		if len(body.Events) == 0 {
			writeError(w, 422, "VALIDATION_ERROR", "events array is required and must not be empty")
			return
		}
		if len(body.Events) > 1000 {
			writeError(w, 422, "VALIDATION_ERROR", "max 1000 events per batch")
			return
		}
		now := time.Now()
		events := make([]*Event, len(body.Events))
		for i, e := range body.Events {
			occuredAt := e.OccurredAt
			if occuredAt.IsZero() {
				occuredAt = now
			}
			events[i] = &Event{
				ID:           newID(),
				ProjectID:    pid,
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
				OccurredAt:   occuredAt,
			}
		}
		store.mu.Lock()
		store.events = append(store.events, events...)
		store.mu.Unlock()
		slog.Info("batch ingested", "project", pid, "count", len(events))
		writeJSON(w, 207, map[string]any{
			"accepted": len(events),
			"rejected": 0,
		})
	}
}

// --- Events Query ---

func handleListEvents(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pid := r.Context().Value(ctxProjectID).(string)
		q := r.URL.Query()

		methodFilter := q.Get("method")
		pathFilter := q.Get("path")
		pageSize := 50
		if ps, err := strconv.Atoi(q.Get("page_size")); err == nil && ps > 0 && ps <= 500 {
			pageSize = ps
		}
		page := 1
		if p, err := strconv.Atoi(q.Get("page")); err == nil && p > 0 {
			page = p
		}

		store.mu.RLock()
		// Collect matching events (newest first)
		var matched []*Event
		for i := len(store.events) - 1; i >= 0; i-- {
			e := store.events[i]
			if e.ProjectID != pid {
				continue
			}
			if methodFilter != "" && !strings.EqualFold(e.Method, methodFilter) {
				continue
			}
			if pathFilter != "" && !strings.Contains(e.Path, pathFilter) {
				continue
			}
			matched = append(matched, e)
		}
		store.mu.RUnlock()

		total := len(matched)
		start := (page - 1) * pageSize
		end := start + pageSize
		if start > total {
			start = total
		}
		if end > total {
			end = total
		}
		page_data := matched[start:end]

		writeJSON(w, 200, map[string]any{
			"events":      page_data,
			"total":       total,
			"page":        page,
			"page_size":   pageSize,
			"total_pages": int(math.Ceil(float64(total) / float64(pageSize))),
		})
	}
}

// --- Analytics ---

func handleAnalyticsSummary(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pid := r.Context().Value(ctxProjectID).(string)
		period := r.URL.Query().Get("period")
		if period == "" {
			period = "24h"
		}
		var since time.Time
		switch period {
		case "1h":
			since = time.Now().Add(-1 * time.Hour)
		case "7d":
			since = time.Now().Add(-7 * 24 * time.Hour)
		case "30d":
			since = time.Now().Add(-30 * 24 * time.Hour)
		default:
			since = time.Now().Add(-24 * time.Hour)
		}

		store.mu.RLock()
		var latencies []float64
		statusCount := map[string]int64{}
		pathStats := map[string]*struct {
			count    int64
			errors   int64
			latencies []float64
		}{}
		totalRequests := int64(0)

		for _, e := range store.events {
			if e.ProjectID != pid {
				continue
			}
			if e.OccurredAt.Before(since) {
				continue
			}
			totalRequests++
			latencies = append(latencies, e.LatencyMs)

			switch {
			case e.StatusCode >= 500:
				statusCount["5xx"]++
			case e.StatusCode >= 400:
				statusCount["4xx"]++
			case e.StatusCode >= 300:
				statusCount["3xx"]++
			default:
				statusCount["2xx"]++
			}

			key := e.Method + " " + e.Path
			if _, ok := pathStats[key]; !ok {
				pathStats[key] = &struct {
					count    int64
					errors   int64
					latencies []float64
				}{}
			}
			ps := pathStats[key]
			ps.count++
			ps.latencies = append(ps.latencies, e.LatencyMs)
			if e.StatusCode >= 400 {
				ps.errors++
			}
		}
		store.mu.RUnlock()

		sort.Float64s(latencies)
		percentile := func(p float64) float64 {
			if len(latencies) == 0 {
				return 0
			}
			idx := int(math.Ceil(p/100*float64(len(latencies)))) - 1
			if idx < 0 {
				idx = 0
			}
			return math.Round(latencies[idx]*100) / 100
		}
		avg := func(vals []float64) float64 {
			if len(vals) == 0 {
				return 0
			}
			sum := 0.0
			for _, v := range vals {
				sum += v
			}
			return math.Round(sum/float64(len(vals))*100) / 100
		}

		errorCount := statusCount["4xx"] + statusCount["5xx"]
		errorRate := 0.0
		availability := 100.0
		if totalRequests > 0 {
			errorRate = math.Round(float64(errorCount)/float64(totalRequests)*10000) / 100
			availability = math.Round(float64(totalRequests-statusCount["5xx"])/float64(totalRequests)*10000) / 100
		}

		// Top 5 paths by request count
		type pathEntry struct {
			key   string
			count int64
		}
		var paths []pathEntry
		for k, v := range pathStats {
			paths = append(paths, pathEntry{k, v.count})
		}
		sort.Slice(paths, func(i, j int) bool { return paths[i].count > paths[j].count })
		if len(paths) > 5 {
			paths = paths[:5]
		}

		topPaths := make([]map[string]any, len(paths))
		for i, p := range paths {
			ps := pathStats[p.key]
			sort.Float64s(ps.latencies)
			pathErrorRate := 0.0
			if ps.count > 0 {
				pathErrorRate = math.Round(float64(ps.errors)/float64(ps.count)*10000) / 100
			}
			parts := strings.SplitN(p.key, " ", 2)
			topPaths[i] = map[string]any{
				"method":         parts[0],
				"path":           parts[1],
				"request_count":  ps.count,
				"error_rate_pct": pathErrorRate,
				"p95_latency_ms": percentile(95),
			}
		}

		writeJSON(w, 200, map[string]any{
			"project_id":       pid,
			"period":           period,
			"total_requests":   totalRequests,
			"error_rate_pct":   errorRate,
			"availability_pct": availability,
			"p50_latency_ms":   percentile(50),
			"p95_latency_ms":   percentile(95),
			"p99_latency_ms":   percentile(99),
			"avg_latency_ms":   avg(latencies),
			"status_breakdown": statusCount,
			"top_paths":        topPaths,
			"computed_at":      time.Now().UTC(),
		})
	}
}

func handleLatencyTimeseries(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pid := r.Context().Value(ctxProjectID).(string)
		since := time.Now().Add(-24 * time.Hour)

		// Bucket events into 1-hour windows
		buckets := map[string][]float64{}
		store.mu.RLock()
		for _, e := range store.events {
			if e.ProjectID != pid || e.OccurredAt.Before(since) {
				continue
			}
			bucket := e.OccurredAt.Truncate(time.Hour).Format(time.RFC3339)
			buckets[bucket] = append(buckets[bucket], e.LatencyMs)
		}
		store.mu.RUnlock()

		type point struct {
			Time     string  `json:"time"`
			P50      float64 `json:"p50"`
			P95      float64 `json:"p95"`
			Requests int     `json:"requests"`
		}
		var points []point
		for bucket, lats := range buckets {
			sort.Float64s(lats)
			p50idx := int(0.5 * float64(len(lats)))
			p95idx := int(0.95 * float64(len(lats)))
			if p95idx >= len(lats) {
				p95idx = len(lats) - 1
			}
			points = append(points, point{
				Time:     bucket,
				P50:      math.Round(lats[p50idx]*100) / 100,
				P95:      math.Round(lats[p95idx]*100) / 100,
				Requests: len(lats),
			})
		}
		sort.Slice(points, func(i, j int) bool { return points[i].Time < points[j].Time })
		writeJSON(w, 200, points)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Seed Data — pre-loads a demo user, project, API key, and realistic events
// ─────────────────────────────────────────────────────────────────────────────

func seed(store *Store) (demoAPIKey string) {
	user := &User{
		ID:           newID(),
		Email:        "demo@apex.dev",
		Name:         "Demo User",
		PasswordHash: hashPassword("password123"),
		CreatedAt:    time.Now().Add(-30 * 24 * time.Hour),
	}
	store.users[user.ID] = user
	store.byEmail[user.Email] = user

	proj := &Project{
		ID:          newID(),
		OwnerID:     user.ID,
		Name:        "My API (Demo)",
		Description: "Pre-seeded demo project with 200 sample events",
		CreatedAt:   time.Now().Add(-14 * 24 * time.Hour),
	}
	store.projects[proj.ID] = proj

	plaintext := generateAPIKey()
	hash := hashKey(plaintext)
	apiKey := &APIKey{
		ID:        newID(),
		ProjectID: proj.ID,
		Name:      "Demo SDK Key",
		KeyPrefix: plaintext[:12],
		KeyHash:   hash,
		CreatedAt: time.Now().Add(-14 * 24 * time.Hour),
	}
	store.apiKeys[hash] = apiKey

	// Generate 200 realistic events spread over the last 48 hours
	endpoints := []struct {
		method string
		path   string
		p50    float64 // typical latency
		errPct float64 // error rate
	}{
		{"GET", "/api/users", 35, 0.02},
		{"POST", "/api/users", 85, 0.05},
		{"GET", "/api/users/{id}", 28, 0.08},
		{"PUT", "/api/users/{id}", 110, 0.03},
		{"DELETE", "/api/users/{id}", 45, 0.01},
		{"GET", "/api/products", 55, 0.01},
		{"POST", "/api/products", 130, 0.04},
		{"GET", "/api/orders", 78, 0.02},
		{"POST", "/api/orders", 210, 0.10},
		{"GET", "/api/health", 5, 0.00},
		{"GET", "/api/auth/me", 22, 0.03},
		{"POST", "/api/auth/login", 95, 0.15},
	}
	statuses := []int{200, 200, 200, 200, 200, 201, 204, 400, 404, 500}

	rng := func(n int) int {
		b, _ := rand.Int(rand.Reader, big.NewInt(int64(n)))
		return int(b.Int64())
	}
	rngFloat := func(base, jitter float64) float64 {
		b, _ := rand.Int(rand.Reader, big.NewInt(1000))
		return base + (float64(b.Int64())/1000)*jitter - jitter/2
	}

	for i := 0; i < 200; i++ {
		ep := endpoints[rng(len(endpoints))]
		ago := time.Duration(rng(48)) * time.Hour
		ago += time.Duration(rng(60)) * time.Minute

		latency := rngFloat(ep.p50, ep.p50*1.5)
		if latency < 1 {
			latency = 1
		}

		statusCode := 200
		roll := rngFloat(0, 1)
		if roll < ep.errPct*0.3 {
			statusCode = 500
		} else if roll < ep.errPct {
			statusCode = 400 + rng(5)*100/100
			if statusCode > 404 {
				statusCode = 404
			}
		} else {
			statusCode = statuses[rng(7)] // bias toward 2xx
		}

		store.events = append(store.events, &Event{
			ID:           newID(),
			ProjectID:    proj.ID,
			Method:       ep.method,
			Path:         ep.path,
			StatusCode:   statusCode,
			LatencyMs:    math.Round(latency*100) / 100,
			RequestSize:  int64(rng(2048)),
			ResponseSize: int64(rng(8192)),
			UserAgent:    "Mozilla/5.0 (demo)",
			OccurredAt:   time.Now().Add(-ago),
		})
	}

	slog.Info("seeded demo data",
		"user", user.Email,
		"project", proj.Name,
		"project_id", proj.ID,
		"events", len(store.events),
		"api_key", plaintext,
	)
	return plaintext
}

// ─────────────────────────────────────────────────────────────────────────────
// Router
// ─────────────────────────────────────────────────────────────────────────────

func newRouter(store *Store) http.Handler {
	r := chi.NewRouter()

	r.Use(chimiddleware.RequestID)
	r.Use(chimiddleware.RealIP)
	r.Use(chimiddleware.Recoverer)
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins: []string{"*"},
		AllowedMethods: []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowedHeaders: []string{"*"},
	}))
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			next.ServeHTTP(w, r)
			slog.Info("→",
				"method", r.Method,
				"path", r.URL.Path,
				"latency", time.Since(start).Round(time.Microsecond).String(),
			)
		})
	})

	r.Get("/health", handleHealth)
	r.Get("/ready", handleHealth)

	r.Route("/api/v1", func(r chi.Router) {
		// Public
		r.Post("/auth/register", handleRegister(store))
		r.Post("/auth/login", handleLogin(store))

		// Ingestion (API key)
		r.Group(func(r chi.Router) {
			r.Use(apiKeyAuth(store))
			r.Post("/ingest/events", handleIngest(store))
			r.Post("/ingest/batch", handleIngestBatch(store))
		})

		// Authenticated user routes (JWT)
		r.Group(func(r chi.Router) {
			r.Use(jwtAuth)
			r.Get("/auth/me", handleMe(store))

			r.Get("/projects", handleListProjects(store))
			r.Post("/projects", handleCreateProject(store))

			r.Route("/projects/{projectID}", func(r chi.Router) {
				r.Use(projectAccess(store))
				r.Get("/", handleGetProject(store))
				r.Get("/api-keys", handleListAPIKeys(store))
				r.Post("/api-keys", handleCreateAPIKey(store))
				r.Get("/events", handleListEvents(store))
				r.Get("/analytics/summary", handleAnalyticsSummary(store))
				r.Get("/analytics/timeseries", handleLatencyTimeseries(store))
			})
		})
	})

	return r
}

// ─────────────────────────────────────────────────────────────────────────────
// Main
// ─────────────────────────────────────────────────────────────────────────────

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})))

	store := NewStore()
	demoKey := seed(store)

	router := newRouter(store)

	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      router,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
	}

	go func() {
		fmt.Println()
		fmt.Println("┌─────────────────────────────────────────────────────────────────┐")
		fmt.Println("│                  🔺  Apex API Server (Demo)                     │")
		fmt.Println("├─────────────────────────────────────────────────────────────────┤")
		fmt.Printf( "│  %-65s│\n", fmt.Sprintf("Server:     http://localhost:%s", port))
		fmt.Printf( "│  %-65s│\n", "Demo login: demo@apex.dev / password123")
		fmt.Printf( "│  %-65s│\n", fmt.Sprintf("API Key:    %s", demoKey))
		fmt.Printf( "│  %-65s│\n", "Docs:       See README.md or run: curl http://localhost:8080/health")
		fmt.Println("└─────────────────────────────────────────────────────────────────┘")
		fmt.Println()
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "err", err)
			os.Exit(1)
		}
	}()

	<-quit
	slog.Info("shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
}