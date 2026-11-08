package openai

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// Error is an OpenAI-style API error with its HTTP status.
type Error struct {
	Status  int     `json:"-"`
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    *string `json:"code"`
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

// Upstream is a 502 for a runtime turn that failed.
func Upstream(format string, args ...any) *Error {
	return newError(http.StatusBadGateway, "server_error", "upstream_error", "", format, args...)
}
