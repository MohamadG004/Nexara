package middleware

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/rs/zerolog"
)

// RequestLogger returns a zerolog-based structured request logging middleware.
// Each request logs: method, path, status, latency, request ID, user agent, IP.
// In production, logs are JSON; in development, they're human-readable with colors.
// This is the single place where HTTP request/response observability is wired.
func RequestLogger(log *zerolog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()

			// Wrap ResponseWriter to capture status code and bytes written
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)

			defer func() {
				latency := time.Since(start)
				status := ww.Status()

				// Determine log level based on status code
				var event *zerolog.Event
				switch {
				case status >= 500:
					event = log.Error()
				case status >= 400:
					event = log.Warn()
				default:
					event = log.Info()
				}

				event.
					Str("request_id", middleware.GetReqID(r.Context())).
					Str("method", r.Method).
					Str("path", r.URL.Path).
					Str("query", r.URL.RawQuery).
					Int("status", status).
					Int("bytes", ww.BytesWritten()).
					Dur("latency", latency).
					Str("ip", r.RemoteAddr).
					Str("user_agent", r.UserAgent()).
					Msg("request")
			}()

			next.ServeHTTP(ww, r)
		})
	}
}
