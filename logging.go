package jev

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
)

// loggerFromEnv builds the default logger: silent unless TYPESAFE_LOG_LEVEL
// selects a level, in which case records at or above it go to slog.Default.
func loggerFromEnv() (*slog.Logger, error) {
	raw := env(EnvLogLevel)
	if raw == "" {
		return slog.New(slog.DiscardHandler), nil
	}
	level, off, err := parseLogLevel(raw)
	if err != nil {
		return nil, err
	}
	if off {
		return slog.New(slog.DiscardHandler), nil
	}
	return slog.New(&levelHandler{Handler: slog.Default().Handler(), level: level}), nil
}

// parseLogLevel returns the level, whether logging is off, and an error for
// unknown values.
func parseLogLevel(raw string) (slog.Level, bool, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "debug":
		return slog.LevelDebug, false, nil
	case "info":
		return slog.LevelInfo, false, nil
	case "warn", "warning":
		return slog.LevelWarn, false, nil
	case "error":
		return slog.LevelError, false, nil
	case "off":
		return 0, true, nil
	default:
		return 0, false, fmt.Errorf("%w: invalid %s %q: expected debug, info, warn, error, or off",
			ErrInvalidConfig, EnvLogLevel, raw)
	}
}

// levelHandler filters an existing handler to a minimum level.
type levelHandler struct {
	slog.Handler
	level slog.Level
}

func (h *levelHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level
}

func (h *levelHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &levelHandler{Handler: h.Handler.WithAttrs(attrs), level: h.level}
}

func (h *levelHandler) WithGroup(name string) slog.Handler {
	return &levelHandler{Handler: h.Handler.WithGroup(name), level: h.level}
}

// Header redaction, matching the official SDKs: credential headers keep
// their scheme and the last four characters of long secrets; cookies and any
// header whose name contains "token" or "secret" are fully masked.
var (
	keyHeaders    = map[string]bool{"authorization": true, "proxy-authorization": true, "x-api-key": true, "api-key": true}
	opaqueHeaders = map[string]bool{"cookie": true, "set-cookie": true}
)

func redactHeaderValue(name, value string) string {
	lower := strings.ToLower(name)
	switch {
	case keyHeaders[lower]:
		return redactKey(value)
	case opaqueHeaders[lower], strings.Contains(lower, "token"), strings.Contains(lower, "secret"):
		return "***"
	default:
		return value
	}
}

func redactKey(value string) string {
	scheme, secret, found := strings.Cut(value, " ")
	if !found {
		secret = value
		scheme = ""
	}
	tail := ""
	if len(secret) > 8 {
		tail = secret[len(secret)-4:]
	}
	if scheme != "" {
		return scheme + " ***" + tail
	}
	return "***" + tail
}

// redactedHeaders renders headers as a slog value with credentials masked.
func redactedHeaders(headers http.Header) slog.Value {
	attrs := make([]slog.Attr, 0, len(headers))
	for name, values := range headers {
		redacted := make([]string, len(values))
		for i, value := range values {
			redacted[i] = redactHeaderValue(name, value)
		}
		attrs = append(attrs, slog.Any(name, redacted))
	}
	return slog.GroupValue(attrs...)
}
