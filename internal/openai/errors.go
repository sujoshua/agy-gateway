package openai

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"time"
)

// Error is an OpenAI-style API error with its HTTP status.
type Error struct {
	Status  int     `json:"-"`
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    *string `json:"code"`
	// ResetsAt (Unix seconds) and ResetsInSeconds tell when an exhausted
	// usage limit resets. They are set only on usage_limit_reached errors
	// whose reset time is known.
	ResetsAt        *int64 `json:"resets_at,omitempty"`
	ResetsInSeconds *int64 `json:"resets_in_seconds,omitempty"`
}

func (e *Error) Error() string { return e.Message }

func newError(status int, typ, code, param, format string, args ...any) *Error {
	e := &Error{Status: status, Type: typ, Message: fmt.Sprintf(format, args...)}
	if code != "" {
		e.Code = &code
	}
	if param != "" {
		e.Param = &param
	}
	return e
}

// InvalidRequest is a 400 invalid_request_error.
func InvalidRequest(param, format string, args ...any) *Error {
	return newError(http.StatusBadRequest, "invalid_request_error", "", param, format, args...)
}

// Unsupported is a 400 for a parameter the gateway cannot honor.
func Unsupported(param, format string, args ...any) *Error {
	return newError(http.StatusBadRequest, "invalid_request_error", "unsupported_parameter", param, format, args...)
}

// ModelNotFound is a 404 for an unknown model.
func ModelNotFound(model string) *Error {
	return newError(http.StatusNotFound, "invalid_request_error", "model_not_found", "model",
		"the model %q does not exist or is not offered by this runtime", model)
}

// Unauthorized is a 401.
func Unauthorized() *Error {
	return newError(http.StatusUnauthorized, "invalid_request_error", "invalid_api_key", "", "invalid or missing API key")
}

// Unavailable is a 503, used while the runtime is down or not signed in.
func Unavailable(format string, args ...any) *Error {
	return newError(http.StatusServiceUnavailable, "server_error", "runtime_unavailable", "", format, args...)
}

// Timeout is a 504 for requests exceeding the gateway deadline.
func Timeout(format string, args ...any) *Error {
	return newError(http.StatusGatewayTimeout, "server_error", "timeout", "", format, args...)
}

// Internal is a 500.
func Internal(format string, args ...any) *Error {
	return newError(http.StatusInternalServerError, "server_error", "", "", format, args...)
}

// Busy is a 409 when a conversation is already being served.
func Busy(format string, args ...any) *Error {
	return newError(http.StatusConflict, "invalid_request_error", "conversation_busy", "", format, args...)
}

// NotFound is a 404 for unknown routes.
func NotFound(format string, args ...any) *Error {
	return newError(http.StatusNotFound, "invalid_request_error", "not_found", "", format, args...)
}

// MarshalEnvelope returns {"error": e}.
func (e *Error) MarshalEnvelope() []byte {
	b, err := json.Marshal(struct {
		Error *Error `json:"error"`
	}{e})
	if err != nil {
		return []byte(`{"error":{"message":"internal error","type":"server_error"}}`)
	}
	return b
}

// TooManySessions is a 429 when the instance holds its maximum number of
// live conversations; a router can fail over to another instance.
func TooManySessions() *Error {
	return newError(http.StatusTooManyRequests, "rate_limit_error", "too_many_sessions", "",
		"this gateway instance is serving its maximum number of conversations")
}

// RateLimited is a 429 relayed from the runtime's upstream quota.
func RateLimited(format string, args ...any) *Error {
	return newError(http.StatusTooManyRequests, "rate_limit_error", "rate_limit_exceeded", "", format, args...)
}

// UsageLimit is a 429 for an exhausted account quota that resets at
// resetAt, or at an unknown time when resetAt is not after now.
func UsageLimit(resetAt, now time.Time) *Error {
	if !resetAt.After(now) {
		return newError(http.StatusTooManyRequests, "usage_limit_reached", "usage_limit_reached", "",
			"the account's usage limit is reached; the runtime did not say when it resets")
	}
	e := newError(http.StatusTooManyRequests, "usage_limit_reached", "usage_limit_reached", "",
		"the account's usage limit is reached; it resets at %s", resetAt.UTC().Format(time.RFC3339))
	at, in := resetAt.Unix(), int64(math.Ceil(resetAt.Sub(now).Seconds()))
	e.ResetsAt, e.ResetsInSeconds = &at, &in
	return e
}

// RetryAfter returns the Retry-After header value in seconds, or "" when
// the error does not say when to retry.
func (e *Error) RetryAfter() string {
	if e.ResetsInSeconds == nil {
		return ""
	}
	return strconv.FormatInt(*e.ResetsInSeconds, 10)
}

// Upstream is a 502 for a runtime turn that failed.
func Upstream(format string, args ...any) *Error {
	return newError(http.StatusBadGateway, "server_error", "upstream_error", "", format, args...)
}

// Conflict is a 409 for a request the gateway's current state rules out.
func Conflict(code, format string, args ...any) *Error {
	return newError(http.StatusConflict, "invalid_request_error", code, "", format, args...)
}
