package jev

import "context"

// Tracer observes SystemOne calls. TraceSystemOneStart runs before the first
// HTTP attempt and its returned context is used for every attempt, so a
// tracer can parent child spans or carry state to TraceSystemOneEnd.
// TraceSystemOneEnd runs once after the call completes, successfully or not.
//
// Implementations must be safe for concurrent use. The Request and Response
// are shared with the caller and must not be modified.
type Tracer interface {
	TraceSystemOneStart(ctx context.Context, data SystemOneStartData) context.Context
	TraceSystemOneEnd(ctx context.Context, data SystemOneEndData)
}

// SystemOneStartData describes a SystemOne call about to be sent.
type SystemOneStartData struct {
	// Request is the caller's request.
	Request *Request
	// Model is the resolved model name sent to the API.
	Model string
}

// SystemOneEndData describes a completed SystemOne call.
type SystemOneEndData struct {
	// Response is the decoded response, or nil when Err is non-nil.
	Response *Response
	// Err is the error returned to the caller, or nil.
	Err error
	// Attempts is the number of HTTP attempts made, including retries.
	Attempts int
}

// MultiTracer returns a Tracer that calls each tracer in order. Start
// contexts chain, so later tracers see values set by earlier ones; End runs
// in reverse order.
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
