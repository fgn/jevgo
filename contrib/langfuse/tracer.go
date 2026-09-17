// Package jevlangfuse records a Langfuse generation for every TypeSafe
// SystemOne call made through github.com/fgn/jevgo, using
// github.com/fgn/go-langfuse.
//
//	client, err := jev.NewClient(jev.WithTracer(jevlangfuse.NewTracer(lf)))
//
// Each call becomes one generation observation, parented by whatever
// observation is in the request context, with the wire model, the request
// as input, the answers as output, token usage, request ID, and attempt
// count. Retries fold into the single generation. Everything recorded flows
// through the core client's privacy controls (Config.Mask, content capture,
// sampling, and payload limits). A nil or disabled Langfuse client records
// nothing.
package jevlangfuse

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"strconv"
	"sync/atomic"

	"github.com/fgn/go-langfuse"
	jev "github.com/fgn/jevgo"
)

// DefaultObservationName is the name of the recorded generation.
const DefaultObservationName = "typesafe.systemone"

// Option configures [NewTracer].
type Option func(*Tracer)

// WithObservationName renames the generation. Default: [DefaultObservationName].
func WithObservationName(name string) Option {
	return func(t *Tracer) {
		if name != "" {
			t.name = name
		}
	}
}

// WithoutContentExport records model, usage, and metadata but never the
// request or the answers.
func WithoutContentExport() Option {
	return func(t *Tracer) { t.content = false }
}

// WithErrorDetails records the full error text of a failed call. By default
// only a category such as "http 429" or "timeout" is recorded, keeping
// server-supplied messages out of the trace.
func WithErrorDetails() Option {
	return func(t *Tracer) { t.errorDetails = true }
}

// WithMetadata adds static metadata to every generation. The tracer's own
// keys (provider, sdk, request_model, request_id, http_status, attempts,
// status) take precedence.
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

// NewTracer returns a tracer recording generations on lf; a nil lf records
// nothing.
func NewTracer(lf *langfuse.Client, opts ...Option) *Tracer {
	t := &Tracer{lf: lf, name: DefaultObservationName, content: true, metadata: map[string]any{}}
	for _, opt := range opts {
		if opt != nil {
			opt(t)
		}
	}
	return t
}

// observationKey is per tracer instance so composed tracers cannot
// overwrite each other's observation.
type observationKey struct{ tracer *Tracer }

// observationNode links the observations one tracer started under a
// context, so a tracer composed twice ends both, newest first.
type observationNode struct {
	observation *langfuse.Observation
	parent      *observationNode
	ended       atomic.Bool
}

func (n *observationNode) pop() *langfuse.Observation {
	for ; n != nil; n = n.parent {
		if n.ended.CompareAndSwap(false, true) {
			return n.observation
		}
	}
	return nil
}

// TraceSystemOneStart starts the generation and stores it in the returned
// context.
func (t *Tracer) TraceSystemOneStart(ctx context.Context, data jev.SystemOneStartData) context.Context {
	metadata := make(map[string]any, len(t.metadata)+3)
	maps.Copy(metadata, t.metadata)
	metadata["provider"] = "typesafe"
	metadata["sdk"] = "jevgo/" + jev.Version
	metadata["request_model"] = data.Model

	attrs := langfuse.ObservationAttributes{Model: data.Model, Metadata: metadata}
	if t.content {
		attrs.Input = wireInput(data.Body)
	}
	parent, _ := ctx.Value(observationKey{t}).(*observationNode)
	ctx, observation := t.lf.StartObservation(ctx, t.name, langfuse.TypeGeneration, attrs)
	return context.WithValue(ctx, observationKey{t}, &observationNode{observation: observation, parent: parent})
}

// wireInput is the wire body without the model, which is a separate field.
func wireInput(body json.RawMessage) any {
	var input map[string]any
	if json.Unmarshal(body, &input) != nil {
		return nil
	}
	delete(input, "model")
	return input
}

// TraceSystemOneEnd completes the generation started by TraceSystemOneStart.
func (t *Tracer) TraceSystemOneEnd(ctx context.Context, data jev.SystemOneEndData) {
	node, _ := ctx.Value(observationKey{t}).(*observationNode)
	observation := node.pop()
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
	case errors.As(err, &connErr):
		status := "connection error"
		if errors.Is(err, jev.ErrTimeout) {
			status = "timeout"
		}
		return failure{status, connErr.RequestID, connErr.StatusCode}
	case errors.Is(err, jev.ErrTimeout):
		return failure{status: "timeout"}
	case errors.As(err, &validationErr):
		return failure{"invalid response", validationErr.RequestID, validationErr.StatusCode}
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return failure{status: "canceled"}
	default:
		return failure{status: "error"}
	}
}
