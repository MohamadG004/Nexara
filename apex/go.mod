module github.com/yourusername/apex

go 1.23

require (
	// HTTP router — stdlib compatible, zero reflection, excellent middleware
	github.com/go-chi/chi/v5 v5.1.0
	github.com/go-chi/cors v1.2.1

	// PostgreSQL driver — pgx v5 is faster than lib/pq, native type support
	github.com/jackc/pgx/v5 v5.7.1

	// Redis — go-redis v9 has generics and context-first API
	github.com/redis/go-redis/v9 v9.7.0

	// JWT — golang-jwt is the actively maintained fork of dgrijalva/jwt-go
	github.com/golang-jwt/jwt/v5 v5.2.1

	// Structured logging — zerolog is the fastest structured logger for Go
	github.com/rs/zerolog v1.33.0

	// Database migrations — supports file-based and embedded migrations
	github.com/golang-migrate/migrate/v4 v4.18.1

	// WebSocket — gorilla/websocket is the de-facto standard
	github.com/gorilla/websocket v1.5.3

	// Validation — go-playground/validator with struct tags
	github.com/go-playground/validator/v10 v10.23.0

	// Password hashing — bcrypt is the standard; use cost 12+
	golang.org/x/crypto v0.28.0

	// UUID generation
	github.com/google/uuid v1.6.0
)
