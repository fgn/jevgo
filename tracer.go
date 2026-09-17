package jev

import (
	"context"
	"encoding/json"
)

// Tracer observes SystemOne calls that passed local validation. The context
// returned by TraceSystemOneStart is used for every HTTP attempt and passed
// to TraceSystemOneEnd, which runs exactly once per traced call.
//
// Implementations must be safe for concurrent use and must not modify the
// data they receive.
type Tracer interface {
	TraceSystemOneStart(ctx context.Context, data SystemOneStartData) context.Context
	TraceSystemOneEnd(ctx context.Context, data SystemOneEndData)
}

// SystemOneStartData describes a call about to be sent.
type SystemOneStartData struct {
	Request *Request
	// Model is the model on the wire, after [Request.Extra] is applied.
	Model string
	// Body is the encoded wire request.
	Body json.RawMessage
}

// SystemOneEndData describes a completed call.
type SystemOneEndData struct {
	// Response is nil when Err is non-nil.
	Response *Response
	Err      error
	// Attempts is the number of HTTP attempts made; zero when none was sent.
	Attempts int
}

// MultiTracer composes tracers. Start contexts chain in order, so each
// tracer sees values set by the ones before it; End runs in reverse order
// with the context returned by the last Start.
func MultiTracer(tracers ...Tracer) Tracer {
	filtered := make([]Tracer, 0, len(tracers))
	for _, t := range tracers {
		if t != nil {
			filtered = append(filtered, t)
		}
	}
	return multiTracer(filtered)
}

type multiTracer []Tracer

func (m multiTracer) TraceSystemOneStart(ctx context.Context, data SystemOneStartData) context.Context {
	for _, t := range m {
		ctx = t.TraceSystemOneStart(ctx, data) //nolint:fatcontext // contexts chain by design.
	}
	return ctx
}

func (m multiTracer) TraceSystemOneEnd(ctx context.Context, data SystemOneEndData) {
	for i := len(m) - 1; i >= 0; i-- {
		m[i].TraceSystemOneEnd(ctx, data)
	}
}
