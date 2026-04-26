-- Migration: 001_initial_schema.up.sql
-- Creates the core Apex schema. Using UUIDs (gen_random_uuid()) for all PKs:
-- - Globally unique (safe to expose in URLs)
-- - No sequential guessing attack
-- - Easy to generate on client side if needed
-- TimescaleDB hypertable for events gives us automatic time-series partitioning.

-- Enable required extensions
CREATE EXTENSION IF NOT EXISTS "pgcrypto";   -- gen_random_uuid()
CREATE EXTENSION IF NOT EXISTS "pg_trgm";    -- trigram indexes for path search
CREATE EXTENSION IF NOT EXISTS "timescaledb" CASCADE; -- time-series partitioning

-- ─── Users ─────────────────────────────────────────────────────────────────

CREATE TABLE users (
    id            UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    email         TEXT        NOT NULL UNIQUE,
    name          TEXT        NOT NULL,
    password_hash TEXT        NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_users_email ON users(email);

-- ─── Projects ───────────────────────────────────────────────────────────────

CREATE TABLE projects (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id    UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name        TEXT        NOT NULL,
    slug        TEXT        NOT NULL,
    description TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    
    UNIQUE(owner_id, slug) -- slugs are unique per user
);

CREATE INDEX idx_projects_owner_id ON projects(owner_id);

-- ─── API Keys ───────────────────────────────────────────────────────────────

CREATE TABLE api_keys (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id  UUID        NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name        TEXT        NOT NULL,
    key_hash    TEXT        NOT NULL UNIQUE, -- SHA-256 hex of the full key
    key_prefix  TEXT        NOT NULL,        -- first 8 chars, for display only
    last_used_at TIMESTAMPTZ,
    expires_at  TIMESTAMPTZ,                 -- NULL = never expires
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_api_keys_project_id ON api_keys(project_id);
CREATE INDEX idx_api_keys_key_prefix ON api_keys(key_prefix); -- fast lookup by prefix

-- ─── Events (time-series) ───────────────────────────────────────────────────
-- This is the hot table — millions of rows. TimescaleDB partitions it by
-- occurred_at automatically. We index path with trigram for LIKE queries.

CREATE TABLE events (
    id             UUID        NOT NULL DEFAULT gen_random_uuid(),
    project_id     UUID        NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    trace_id       TEXT,
    method         TEXT        NOT NULL,
    path           TEXT        NOT NULL,
    status_code    SMALLINT    NOT NULL,
    latency_ms     DOUBLE PRECISION NOT NULL,
    request_size_bytes  BIGINT NOT NULL DEFAULT 0,
    response_size_bytes BIGINT NOT NULL DEFAULT 0,
    user_agent     TEXT,
    ip             INET,        -- use INET type for proper IP storage
    tags           JSONB,       -- arbitrary metadata; JSONB is indexed and queryable
    occurred_at    TIMESTAMPTZ NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    
    PRIMARY KEY (id, occurred_at) -- TimescaleDB requires time column in PK
);

-- Convert to hypertable — partitions by month automatically
SELECT create_hypertable('events', 'occurred_at', chunk_time_interval => INTERVAL '1 month');

-- Indexes for common query patterns
CREATE INDEX idx_events_project_time ON events(project_id, occurred_at DESC);
CREATE INDEX idx_events_status ON events(project_id, status_code, occurred_at DESC);
CREATE INDEX idx_events_path ON events USING GIN (path gin_trgm_ops); -- trigram for LIKE
CREATE INDEX idx_events_tags ON events USING GIN (tags);             -- JSONB containment

-- Continuous aggregate: pre-compute hourly stats for fast dashboard queries
-- This is the key performance optimization — aggregate queries on millions of
-- events become instant reads from this materialized view.
CREATE MATERIALIZED VIEW event_stats_hourly
WITH (timescaledb.continuous) AS
SELECT
    project_id,
    time_bucket('1 hour', occurred_at) AS bucket,
    method,
    path,
    COUNT(*)                                          AS request_count,
    COUNT(*) FILTER (WHERE status_code >= 400)        AS error_count,
    COUNT(*) FILTER (WHERE status_code >= 500)        AS server_error_count,
    AVG(latency_ms)                                   AS avg_latency_ms,
    PERCENTILE_CONT(0.50) WITHIN GROUP (ORDER BY latency_ms) AS p50_latency_ms,
    PERCENTILE_CONT(0.95) WITHIN GROUP (ORDER BY latency_ms) AS p95_latency_ms,
    PERCENTILE_CONT(0.99) WITHIN GROUP (ORDER BY latency_ms) AS p99_latency_ms,
    SUM(request_size_bytes)                           AS total_request_bytes,
    SUM(response_size_bytes)                          AS total_response_bytes
FROM events
GROUP BY project_id, bucket, method, path
WITH NO DATA;

-- Refresh policy: keep aggregate up to 1 minute fresh
SELECT add_continuous_aggregate_policy('event_stats_hourly',
    start_offset => INTERVAL '2 hours',
    end_offset   => INTERVAL '1 minute',
    schedule_interval => INTERVAL '1 minute'
);

-- Data retention: drop raw events older than 90 days (keep aggregates forever)
SELECT add_retention_policy('events', INTERVAL '90 days');

-- ─── Alerts ─────────────────────────────────────────────────────────────────

CREATE TABLE alerts (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id   UUID        NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name         TEXT        NOT NULL,
    metric       TEXT        NOT NULL CHECK (metric IN ('error_rate', 'latency_p95', 'request_count', 'availability')),
    operator     TEXT        NOT NULL CHECK (operator IN ('gt', 'lt')),
    threshold    DOUBLE PRECISION NOT NULL,
    window_secs  INTEGER     NOT NULL DEFAULT 300,
    channels     JSONB       NOT NULL DEFAULT '[]',
    enabled      BOOLEAN     NOT NULL DEFAULT TRUE,
    last_fired_at TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_alerts_project_id ON alerts(project_id);
CREATE INDEX idx_alerts_enabled ON alerts(enabled) WHERE enabled = TRUE;

-- ─── Alert Incidents ────────────────────────────────────────────────────────

CREATE TABLE alert_incidents (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    alert_id    UUID        NOT NULL REFERENCES alerts(id) ON DELETE CASCADE,
    triggered_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    resolved_at TIMESTAMPTZ,
    value       DOUBLE PRECISION NOT NULL, -- the value that breached the threshold
    notified    BOOLEAN     NOT NULL DEFAULT FALSE
);

CREATE INDEX idx_incidents_alert_id ON alert_incidents(alert_id);

-- ─── Triggers for updated_at ────────────────────────────────────────────────

CREATE OR REPLACE FUNCTION update_updated_at()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = NOW();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_users_updated_at
    BEFORE UPDATE ON users
    FOR EACH ROW EXECUTE FUNCTION update_updated_at();

CREATE TRIGGER trg_projects_updated_at
    BEFORE UPDATE ON projects
    FOR EACH ROW EXECUTE FUNCTION update_updated_at();

CREATE TRIGGER trg_alerts_updated_at
    BEFORE UPDATE ON alerts
    FOR EACH ROW EXECUTE FUNCTION update_updated_at();
