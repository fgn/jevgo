package jev

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
)

func loggerFromEnv() (*slog.Logger, error) {
	raw := env(EnvLogLevel)
	if raw == "" {
		return slog.New(slog.DiscardHandler), nil
	}
	level, off, err := parseLogLevel(raw)
	if err != nil || off {
		return slog.New(slog.DiscardHandler), err
	}
	return slog.New(&levelHandler{Handler: slog.Default().Handler(), level: level}), nil
}

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

var (
	keyHeaders    = map[string]bool{"authorization": true, "proxy-authorization": true, "x-api-key": true, "api-key": true}
	opaqueHeaders = map[string]bool{"cookie": true, "set-cookie": true}
)

// redactHeaderValue matches the official SDKs: credential headers keep the
// scheme and the last four characters of long secrets; cookies and headers
// named like tokens or secrets are fully masked.
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
		scheme, secret = "", value
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
