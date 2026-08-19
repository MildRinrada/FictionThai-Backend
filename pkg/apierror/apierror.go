// Package apierror defines the stable, machine-readable error contract shared by
// every FictionThai API client (web, future mobile, admin).
//
// Clients must branch on Code, never on Message - see docs/09 - API Specification.md §7.
package apierror

import "net/http"

// Stable error codes. Add new codes here rather than inventing strings at call
// sites, so the OpenAPI contract and the frontend stay in sync.
const (
	CodeBadRequest      = "BAD_REQUEST"
	CodeUnauthorized    = "UNAUTHORIZED"
	CodeForbidden       = "FORBIDDEN"
	CodeNotFound        = "NOT_FOUND"
	CodeConflict        = "CONFLICT"
	CodeValidation      = "VALIDATION_ERROR"
	CodeRateLimited     = "RATE_LIMIT_EXCEEDED"
	CodePayloadTooLarge = "PAYLOAD_TOO_LARGE"
	CodeInternal        = "INTERNAL_ERROR"
	CodeUnavailable     = "SERVICE_UNAVAILABLE"

	// Fiction Format System - docs/09 - API Specification.md §36.
	CodeInvalidFictionFormat = "INVALID_FICTION_FORMAT"
)

// Error is an API-facing error. It carries the HTTP status it should be
// rendered with, so handlers can return a single value and let the response
// layer decide the status.
//
// Fields is optional and used for per-field validation failures.
type Error struct {
	Status  int                 `json:"-"`
	Code    string              `json:"code"`
	Message string              `json:"message"`
	Fields  map[string][]string `json:"fields,omitempty"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// New builds an API error with an explicit status.
func New(status int, code, message string) *Error {
	return &Error{Status: status, Code: code, Message: message}
}

func BadRequest(message string) *Error {
	return New(http.StatusBadRequest, CodeBadRequest, message)
}

func Unauthorized(message string) *Error {
	return New(http.StatusUnauthorized, CodeUnauthorized, message)
}

func Forbidden(message string) *Error {
	return New(http.StatusForbidden, CodeForbidden, message)
}

func NotFound(message string) *Error {
	return New(http.StatusNotFound, CodeNotFound, message)
}

func Conflict(message string) *Error {
	return New(http.StatusConflict, CodeConflict, message)
}

// Validation returns a 422 carrying per-field messages.
func Validation(fields map[string][]string) *Error {
	return &Error{
		Status:  http.StatusUnprocessableEntity,
		Code:    CodeValidation,
		Message: "Validation failed.",
		Fields:  fields,
	}
}

// Internal is deliberately opaque: internal detail belongs in the logs, never in
// the response body (docs/11 - Security & Privacy.md §67).
func Internal() *Error {
	return New(http.StatusInternalServerError, CodeInternal, "An unexpected error occurred.")
}
