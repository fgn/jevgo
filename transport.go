package jev

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"strconv"
	"time"
)

// maxResponseBytes bounds memory per response; API bodies are kilobytes.
const maxResponseBytes = 16 << 20

// result is the outcome of the last HTTP attempt of a call.
type result struct {
	method   string
	url      string
	status   int
	header   http.Header
	body     []byte
	attempts int
}

// do sends one API call with retries. The result is returned even on error
// so callers can read the attempt count.
func (c *config) do(ctx context.Context, method, path string, body []byte) (result, error) {
	res := result{method: method, url: c.baseURL + path}
	policy := c.retry
	callCtx := ctx
	if policy.TotalTimeout > 0 {
		var cancel context.CancelFunc
		callCtx, cancel = context.WithTimeout(ctx, policy.TotalTimeout)
		defer cancel()
	}
	started := time.Now()
	for attempt := 0; ; attempt++ {
		err := ctx.Err()
		if err != nil {
			return res, c.contextError(res, err)
		}
		err = callCtx.Err()
		if err != nil {
			return res, c.budgetError(res, started, err)
		}
		err = c.attempt(ctx, callCtx, &res, body, attempt)
		if err == nil {
			return res, nil
		}
		if ctx.Err() != nil || callCtx.Err() != nil {
			return res, err
		}
		if policy.MaxRetries-attempt <= 0 || !policy.retryable(err) {
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
			slog.String("method", method), slog.String("url", res.url), slog.Duration("delay", delay),
			slog.Int("retry", attempt+1), slog.Int("max_retries", policy.MaxRetries),
			slog.String("reason", retryReason(err)))
		err = sleep(callCtx, delay)
		if err != nil {
			if ctx.Err() != nil {
				return res, c.contextError(res, ctx.Err())
			}
			return res, c.budgetError(res, started, err)
		}
	}
}

// budgetError reports an expired TotalTimeout outside an attempt.
func (c *config) budgetError(res result, started time.Time, err error) error {
	return &ConnectionError{
		Method: res.method, URL: res.url, Elapsed: time.Since(started), Timeout: c.retry.TotalTimeout,
		StatusCode: res.status, Header: res.header, RequestID: res.header.Get(requestIDHeader),
		Attempts: res.attempts, Err: err,
	}
}

// attempt performs one HTTP round trip. ctx is the caller's context and
// callCtx additionally carries the TotalTimeout deadline.
func (c *config) attempt(ctx, callCtx context.Context, res *result, body []byte, attempt int) error {
	res.status, res.header, res.body = 0, nil, nil

	attemptCtx, cancel := context.WithTimeout(callCtx, c.timeout)
	defer cancel()

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(attemptCtx, res.method, res.url, reader)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	c.setHeaders(req, body != nil, attempt)
	if c.logger.Enabled(ctx, slog.LevelDebug) {
		c.logger.LogAttrs(ctx, slog.LevelDebug, "jev: request",
			slog.String("method", res.method), slog.String("url", res.url), slog.Int("attempt", attempt+1),
			slog.Any("headers", redactedHeaders(req.Header)), slog.String("body", string(body)))
	}

	res.attempts++
	started := time.Now()
	httpRes, err := c.httpClient.Do(req) //nolint:bodyclose // closed by readBody.
	if err == nil {
		res.status = httpRes.StatusCode
		res.header = httpRes.Header
		res.body, err = readBody(httpRes.Body)
	}
	elapsed := time.Since(started)
	if errors.Is(err, ErrResponseTooLarge) {
		return newResponseValidationError(*res, "", err)
	}
	if err != nil {
		ctxErr := ctx.Err()
		if ctxErr != nil {
			c.logger.LogAttrs(ctx, slog.LevelInfo, "jev: request canceled",
				slog.String("method", res.method), slog.String("url", res.url), slog.Duration("elapsed", elapsed))
			return c.contextError(*res, ctxErr)
		}
		connErr := &ConnectionError{
			Method: res.method, URL: res.url, Elapsed: elapsed, StatusCode: res.status, Header: res.header,
			RequestID: res.header.Get(requestIDHeader), Attempts: res.attempts, Err: err,
		}
		switch {
		case c.retry.TotalTimeout > 0 && callCtx.Err() != nil:
			connErr.Timeout = c.retry.TotalTimeout
		case errors.Is(attemptCtx.Err(), context.DeadlineExceeded):
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

func readBody(body io.ReadCloser) ([]byte, error) {
	defer body.Close()
	data, err := io.ReadAll(io.LimitReader(body, maxResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxResponseBytes {
		return nil, ErrResponseTooLarge
	}
	return data, nil
}

// setHeaders applies caller headers first so the protected ones always win.
func (c *config) setHeaders(req *http.Request, hasBody bool, attempt int) {
	maps.Copy(req.Header, c.headers)
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

func retryReason(err error) string {
	var api *APIError
	switch {
	case errors.As(err, &api):
		return "http " + strconv.Itoa(api.StatusCode)
	case errors.Is(err, ErrTimeout):
		return "timeout"
	case errors.Is(err, ErrConnection):
		return "connection error"
	default:
		return "error"
	}
}
