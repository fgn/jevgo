// Package jevlangfuse records a Langfuse generation observation for every
// TypeSafe SystemOne call made through github.com/fgn/jevgo, using
// github.com/fgn/go-langfuse.
//
// Attach it where the client is constructed; call sites do not change:
//
//	client, err := jev.NewClient(jev.WithTracer(jevlangfuse.NewTracer(lf)))
//
// Each SystemOne call becomes one generation observation, parented by
// whatever observation is in the request context, carrying the request
// model, the state and questions as input, the answers as output, exact
// token usage, the request ID, and the number of HTTP attempts. Everything
// recorded flows through the core client's privacy controls: Config.Mask,
// LANGFUSE_CONTENT_CAPTURE_ENABLED, sampling, and payload limits apply
// unchanged. A nil or disabled Langfuse client records nothing.
package jevlangfuse

import (
	"context"
	"errors"
	"maps"
	"strconv"

	"github.com/fgn/go-langfuse"
	jev "github.com/fgn/jevgo"
)

// DefaultObservationName is the name of the recorded generation.
const DefaultObservationName = "typesafe.systemone"

// Option configures [NewTracer].
type Option func(*Tracer)

// WithObservationName sets the generation's name. Default:
// [DefaultObservationName].
func WithObservationName(name string) Option {
	return func(t *Tracer) {
		if name != "" {
			t.name = name
		}
	}
}

// WithoutContentExport keeps model, usage, and metadata but never records
// the state, questions, or answers as observation input and output.
func WithoutContentExport() Option {
	return func(t *Tracer) { t.content = false }
}

// WithErrorDetails records the full error text of a failed call. By default
// only a fixed category such as "http 429" or "timeout" is recorded, which
// keeps server-supplied error messages out of the trace.
func WithErrorDetails() Option {
	return func(t *Tracer) { t.errorDetails = true }
}

// WithMetadata adds static metadata to every recorded generation, for
// example a deployment or feature label. Keys set here are overridden by the
// per-call keys the tracer records.
func WithMetadata(metadata map[string]any) Option {
	return func(t *Tracer) { maps.Copy(t.metadata, metadata) }
}

// Tracer is a [jev.Tracer] backed by a Langfuse client.
type Tracer struct {
	lf           *langfuse.Client
	name         string
	content      bool
	errorDetails bool
	metadata     map[string]any
}

// NewTracer returns a tracer recording generations on lf. A nil lf is
// accepted and records nothing.
func NewTracer(lf *langfuse.Client, opts ...Option) *Tracer {
	t := &Tracer{lf: lf, name: DefaultObservationName, content: true, metadata: map[string]any{}}
	for _, opt := range opts {
		if opt != nil {
			opt(t)
		}
	}
	return t
}

type observationKey struct{}

// TraceSystemOneStart starts the generation and stores it in the returned
// context.
func (t *Tracer) TraceSystemOneStart(ctx context.Context, data jev.SystemOneStartData) context.Context {
	metadata := make(map[string]any, len(t.metadata)+3)
	maps.Copy(metadata, t.metadata)
	metadata["provider"] = "typesafe"
	metadata["sdk"] = "jevgo/" + jev.Version
	metadata["request_model"] = data.Model

	attrs := langfuse.ObservationAttributes{Model: data.Model, Metadata: metadata}
	if t.content && data.Request != nil {
		attrs.Input = map[string]any{"state": data.Request.State, "questions": data.Request.Questions}
	}
	ctx, observation := t.lf.StartObservation(ctx, t.name, langfuse.TypeGeneration, attrs)
	return context.WithValue(ctx, observationKey{}, observation)
}

// TraceSystemOneEnd completes the generation started by TraceSystemOneStart.
func (t *Tracer) TraceSystemOneEnd(ctx context.Context, data jev.SystemOneEndData) {
	observation, _ := ctx.Value(observationKey{}).(*langfuse.Observation)
	if observation == nil {
		return
	}
	metadata := map[string]any{"attempts": data.Attempts}
	if data.Response != nil {
		metadata["request_id"] = data.Response.RequestID
		metadata["http_status"] = data.Response.StatusCode
		update := langfuse.ObservationAttributes{
			Model: data.Response.Model,
			Usage: &langfuse.Usage{
				InputTokens:  int64(data.Response.Usage.InputTokens),
				OutputTokens: int64(data.Response.Usage.OutputTokens),
			},
			Metadata: metadata,
		}
		if t.content {
			update.Output = data.Response.Answers
		}
		observation.Update(update)
	} else {
		failure := classify(data.Err)
		metadata["status"] = failure.status
		if failure.requestID != "" {
			metadata["request_id"] = failure.requestID
		}
		if failure.httpStatus != 0 {
			metadata["http_status"] = failure.httpStatus
		}
		observation.Update(langfuse.ObservationAttributes{Metadata: metadata})
		if t.errorDetails {
			observation.RecordError(data.Err)
		} else {
			observation.RecordError(errors.New(failure.status))
		}
	}
	observation.End()
}

// failure is a fixed status category plus the request ID and HTTP status
// when the failure was an API response.
type failure struct {
	status     string
	requestID  string
	httpStatus int
}

func classify(err error) failure {
	var apiErr *jev.APIError
	var connErr *jev.ConnectionError
	var validationErr *jev.ResponseValidationError
	switch {
	case errors.As(err, &apiErr):
		return failure{"http " + strconv.Itoa(apiErr.StatusCode), apiErr.RequestID, apiErr.StatusCode}
	case errors.As(err, &connErr) && connErr.Timeout > 0:
		return failure{status: "timeout"}
	case errors.As(err, &connErr):
		return failure{status: "connection error"}
	case errors.As(err, &validationErr):
		return failure{"invalid response", validationErr.RequestID, validationErr.StatusCode}
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return failure{status: "canceled"}
	case errors.Is(err, jev.ErrInvalidRequest):
		return failure{status: "invalid request"}
	default:
		return failure{status: "error"}
	}
}
