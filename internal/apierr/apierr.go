// Package apierr defines structured API errors with stable error codes.
// Returning a stable `code` (not just a human string) lets clients branch on
// behaviour — e.g. retry on rate_limit_exceeded, but never on invalid_request.
package apierr

import "net/http"

type Code string

const (
	CodeInvalidRequest      Code = "invalid_request"
	CodeUnauthorized        Code = "unauthorized"
	CodeForbidden           Code = "forbidden"
	CodeNotFound            Code = "not_found"
	CodeConflict            Code = "conflict"
	CodeRateLimit           Code = "rate_limit_exceeded"
	CodeProviderError       Code = "provider_error"
	CodeInternal            Code = "internal_error"
	CodeIdempotencyConflict Code = "idempotency_conflict"
	// CodeIdempotencyInUse: a request with this key is still being processed.
	// Unlike idempotency_conflict, retrying later with the same key is correct.
	CodeIdempotencyInUse Code = "idempotency_key_in_use"
)

type Error struct {
	Status  int                    `json:"-"`
	Code    Code                   `json:"code"`
	Message string                 `json:"message"`
	Details map[string]interface{} `json:"details,omitempty"`
}

func (e *Error) Error() string { return string(e.Code) + ": " + e.Message }

func New(status int, code Code, msg string) *Error {
	return &Error{Status: status, Code: code, Message: msg}
}

func InvalidRequest(msg string) *Error { return New(http.StatusBadRequest, CodeInvalidRequest, msg) }
func Unauthorized(msg string) *Error   { return New(http.StatusUnauthorized, CodeUnauthorized, msg) }
func Forbidden(msg string) *Error      { return New(http.StatusForbidden, CodeForbidden, msg) }
func NotFound(msg string) *Error       { return New(http.StatusNotFound, CodeNotFound, msg) }
func Conflict(msg string) *Error       { return New(http.StatusConflict, CodeConflict, msg) }
func Internal(msg string) *Error       { return New(http.StatusInternalServerError, CodeInternal, msg) }
func Provider(msg string) *Error       { return New(http.StatusBadGateway, CodeProviderError, msg) }
func IdempotencyConflict(m string) *Error {
	return New(http.StatusConflict, CodeIdempotencyConflict, m)
}
func IdempotencyInUse(m string) *Error { return New(http.StatusConflict, CodeIdempotencyInUse, m) }
func RateLimited(m string) *Error      { return New(http.StatusTooManyRequests, CodeRateLimit, m) }
