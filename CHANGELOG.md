# Changelog

## v0.3.0 (2026-09-17)

- Validation and tracing use the request as sent: `Request.Extra` overrides
  of `questions` or `model` are checked and reported correctly.
- Responses are checked against the criteria sent: option and level sets,
  distribution sums, the selected option being the most probable, the score
  being the weighted level, and legend descriptions being strings, objects,
  or arrays.
- A response over 16 MiB is a `ResponseValidationError` wrapping
  `ErrResponseTooLarge` and is not retried.
- `RetryPolicy.TotalTimeout` also bounds backoff waits and the gap before a
  retry.
- `ConnectionError` gains `Header`.
- Error messages truncate on rune boundaries.
- Langfuse adapter: timeouts keep the request ID and HTTP status; the same
  tracer composed twice ends both observations.

## v0.2.0 (2026-09-17)

Breaking: `Choice.Criteria` and `Score.Criteria` are `any` (any JSON
object or array shaped value), `Model.ReleaseDate` is a string, and
`WithHeader` replaces instead of appending.

- Fix a data race when a `WithRetryPolicy` option is reused.
- Reject base URLs with credentials, query, or fragment without echoing them.
- `TotalTimeout` bounds running attempts; timeouts report the limit that
  elapsed.
- Strict response validation; typed nil questions are rejected.
- One-level scores accepted per the OpenAPI schema.
- Add `WithMaxRetries`, `ScoreAnswer.Level`, attempt counts on responses and
  errors, and executable examples.

## v0.1.0 (2026-09-17)

Initial release.
