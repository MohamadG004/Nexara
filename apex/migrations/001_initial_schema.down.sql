-- Migration: 001_initial_schema.down.sql
-- Reverses the initial schema. Order matters: drop dependents first.

DROP TABLE IF EXISTS alert_incidents CASCADE;
DROP TABLE IF EXISTS alerts CASCADE;
DROP MATERIALIZED VIEW IF EXISTS event_stats_hourly CASCADE;
DROP TABLE IF EXISTS events CASCADE;
DROP TABLE IF EXISTS api_keys CASCADE;
DROP TABLE IF EXISTS projects CASCADE;
DROP TABLE IF EXISTS users CASCADE;

DROP FUNCTION IF EXISTS update_updated_at CASCADE;

DROP EXTENSION IF EXISTS timescaledb CASCADE;
DROP EXTENSION IF EXISTS pg_trgm;
DROP EXTENSION IF EXISTS pgcrypto;
