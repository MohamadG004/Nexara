package api

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/rs/zerolog"

	"github.com/yourusername/apex/internal/api/handlers"
	"github.com/yourusername/apex/internal/api/middleware"
	"github.com/yourusername/apex/internal/config"
	"github.com/yourusername/apex/internal/database"
	"github.com/yourusername/apex/internal/services"
)

// RouterDeps bundles all dependencies the router needs to wire handlers.
// Using a struct avoids a long parameter list and makes testing easy —
// you can substitute fakes for any field.
type RouterDeps struct {
	Config *config.Config
	DB     *database.PostgresDB
	Redis  *database.RedisClient
	Logger *zerolog.Logger
}

// NewRouter builds the complete chi router with all middleware and routes.
// chi was chosen over Gin/Echo because it's stdlib-compatible (http.Handler),
// has excellent middleware composition, and uses zero reflection.
func NewRouter(deps RouterDeps) http.Handler {
	r := chi.NewRouter()

	// --- Global middleware stack (order matters) ---

	// Request ID — attached to context and response header for traceability
	r.Use(chimiddleware.RequestID)

	// Real IP — extract from X-Forwarded-For / X-Real-IP (set by load balancer)
	r.Use(chimiddleware.RealIP)

	// Structured request logging with zerolog
	r.Use(middleware.RequestLogger(deps.Logger))

	// Recover from panics and return 500 instead of crashing
	r.Use(chimiddleware.Recoverer)

	// Security headers — applied before any response body
	r.Use(middleware.SecurityHeaders)

	// CORS — permissive in development, locked down in production
	r.Use(cors.Handler(corsOptions(deps.Config)))

	// Timeout — cancel context after 30s to prevent goroutine leaks
	r.Use(chimiddleware.Timeout(30 * time.Second))

	// Content-Type enforcement for non-GET requests
	r.Use(chimiddleware.AllowContentType("application/json"))

	// --- Service layer initialization ---
	// Services encapsulate business logic; handlers are thin adapters
	svc := services.NewContainer(deps.DB, deps.Redis, deps.Config, deps.Logger)

	// --- Handler initialization ---
	healthH := handlers.NewHealthHandler(deps.DB, deps.Redis, deps.Config)
	authH := handlers.NewAuthHandler(svc.Auth)
	projectH := handlers.NewProjectHandler(svc.Project)
	eventH := handlers.NewEventHandler(svc.Event)
	analyticsH := handlers.NewAnalyticsHandler(svc.Analytics)
	alertH := handlers.NewAlertHandler(svc.Alert)

	// --- Route groups ---

	// Health/readiness — no auth, no rate limiting (used by k8s probes)
	r.Get("/health", healthH.Health)
	r.Get("/ready", healthH.Ready)
	r.Get("/metrics", healthH.Metrics) // Prometheus metrics endpoint

	// API v1
	r.Route("/api/v1", func(r chi.Router) {

		// ── Public routes (no auth) ──────────────────────────────────────
		r.Group(func(r chi.Router) {
			r.Post("/auth/register", authH.Register)
			r.Post("/auth/login", authH.Login)
			r.Post("/auth/refresh", authH.RefreshToken)
		})

		// ── Ingestion endpoint — authenticated by SDK API key ─────────────
		// Separate auth so SDKs use API keys, not user JWTs
		r.Group(func(r chi.Router) {
			r.Use(middleware.APIKeyAuth(svc.Auth))
			r.Use(middleware.RateLimiter(deps.Redis, 10000, time.Minute)) // 10k req/min per key
			r.Post("/ingest/events", eventH.Ingest)
			r.Post("/ingest/batch", eventH.IngestBatch)
		})

		// ── Authenticated user routes ─────────────────────────────────────
		r.Group(func(r chi.Router) {
			r.Use(middleware.JWTAuth(deps.Config.JWTSecret))
			r.Use(middleware.RateLimiter(deps.Redis, 100, time.Minute))

			// Auth
			r.Post("/auth/logout", authH.Logout)
			r.Get("/auth/me", authH.Me)

			// Projects (multi-tenant isolation)
			r.Route("/projects", func(r chi.Router) {
				r.Get("/", projectH.List)
				r.Post("/", projectH.Create)
				r.Route("/{projectID}", func(r chi.Router) {
					r.Use(middleware.ProjectAccess(svc.Project)) // ownership check
					r.Get("/", projectH.Get)
					r.Put("/", projectH.Update)
					r.Delete("/", projectH.Delete)

					// API Keys for this project
					r.Get("/api-keys", projectH.ListAPIKeys)
					r.Post("/api-keys", projectH.CreateAPIKey)
					r.Delete("/api-keys/{keyID}", projectH.RevokeAPIKey)

					// Analytics for this project
					r.Route("/analytics", func(r chi.Router) {
						r.Get("/summary", analyticsH.Summary)
						r.Get("/latency", analyticsH.LatencyPercentiles)
						r.Get("/errors", analyticsH.ErrorBreakdown)
						r.Get("/endpoints", analyticsH.TopEndpoints)
						r.Get("/timeseries", analyticsH.Timeseries)
					})

					// Events query
					r.Route("/events", func(r chi.Router) {
						r.Get("/", eventH.List)
						r.Get("/{eventID}", eventH.Get)
					})

					// Alerts
					r.Route("/alerts", func(r chi.Router) {
						r.Get("/", alertH.List)
						r.Post("/", alertH.Create)
						r.Put("/{alertID}", alertH.Update)
						r.Delete("/{alertID}", alertH.Delete)
					})
				})
			})
		})
	})

	// WebSocket endpoint for real-time event streaming (outside /api/v1 REST)
	r.Get("/ws/projects/{projectID}/stream", eventH.Stream)

	return r
}

func corsOptions(cfg *config.Config) cors.Options {
	if cfg.IsDevelopment() {
		return cors.Options{
			AllowedOrigins:   []string{"*"},
			AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
			AllowedHeaders:   []string{"*"},
			AllowCredentials: false,
		}
	}
	return cors.Options{
		AllowedOrigins:   []string{"https://app.apex.dev", "https://apex.dev"},
		AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Authorization", "Content-Type", "X-Request-ID"},
		AllowCredentials: true,
		MaxAge:           300,
	}
}
