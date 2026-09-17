package jevlangfuse_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fgn/go-langfuse"
	"github.com/matryer/is"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	jev "github.com/fgn/jevgo"
	jevlangfuse "github.com/fgn/jevgo/contrib/langfuse"
)

const okBody = `{"model":"jev-1.13","answers":{"urgent":{"type":"noul","noul":0.92}},` +
	`"usage":{"input_tokens":12,"output_tokens":3}}`

var request = jev.Request{
	State:     "Help! My payouts have been failing for 3 days.",
	Questions: jev.Questions{"urgent": jev.Noul{Instructions: "Does this convey urgency?"}},
}

// newLangfuse returns a Langfuse client attached to a caller-owned provider
// whose spans are captured by the returned recorder.
func newLangfuse(t *testing.T) (*langfuse.Client, *tracetest.SpanRecorder) {
	t.Helper()
	is := is.New(t)
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	cfg := langfuse.Config{
		BaseURL:        "http://127.0.0.1:1",
		PublicKey:      "pk-lf-test",
		SecretKey:      "sk-lf-test",
		TracerProvider: provider,
	}
	lf, err := langfuse.New(t.Context(), cfg)
	is.NoErr(err)
	t.Cleanup(func() {
		_ = lf.Shutdown(context.Background()) //nolint:usetesting // cleanup needs a live context.
		_ = provider.Shutdown(context.Background())
	})
	return lf, recorder
}

func newClient(t *testing.T, status int, body string, tracer jev.Tracer) *jev.Client {
	t.Helper()
	is := is.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("x-typesafe-request-id", "req-123")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	client, err := jev.NewClient(jev.WithAPIKey("key"), jev.WithBaseURL(server.URL),
		jev.WithRetryPolicy(jev.RetryPolicy{}), jev.WithTracer(tracer))
	is.NoErr(err)
	return client
}

func attributes(span sdktrace.ReadOnlySpan) map[string]string {
	out := map[string]string{}
	for _, kv := range span.Attributes() {
		out[string(kv.Key)] = kv.Value.String()
	}
	return out
}

func TestTracerRecordsGeneration(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	lf, recorder := newLangfuse(t)
	tracer := jevlangfuse.NewTracer(lf, jevlangfuse.WithMetadata(map[string]any{"feature": "triage"}))
	client := newClient(t, http.StatusOK, okBody, tracer)

	ctx, root := lf.StartObservation(t.Context(), "triage", langfuse.TypeSpan, langfuse.ObservationAttributes{})
	resp, err := client.SystemOne(ctx, request)
	is.NoErr(err)
	root.End()

	spans := recorder.Ended()
	is.Equal(len(spans), 2)
	generation := spans[0]
	is.Equal(generation.Name(), jevlangfuse.DefaultObservationName)
	is.Equal(generation.Parent().SpanID(), spans[1].SpanContext().SpanID()) // parented by the caller's observation
	is.True(langfuse.IsLangfuseSpan(generation))

	attrs := attributes(generation)
	is.Equal(attrs["langfuse.observation.type"], "generation")
	is.Equal(attrs["langfuse.observation.model.name"], resp.Model)
	is.True(strings.Contains(attrs["langfuse.observation.usage_details"], "12"))
	is.True(strings.Contains(attrs["langfuse.observation.usage_details"], "3"))
	is.Equal(attrs["langfuse.observation.metadata.provider"], "typesafe")
	is.Equal(attrs["langfuse.observation.metadata.feature"], "triage")
	is.Equal(attrs["langfuse.observation.metadata.request_model"], jev.DefaultModel)
	is.Equal(attrs["langfuse.observation.metadata.request_id"], "req-123")
	is.Equal(attrs["langfuse.observation.metadata.attempts"], "1")
	is.Equal(attrs["langfuse.observation.metadata.http_status"], "200")

	var input map[string]any
	is.NoErr(json.Unmarshal([]byte(attrs["langfuse.observation.input"]), &input))
	is.Equal(input["state"], request.State)
	is.True(strings.Contains(attrs["langfuse.observation.input"], `"type":"noul"`))
	is.True(strings.Contains(attrs["langfuse.observation.output"], `"noul":0.92`))
}

func TestTracerWithoutContentExport(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	lf, recorder := newLangfuse(t)
	tracer := jevlangfuse.NewTracer(lf, jevlangfuse.WithoutContentExport(), jevlangfuse.WithObservationName("jev"))
	client := newClient(t, http.StatusOK, okBody, tracer)
	_, err := client.SystemOne(t.Context(), request)
	is.NoErr(err)

	spans := recorder.Ended()
	is.Equal(len(spans), 1)
	is.Equal(spans[0].Name(), "jev")
	attrs := attributes(spans[0])
	_, hasInput := attrs["langfuse.observation.input"]
	_, hasOutput := attrs["langfuse.observation.output"]
	is.True(!hasInput)
	is.True(!hasOutput)
	is.Equal(attrs["langfuse.observation.model.name"], "jev-1.13") // usage and model are still recorded
}

func TestTracerRecordsFailures(t *testing.T) {
	t.Parallel()
	t.Run("api error category only", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)
		lf, recorder := newLangfuse(t)
		client := newClient(t, http.StatusTooManyRequests, `{"error":"secret server text"}`, jevlangfuse.NewTracer(lf))
		_, err := client.SystemOne(t.Context(), request)
		is.True(errors.Is(err, jev.ErrRateLimit))

		spans := recorder.Ended()
		is.Equal(len(spans), 1)
		attrs := attributes(spans[0])
		is.Equal(attrs["langfuse.observation.metadata.status"], "http 429")
		is.Equal(attrs["langfuse.observation.metadata.http_status"], "429")
		is.Equal(attrs["langfuse.observation.metadata.request_id"], "req-123")
		is.Equal(attrs["langfuse.observation.level"], "ERROR")
		dump := spanDump(spans[0])
		is.True(strings.Contains(dump, "http 429"))
		is.True(!strings.Contains(dump, "secret server text")) // server text stays out by default
	})
	t.Run("error details opt-in", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)
		lf, recorder := newLangfuse(t)
		client := newClient(t, http.StatusTooManyRequests, `{"error":"server text"}`,
			jevlangfuse.NewTracer(lf, jevlangfuse.WithErrorDetails()))
		_, _ = client.SystemOne(t.Context(), request)
		is.True(strings.Contains(spanDump(recorder.Ended()[0]), "server text"))
	})
	t.Run("invalid request never starts a generation", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)
		lf, recorder := newLangfuse(t)
		client := newClient(t, http.StatusOK, okBody, jevlangfuse.NewTracer(lf))
		_, err := client.SystemOne(t.Context(), jev.Request{State: "x"})
		is.True(errors.Is(err, jev.ErrInvalidRequest))
		is.Equal(len(recorder.Ended()), 0)
	})
}

func TestNilClientIsNoop(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	client := newClient(t, http.StatusOK, okBody, jevlangfuse.NewTracer(nil))
	resp, err := client.SystemOne(t.Context(), request)
	is.NoErr(err)
	is.Equal(resp.Model, "jev-1.13")
}

// spanDump renders attributes and events for substring assertions.
func spanDump(span sdktrace.ReadOnlySpan) string {
	var b strings.Builder
	for _, kv := range span.Attributes() {
		b.WriteString(string(kv.Key) + "=" + kv.Value.String() + "\n")
	}
	for _, event := range span.Events() {
		b.WriteString(event.Name + "\n")
		for _, kv := range event.Attributes {
			b.WriteString(string(kv.Key) + "=" + kv.Value.String() + "\n")
		}
	}
	b.WriteString(span.Status().Description)
	return b.String()
}
