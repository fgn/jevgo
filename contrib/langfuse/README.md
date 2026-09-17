# jevlangfuse

Opt-in [Langfuse](https://langfuse.com) instrumentation for
[jevgo](https://github.com/fgn/jevgo), the Go client for TypeSafe AI's
System One API, built on [go-langfuse](https://github.com/fgn/go-langfuse).
The core `jevgo` module has no Langfuse or OpenTelemetry dependency; install
this adapter only when you want it:

```sh
go get github.com/fgn/jevgo/contrib/langfuse
```

## Wiring

```go
lf, err := langfuse.New(ctx, langfuse.ConfigFromEnv())
...
client, err := jev.NewClient(jev.WithTracer(jevlangfuse.NewTracer(lf)))
```

Every `SystemOne` call now records one generation observation, parented by
whatever observation is in the request context:

| Field | Source |
| --- | --- |
| Name and type | `typesafe.systemone`, generation (rename with `WithObservationName`) |
| Model | request model at start, response model on completion |
| Input / output | state and questions; answers keyed by question name |
| Usage | exact `input_tokens` and `output_tokens` from the response |
| Metadata | `provider`, `sdk`, `request_model`, `request_id`, `http_status`, `attempts`, plus `WithMetadata` |
| Status | on failure, a fixed category: `http <code>`, `timeout`, `connection error`, `invalid response`, `canceled`, `invalid request` |

Retries fold into the single generation; `attempts` counts HTTP attempts.
Usage comes from the final successful response only, so nothing is double
counted.

## Privacy

Input and output flow through the core client's controls: `Config.Mask`,
`LANGFUSE_CONTENT_CAPTURE_ENABLED`, sampling, and payload limits apply
unchanged. `WithoutContentExport()` keeps model, usage, and metadata but
never records input or output. Failed calls record only a category by
default; `WithErrorDetails()` records the full error text, which can include
the server's error message. The API key never reaches the trace. A nil or
disabled Langfuse client records nothing.

## Example

```go
ctx, root := lf.StartObservation(ctx, "triage-ticket", langfuse.TypeSpan,
	langfuse.ObservationAttributes{Input: ticket})
defer root.End()

resp, err := client.SystemOne(ctx, jev.Request{
	State: ticket,
	Questions: jev.Questions{
		"department": jev.Choice{
			Instructions: "Which team should handle this?",
			Criteria:     map[string]any{"billing": nil, "technical": nil, "sales": nil},
		},
	},
})
```

See [examples/basic](examples/basic/main.go) for a runnable program.
