package models

import (
	"time"
)

// Event is the core domain model — a single captured API request.
// This is what SDKs send and what the analytics layer aggregates.
type Event struct {
	ID           string            `json:"id" db:"id"`
	ProjectID    string            `json:"project_id" db:"project_id"`
	TraceID      string            `json:"trace_id,omitempty" db:"trace_id"`
	Method       string            `json:"method" db:"method"`
	Path         string            `json:"path" db:"path"`
	StatusCode   int               `json:"status_code" db:"status_code"`
	LatencyMs    float64           `json:"latency_ms" db:"latency_ms"`
	RequestSize  int64             `json:"request_size_bytes" db:"request_size_bytes"`
	ResponseSize int64             `json:"response_size_bytes" db:"response_size_bytes"`
	UserAgent    string            `json:"user_agent,omitempty" db:"user_agent"`
	IP           string            `json:"ip,omitempty" db:"ip"`
	Tags         map[string]string `json:"tags,omitempty" db:"tags"`
	OccurredAt   time.Time         `json:"occurred_at" db:"occurred_at"`
	CreatedAt    time.Time         `json:"created_at" db:"created_at"`
}

// IsError returns true if the event represents an HTTP error.
func (e Event) IsError() bool {
	return e.StatusCode >= 400
}

// IsServerError returns true for 5xx responses.
func (e Event) IsServerError() bool {
	return e.StatusCode >= 500
}

// User represents an Apex platform user.
type User struct {
	ID           string    `json:"id" db:"id"`
	Email        string    `json:"email" db:"email"`
	Name         string    `json:"name" db:"name"`
	PasswordHash string    `json:"-" db:"password_hash"` // never serialized
	CreatedAt    time.Time `json:"created_at" db:"created_at"`
	UpdatedAt    time.Time `json:"updated_at" db:"updated_at"`
}

// Project is a logical grouping of events — typically one per application/service.
type Project struct {
	ID          string    `json:"id" db:"id"`
	OwnerID     string    `json:"owner_id" db:"owner_id"`
	Name        string    `json:"name" db:"name"`
	Slug        string    `json:"slug" db:"slug"`
	Description string    `json:"description,omitempty" db:"description"`
	CreatedAt   time.Time `json:"created_at" db:"created_at"`
	UpdatedAt   time.Time `json:"updated_at" db:"updated_at"`
}

// APIKey is used by SDKs to authenticate event ingestion.
// Only the SHA-256 hash is stored; the plaintext is shown once on creation.
type APIKey struct {
	ID          string     `json:"id" db:"id"`
	ProjectID   string     `json:"project_id" db:"project_id"`
	Name        string     `json:"name" db:"name"`
	KeyHash     string     `json:"-" db:"key_hash"`   // SHA-256 hex, never returned
	KeyPrefix   string     `json:"prefix" db:"key_prefix"` // first 8 chars, for display
	LastUsedAt  *time.Time `json:"last_used_at,omitempty" db:"last_used_at"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty" db:"expires_at"`
	CreatedAt   time.Time  `json:"created_at" db:"created_at"`
}

// Alert defines a threshold-based alert rule for a project.
type Alert struct {
	ID          string            `json:"id" db:"id"`
	ProjectID   string            `json:"project_id" db:"project_id"`
	Name        string            `json:"name" db:"name"`
	Metric      AlertMetric       `json:"metric" db:"metric"`
	Operator    AlertOperator     `json:"operator" db:"operator"`
	Threshold   float64           `json:"threshold" db:"threshold"`
	WindowSecs  int               `json:"window_secs" db:"window_secs"` // evaluation window
	Channels    []AlertChannel    `json:"channels" db:"channels"`       // slack, email, webhook
	Enabled     bool              `json:"enabled" db:"enabled"`
	CreatedAt   time.Time         `json:"created_at" db:"created_at"`
}

// AlertMetric is the observable being tracked.
type AlertMetric string

const (
	MetricErrorRate    AlertMetric = "error_rate"    // % of 4xx/5xx
	MetricLatencyP95   AlertMetric = "latency_p95"   // 95th percentile ms
	MetricRequestCount AlertMetric = "request_count" // requests per window
	MetricAvailability AlertMetric = "availability"  // % of non-5xx
)

// AlertOperator defines comparison direction.
type AlertOperator string

const (
	OpGreaterThan AlertOperator = "gt"
	OpLessThan    AlertOperator = "lt"
)

// AlertChannel is a notification destination.
type AlertChannel struct {
	Type    string `json:"type"`    // "email" | "slack" | "webhook"
	Target  string `json:"target"`  // email address, Slack webhook URL, or generic webhook URL
}

// AnalyticsSummary is a pre-aggregated snapshot for the dashboard.
type AnalyticsSummary struct {
	ProjectID      string    `json:"project_id"`
	Period         string    `json:"period"` // "1h" | "24h" | "7d" | "30d"
	TotalRequests  int64     `json:"total_requests"`
	ErrorRate      float64   `json:"error_rate_pct"`
	P50LatencyMs   float64   `json:"p50_latency_ms"`
	P95LatencyMs   float64   `json:"p95_latency_ms"`
	P99LatencyMs   float64   `json:"p99_latency_ms"`
	AvgLatencyMs   float64   `json:"avg_latency_ms"`
	Availability   float64   `json:"availability_pct"`
	TopPaths       []PathStat `json:"top_paths"`
	StatusBreakdown map[string]int64 `json:"status_breakdown"`
	ComputedAt     time.Time `json:"computed_at"`
}

// PathStat summarizes metrics for a specific endpoint.
type PathStat struct {
	Method       string  `json:"method"`
	Path         string  `json:"path"`
	RequestCount int64   `json:"request_count"`
	ErrorRate    float64 `json:"error_rate_pct"`
	P95LatencyMs float64 `json:"p95_latency_ms"`
}
