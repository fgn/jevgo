package jev

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"
)

// result is the outcome of the last HTTP attempt of a call.
type result struct {
	method   string
	url      string
	status   int
	header   http.Header
	body     []byte
	attempts int
}

// do sends one API call, retrying according to the policy. On error, the
// returned result still reports the attempt count and, for an API error,
// the last response.
func (c *config) do(ctx context.Context, method, path string, body []byte) (result, error) {
	res := result{method: method, url: c.baseURL + path}
	policy := c.retry
	started := time.Now()
	for attempt := 0; ; attempt++ {
		res.attempts = attempt + 1
		err := ctx.Err()
		if err != nil {
			return res, c.contextError(res, err)
		}
		err = c.attempt(ctx, &res, body, attempt)
		if err == nil {
			return res, nil
		}
		if ctx.Err() != nil {
			return res, err
		}
		retriesLeft := policy.MaxRetries - attempt
		if retriesLeft <= 0 || !policy.retryable(err) {
			return res, err
		}
		delay := policy.delay(attempt, res.header, defaultRandom)
		if policy.TotalTimeout > 0 && time.Since(started)+delay >= policy.TotalTimeout {
			c.logger.LogAttrs(ctx, slog.LevelInfo, "jev: retry budget exhausted",
				slog.String("method", method), slog.String("url", res.url),
				slog.Duration("total_timeout", policy.TotalTimeout), slog.Int("attempts", res.attempts))
			return res, err
		}
		c.logger.LogAttrs(ctx, slog.LevelInfo, "jev: retrying",
			slog.String("method", method), slog.String("url", res.url),
			slog.Duration("delay", delay), slog.Int("retry", attempt+1),
			slog.Int("max_retries", policy.MaxRetries), slog.String("reason", retryReason(err)))
		err = sleep(ctx, delay)
		if err != nil {
			return res, c.contextError(res, err)
		}
	}
}

// attempt performs one HTTP round trip under the per-attempt timeout and
// records the outcome in res.
func (c *config) attempt(ctx context.Context, res *result, body []byte, attempt int) error {
	res.status, res.header, res.body = 0, nil, nil

	attemptCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(attemptCtx, res.method, res.url, reader)
	if err != nil {
		return fmt.Errorf("%w: build request: %w", ErrInvalidRequest, err)
	}
	c.setHeaders(req, body != nil, attempt)

	if c.logger.Enabled(ctx, slog.LevelDebug) {
		c.logger.LogAttrs(ctx, slog.LevelDebug, "jev: request",
			slog.String("method", res.method), slog.String("url", res.url), slog.Int("attempt", attempt+1),
			slog.Any("headers", redactedHeaders(req.Header)), slog.String("body", string(body)))
	}

	started := time.Now()
	httpRes, err := c.httpClient.Do(req)
	if err == nil {
		res.status = httpRes.StatusCode
		res.header = httpRes.Header
		res.body, err = io.ReadAll(httpRes.Body)
		_ = httpRes.Body.Close()
	}
	elapsed := time.Since(started)
	if err != nil {
		ctxErr := ctx.Err()
		if ctxErr != nil {
			c.logger.LogAttrs(ctx, slog.LevelInfo, "jev: request canceled",
				slog.String("method", res.method), slog.String("url", res.url), slog.Duration("elapsed", elapsed))
			return c.contextError(*res, ctxErr)
		}
		connErr := &ConnectionError{Method: res.method, URL: res.url, Err: err}
		if errors.Is(attemptCtx.Err(), context.DeadlineExceeded) || isTimeout(err) {
			connErr.Timeout = c.timeout
		}
		c.logger.LogAttrs(ctx, slog.LevelInfo, "jev: request failed",
			slog.String("method", res.method), slog.String("url", res.url),
			slog.Duration("elapsed", elapsed), slog.String("error", connErr.Error()))
		return connErr
	}

	c.logger.LogAttrs(ctx, slog.LevelInfo, "jev: response",
		slog.String("method", res.method), slog.String("url", res.url), slog.Int("status", res.status),
		slog.Duration("elapsed", elapsed), slog.String("request_id", res.header.Get(requestIDHeader)),
		slog.Int("attempt", attempt+1))
	if c.logger.Enabled(ctx, slog.LevelDebug) {
		c.logger.LogAttrs(ctx, slog.LevelDebug, "jev: response body",
			slog.Any("headers", redactedHeaders(res.header)), slog.String("body", string(res.body)))
	}
	if res.status >= 200 && res.status < 300 {
		return nil
	}
	return newAPIError(*res)
}

// setHeaders applies caller headers first so protected SDK headers always
// win.
func (c *config) setHeaders(req *http.Request, hasBody bool, attempt int) {
	for name, values := range c.headers {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set(sdkHeader, userAgent)
	req.Header.Set(runtimeHeader, runtimeDescription)
	req.Header.Del(retryCountHeader)
	if attempt > 0 {
		req.Header.Set(retryCountHeader, strconv.Itoa(attempt))
	}
	if hasBody {
		req.Header.Set("Content-Type", "application/json")
	} else {
		req.Header.Del("Content-Type")
	}
}

func (c *config) contextError(res result, err error) error {
	return fmt.Errorf("jev: %s %s: %w", res.method, res.url, err)
}

func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func retryReason(err error) string {
	var api *APIError
	var conn *ConnectionError
	switch {
	case errors.As(err, &api):
		return "http " + strconv.Itoa(api.StatusCode)
	case errors.As(err, &conn) && conn.Timeout > 0:
		return "timeout"
	case errors.As(err, &conn):
		return "connection error"
	default:
		return "error"
	}
}
