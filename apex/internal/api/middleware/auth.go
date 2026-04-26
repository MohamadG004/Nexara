package middleware

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/yourusername/apex/internal/services"
	"github.com/yourusername/apex/pkg/response"
)

// Context key types — always use typed keys, never plain strings,
// to avoid collisions across packages.
type contextKey string

const (
	UserIDKey    contextKey = "user_id"
	ProjectIDKey contextKey = "project_id"
	ClaimsKey    contextKey = "jwt_claims"
)

// ApexClaims extends standard JWT claims with our app-specific fields.
type ApexClaims struct {
	UserID string `json:"uid"`
	Email  string `json:"email"`
	jwt.RegisteredClaims
}

// JWTAuth validates Bearer tokens on protected routes.
// On success, it injects claims into the request context.
func JWTAuth(secret string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authHeader := r.Header.Get("Authorization")
			if authHeader == "" {
				response.Unauthorized(w, "missing authorization header")
				return
			}

			parts := strings.SplitN(authHeader, " ", 2)
			if len(parts) != 2 || !strings.EqualFold(parts[0], "bearer") {
				response.Unauthorized(w, "authorization header must be 'Bearer <token>'")
				return
			}

			claims := &ApexClaims{}
			token, err := jwt.ParseWithClaims(parts[1], claims, func(t *jwt.Token) (any, error) {
				// Verify algorithm — prevents the "alg:none" attack
				if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
					return nil, jwt.ErrSignatureInvalid
				}
				return []byte(secret), nil
			})

			if err != nil || !token.Valid {
				response.Unauthorized(w, "invalid or expired token")
				return
			}

			ctx := context.WithValue(r.Context(), ClaimsKey, claims)
			ctx = context.WithValue(ctx, UserIDKey, claims.UserID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// APIKeyAuth validates SDK API keys (used for event ingestion).
// Keys are stored hashed (SHA-256) in the database; only the prefix
// is used for lookup, then we compare hashes.
func APIKeyAuth(authSvc services.AuthService) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := r.Header.Get("X-API-Key")
			if key == "" {
				// Also accept Bearer for API keys (some HTTP clients set this)
				key = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			}
			if key == "" {
				response.Unauthorized(w, "missing X-API-Key header")
				return
			}

			apiKey, err := authSvc.ValidateAPIKey(r.Context(), key)
			if err != nil {
				// Don't distinguish between "not found" and "invalid" — prevents enumeration
				response.Unauthorized(w, "invalid API key")
				return
			}

			ctx := context.WithValue(r.Context(), ProjectIDKey, apiKey.ProjectID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// ProjectAccess ensures the authenticated user owns (or has access to) the
// project in the URL. Must be used after JWTAuth.
func ProjectAccess(projectSvc services.ProjectService) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			userID, ok := r.Context().Value(UserIDKey).(string)
			if !ok || userID == "" {
				response.Unauthorized(w, "unauthenticated")
				return
			}

			projectID := chi.URLParam(r, "projectID")
			if err := projectSvc.AssertAccess(r.Context(), userID, projectID); err != nil {
				if err == services.ErrForbidden {
					response.Forbidden(w, "you don't have access to this project")
					return
				}
				response.InternalError(w, r, err)
				return
			}

			ctx := context.WithValue(r.Context(), ProjectIDKey, projectID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RateLimiter implements a sliding window rate limiter backed by Redis.
// This is a token bucket at the project level for ingestion and per-user for API.
func RateLimiter(rdb services.RedisClient, limit int, window time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Rate limit key: prefer project, fall back to user, then IP
			key := ""
			if pid, ok := r.Context().Value(ProjectIDKey).(string); ok && pid != "" {
				key = "rl:project:" + pid
			} else if uid, ok := r.Context().Value(UserIDKey).(string); ok && uid != "" {
				key = "rl:user:" + uid
			} else {
				key = "rl:ip:" + r.RemoteAddr
			}

			allowed, remaining, reset, err := slidingWindowCheck(r.Context(), rdb, key, limit, window)
			if err != nil {
				// Rate limiter failure should fail open (don't block traffic) —
				// but log it for alerting
				next.ServeHTTP(w, r)
				return
			}

			// Always set rate limit headers so clients can self-throttle
			w.Header().Set("X-RateLimit-Limit", strconv.Itoa(limit))
			w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset, 10))

			if !allowed {
				w.Header().Set("Retry-After", strconv.FormatInt(reset-time.Now().Unix(), 10))
				response.TooManyRequests(w, "rate limit exceeded")
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// slidingWindowCheck uses a Redis sorted set to implement an accurate sliding
// window counter. O(log N) per request, where N is requests in the window.
// Atomicity via Lua script prevents race conditions under high concurrency.
func slidingWindowCheck(ctx context.Context, rdb services.RedisClient, key string, limit int, window time.Duration) (allowed bool, remaining int, resetAt int64, err error) {
	now := time.Now()
	windowStart := now.Add(-window).UnixMilli()
	expireAt := now.Add(window).Unix()

	script := `
		local key = KEYS[1]
		local now = tonumber(ARGV[1])
		local window_start = tonumber(ARGV[2])
		local limit = tonumber(ARGV[3])
		local expire = tonumber(ARGV[4])

		-- Remove expired entries
		redis.call('ZREMRANGEBYSCORE', key, '-inf', window_start)
		
		local count = redis.call('ZCARD', key)
		if count < limit then
			redis.call('ZADD', key, now, now .. math.random())
			redis.call('EXPIREAT', key, expire)
			return {1, limit - count - 1, expire}
		end
		return {0, 0, expire}
	`

	result, err := rdb.Eval(ctx, script,
		[]string{key},
		now.UnixMilli(), windowStart, limit, expireAt,
	).Slice()
	if err != nil {
		return false, 0, 0, err
	}

	return result[0].(int64) == 1,
		int(result[1].(int64)),
		result[2].(int64),
		nil
}

// SecurityHeaders adds standard security headers to all responses.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-XSS-Protection", "1; mode=block")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		// Content-Security-Policy is set by the frontend separately
		h.Set("Permissions-Policy", "geolocation=(), microphone=(), camera=()")
		next.ServeHTTP(w, r)
	})
}
