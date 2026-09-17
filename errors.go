package jev

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
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
	maxMessageLength   = 200
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
	// ErrTimeout matches a ConnectionError caused by a timeout: the
	// per-attempt timeout, [RetryPolicy.TotalTimeout], or the transport's own.
	ErrTimeout = errors.New("jev: request timed out")
)

// ErrResponseTooLarge is the Err of a [*ResponseValidationError] for a
// response body over 16 MiB. It is never retried.
var ErrResponseTooLarge = errors.New("jev: response body exceeds 16 MiB")

// APIError is an unsuccessful HTTP response, returned after any retries.
// It matches the status sentinels through errors.Is:
//
//	if errors.Is(err, jev.ErrRateLimit) { ... }
type APIError struct {
	StatusCode int
	Method     string
	URL        string
	Header     http.Header
	// Body is the raw response body, or nil when empty.
	Body []byte
	// Message is the server's message extracted from Body, or the body
	// itself, truncated to 200 characters. It can contain anything the
	// server reflected, including request data.
	Message string
	// RequestID is the X-Typesafe-Request-Id header, or empty when absent.
	RequestID string
	// Attempts is the number of HTTP attempts made, including retries.
	Attempts int
}

func newAPIError(res result) *APIError {
	return &APIError{
		StatusCode: res.status,
		Method:     res.method,
		URL:        res.url,
		Header:     res.header,
		Body:       res.body,
		Message:    truncate(describeBody(res.body)),
		RequestID:  res.header.Get(requestIDHeader),
		Attempts:   res.attempts,
	}
}

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

// RetryAfter returns the delay requested by the Retry-After-Ms or
// Retry-After header, when present and valid.
func (e *APIError) RetryAfter() (time.Duration, bool) {
	return parseRetryAfter(e.Header)
}

// ConnectionError is a request that failed without a complete HTTP
// response, returned after any retries. It matches [ErrConnection], and
// [ErrTimeout] when the failure was a timeout.
type ConnectionError struct {
	Method string
	URL    string
	// Elapsed is how long the failed attempt ran.
	Elapsed time.Duration
	// Timeout is the SDK limit that elapsed: the per-attempt timeout or
	// [RetryPolicy.TotalTimeout]. It is zero when the failure was not an SDK
	// timeout, including timeouts raised by the transport itself.
	Timeout time.Duration
	// StatusCode, Header, and RequestID are set when the response headers
	// arrived before the body failed.
	StatusCode int
	Header     http.Header
	RequestID  string
	Attempts   int
	Err        error
}

func (e *ConnectionError) Error() string {
	switch {
	case e.Timeout > 0:
		return fmt.Sprintf("jev: %s %s: timed out after %v (limit %v)", e.Method, e.URL, e.Elapsed, e.Timeout)
	case isTimeout(e.Err):
		return fmt.Sprintf("jev: %s %s: timed out after %v: %v", e.Method, e.URL, e.Elapsed, e.Err)
	default:
		return fmt.Sprintf("jev: %s %s: connection error after %v: %v", e.Method, e.URL, e.Elapsed, e.Err)
	}
}

func (e *ConnectionError) Unwrap() error { return e.Err }

// Is reports whether target is [ErrConnection] or, for a timeout, [ErrTimeout].
func (e *ConnectionError) Is(target error) bool {
	switch target {
	case ErrConnection:
		return true
	case ErrTimeout:
		return e.Timeout > 0 || isTimeout(e.Err)
	default:
		return false
	}
}

func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// ResponseValidationError is a successful HTTP response whose body does not
// match the API contract or the questions asked.
type ResponseValidationError struct {
	StatusCode int
	Method     string
	URL        string
	// Path is the dotted path of the offending field, such as
	// "answers.tone.confidence".
	Path      string
	Header    http.Header
	Body      json.RawMessage
	RequestID string
	Attempts  int
	Err       error
}

func newResponseValidationError(res result, path string, err error) *ResponseValidationError {
	return &ResponseValidationError{
		StatusCode: res.status,
		Method:     res.method,
		URL:        res.url,
		Path:       path,
		Header:     res.header,
		Body:       res.body,
		RequestID:  res.header.Get(requestIDHeader),
		Attempts:   res.attempts,
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

// truncate keeps at most maxMessageLength characters, cutting on a rune
// boundary.
func truncate(s string) string {
	count := 0
	for i := range s {
		if count == maxMessageLength {
			return s[:i] + "…"
		}
		count++
	}
	return s
}

// describeBody extracts a message the way the official SDKs do: a plain
// string, error, error.message, message, detail, detail.message, or a list
// of validation errors with loc and msg.
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
	return string(body)
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
				if s := fmt.Sprint(item); s != "body" {
					loc = append(loc, s)
				}
			}
		}
		if len(loc) > 0 {
			msg = strings.Join(loc, ".") + ": " + msg
		}
		parts = append(parts, msg)
	}
	return strings.Join(parts, "; ")
}

// parseRetryAfter prefers Retry-After-Ms (milliseconds) over Retry-After
// (seconds or an HTTP date). Values beyond the representable range saturate.
func parseRetryAfter(headers http.Header) (time.Duration, bool) {
	if raw := strings.TrimSpace(headers.Get(retryAfterMSHeader)); raw != "" {
		ms, err := strconv.ParseFloat(raw, 64)
		if err == nil && ms >= 0 && !math.IsInf(ms, 0) {
			return durationOf(ms * float64(time.Millisecond)), true
		}
	}
	raw := strings.TrimSpace(headers.Get(retryAfterHeader))
	if raw == "" {
		return 0, false
	}
	seconds, err := strconv.ParseFloat(raw, 64)
	if err == nil {
		if seconds < 0 || math.IsInf(seconds, 0) || math.IsNaN(seconds) {
			return 0, false
		}
		return durationOf(seconds * float64(time.Second)), true
	}
	date, err := http.ParseTime(raw)
	if err == nil {
		return max(0, time.Until(date)), true
	}
	return 0, false
}

func durationOf(nanoseconds float64) time.Duration {
	if nanoseconds >= math.MaxInt64 {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(nanoseconds)
}
