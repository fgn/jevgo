package jev

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"net/http"
	"slices"
	"time"
)

// RetryPolicy controls how failed attempts are retried. Start from
// [DefaultRetryPolicy] and adjust fields; the zero value disables retries.
type RetryPolicy struct {
	// MaxRetries is the maximum number of retries after the initial attempt;
	// 0 disables retries. Default: 2.
	MaxRetries int
	// InitialBackoff is the first backoff delay, doubled each retry up to
	// MaxBackoff. Default: 500ms.
	InitialBackoff time.Duration
	// MaxBackoff caps the backoff delay. Default: 5s.
	MaxBackoff time.Duration
	// Jitter is the fraction of each backoff delay randomly subtracted, from
	// 0 to 1. Default: 0.25.
	Jitter float64
	// Statuses lists the HTTP status codes that are retried. Default: 408,
	// 429, and 500 through 599.
	Statuses []int
	// RespectRetryAfter honors retry-after-ms and Retry-After response headers
	// up to MaxRetryAfter; longer delays fall back to backoff. Default: true.
	RespectRetryAfter bool
	// MaxRetryAfter is the longest server-requested delay honored. Default: 60s.
	MaxRetryAfter time.Duration
	// ConnectionErrors retries connection failures, including interrupted
	// response bodies. Default: true.
	ConnectionErrors bool
	// Timeouts retries per-attempt timeouts. Default: true.
	Timeouts bool
	// TotalTimeout bounds one call's attempts and delays together: a retry
	// whose delay would reach the budget is skipped and the last error
	// returned. Zero disables the budget; the caller's context deadline
	// always applies. Default: 0.
	TotalTimeout time.Duration
	// ShouldRetry, when set, is consulted for errors the built-in rules do
	// not retry; returning true retries them. It never sees context errors.
	ShouldRetry func(error) bool
}

// DefaultRetryPolicy returns the SDK default policy, which matches the
// official TypeSafe SDKs.
func DefaultRetryPolicy() RetryPolicy {
	statuses := []int{http.StatusRequestTimeout, http.StatusTooManyRequests}
	for status := 500; status <= 599; status++ {
		statuses = append(statuses, status)
	}
	return RetryPolicy{
		MaxRetries:        2,
		InitialBackoff:    500 * time.Millisecond,
		MaxBackoff:        5 * time.Second,
		Jitter:            0.25,
		Statuses:          statuses,
		RespectRetryAfter: true,
		MaxRetryAfter:     time.Minute,
		ConnectionErrors:  true,
		Timeouts:          true,
	}
}

func (p RetryPolicy) validate() error {
	switch {
	case p.MaxRetries < 0:
		return fmt.Errorf("%w: retry MaxRetries must not be negative, got %d", ErrInvalidConfig, p.MaxRetries)
	case p.InitialBackoff < 0 || p.MaxBackoff < 0 || p.MaxRetryAfter < 0 || p.TotalTimeout < 0:
		return fmt.Errorf("%w: retry delays must not be negative", ErrInvalidConfig)
	case math.IsNaN(p.Jitter) || p.Jitter < 0 || p.Jitter > 1:
		return fmt.Errorf("%w: retry Jitter must be between 0 and 1, got %v", ErrInvalidConfig, p.Jitter)
	}
	for _, status := range p.Statuses {
		if status < 100 || status > 999 {
			return fmt.Errorf("%w: retry Statuses must be HTTP status codes, got %d", ErrInvalidConfig, status)
		}
	}
	return nil
}

// retryable decides whether err, from one attempt, may be retried. Context
// errors from caller cancellation are never retried; the loop checks the
// context before calling this.
func (p RetryPolicy) retryable(err error) bool {
	var conn *ConnectionError
	var api *APIError
	switch {
	case errors.As(err, &conn):
		if conn.Timeout > 0 {
			if p.Timeouts {
				return true
			}
		} else if p.ConnectionErrors {
			return true
		}
	case errors.As(err, &api):
		if slices.Contains(p.Statuses, api.StatusCode) {
			return true
		}
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return false
	}
	return p.ShouldRetry != nil && p.ShouldRetry(err)
}

// delay computes the wait before retry number attempt (zero-based): the
// server's Retry-After when allowed, otherwise capped exponential backoff
// with jitter.
func (p RetryPolicy) delay(attempt int, headers http.Header, random func() float64) time.Duration {
	if p.RespectRetryAfter && headers != nil {
		if after, ok := parseRetryAfter(headers); ok && after <= p.MaxRetryAfter {
			return after
		}
	}
	backoff := float64(p.InitialBackoff) * math.Pow(2, float64(attempt))
	backoff = math.Min(backoff, float64(p.MaxBackoff))
	return time.Duration(backoff * (1 - random()*p.Jitter))
}

func defaultRandom() float64 { return rand.Float64() }

// sleep waits for d or until ctx is done.
func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
