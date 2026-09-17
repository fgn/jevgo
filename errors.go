package jev

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Header names in canonical form; the API reads them case-insensitively.
const (
	requestIDHeader    = "X-Typesafe-Request-Id"
	retryAfterHeader   = "Retry-After"
	retryAfterMSHeader = "Retry-After-Ms"
	sdkHeader          = "X-Typesafe-Sdk"
	runtimeHeader      = "X-Typesafe-Runtime"
	retryCountHeader   = "X-Typesafe-Retry-Count"
	maxErrorBodyLength = 200
)

// Status sentinels matched by [*APIError] through errors.Is.
var (
	ErrBadRequest          = errors.New("jev: bad request")           // HTTP 400
	ErrAuthentication      = errors.New("jev: authentication failed") // HTTP 401
	ErrPermissionDenied    = errors.New("jev: permission denied")     // HTTP 403
	ErrNotFound            = errors.New("jev: not found")             // HTTP 404
	ErrUnprocessableEntity = errors.New("jev: unprocessable entity")  // HTTP 422
	ErrRateLimit           = errors.New("jev: rate limit exceeded")   // HTTP 429
	ErrOverloaded          = errors.New("jev: service overloaded")    // HTTP 529
	ErrInternalServer      = errors.New("jev: internal server error") // HTTP 5xx, including 529
)

// Transport sentinels matched by [*ConnectionError] through errors.Is.
var (
	// ErrConnection matches every ConnectionError.
	ErrConnection = errors.New("jev: connection error")
	// ErrTimeout matches a ConnectionError caused by the per-attempt timeout.
	ErrTimeout = errors.New("jev: request timed out")
)

// APIError is an unsuccessful HTTP response from the API, returned after
// any retries. It matches the status sentinels through errors.Is:
//
//	if errors.Is(err, jev.ErrRateLimit) { ... }
//
// or inspect it directly:
//
//	var apiErr *jev.APIError
//	if errors.As(err, &apiErr) { log.Println(apiErr.StatusCode, apiErr.RequestID) }
type APIError struct {
	// StatusCode is the HTTP status code.
	StatusCode int
	// Method and URL identify the request, without credentials.
	Method string
	URL    string
	// Header holds the HTTP response headers.
	Header http.Header
	// Body is the raw response body, or nil when empty.
	Body []byte
	// Message is the error message extracted from the body, or the truncated
	// body when it has no recognizable message.
	Message string
	// RequestID is the x-typesafe-request-id header, or empty when absent.
	RequestID string
}

func newAPIError(res result) *APIError {
	return &APIError{
		StatusCode: res.status,
		Method:     res.method,
		URL:        res.url,
		Header:     res.header,
		Body:       res.body,
		Message:    describeBody(res.body),
		RequestID:  res.header.Get(requestIDHeader),
	}
}

// Error formats as "jev: METHOD URL: STATUS message (request_id=...)".
func (e *APIError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "jev: %s %s: %d", e.Method, e.URL, e.StatusCode)
	if e.Message != "" {
		b.WriteString(" " + e.Message)
	}
	if e.RequestID != "" {
		fmt.Fprintf(&b, " (request_id=%s)", e.RequestID)
	}
	return b.String()
}

// Is reports whether target is the status sentinel for this error.
func (e *APIError) Is(target error) bool {
	switch target {
	case ErrBadRequest:
		return e.StatusCode == http.StatusBadRequest
	case ErrAuthentication:
		return e.StatusCode == http.StatusUnauthorized
	case ErrPermissionDenied:
		return e.StatusCode == http.StatusForbidden
	case ErrNotFound:
		return e.StatusCode == http.StatusNotFound
	case ErrUnprocessableEntity:
		return e.StatusCode == http.StatusUnprocessableEntity
	case ErrRateLimit:
		return e.StatusCode == http.StatusTooManyRequests
	case ErrOverloaded:
		return e.StatusCode == 529
	case ErrInternalServer:
		return e.StatusCode >= 500
	default:
		return false
	}
}

// RetryAfter returns the delay requested by the retry-after-ms or
// Retry-After header, when present and valid.
func (e *APIError) RetryAfter() (time.Duration, bool) {
	return parseRetryAfter(e.Header)
}

// ConnectionError is a request that failed without an HTTP response: the
// connection could not be made, the response body was interrupted, or the
// per-attempt timeout elapsed. It is returned after any retries and
// matches [ErrConnection] and, when Timeout is set, [ErrTimeout].
type ConnectionError struct {
	// Method and URL identify the request, without credentials.
	Method string
	URL    string
	// Timeout is the per-attempt timeout that elapsed, or zero when the
	// failure was not a timeout.
	Timeout time.Duration
	// Err is the underlying transport error.
	Err error
}

func (e *ConnectionError) Error() string {
	if e.Timeout > 0 {
		return fmt.Sprintf("jev: %s %s: request timed out after %v: %v", e.Method, e.URL, e.Timeout, e.Err)
	}
	return fmt.Sprintf("jev: %s %s: connection error: %v", e.Method, e.URL, e.Err)
}

func (e *ConnectionError) Unwrap() error { return e.Err }

// Is reports whether target is [ErrConnection] or, for a timeout, [ErrTimeout].
func (e *ConnectionError) Is(target error) bool {
	switch target {
	case ErrConnection:
		return true
	case ErrTimeout:
		return e.Timeout > 0
	default:
		return false
	}
}

// ResponseValidationError is a successful HTTP response whose body does not
// match the API contract. Path names the first missing or invalid field,
// such as "answers.tone.confidence".
type ResponseValidationError struct {
	// StatusCode is the HTTP status code.
	StatusCode int
	// Method and URL identify the request, without credentials.
	Method string
	URL    string
	// Path is the dotted path of the offending field.
	Path string
	// Body is the raw response body.
	Body json.RawMessage
	// RequestID is the x-typesafe-request-id header, or empty when absent.
	RequestID string
	// Err is the underlying decode error.
	Err error
}

func newResponseValidationError(res result, path string, err error) *ResponseValidationError {
	return &ResponseValidationError{
		StatusCode: res.status,
		Method:     res.method,
		URL:        res.url,
		Path:       path,
		Body:       res.body,
		RequestID:  res.header.Get(requestIDHeader),
		Err:        err,
	}
}

func (e *ResponseValidationError) Error() string {
	msg := fmt.Sprintf("jev: %s %s: invalid response data at %q: %v", e.Method, e.URL, e.Path, e.Err)
	if e.RequestID != "" {
		msg += fmt.Sprintf(" (request_id=%s)", e.RequestID)
	}
	return msg
}

func (e *ResponseValidationError) Unwrap() error { return e.Err }

// describeBody extracts a message from an error body the way the official
// SDKs do: a plain string, error, error.message, message, detail,
// detail.message, or a list of validation errors with loc and msg. Without
// a recognizable message the body is truncated to 200 characters.
func describeBody(body []byte) string {
	if len(body) == 0 {
		return "status code (no body)"
	}
	var parsed any
	err := json.Unmarshal(body, &parsed)
	if err != nil {
		parsed = string(body)
	}
	if message := extractMessage(parsed); message != "" {
		return message
	}
	raw := string(body)
	if len(raw) > maxErrorBodyLength {
		return raw[:maxErrorBodyLength] + "…"
	}
	return raw
}

func extractMessage(body any) string {
	switch v := body.(type) {
	case string:
		return v
	case map[string]any:
		if s, ok := v["error"].(string); ok {
			return s
		}
		if m, ok := v["error"].(map[string]any); ok {
			if s, ok := m["message"].(string); ok {
				return s
			}
		}
		if s, ok := v["message"].(string); ok {
			return s
		}
		switch detail := v["detail"].(type) {
		case string:
			return detail
		case map[string]any:
			if s, ok := detail["message"].(string); ok {
				return s
			}
		case []any:
			return describeValidationErrors(detail)
		}
	}
	return ""
}

func describeValidationErrors(entries []any) string {
	var parts []string
	for _, entry := range entries {
		m, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		msg, ok := m["msg"].(string)
		if !ok {
			continue
		}
		var loc []string
		if items, ok := m["loc"].([]any); ok {
			for _, item := range items {
				s := fmt.Sprint(item)
				if s != "body" {
					loc = append(loc, s)
				}
			}
		}
		if len(loc) > 0 {
			parts = append(parts, strings.Join(loc, ".")+": "+msg)
		} else {
			parts = append(parts, msg)
		}
	}
	return strings.Join(parts, "; ")
}

// parseRetryAfter reads retry-after-ms (milliseconds) or Retry-After
// (seconds or an HTTP date), preferring retry-after-ms.
func parseRetryAfter(headers http.Header) (time.Duration, bool) {
	if raw := strings.TrimSpace(headers.Get(retryAfterMSHeader)); raw != "" {
		ms, err := strconv.ParseFloat(raw, 64)
		if err == nil && ms >= 0 && !isInf(ms) {
			return time.Duration(ms * float64(time.Millisecond)), true
		}
	}
	raw := strings.TrimSpace(headers.Get(retryAfterHeader))
	if raw == "" {
		return 0, false
	}
	seconds, err := strconv.ParseFloat(raw, 64)
	if err == nil {
		if seconds < 0 || isInf(seconds) {
			return 0, false
		}
		return time.Duration(seconds * float64(time.Second)), true
	}
	date, err := http.ParseTime(raw)
	if err == nil {
		return max(0, time.Until(date)), true
	}
	return 0, false
}

func isInf(f float64) bool { return f > 1e300 || f < -1e300 || f != f }
