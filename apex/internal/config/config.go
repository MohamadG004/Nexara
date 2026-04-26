package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds all application configuration. Values come from environment
// variables — never from hardcoded defaults for secrets. The Load() function
// enforces required fields and provides sane defaults for optional ones.
type Config struct {
	// Server
	AppEnv  string
	Version string
	Port    int

	// Database
	DatabaseURL    string
	MaxDBConns     int
	DBConnIdleTime time.Duration

	// Redis
	RedisURL string

	// Auth — JWTs signed with HMAC-SHA256
	JWTSecret          string
	JWTExpiry          time.Duration
	RefreshTokenExpiry time.Duration

	// Ingestion limits
	MaxEventsPerBatch int
	MaxPayloadBytes   int64

	// Telemetry
	OTelEndpoint string // OpenTelemetry collector endpoint (optional)

	// Logging
	LogLevel string
}

// Load reads config from environment. Call once at startup; pass the returned
// *Config through dependency injection rather than accessing it globally.
func Load() (*Config, error) {
	cfg := &Config{
		AppEnv:             getEnv("APP_ENV", "development"),
		Version:            getEnv("APP_VERSION", "0.1.0"),
		Port:               getEnvInt("PORT", 8080),
		MaxDBConns:         getEnvInt("DB_MAX_CONNS", 25),
		DBConnIdleTime:     getEnvDuration("DB_CONN_IDLE_TIME", 5*time.Minute),
		JWTExpiry:          getEnvDuration("JWT_EXPIRY", 24*time.Hour),
		RefreshTokenExpiry: getEnvDuration("REFRESH_TOKEN_EXPIRY", 7*24*time.Hour),
		MaxEventsPerBatch:  getEnvInt("MAX_EVENTS_PER_BATCH", 1000),
		MaxPayloadBytes:    int64(getEnvInt("MAX_PAYLOAD_BYTES", 5*1024*1024)), // 5MB
		LogLevel:           getEnv("LOG_LEVEL", "info"),
		OTelEndpoint:       getEnv("OTEL_ENDPOINT", ""),
	}

	// Required fields — fail fast rather than running with broken config
	var missing []string
	required := map[string]*string{
		"DATABASE_URL": &cfg.DatabaseURL,
		"REDIS_URL":    &cfg.RedisURL,
		"JWT_SECRET":   &cfg.JWTSecret,
	}

	for key, dest := range required {
		val := os.Getenv(key)
		if val == "" {
			missing = append(missing, key)
			continue
		}
		*dest = val
	}

	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
	}

	// Validate JWT secret strength in production
	if cfg.AppEnv == "production" && len(cfg.JWTSecret) < 32 {
		return nil, fmt.Errorf("JWT_SECRET must be at least 32 characters in production")
	}

	return cfg, nil
}

func (c *Config) IsProduction() bool {
	return c.AppEnv == "production"
}

func (c *Config) IsDevelopment() bool {
	return c.AppEnv == "development"
}

// --- helpers ---

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func getEnvDuration(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}
