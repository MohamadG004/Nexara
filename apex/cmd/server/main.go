package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/yourusername/apex/internal/api"
	"github.com/yourusername/apex/internal/config"
	"github.com/yourusername/apex/internal/database"
	"github.com/yourusername/apex/pkg/logger"
)

// @title           Apex API
// @version         1.0
// @description     Real-time API Observability & Analytics Platform
// @termsOfService  https://apex.dev/terms

// @contact.name   API Support
// @contact.url    https://apex.dev/support
// @contact.email  support@apex.dev

// @host      localhost:8080
// @BasePath  /api/v1

// @securityDefinitions.apikey BearerAuth
// @in header
// @name Authorization

func main() {
	// Load configuration — fails fast if required env vars are missing
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config: %v\n", err)
		os.Exit(1)
	}

	// Initialize structured logger (zerolog) — first so all downstream has it
	log := logger.New(cfg.LogLevel, cfg.AppEnv)
	log.Info().Str("env", cfg.AppEnv).Str("version", cfg.Version).Msg("starting apex server")

	// Connect to PostgreSQL with connection pooling (pgx v5)
	db, err := database.NewPostgres(cfg.DatabaseURL)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to connect to postgres")
	}
	defer db.Close()
	log.Info().Msg("connected to postgres")

	// Connect to Redis for rate limiting and pub/sub
	rdb, err := database.NewRedis(cfg.RedisURL)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to connect to redis")
	}
	defer rdb.Close()
	log.Info().Msg("connected to redis")

	// Run database migrations on startup
	if err := database.Migrate(cfg.DatabaseURL, "migrations"); err != nil {
		log.Fatal().Err(err).Msg("migration failed")
	}
	log.Info().Msg("database migrations applied")

	// Build and wire the HTTP router with all dependencies injected
	router := api.NewRouter(api.RouterDeps{
		Config: cfg,
		DB:     db,
		Redis:  rdb,
		Logger: log,
	})

	srv := &http.Server{
		Addr:         fmt.Sprintf(":%d", cfg.Port),
		Handler:      router,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Start server in goroutine so we can listen for shutdown signals
	serverErr := make(chan error, 1)
	go func() {
		log.Info().Int("port", cfg.Port).Msg("server listening")
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			serverErr <- err
		}
	}()

	// Graceful shutdown: wait for SIGTERM or SIGINT (e.g. Kubernetes rolling deploy)
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-serverErr:
		log.Fatal().Err(err).Msg("server error")
	case sig := <-quit:
		log.Info().Str("signal", sig.String()).Msg("shutdown signal received")
	}

	// Give in-flight requests up to 30s to complete before hard exit
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		log.Error().Err(err).Msg("forced shutdown")
	}
	log.Info().Msg("server exited cleanly")
}
