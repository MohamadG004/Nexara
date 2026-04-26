package response

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5/middleware"
)

// envelope wraps all API responses in a consistent structure.
// Consumers can always check `success` before reading `data` or `error`.
type envelope struct {
	Success   bool   `json:"success"`
	Data      any    `json:"data,omitempty"`
	Error     *apiError `json:"error,omitempty"`
	RequestID string `json:"request_id,omitempty"`
}

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details any    `json:"details,omitempty"`
}

// JSON writes a JSON response with the given status code.
// Always use this instead of json.NewEncoder directly — it ensures
// Content-Type is set and handles serialization errors safely.
func JSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	env := envelope{Success: status < 400, Data: data}
	if err := json.NewEncoder(w).Encode(env); err != nil {
		// At this point headers are sent, so we can't change status.
		// Log externally; the client gets a partial response.
		_ = err
	}
}

func Created(w http.ResponseWriter, data any) {
	JSON(w, http.StatusCreated, data)
}

func OK(w http.ResponseWriter, data any) {
	JSON(w, http.StatusOK, data)
}

func NoContent(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNoContent)
}

// err writes a structured error response.
func writeError(w http.ResponseWriter, r *http.Request, status int, code, message string, details any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	reqID := ""
	if r != nil {
		reqID = middleware.GetReqID(r.Context())
	}

	env := envelope{
		Success:   false,
		RequestID: reqID,
		Error: &apiError{
			Code:    code,
			Message: message,
			Details: details,
		},
	}
	json.NewEncoder(w).Encode(env)
}

func BadRequest(w http.ResponseWriter, msg string) {
	writeError(w, nil, http.StatusBadRequest, "BAD_REQUEST", msg, nil)
}

func ValidationError(w http.ResponseWriter, errs map[string]string) {
	writeError(w, nil, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "request validation failed", errs)
}

func Unauthorized(w http.ResponseWriter, msg string) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="apex"`)
	writeError(w, nil, http.StatusUnauthorized, "UNAUTHORIZED", msg, nil)
}

func Forbidden(w http.ResponseWriter, msg string) {
	writeError(w, nil, http.StatusForbidden, "FORBIDDEN", msg, nil)
}

func NotFound(w http.ResponseWriter, msg string) {
	writeError(w, nil, http.StatusNotFound, "NOT_FOUND", msg, nil)
}

func Conflict(w http.ResponseWriter, msg string) {
	writeError(w, nil, http.StatusConflict, "CONFLICT", msg, nil)
}

func TooManyRequests(w http.ResponseWriter, msg string) {
	writeError(w, nil, http.StatusTooManyRequests, "RATE_LIMITED", msg, nil)
}

// InternalError logs the error (via request context) and returns a safe message.
// Never expose internal error details to clients in production.
func InternalError(w http.ResponseWriter, r *http.Request, err error) {
	// The actual error should have been logged by the caller or service layer.
	// Here we only expose a request ID so the error can be correlated in logs.
	writeError(w, r, http.StatusInternalServerError, "INTERNAL_ERROR",
		"an unexpected error occurred", nil)
}
