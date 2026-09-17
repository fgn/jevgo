# jevgo

[![Go Reference](https://pkg.go.dev/badge/github.com/fgn/jevgo.svg)](https://pkg.go.dev/github.com/fgn/jevgo)
[![License](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

A Go client for [TypeSafe AI](https://typesafe.ai)'s System One API and its
flagship model, Jev. Send a state and typed questions; get typed answers and
probabilities your code can act on directly. Feature parity with the official
[Python](https://github.com/typesafe-ai/typesafe-sdk-python) and
[TypeScript](https://github.com/typesafe-ai/typesafe-sdk-js) SDKs, written the
way a Go library should be: functional options, context cancellation, sentinel
errors, `log/slog`, and no dependencies outside the standard library.

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
				Criteria:     []any{"Calm", "Frustrated", "Very angry"},
			},
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	department, _ := resp.Choice("department")
	fmt.Println(department.Choice, department.Confidence)
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
| `jev.Score` | `jev.ScoreAnswer` | weighted position in `Score`, `Probabilities` and `Legend` per level, `Confidence` |

```go
for name, answer := range resp.Answers {
	switch a := answer.(type) {
	case jev.NoulAnswer:
		fmt.Println(name, a.Noul)
	case jev.ChoiceAnswer:
		fmt.Println(name, a.Choice, a.Confidence)
	case jev.ScoreAnswer:
		fmt.Println(name, a.Score, a.Legend[int(a.Score)])
	case jev.UnknownAnswer: // an answer kind newer than this SDK; a.Raw holds the JSON
	}
}
```

State, instructions, and criteria accept strings or structured JSON: maps,
slices, or structs with `json` tags. `jev.RawQuestion` sends a question
verbatim for fields this version does not model, and `Request.Extra` adds
top-level request fields. `Response.RawBody` keeps the complete response.

## Configuration

Explicit options win over environment variables, which win over defaults.
Every option is also accepted per call as an override.

| Option | Environment | Default |
| --- | --- | --- |
| `WithAPIKey` | `TYPESAFE_API_KEY` | required |
| `WithBaseURL` | `TYPESAFE_BASE_URL` | `https://api.typesafe.ai` |
| `WithDefaultModel` | `TYPESAFE_DEFAULT_MODEL` | `jev-latest` |
| `WithLogger` | `TYPESAFE_LOG_LEVEL` | no logging |
| `WithTimeout` (per attempt) | | 10s |
| `WithRetryPolicy` | | 2 retries, 500ms to 5s backoff, honors `Retry-After` |
| `WithHTTPClient`, `WithHeader`, `WithTracer` | | |

```go
policy := jev.DefaultRetryPolicy()
policy.MaxRetries = 4
policy.TotalTimeout = 30 * time.Second

client, err := jev.NewClient(
	jev.WithRetryPolicy(policy),
	jev.WithLogger(slog.Default()),
)
resp, err := client.SystemOne(ctx, req, jev.WithTimeout(3*time.Second))
```

## Errors

```go
resp, err := client.SystemOne(ctx, req)
switch {
case errors.Is(err, jev.ErrRateLimit):
	// HTTP 429 after retries; also ErrAuthentication, ErrUnprocessableEntity,
	// ErrOverloaded (529), ErrInternalServer, ...
case errors.Is(err, jev.ErrTimeout):
	// per-attempt timeout after retries; ErrConnection covers all transport failures
case errors.Is(err, context.Canceled):
	// the caller's context
case errors.Is(err, jev.ErrInvalidRequest):
	// rejected before sending: no questions, a Score with fewer than two levels, ...
}

var apiErr *jev.APIError
if errors.As(err, &apiErr) {
	log.Println(apiErr.StatusCode, apiErr.Message, apiErr.RequestID)
}
```

A successful response that does not match the API contract is a
`*jev.ResponseValidationError` naming the offending field.

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

## Examples

- [examples/basic](examples/basic/main.go): all three question types.
- [examples/structured](examples/structured/main.go): structured state and criteria, type switching.
- [contrib/langfuse/examples/basic](contrib/langfuse/examples/basic/main.go): Langfuse tracing.

## Development

```sh
task        # format, lint, test
task test:live   # exercises the real API with TYPESAFE_API_KEY
```

The docs, official SDK sources, and OpenAPI spec this client was built from
are kept locally under `ref/` (not committed); see the
[TypeSafe docs](https://docs.typesafe.ai) for the live versions.
