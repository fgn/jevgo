# jevgo

[![Go Reference](https://pkg.go.dev/badge/github.com/fgn/jevgo.svg)](https://pkg.go.dev/github.com/fgn/jevgo)
[![License](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

A Go client for [TypeSafe AI](https://typesafe.ai)'s System One API and its
flagship model, Jev. Send a state and typed questions; get typed answers and
probabilities your code can act on directly. It covers the same API surface as
the official [Python](https://github.com/typesafe-ai/typesafe-sdk-python) and
[TypeScript](https://github.com/typesafe-ai/typesafe-sdk-js) SDKs with the same
defaults, written the way a Go library should be: functional options, context
cancellation, sentinel errors, `log/slog`, and no dependencies outside the
standard library. See [Compatibility](#compatibility) for where behavior
intentionally differs.

Optional [Langfuse](https://langfuse.com) instrumentation ships as a separate
module, [contrib/langfuse](contrib/langfuse), built on
[go-langfuse](https://github.com/fgn/go-langfuse). This is a community client;
it is not affiliated with TypeSafe.

## Install

```sh
go get github.com/fgn/jevgo
```

Pass the API key explicitly, or set `TYPESAFE_API_KEY` in the environment
and call `jev.NewClient()` with no options:

```go
client, err := jev.NewClient(jev.WithAPIKey(os.Getenv("MY_TYPESAFE_KEY")))
```

A complete program:

```go
package main

import (
	"context"
	"fmt"
	"log"

	jev "github.com/fgn/jevgo"
)

func main() {
	client, err := jev.NewClient()
	if err != nil {
		log.Fatal(err)
	}
	resp, err := client.SystemOne(context.Background(), jev.Request{
		State: "I was charged twice. Please fix this ASAP.",
		Questions: jev.Questions{
			"urgent": jev.Noul{Instructions: "Does this convey urgency?"},
			"department": jev.Choice{
				Instructions: "Which team should handle this?",
				Criteria:     map[string]any{"billing": nil, "technical": nil, "other": nil},
			},
			"frustration": jev.Score{
				Instructions: "How frustrated is the customer?",
				Criteria:     []string{"Calm", "Frustrated", "Very angry"},
			},
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	if department, ok := resp.Choice("department"); ok {
		fmt.Println(department.Choice, department.Confidence)
	}
}
```

## Questions and answers

The three question types map to three answer types. Answers come back under
the names you chose, as a sealed interface you can type switch on, or through
typed getters.

| Question | Answer | What you get |
| --- | --- | --- |
| `jev.Noul` | `jev.NoulAnswer` | probability of yes in `Noul` |
| `jev.Choice` | `jev.ChoiceAnswer` | selected option in `Choice`, `Probabilities` per option, `Confidence` |
| `jev.Score` | `jev.ScoreAnswer` | weighted position in `Score`, `Probabilities` and `Legend` per level, `Confidence`, `Level()` for the most likely level |

```go
for name, answer := range resp.Answers {
	switch a := answer.(type) {
	case jev.NoulAnswer:
		fmt.Println(name, a.Noul)
	case jev.ChoiceAnswer:
		fmt.Println(name, a.Choice, a.Confidence)
	case jev.ScoreAnswer:
		fmt.Println(name, a.Score, a.Legend[a.Level()]) // Score is a weighted average, not a level
	case jev.UnknownAnswer: // an answer kind newer than this SDK; a.Raw holds the JSON
	}
}
```

State, instructions, and criteria accept strings or structured JSON: maps,
slices, or structs with `json` tags, so a `[]string` of levels or a
`map[string]string` of options works as is. `jev.RawQuestion` sends a
question verbatim for fields this version does not model, and `Request.Extra`
adds top-level request fields. `Response.RawBody` keeps the complete response.
Responses are validated against the API contract and the questions asked: a
missing answer, a null probability, or a level key that is not an index is a
`*jev.ResponseValidationError` rather than a silent zero.

`Request` and `Response` are not the wire format; the client encodes and
decodes them. Individual questions and answers marshal to their wire JSON.

## Configuration

Explicit options win over environment variables, which win over defaults. An
empty string means "not set". Every option is also accepted per call as an
override, and options are safe to build once and reuse across goroutines.

| Option | Environment | Default |
| --- | --- | --- |
| `WithAPIKey` | `TYPESAFE_API_KEY` | required |
| `WithBaseURL` | `TYPESAFE_BASE_URL` | `https://api.typesafe.ai` |
| `WithDefaultModel` | `TYPESAFE_DEFAULT_MODEL` | `jev-latest` |
| `WithLogger` | `TYPESAFE_LOG_LEVEL` | no logging |
| `WithTimeout` (per attempt) | | 10s |
| `WithMaxRetries`, `WithRetryPolicy` | | 2 retries, 500ms to 5s backoff, honors `Retry-After` |
| `WithHTTPClient`, `WithHeader`, `WithTracer` | | |

```go
policy := jev.DefaultRetryPolicy()
policy.TotalTimeout = 30 * time.Second // deadline for attempts and delays together

client, err := jev.NewClient(
	jev.WithRetryPolicy(policy),
	jev.WithLogger(slog.Default()),
)
resp, err := client.SystemOne(ctx, req, jev.WithTimeout(3*time.Second), jev.WithMaxRetries(4))
```

`WithTimeout` bounds each attempt; `TotalTimeout` bounds the whole call and
reports as `ErrTimeout` when it expires. The caller's context deadline always
applies as well.

## Errors

```go
resp, err := client.SystemOne(ctx, req)
switch {
case errors.Is(err, jev.ErrRateLimit):
	// HTTP 429 after retries; also ErrAuthentication, ErrUnprocessableEntity,
	// ErrOverloaded (529), ErrInternalServer, ...
case errors.Is(err, jev.ErrTimeout):
	// per-attempt, total, or transport timeout after retries; check this before
	// context.DeadlineExceeded, which SDK timeouts also wrap
case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
	// the caller's context
case errors.Is(err, jev.ErrInvalidRequest):
	// rejected before sending: no questions, a Score with fewer than two levels, ...
}

var apiErr *jev.APIError
if errors.As(err, &apiErr) {
	log.Println(apiErr.StatusCode, apiErr.Message, apiErr.RequestID, apiErr.Attempts)
}
```

A successful response that does not match the API contract is a
`*jev.ResponseValidationError` naming the offending field. Every error and
response carries the request ID and the number of HTTP attempts made.
Error messages never contain the API key; debug logs contain request and
response bodies, so enable them deliberately.

## Instrumentation

`jev.Tracer` observes every `SystemOne` call with typed start and end data,
and the context it returns is used for the HTTP requests, so spans parent
correctly. The [contrib/langfuse](contrib/langfuse) module records one
Langfuse generation per call with model, input, output, token usage, request
ID, and attempt count:

```go
client, err := jev.NewClient(jev.WithTracer(jevlangfuse.NewTracer(lf)))
```

For transport-level instrumentation, pass an `*http.Client` with your own
`RoundTripper` through `WithHTTPClient`.

## Compatibility

Behavior follows the [API reference](https://docs.typesafe.ai/api) and the
OpenAPI schema, and defaults match the official SDKs. Known differences:

| Behavior | This client | Python | TypeScript |
| --- | --- | --- | --- |
| Score with one level | accepted (OpenAPI `minItems: 1`) | accepted | rejected |
| Missing or null response fields | validation error | validation error | passed through |
| Unknown answer kinds | `UnknownAnswer` with raw JSON | skipped | passed through |
| `Retry-After` above 60s | falls back to backoff | honored, 30s call budget | falls back to backoff |
| Total call budget | off by default | 30s by default | none |
| Per-call header with the same name | replaces | replaces | replaces |
| Model `release_date` | string as sent | string | string |

## Examples

- [examples/basic](examples/basic/main.go): all three question types.
- [examples/structured](examples/structured/main.go): structured state and criteria, type switching.
- [contrib/langfuse/examples/basic](contrib/langfuse/examples/basic/main.go): Langfuse tracing.

## Development

```sh
task        # format, lint, test
task test:live   # exercises the real API with TYPESAFE_API_KEY
```

Behavior follows the [TypeSafe API reference](https://docs.typesafe.ai/api)
and the official SDKs; the defaults for timeouts, retries, and headers match
theirs so that switching between SDKs is not a behavior change.
