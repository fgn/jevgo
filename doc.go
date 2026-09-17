// Package jev is a Go client for the TypeSafe AI System One API and its
// flagship model, Jev.
//
// A request evaluates a state (text or structured data) against named,
// typed questions and returns one typed answer per question:
//
//	client, err := jev.NewClient() // reads TYPESAFE_API_KEY
//	if err != nil {
//		return err
//	}
//	resp, err := client.SystemOne(ctx, jev.Request{
//		State: "I was charged twice. Please fix this ASAP.",
//		Questions: jev.Questions{
//			"urgent":     jev.Noul{Instructions: "Does this convey urgency?"},
//			"department": jev.Choice{
//				Instructions: "Which team should handle this?",
//				Criteria:     map[string]any{"billing": nil, "technical": nil, "other": nil},
//			},
//			"frustration": jev.Score{
//				Instructions: "How frustrated is the customer?",
//				Criteria:     []any{"Calm", "Frustrated", "Very angry"},
//			},
//		},
//	})
//	if err != nil {
//		return err
//	}
//	if a, ok := resp.Choice("department"); ok {
//		fmt.Println(a.Choice, a.Confidence)
//	}
//
// Answers are a sealed interface: type switch on [NoulAnswer], [ChoiceAnswer],
// [ScoreAnswer], and [UnknownAnswer], or use the typed getters on [Response].
//
// # Configuration
//
// [NewClient] accepts functional options. Explicit options take precedence
// over the TYPESAFE_API_KEY, TYPESAFE_BASE_URL, TYPESAFE_DEFAULT_MODEL, and
// TYPESAFE_LOG_LEVEL environment variables, which take precedence over SDK
// defaults. Every option is also accepted by [Client.SystemOne] and
// [Client.ListModels] as a per-call override.
//
// # Errors
//
// Failed HTTP responses are returned as [*APIError], which matches the
// status sentinels ([ErrAuthentication], [ErrRateLimit], [ErrOverloaded], ...)
// through errors.Is. Transport failures are [*ConnectionError], which matches
// [ErrConnection] and, for per-attempt timeouts, [ErrTimeout]. A successful
// response whose body does not match the API contract is a
// [*ResponseValidationError]. Caller cancellation surfaces as the context's
// error, so errors.Is(err, context.Canceled) works as usual.
//
// # Retries
//
// Requests are retried with capped exponential backoff and Retry-After
// support according to a [RetryPolicy]; see [DefaultRetryPolicy] for the
// defaults, which match the official SDKs.
//
// # Instrumentation
//
// A [Tracer] observes every SystemOne call with typed start and end data.
// The github.com/fgn/jevgo/contrib/langfuse module provides a Tracer that
// records a Langfuse generation per call; the core package has no
// dependencies beyond the standard library.
package jev
