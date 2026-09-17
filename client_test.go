package jev_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/matryer/is"

	jev "github.com/fgn/jevgo"
)

const testKey = "apikey-0123456789abcdef"

const okBody = `{"model":"jev-1.13","answers":{"urgent":{"type":"noul","noul":0.92}},` +
	`"usage":{"input_tokens":12,"output_tokens":3}}`

var basicRequest = jev.Request{
	State:     "Help! My payouts have been failing for 3 days.",
	Questions: jev.Questions{"urgent": jev.Noul{Instructions: "Does this convey urgency?"}},
}

// fastRetry keeps retry tests quick while preserving the default shape.
func fastRetry() jev.RetryPolicy {
	p := jev.DefaultRetryPolicy()
	p.InitialBackoff = time.Millisecond
	p.MaxBackoff = 2 * time.Millisecond
	return p
}

// newClient starts a test server and returns a client pointed at it.
func newClient(t *testing.T, handler http.HandlerFunc, opts ...jev.Option) *jev.Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	base := []jev.Option{jev.WithAPIKey(testKey), jev.WithBaseURL(server.URL), jev.WithRetryPolicy(fastRetry())}
	client, err := jev.NewClient(append(base, opts...)...)
	is.New(t).NoErr(err)
	return client
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("x-typesafe-request-id", "req-123")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// waitForClient drains the request body, so the server notices a client
// disconnect, then blocks until the client goes away or the fallback elapses.
func waitForClient(r *http.Request, fallback time.Duration) {
	_, _ = io.Copy(io.Discard, r.Body)
	select {
	case <-r.Context().Done():
	case <-time.After(fallback):
	}
}

func okHandler(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, okBody) }

//nolint:paralleltest // uses t.Setenv.
func TestNewClientConfiguration(t *testing.T) {
	t.Run("missing api key", func(t *testing.T) {
		is := is.New(t)
		t.Setenv(jev.EnvAPIKey, "  ")
		_, err := jev.NewClient()
		is.True(errors.Is(err, jev.ErrInvalidConfig)) // blank env key is ignored
	})
	t.Run("environment fallback and precedence", func(t *testing.T) {
		is := is.New(t)
		t.Setenv(jev.EnvAPIKey, " env-key ")
		t.Setenv(jev.EnvBaseURL, "https://proxy.example/v2/ ")
		t.Setenv(jev.EnvDefaultModel, "jev-preview")
		client, err := jev.NewClient()
		is.NoErr(err)
		is.Equal(client.BaseURL(), "https://proxy.example/v2") // trimmed, no trailing slash
		is.Equal(client.DefaultModel(), "jev-preview")

		client, err = jev.NewClient(jev.WithBaseURL("http://localhost:1/"), jev.WithDefaultModel("jev-latest"))
		is.NoErr(err)
		is.Equal(client.BaseURL(), "http://localhost:1") // explicit beats env
		is.Equal(client.DefaultModel(), "jev-latest")
	})
	t.Run("defaults", func(t *testing.T) {
		is := is.New(t)
		t.Setenv(jev.EnvBaseURL, "")
		t.Setenv(jev.EnvDefaultModel, "")
		client, err := jev.NewClient(jev.WithAPIKey(testKey))
		is.NoErr(err)
		is.Equal(client.BaseURL(), jev.DefaultBaseURL)
		is.Equal(client.DefaultModel(), jev.DefaultModel)
	})
	t.Run("invalid log level", func(t *testing.T) {
		is := is.New(t)
		t.Setenv(jev.EnvLogLevel, "loud")
		_, err := jev.NewClient(jev.WithAPIKey(testKey))
		is.True(errors.Is(err, jev.ErrInvalidConfig))
	})
}

func TestInvalidOptions(t *testing.T) {
	t.Parallel()
	invalid := map[string]jev.Option{
		"base url without scheme": jev.WithBaseURL("api.typesafe.ai"),
		"base url ftp":            jev.WithBaseURL("ftp://x"),
		"zero timeout":            jev.WithTimeout(0),
		"negative retries":        jev.WithRetryPolicy(jev.RetryPolicy{MaxRetries: -1}),
		"jitter out of range":     jev.WithRetryPolicy(jev.RetryPolicy{Jitter: 1.5}),
		"bad status":              jev.WithRetryPolicy(jev.RetryPolicy{Statuses: []int{42}}),
	}
	for name, opt := range invalid {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			is := is.New(t)
			_, err := jev.NewClient(jev.WithAPIKey(testKey), opt)
			is.True(errors.Is(err, jev.ErrInvalidConfig))
		})
	}
}

func TestSystemOneRequestWire(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	var got struct {
		method, path string
		header       http.Header
		body         map[string]any
	}
	client := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		got.method, got.path, got.header = r.Method, r.URL.Path, r.Header.Clone()
		_ = json.NewDecoder(r.Body).Decode(&got.body)
		writeJSON(w, http.StatusOK, okBody)
	}, jev.WithHeader("X-Custom", "yes"), jev.WithHeader("Authorization", "Bearer stolen"))

	req := jev.Request{
		State: map[string]any{"document": "I was charged twice."},
		Questions: jev.Questions{
			"urgent": jev.Noul{Instructions: "Urgent?", Criteria: &jev.NoulCriteria{True: "Time-sensitive"}},
			"department": jev.Choice{
				Instructions: "Which team?",
				Criteria:     map[string]any{"billing": nil, "other": "Anything else"},
			},
			"severity": jev.Score{Instructions: []any{"How severe?"}, Criteria: []any{"Low", map[string]any{"what": "High"}}},
			"raw":      jev.RawQuestion{"type": "noul", "instructions": "Raw?", "weight": 2},
		},
		Extra: map[string]any{"beam_width": 4, "model": "jev-preview"},
	}
	_, err := client.SystemOne(t.Context(), req, jev.WithHeader("X-Call", "1"))
	is.NoErr(err)

	is.Equal(got.method, http.MethodPost)
	is.Equal(got.path, "/v1/systemone")
	is.Equal(got.header.Get("Authorization"), "Bearer "+testKey) // caller cannot override auth
	is.Equal(got.header.Get("Accept"), "application/json")
	is.Equal(got.header.Get("Content-Type"), "application/json")
	is.Equal(got.header.Get("User-Agent"), "jevgo/"+jev.Version)
	is.Equal(got.header.Get("X-TypeSafe-SDK"), "jevgo/"+jev.Version)
	is.True(strings.HasPrefix(got.header.Get("X-TypeSafe-Runtime"), "go/"))
	is.Equal(got.header.Get("X-Custom"), "yes") // client header
	is.Equal(got.header.Get("X-Call"), "1")     // per-call header
	_, hasRetryCount := got.header["X-Typesafe-Retry-Count"]
	is.True(!hasRetryCount) // no retry count on the first attempt

	want := map[string]any{
		"state":      map[string]any{"document": "I was charged twice."},
		"model":      "jev-preview", // Extra overrides model, last write wins
		"beam_width": 4.0,
		"questions": map[string]any{
			"urgent": map[string]any{
				"type": "noul", "instructions": "Urgent?", "criteria": map[string]any{"true": "Time-sensitive"},
			},
			"department": map[string]any{
				"type": "choice", "instructions": "Which team?",
				"criteria": map[string]any{"billing": nil, "other": "Anything else"},
			},
			"severity": map[string]any{
				"type": "score", "instructions": []any{"How severe?"},
				"criteria": []any{"Low", map[string]any{"what": "High"}},
			},
			"raw": map[string]any{"type": "noul", "instructions": "Raw?", "weight": 2.0},
		},
	}
	is.Equal(got.body, want)
}

func TestSystemOneModelResolution(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	var body map[string]any
	client := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		writeJSON(w, http.StatusOK, okBody)
	}, jev.WithDefaultModel("jev-preview"))

	req := jev.Request{State: "x", Questions: jev.Questions{"q": jev.Noul{}}}
	_, err := client.SystemOne(t.Context(), req)
	is.NoErr(err)
	is.Equal(body["model"], "jev-preview") // client default model
	question := body["questions"].(map[string]any)["q"].(map[string]any)
	_, hasInstructions := question["instructions"]
	is.True(!hasInstructions) // nil instructions are omitted

	req.Model = "jev-1.13"
	_, err = client.SystemOne(t.Context(), req)
	is.NoErr(err)
	is.Equal(body["model"], "jev-1.13") // request model wins
}

func TestRequestValidation(t *testing.T) {
	t.Parallel()
	var requests atomic.Int32
	client := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		writeJSON(w, http.StatusOK, okBody)
	})
	cases := map[string]jev.Questions{
		"no questions":        nil,
		"nil question":        {"q": nil},
		"score one level":     {"q": jev.Score{Criteria: []any{"only"}}},
		"score nil criteria":  {"q": jev.Score{}},
		"choice nil criteria": {"q": jev.Choice{Instructions: "?"}},
		"raw without type":    {"q": jev.RawQuestion{"instructions": "?"}},
		"raw score one level": {"q": jev.RawQuestion{"type": "score", "criteria": []any{"a"}}},
	}
	for name, questions := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			is := is.New(t)
			_, err := client.SystemOne(t.Context(), jev.Request{State: "x", Questions: questions})
			is.True(errors.Is(err, jev.ErrInvalidRequest))
		})
	}
	t.Run("unencodable state", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)
		_, err := client.SystemOne(t.Context(), jev.Request{State: make(chan int), Questions: jev.Questions{"q": jev.Noul{}}})
		is.True(errors.Is(err, jev.ErrInvalidRequest))
	})
	t.Cleanup(func() { is.New(t).Equal(requests.Load(), int32(0)) }) // nothing reached the server
}

func TestSystemOneResponseDecoding(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	body := `{
	  "model": "jev-1.13",
	  "answers": {
	    "urgent": {"type": "noul", "noul": 0.92},
	    "department": {"type": "choice", "choice": "technical",
	      "probabilities": {"billing": 0.08, "technical": 0.85, "sales": 0.07}, "confidence": 0.82},
	    "frustration": {"type": "score", "score": 1.6,
	      "legend": {"0": "Calm", "1": {"what": "Frustrated"}, "2": ["Very", "angry"]},
	      "probabilities": {"0": 0.05, "1": 0.3, "2": 0.65}, "confidence": 0.78},
	    "future": {"type": "ranking", "order": ["a", "b"]}
	  },
	  "usage": {"input_tokens": 312, "output_tokens": 48},
	  "new_field": true
	}`
	client := newClient(t, func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, body) })
	resp, err := client.SystemOne(t.Context(), basicRequest)
	is.NoErr(err)

	is.Equal(resp.Model, "jev-1.13")
	is.Equal(resp.Usage, jev.Usage{InputTokens: 312, OutputTokens: 48})
	is.Equal(resp.RequestID, "req-123")
	is.Equal(resp.StatusCode, http.StatusOK)
	is.Equal(string(resp.RawBody), body)
	is.Equal(len(resp.Answers), 4)

	noul, ok := resp.Noul("urgent")
	is.True(ok)
	is.Equal(noul.Noul, 0.92)

	choice, ok := resp.Choice("department")
	is.True(ok)
	is.Equal(choice.Choice, "technical")
	is.Equal(choice.Confidence, 0.82)
	is.Equal(choice.Probabilities["technical"], 0.85)

	score, ok := resp.Score("frustration")
	is.True(ok)
	is.Equal(score.Score, 1.6)
	is.Equal(score.Confidence, 0.78)
	is.Equal(score.Probabilities[2], 0.65)
	is.Equal(score.Legend, map[int]any{0: "Calm", 1: map[string]any{"what": "Frustrated"}, 2: []any{"Very", "angry"}})

	unknown, ok := resp.Answers["future"].(jev.UnknownAnswer)
	is.True(ok)
	is.Equal(unknown.Type, "ranking")
	is.True(strings.Contains(string(unknown.Raw), `"order"`))

	_, ok = resp.Noul("department")
	is.True(!ok) // getter does not match another kind
	_, ok = resp.Noul("missing")
	is.True(!ok)
	is.Equal(len(resp.Nouls())+len(resp.Choices())+len(resp.Scores()), 3)

	encoded, err := json.Marshal(resp.Answers)
	is.NoErr(err)
	var roundTrip map[string]map[string]any
	is.NoErr(json.Unmarshal(encoded, &roundTrip))
	is.Equal(roundTrip["urgent"]["type"], "noul")
	is.Equal(roundTrip["department"]["type"], "choice")
	is.Equal(roundTrip["frustration"]["type"], "score")
	is.Equal(roundTrip["future"]["type"], "ranking")
	is.Equal(roundTrip["frustration"]["legend"].(map[string]any)["0"], "Calm")
}

func TestResponseValidationErrors(t *testing.T) {
	t.Parallel()
	const prefix = `{"model":"m","answers":{"q":`
	const suffix = `},"usage":{}}`
	cases := map[string]struct{ body, path string }{
		"not json":            {`<html>`, ""},
		"missing model":       {`{"answers":{},"usage":{}}`, "model"},
		"missing answers":     {`{"model":"m","usage":{}}`, "answers"},
		"missing usage":       {`{"model":"m","answers":{}}`, "usage"},
		"wrong top type":      {`{"model":5,"answers":{},"usage":{}}`, "model"},
		"answer without type": {prefix + `{"noul":0.5}` + suffix, "answers.q.type"},
		"noul missing value":  {prefix + `{"type":"noul"}` + suffix, "answers.q.noul"},
		"noul wrong type":     {prefix + `{"type":"noul","noul":"high"}` + suffix, "answers.q.noul"},
		"choice missing conf": {
			prefix + `{"type":"choice","choice":"a","probabilities":{"a":1}}` + suffix,
			"answers.q.confidence",
		},
		"score bad level key": {
			prefix + `{"type":"score","score":1,"confidence":1,"legend":{"low":"x"},"probabilities":{"0":1}}` + suffix,
			"answers.q.legend",
		},
		"score missing probs": {
			prefix + `{"type":"score","score":1,"confidence":1,"legend":{"0":"x"}}` + suffix,
			"answers.q.probabilities",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			is := is.New(t)
			client := newClient(t, func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, tc.body) })
			_, err := client.SystemOne(t.Context(), basicRequest)
			var verr *jev.ResponseValidationError
			is.True(errors.As(err, &verr))
			is.Equal(verr.Path, tc.path)
			is.Equal(verr.StatusCode, http.StatusOK)
			is.Equal(verr.RequestID, "req-123")
			is.True(strings.Contains(verr.Error(), "invalid response data"))
		})
	}
}

func TestAPIErrorSentinels(t *testing.T) {
	t.Parallel()
	cases := []struct {
		status   int
		sentinel error
	}{
		{http.StatusBadRequest, jev.ErrBadRequest},
		{http.StatusUnauthorized, jev.ErrAuthentication},
		{http.StatusForbidden, jev.ErrPermissionDenied},
		{http.StatusNotFound, jev.ErrNotFound},
		{http.StatusUnprocessableEntity, jev.ErrUnprocessableEntity},
		{http.StatusTooManyRequests, jev.ErrRateLimit},
		{http.StatusInternalServerError, jev.ErrInternalServer},
		{http.StatusServiceUnavailable, jev.ErrInternalServer},
		{529, jev.ErrOverloaded},
		{529, jev.ErrInternalServer},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.status)+"/"+tc.sentinel.Error(), func(t *testing.T) {
			t.Parallel()
			is := is.New(t)
			client := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, tc.status, `{"error":"nope"}`)
			}, jev.WithRetryPolicy(jev.RetryPolicy{}))
			_, err := client.SystemOne(t.Context(), basicRequest)
			is.True(errors.Is(err, tc.sentinel))
			var apiErr *jev.APIError
			is.True(errors.As(err, &apiErr))
			is.Equal(apiErr.StatusCode, tc.status)
			is.Equal(apiErr.RequestID, "req-123")
			is.Equal(apiErr.Message, "nope")
			is.True(!errors.Is(err, jev.ErrConnection))
			is.Equal(errors.Is(err, jev.ErrRateLimit), tc.status == http.StatusTooManyRequests)
		})
	}
	t.Run("status without sentinel", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)
		client := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusTeapot, "")
		}, jev.WithRetryPolicy(jev.RetryPolicy{}))
		_, err := client.SystemOne(t.Context(), basicRequest)
		var apiErr *jev.APIError
		is.True(errors.As(err, &apiErr))
		is.Equal(apiErr.StatusCode, http.StatusTeapot)
		is.Equal(apiErr.Message, "status code (no body)")
		for _, sentinel := range []error{jev.ErrBadRequest, jev.ErrInternalServer, jev.ErrRateLimit} {
			is.True(!errors.Is(err, sentinel))
		}
	})
}

func TestAPIErrorMessages(t *testing.T) {
	t.Parallel()
	long := `{"code":"` + strings.Repeat("x", 300) + `"}`
	cases := map[string]struct{ body, want string }{
		"plain text":        {`plain failure`, "plain failure"},
		"error string":      {`{"error":"bad key"}`, "bad key"},
		"error object":      {`{"error":{"message":"nested"}}`, "nested"},
		"message":           {`{"message":"msg"}`, "msg"},
		"detail string":     {`{"detail":"det"}`, "det"},
		"detail object":     {`{"detail":{"message":"dm"}}`, "dm"},
		"unrecognized json": {`{"code":7}`, `{"code":7}`},
		"long json":         {long, long[:200] + "…"},
		"validation list": {
			`{"detail":[{"loc":["body","questions","tone"],"msg":"field required"},{"msg":"other"}]}`,
			"questions.tone: field required; other",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			is := is.New(t)
			client := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, http.StatusUnprocessableEntity, tc.body)
			}, jev.WithRetryPolicy(jev.RetryPolicy{}))
			_, err := client.SystemOne(t.Context(), basicRequest)
			var apiErr *jev.APIError
			is.True(errors.As(err, &apiErr))
			is.Equal(apiErr.Message, tc.want)
			is.True(strings.HasPrefix(err.Error(), "jev: POST http"))
			is.True(strings.HasSuffix(err.Error(), "(request_id=req-123)"))
		})
	}
}

// recordingTracer records SystemOne start and end data.
type recordingTracer struct {
	starts []jev.SystemOneStartData
	ends   []jev.SystemOneEndData
}

type ctxKey struct{}

func (r *recordingTracer) TraceSystemOneStart(ctx context.Context, data jev.SystemOneStartData) context.Context {
	r.starts = append(r.starts, data)
	return context.WithValue(ctx, ctxKey{}, "traced")
}

func (r *recordingTracer) TraceSystemOneEnd(_ context.Context, data jev.SystemOneEndData) {
	r.ends = append(r.ends, data)
}

func TestRetryAfterHeaders(t *testing.T) {
	t.Parallel()
	t.Run("retry-after seconds then success", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)
		var calls atomic.Int32
		var retryCounts []string
		tracer := &recordingTracer{}
		client := newClient(t, func(w http.ResponseWriter, r *http.Request) {
			retryCounts = append(retryCounts, r.Header.Get("X-TypeSafe-Retry-Count"))
			if calls.Add(1) < 3 {
				w.Header().Set("Retry-After", "0")
				writeJSON(w, http.StatusTooManyRequests, `{"error":"slow down"}`)
				return
			}
			writeJSON(w, http.StatusOK, okBody)
		}, jev.WithTracer(tracer))
		resp, err := client.SystemOne(t.Context(), basicRequest)
		is.NoErr(err)
		is.Equal(resp.Model, "jev-1.13")
		is.Equal(calls.Load(), int32(3))
		is.Equal(retryCounts, []string{"", "1", "2"})
		is.Equal(len(tracer.ends), 1)
		is.Equal(tracer.ends[0].Attempts, 3)
		is.NoErr(tracer.ends[0].Err)
	})
	t.Run("retry-after-ms is honored", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)
		var calls atomic.Int32
		client := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
			if calls.Add(1) == 1 {
				w.Header().Set("retry-after-ms", "60")
				writeJSON(w, http.StatusServiceUnavailable, "")
				return
			}
			writeJSON(w, http.StatusOK, okBody)
		})
		started := time.Now()
		_, err := client.SystemOne(t.Context(), basicRequest)
		is.NoErr(err)
		is.True(time.Since(started) >= 60*time.Millisecond) // waited the requested delay
	})
	t.Run("retry-after above cap falls back to backoff", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)
		var calls atomic.Int32
		policy := fastRetry()
		policy.MaxRetryAfter = 10 * time.Millisecond
		client := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
			if calls.Add(1) == 1 {
				w.Header().Set("Retry-After", "30")
				writeJSON(w, http.StatusTooManyRequests, "")
				return
			}
			writeJSON(w, http.StatusOK, okBody)
		}, jev.WithRetryPolicy(policy))
		started := time.Now()
		_, err := client.SystemOne(t.Context(), basicRequest)
		is.NoErr(err)
		is.True(time.Since(started) < time.Second) // a 30s Retry-After was not honored
	})
	t.Run("APIError exposes RetryAfter", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)
		client := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "2.5")
			writeJSON(w, http.StatusTooManyRequests, "")
		}, jev.WithRetryPolicy(jev.RetryPolicy{}))
		_, err := client.SystemOne(t.Context(), basicRequest)
		var apiErr *jev.APIError
		is.True(errors.As(err, &apiErr))
		after, ok := apiErr.RetryAfter()
		is.True(ok)
		is.Equal(after, 2500*time.Millisecond)
	})
}

func TestRetryStatusRules(t *testing.T) {
	t.Parallel()
	t.Run("exhausted retries return the last error", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)
		var calls atomic.Int32
		tracer := &recordingTracer{}
		client := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			writeJSON(w, http.StatusInternalServerError, `{"error":"boom"}`)
		}, jev.WithTracer(tracer))
		_, err := client.SystemOne(t.Context(), basicRequest)
		is.True(errors.Is(err, jev.ErrInternalServer))
		is.Equal(calls.Load(), int32(3)) // initial attempt plus two retries
		is.Equal(tracer.ends[0].Attempts, 3)
		is.True(tracer.ends[0].Response == nil)
		is.True(tracer.ends[0].Err != nil)
	})
	t.Run("non-retryable status is not retried", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)
		var calls atomic.Int32
		client := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			writeJSON(w, http.StatusUnprocessableEntity, "")
		})
		_, err := client.SystemOne(t.Context(), basicRequest)
		is.True(errors.Is(err, jev.ErrUnprocessableEntity))
		is.Equal(calls.Load(), int32(1))
	})
	t.Run("ShouldRetry extends the rules", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)
		var calls atomic.Int32
		policy := fastRetry()
		policy.ShouldRetry = func(err error) bool { return errors.Is(err, jev.ErrUnprocessableEntity) }
		client := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
			if calls.Add(1) == 1 {
				writeJSON(w, http.StatusUnprocessableEntity, "")
				return
			}
			writeJSON(w, http.StatusOK, okBody)
		}, jev.WithRetryPolicy(policy))
		_, err := client.SystemOne(t.Context(), basicRequest)
		is.NoErr(err)
		is.Equal(calls.Load(), int32(2))
	})
	t.Run("per-call policy override does not leak", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)
		var calls atomic.Int32
		client := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			writeJSON(w, http.StatusInternalServerError, "")
		})
		_, _ = client.SystemOne(t.Context(), basicRequest, jev.WithRetryPolicy(jev.RetryPolicy{}))
		is.Equal(calls.Load(), int32(1)) // zero policy: single attempt
		_, _ = client.SystemOne(t.Context(), basicRequest)
		is.Equal(calls.Load(), int32(4)) // client policy restored: three attempts
	})
	t.Run("total timeout budget", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)
		var calls atomic.Int32
		policy := fastRetry()
		policy.TotalTimeout = 50 * time.Millisecond
		client := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			w.Header().Set("Retry-After", "1")
			writeJSON(w, http.StatusServiceUnavailable, "")
		}, jev.WithRetryPolicy(policy))
		started := time.Now()
		_, err := client.SystemOne(t.Context(), basicRequest)
		is.True(errors.Is(err, jev.ErrInternalServer))
		is.Equal(calls.Load(), int32(1))                    // the 1s retry would exceed the budget
		is.True(time.Since(started) < 500*time.Millisecond) // and was skipped
	})
}

func TestRetryBackoffTiming(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	policy := jev.DefaultRetryPolicy()
	policy.InitialBackoff = 20 * time.Millisecond
	policy.MaxBackoff = 25 * time.Millisecond
	policy.Jitter = 0
	var calls atomic.Int32
	client := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) < 3 {
			writeJSON(w, http.StatusInternalServerError, "")
			return
		}
		writeJSON(w, http.StatusOK, okBody)
	}, jev.WithRetryPolicy(policy))
	started := time.Now()
	_, err := client.SystemOne(t.Context(), basicRequest)
	is.NoErr(err)
	is.True(time.Since(started) >= 45*time.Millisecond) // 20ms, then min(40ms, 25ms)
}

func TestConnectionErrors(t *testing.T) {
	t.Parallel()
	closeConnection := func(w http.ResponseWriter) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}
	t.Run("connection error is retried", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)
		var calls atomic.Int32
		client := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
			if calls.Add(1) == 1 {
				closeConnection(w)
				return
			}
			writeJSON(w, http.StatusOK, okBody)
		})
		_, err := client.SystemOne(t.Context(), basicRequest)
		is.NoErr(err)
		is.Equal(calls.Load(), int32(2))
	})
	t.Run("connection error without retries", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)
		policy := fastRetry()
		policy.ConnectionErrors = false
		client := newClient(t, func(w http.ResponseWriter, _ *http.Request) { closeConnection(w) },
			jev.WithRetryPolicy(policy))
		_, err := client.SystemOne(t.Context(), basicRequest)
		var connErr *jev.ConnectionError
		is.True(errors.As(err, &connErr))
		is.Equal(connErr.Timeout, time.Duration(0))
		is.True(errors.Is(err, jev.ErrConnection))
		is.True(!errors.Is(err, jev.ErrTimeout))
	})
	t.Run("timeout is classified and retried", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)
		var calls atomic.Int32
		policy := fastRetry()
		policy.MaxRetries = 1
		client := newClient(t, func(_ http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			waitForClient(r, 2*time.Second)
		}, jev.WithRetryPolicy(policy), jev.WithTimeout(30*time.Millisecond))
		_, err := client.SystemOne(t.Context(), basicRequest)
		var connErr *jev.ConnectionError
		is.True(errors.As(err, &connErr))
		is.Equal(connErr.Timeout, 30*time.Millisecond)
		is.True(errors.Is(err, jev.ErrTimeout))
		is.True(errors.Is(err, jev.ErrConnection))
		is.Equal(calls.Load(), int32(2))

		policy.Timeouts = false
		calls.Store(0)
		_, err = client.SystemOne(t.Context(), basicRequest, jev.WithRetryPolicy(policy))
		is.True(errors.Is(err, jev.ErrTimeout))
		is.Equal(calls.Load(), int32(1)) // timeouts not retried
	})
}

func TestCallerCancellation(t *testing.T) {
	t.Parallel()
	t.Run("during backoff", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)
		client := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "5")
			writeJSON(w, http.StatusTooManyRequests, "")
		})
		ctx, cancel := context.WithCancel(t.Context())
		go func() {
			time.Sleep(20 * time.Millisecond)
			cancel()
		}()
		started := time.Now()
		_, err := client.SystemOne(ctx, basicRequest)
		is.True(errors.Is(err, context.Canceled))
		is.True(time.Since(started) < time.Second) // cancellation interrupted the backoff
		is.True(!errors.Is(err, jev.ErrConnection))
		is.True(!errors.Is(err, jev.ErrRateLimit))
	})
	t.Run("deadline during request", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)
		client := newClient(t, func(_ http.ResponseWriter, r *http.Request) { waitForClient(r, 2*time.Second) })
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
		defer cancel()
		_, err := client.SystemOne(ctx, basicRequest)
		is.True(errors.Is(err, context.DeadlineExceeded))
		is.True(!errors.Is(err, jev.ErrTimeout)) // the caller's deadline is not an SDK timeout
	})
	t.Run("already canceled context sends nothing", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)
		var calls atomic.Int32
		client := newClient(t, func(_ http.ResponseWriter, _ *http.Request) { calls.Add(1) })
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := client.SystemOne(ctx, basicRequest)
		is.True(errors.Is(err, context.Canceled))
		is.Equal(calls.Load(), int32(0))
	})
}

func TestListModels(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	var got *http.Request
	client := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(context.Background()) //nolint:usetesting // detached copy for inspection.
		writeJSON(w, http.StatusOK, `{"models":[{"name":"jev-latest","description":"Latest",`+
			`"release_date":"2026-09-10T18:38:01.391457+00:00"}]}`)
	})
	resp, err := client.ListModels(t.Context())
	is.NoErr(err)
	is.Equal(got.Method, http.MethodGet)
	is.Equal(got.URL.Path, "/v1/models")
	is.Equal(got.Header.Get("Content-Type"), "") // no body, no content type
	is.Equal(len(resp.Models), 1)
	is.Equal(resp.Models[0].Name, "jev-latest")
	is.Equal(resp.Models[0].ReleaseDate.Year(), 2026)
	is.Equal(resp.RequestID, "req-123")

	client = newClient(t, func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, `{"data":[]}`) })
	_, err = client.ListModels(t.Context())
	var verr *jev.ResponseValidationError
	is.True(errors.As(err, &verr))
	is.Equal(verr.Path, "models")
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestTracerContextPropagation(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	tracer := &recordingTracer{}
	var seen atomic.Value
	server := httptest.NewServer(http.HandlerFunc(okHandler))
	t.Cleanup(server.Close)
	transport := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		value, _ := r.Context().Value(ctxKey{}).(string)
		seen.Store(value)
		return http.DefaultTransport.RoundTrip(r)
	})
	client, err := jev.NewClient(jev.WithAPIKey(testKey), jev.WithBaseURL(server.URL),
		jev.WithHTTPClient(&http.Client{Transport: transport}), jev.WithTracer(jev.MultiTracer(nil, tracer)))
	is.NoErr(err)

	req := basicRequest
	req.Model = "jev-preview"
	resp, err := client.SystemOne(t.Context(), req)
	is.NoErr(err)
	is.Equal(seen.Load(), "traced") // the tracer's context reached the transport
	is.Equal(len(tracer.starts), 1)
	is.Equal(tracer.starts[0].Model, "jev-preview")
	is.Equal(tracer.starts[0].Request.State, req.State)
	is.Equal(len(tracer.ends), 1)
	is.True(tracer.ends[0].Response == resp)
	is.Equal(tracer.ends[0].Attempts, 1)

	_, err = client.SystemOne(t.Context(), req, jev.WithTracer(nil))
	is.NoErr(err)
	is.Equal(len(tracer.starts), 1) // a nil per-call tracer disables tracing for that call
}

func TestLoggingRedaction(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	client := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Set-Cookie", "session=secret")
		writeJSON(w, http.StatusOK, okBody)
	}, jev.WithLogger(logger), jev.WithHeader("X-Access-Token", "tok"))
	_, err := client.SystemOne(t.Context(), basicRequest)
	is.NoErr(err)
	out := buf.String()
	for _, want := range []string{
		"jev: request", "jev: response", "status=200", "request_id=req-123",
		"Bearer ***cdef", "X-Access-Token=[***]", "Set-Cookie=[***]", "payouts",
	} {
		is.True(strings.Contains(out, want)) // expected log content
	}
	is.True(!strings.Contains(out, testKey))
	is.True(!strings.Contains(out, "session=secret"))
	is.True(!strings.Contains(out, "[tok]"))
}

//nolint:paralleltest // uses t.Setenv and the global default logger.
func TestLogLevelFromEnvironment(t *testing.T) {
	is := is.New(t)
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	t.Setenv(jev.EnvLogLevel, "info")
	client := newClient(t, okHandler)
	_, err := client.SystemOne(t.Context(), basicRequest)
	is.NoErr(err)
	is.True(strings.Contains(buf.String(), "jev: response"))  // info summaries are logged
	is.True(!strings.Contains(buf.String(), "jev: request ")) // debug bodies are not

	buf.Reset()
	t.Setenv(jev.EnvLogLevel, "off")
	client = newClient(t, okHandler)
	_, err = client.SystemOne(t.Context(), basicRequest)
	is.NoErr(err)
	is.Equal(buf.Len(), 0)
}

// TestLive exercises the real API when TYPESAFE_API_KEY is set.
//
//nolint:paralleltest // reads the environment.
func TestLive(t *testing.T) {
	is := is.New(t)
	client, err := jev.NewClient()
	if err != nil {
		t.Skipf("live test skipped: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()

	models, err := client.ListModels(ctx)
	is.NoErr(err)
	is.True(len(models.Models) > 0)

	resp, err := client.SystemOne(ctx, jev.Request{
		State: "Export to PDF fails with a spinner that never finishes. CSV export still works.",
		Questions: jev.Questions{
			"urgent": jev.Noul{Instructions: "Does this convey urgency?"},
			"department": jev.Choice{
				Instructions: "Which team should handle this?",
				Criteria:     map[string]any{"billing": nil, "technical": nil, "sales": nil},
			},
			"severity": jev.Score{Instructions: "How severe is the reported issue?", Criteria: []any{
				map[string]any{"what": "Cosmetic; no impact"},
				map[string]any{"what": "Broken feature, workaround exists"},
				map[string]any{"what": "Blocking; no workaround"},
			}},
		},
	})
	is.NoErr(err)
	t.Logf("model=%s request_id=%s usage=%+v", resp.Model, resp.RequestID, resp.Usage)
	for name, answer := range resp.Answers {
		t.Logf("%s: %#v", name, answer)
	}
	_, ok := resp.Choice("department")
	is.True(ok)
	severity, ok := resp.Score("severity")
	is.True(ok)
	is.Equal(len(severity.Legend), 3)
}
