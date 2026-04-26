package database

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

// PostgresDB wraps pgxpool.Pool and adds app-level helpers.
// We use pgx instead of database/sql + lib/pq because pgx v5 has:
// - native support for PostgreSQL types (arrays, JSONB, UUID, etc.)
// - better performance (no interface{} boxing)
// - structured error types
type PostgresDB struct {
	*pgxpool.Pool
}

// NewPostgres creates a connection pool to PostgreSQL.
// pgxpool is safe for concurrent use and manages connection lifecycle.
func NewPostgres(dsn string) (*PostgresDB, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}

	// Pool sizing: start conservative, tune with load testing.
	// Formula baseline: (num_cores * 2) + effective_spindle_count
	cfg.MaxConns = 25
	cfg.MinConns = 5
	cfg.MaxConnLifetime = 1 * time.Hour
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.HealthCheckPeriod = 1 * time.Minute

	// Fail on startup if we can't reach the DB — better than silent failure
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}

	// Ping confirms the pool can acquire a connection
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}

	return &PostgresDB{Pool: pool}, nil
}

// HealthCheck returns nil if the database is reachable.
// Used by the /health endpoint and readiness probe.
func (db *PostgresDB) HealthCheck(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return db.Ping(ctx)
}

// RedisClient wraps go-redis and adds app-level helpers.
type RedisClient struct {
	*redis.Client
}

// NewRedis creates and validates a Redis connection.
// go-redis v9 uses generics and context-first APIs.
func NewRedis(url string) (*RedisClient, error) {
	opts, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("parse redis url: %w", err)
	}

	// Sensible timeouts — Redis should be fast; if it's not, fail loudly
	opts.DialTimeout = 5 * time.Second
	opts.ReadTimeout = 3 * time.Second
	opts.WriteTimeout = 3 * time.Second
	opts.PoolSize = 20

	client := redis.NewClient(opts)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("ping redis: %w", err)
	}

	return &RedisClient{Client: client}, nil
}

// HealthCheck returns nil if Redis is reachable.
func (r *RedisClient) HealthCheck(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return r.Ping(ctx).Err()
}
