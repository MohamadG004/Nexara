package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"runtime"
	"time"

	"github.com/yourusername/apex/internal/config"
	"github.com/yourusername/apex/internal/database"
)

// HealthHandler provides liveness, readiness, and metrics endpoints.
// These are called by Kubernetes probes and monitoring systems — they must
// be fast (< 100ms) and never block on heavy operations.
type HealthHandler struct {
	db    *database.PostgresDB
	redis *database.RedisClient
	cfg   *config.Config
	start time.Time
}

func NewHealthHandler(db *database.PostgresDB, redis *database.RedisClient, cfg *config.Config) *HealthHandler {
	return &HealthHandler{db: db, redis: redis, cfg: cfg, start: time.Now()}
}

type healthResponse struct {
	Status    string            `json:"status"` // "ok" | "degraded" | "down"
	Version   string            `json:"version"`
	Uptime    string            `json:"uptime"`
	Checks    map[string]string `json:"checks"`
	Timestamp time.Time         `json:"timestamp"`
}

// Health is the liveness probe — returns 200 if the process is alive.
// Kubernetes restarts the pod if this fails. Keep it trivial.
// GET /health
func (h *HealthHandler) Health(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{
		"status":  "ok",
		"version": h.cfg.Version,
	})
}

// Ready is the readiness probe — returns 200 only if the service can handle traffic.
// Kubernetes stops routing to the pod if this fails (e.g. during startup or DB outage).
// GET /ready
func (h *HealthHandler) Ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	checks := make(map[string]string)
	status := http.StatusOK
	overall := "ok"

	// Check PostgreSQL
	if err := h.db.HealthCheck(ctx); err != nil {
		checks["postgres"] = "down: " + err.Error()
		status = http.StatusServiceUnavailable
		overall = "down"
	} else {
		checks["postgres"] = "ok"
	}

	// Check Redis
	if err := h.redis.HealthCheck(ctx); err != nil {
		checks["redis"] = "down: " + err.Error()
		if status == http.StatusOK {
			status = http.StatusServiceUnavailable
			overall = "degraded" // Redis is not critical for all endpoints
		}
	} else {
		checks["redis"] = "ok"
	}

	resp := healthResponse{
		Status:    overall,
		Version:   h.cfg.Version,
		Uptime:    time.Since(h.start).Round(time.Second).String(),
		Checks:    checks,
		Timestamp: time.Now().UTC(),
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(resp)
}

// Metrics exposes Prometheus-compatible metrics.
// In production, use prometheus/client_golang for full histogram support.
// GET /metrics
func (h *HealthHandler) Metrics(w http.ResponseWriter, r *http.Request) {
	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)

	stats := h.db.Stat()

	// Plain text Prometheus format
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprintf(w, "# HELP apex_go_goroutines Current number of goroutines\n")
	fmt.Fprintf(w, "# TYPE apex_go_goroutines gauge\n")
	fmt.Fprintf(w, "apex_go_goroutines %d\n\n", runtime.NumGoroutine())

	fmt.Fprintf(w, "# HELP apex_go_alloc_bytes Allocated heap bytes\n")
	fmt.Fprintf(w, "# TYPE apex_go_alloc_bytes gauge\n")
	fmt.Fprintf(w, "apex_go_alloc_bytes %d\n\n", memStats.Alloc)

	fmt.Fprintf(w, "# HELP apex_db_pool_total Total connections in pool\n")
	fmt.Fprintf(w, "# TYPE apex_db_pool_total gauge\n")
	fmt.Fprintf(w, "apex_db_pool_total %d\n\n", stats.TotalConns())

	fmt.Fprintf(w, "# HELP apex_db_pool_idle Idle connections in pool\n")
	fmt.Fprintf(w, "# TYPE apex_db_pool_idle gauge\n")
	fmt.Fprintf(w, "apex_db_pool_idle %d\n\n", stats.IdleConns())

	fmt.Fprintf(w, "# HELP apex_uptime_seconds Server uptime in seconds\n")
	fmt.Fprintf(w, "# TYPE apex_uptime_seconds counter\n")
	fmt.Fprintf(w, "apex_uptime_seconds %.0f\n", time.Since(h.start).Seconds())
}
