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
	"runtime"
	"strings"
	"sync"
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

func fastRetry() jev.RetryPolicy {
	p := jev.DefaultRetryPolicy()
	p.InitialBackoff = time.Millisecond
	p.MaxBackoff = 2 * time.Millisecond
	return p
}

func newClient(t *testing.T, handler http.HandlerFunc, opts ...jev.Option) *jev.Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	base := []jev.Option{jev.WithAPIKey(testKey), jev.WithBaseURL(server.URL), jev.WithRetryPolicy(fastRetry())}
	client, err := jev.NewClient(append(base, opts...)...)
	is.New(t).NoErr(err)
	return client
}

// transportClient skips the network so wire-level probes stay fast.
func transportClient(t *testing.T, rt roundTripperFunc, opts ...jev.Option) *jev.Client {
	t.Helper()
	base := []jev.Option{
		jev.WithAPIKey(testKey), jev.WithBaseURL("https://unit.invalid"),
		jev.WithRetryPolicy(jev.RetryPolicy{}), jev.WithHTTPClient(&http.Client{Transport: rt}),
	}
	client, err := jev.NewClient(append(base, opts...)...)
	is.New(t).NoErr(err)
	return client
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func reply(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"X-Typesafe-Request-Id": {"req-123"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Typesafe-Request-Id", "req-123")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func okHandler(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, okBody) }

// waitForClient drains the body so the server notices a disconnect, then
// blocks until the client goes away or the fallback elapses.
func waitForClient(r *http.Request, fallback time.Duration) {
	_, _ = io.Copy(io.Discard, r.Body)
	select {
	case <-r.Context().Done():
	case <-time.After(fallback):
	}
}

//nolint:paralleltest // uses t.Setenv.
func TestNewClientConfiguration(t *testing.T) {
	t.Run("missing api key", func(t *testing.T) {
		is := is.New(t)
		t.Setenv(jev.EnvAPIKey, "  ")
		_, err := jev.NewClient()
		is.True(errors.Is(err, jev.ErrInvalidConfig))
	})
	t.Run("environment fallback and precedence", func(t *testing.T) {
		is := is.New(t)
		t.Setenv(jev.EnvAPIKey, " env-key ")
		t.Setenv(jev.EnvBaseURL, "https://proxy.example/v2/ ")
		t.Setenv(jev.EnvDefaultModel, "jev-preview")
		client, err := jev.NewClient()
		is.NoErr(err)
		is.Equal(client.BaseURL(), "https://proxy.example/v2")
		is.Equal(client.DefaultModel(), "jev-preview")

		client, err = jev.NewClient(jev.WithBaseURL("http://localhost:1/"), jev.WithDefaultModel("jev-latest"))
		is.NoErr(err)
		is.Equal(client.BaseURL(), "http://localhost:1")
		is.Equal(client.DefaultModel(), "jev-latest")

		client, err = jev.NewClient(jev.WithAPIKey(""), jev.WithDefaultModel(""))
		is.NoErr(err)
		is.Equal(client.DefaultModel(), "jev-preview") // empty means unset
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
		"base url credentials":    jev.WithBaseURL("https://user:secret-password@unit.invalid"),
		"base url query":          jev.WithBaseURL("https://unit.invalid/proxy?api_key=secret-query"),
		"base url fragment":       jev.WithBaseURL("https://unit.invalid/#frag"),
		"zero timeout":            jev.WithTimeout(0),
		"negative retries":        jev.WithRetryPolicy(jev.RetryPolicy{MaxRetries: -1}),
		"negative max retries":    jev.WithMaxRetries(-1),
		"jitter out of range":     jev.WithRetryPolicy(jev.RetryPolicy{Jitter: 1.5}),
		"bad status":              jev.WithRetryPolicy(jev.RetryPolicy{Statuses: []int{42}}),
	}
	for name, opt := range invalid {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			is := is.New(t)
			_, err := jev.NewClient(jev.WithAPIKey(testKey), opt)
			is.True(errors.Is(err, jev.ErrInvalidConfig))
			is.True(!strings.Contains(err.Error(), "secret")) // never echoed
		})
	}
}

func TestZeroClientIsRejected(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	var client jev.Client
	req := basicRequest
	req.Model = "jev-latest"
	_, err := client.SystemOne(t.Context(), req)
	is.True(errors.Is(err, jev.ErrInvalidConfig))
	_, err = client.ListModels(t.Context())
	is.True(errors.Is(err, jev.ErrInvalidConfig))
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
		writeJSON(w, http.StatusOK, `{"model":"m","answers":{`+
			`"urgent":{"type":"noul","noul":0.5},`+
			`"department":{"type":"choice","choice":"billing","confidence":1,"probabilities":{"billing":1,"other":0}},`+
			`"severity":{"type":"score","score":1,"confidence":1,`+
			`"legend":{"0":"Low","1":{"what":"High"}},"probabilities":{"0":0,"1":1}},`+
			`"raw":{"type":"noul","noul":0.1}},"usage":{"input_tokens":1,"output_tokens":1}}`)
	}, jev.WithHeader("X-Custom", "yes"), jev.WithHeader("Authorization", "Bearer stolen"),
		jev.WithHeader("X-Tenant", "client"), jev.WithHeader("X-Typesafe-Retry-Count", "99"))

	req := jev.Request{
		State: map[string]any{"document": "I was charged twice."},
		Questions: jev.Questions{
			"urgent": jev.Noul{Instructions: "Urgent?", Criteria: &jev.NoulCriteria{True: "Time-sensitive"}},
			"department": jev.Choice{
				Instructions: "Which team?",
				Criteria:     map[string]any{"billing": nil, "other": "Anything else"},
			},
			"severity": jev.Score{
				Instructions: []any{"How severe?"},
				Criteria:     []any{"Low", map[string]any{"what": "High"}},
			},
			"raw": jev.RawQuestion{"type": "noul", "instructions": "Raw?", "weight": 2},
		},
		Extra: map[string]any{"beam_width": 4, "model": "jev-preview"},
	}
	_, err := client.SystemOne(t.Context(), req, jev.WithHeader("X-Call", "1"), jev.WithHeader("x-tenant", "per-call"))
	is.NoErr(err)

	is.Equal(got.method, http.MethodPost)
	is.Equal(got.path, "/v1/systemone")
	is.Equal(got.header.Get("Authorization"), "Bearer "+testKey)
	is.Equal(got.header.Get("Accept"), "application/json")
	is.Equal(got.header.Get("Content-Type"), "application/json")
	is.Equal(got.header.Get("User-Agent"), "jevgo/"+jev.Version)
	is.Equal(got.header.Get("X-Typesafe-Sdk"), "jevgo/"+jev.Version)
	is.True(strings.HasPrefix(got.header.Get("X-Typesafe-Runtime"), "go/"))
	is.Equal(got.header.Get("X-Custom"), "yes")
	is.Equal(got.header.Get("X-Call"), "1")
	is.Equal(got.header.Values("X-Tenant"), []string{"per-call"}) // per-call replaces, case-insensitively
	_, hasRetryCount := got.header["X-Typesafe-Retry-Count"]
	is.True(!hasRetryCount)

	want := map[string]any{
		"state":      map[string]any{"document": "I was charged twice."},
		"model":      "jev-preview",
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

	req := jev.Request{State: "x", Questions: jev.Questions{"urgent": jev.Noul{}}}
	_, err := client.SystemOne(t.Context(), req)
	is.NoErr(err)
	is.Equal(body["model"], "jev-preview")
	question := body["questions"].(map[string]any)["urgent"].(map[string]any)
	_, hasInstructions := question["instructions"]
	is.True(!hasInstructions) // nil instructions are omitted

	req.Model = "jev-1.13"
	_, err = client.SystemOne(t.Context(), req)
	is.NoErr(err)
	is.Equal(body["model"], "jev-1.13")
}

func TestQuestionShapes(t *testing.T) {
	t.Parallel()
	valid := map[string]jev.Question{
		"score string slice":   jev.Score{Criteria: []string{"low", "high"}},
		"score one level":      jev.Score{Criteria: []string{"only"}},
		"score pointer":        &jev.Score{Criteria: []any{"a", "b"}},
		"choice string map":    jev.Choice{Criteria: map[string]string{"a": "A"}},
		"choice empty":         jev.Choice{},
		"raw score strings":    jev.RawQuestion{"type": "score", "criteria": []string{"low", "high"}},
		"raw score array":      jev.RawQuestion{"type": "score", "criteria": [2]string{"low", "high"}},
		"raw score raw json":   jev.RawQuestion{"type": "score", "criteria": json.RawMessage(`["low","high"]`)},
		"raw future kind":      jev.RawQuestion{"type": "future", "anything": nil},
		"noul nil instruction": jev.Noul{},
	}
	for name, question := range valid {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			is := is.New(t)
			var sent bool
			client := transportClient(t, func(r *http.Request) (*http.Response, error) {
				sent = true
				var body struct {
					Questions map[string]map[string]any `json:"questions"`
				}
				is.NoErr(json.NewDecoder(r.Body).Decode(&body))
				is.True(body.Questions["q"]["type"] != "")
				answers := map[string]string{
					"noul":   `{"type":"noul","noul":0.5}`,
					"choice": `{"type":"choice","choice":"a","confidence":1,"probabilities":{"a":1}}`,
					"score":  `{"type":"score","score":0,"confidence":1,"legend":{"0":"x"},"probabilities":{"0":1}}`,
					"future": `{"type":"future"}`,
				}
				return reply(
					http.StatusOK,
					`{"model":"m","answers":{"q":`+answers[body.Questions["q"]["type"].(string)]+`},`+
						`"usage":{"input_tokens":1,"output_tokens":1}}`,
				), nil
			})
			_, err := client.SystemOne(t.Context(), jev.Request{State: "x", Questions: jev.Questions{"q": question}})
			is.NoErr(err)
			is.True(sent)
		})
	}
}

func TestRequestValidation(t *testing.T) {
	t.Parallel()
	var requests atomic.Int32
	client := transportClient(t, func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return reply(http.StatusOK, okBody), nil
	})
	cases := map[string]jev.Questions{
		"no questions":         nil,
		"nil question":         {"q": nil},
		"typed nil noul":       {"q": (*jev.Noul)(nil)},
		"typed nil choice":     {"q": (*jev.Choice)(nil)},
		"typed nil score":      {"q": (*jev.Score)(nil)},
		"typed nil raw":        {"q": (*jev.RawQuestion)(nil)},
		"nil raw map":          {"q": jev.RawQuestion(nil)},
		"score nil criteria":   {"q": jev.Score{}},
		"score empty":          {"q": jev.Score{Criteria: []string{}}},
		"score not an array":   {"q": jev.Score{Criteria: map[string]any{"a": "b"}}},
		"choice not a map":     {"q": jev.Choice{Criteria: []string{"a"}}},
		"raw without type":     {"q": jev.RawQuestion{"instructions": "?"}},
		"raw empty type":       {"q": jev.RawQuestion{"type": ""}},
		"raw choice no map":    {"q": jev.RawQuestion{"type": "choice"}},
		"raw score empty":      {"q": jev.RawQuestion{"type": "score", "criteria": []any{}}},
		"unencodable criteria": {"q": jev.Choice{Criteria: map[string]any{"a": make(chan int)}}},
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
		_, err := client.SystemOne(t.Context(), jev.Request{State: make(chan int), Questions: basicRequest.Questions})
		is.True(errors.Is(err, jev.ErrInvalidRequest))
	})
	t.Cleanup(func() { is.New(t).Equal(requests.Load(), int32(0)) })
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
	    "future": {"type": "ranking", "order": ["a", "b"]},
	    "unrequested": {"type": "noul", "noul": 0.1}
	  },
	  "usage": {"input_tokens": 312, "output_tokens": 48},
	  "new_field": true
	}`
	client := newClient(t, func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, body) })
	resp, err := client.SystemOne(t.Context(), jev.Request{State: "x", Questions: jev.Questions{
		"urgent":      jev.Noul{},
		"department":  jev.Choice{Criteria: map[string]any{"billing": nil, "technical": nil, "sales": nil}},
		"frustration": jev.Score{Criteria: []string{"Calm", "Frustrated", "Very angry"}},
		"future":      jev.RawQuestion{"type": "ranking"},
	}})
	is.NoErr(err)

	is.Equal(resp.Model, "jev-1.13")
	is.Equal(resp.Usage, jev.Usage{InputTokens: 312, OutputTokens: 48})
	is.Equal(resp.RequestID, "req-123")
	is.Equal(resp.StatusCode, http.StatusOK)
	is.Equal(resp.Attempts, 1)
	is.Equal(string(resp.RawBody), body)
	is.Equal(len(resp.Answers), 5)

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
	is.Equal(score.Level(), 2)
	is.Equal(score.Legend, map[int]any{0: "Calm", 1: map[string]any{"what": "Frustrated"}, 2: []any{"Very", "angry"}})

	unknown, ok := resp.Answers["future"].(jev.UnknownAnswer)
	is.True(ok)
	is.Equal(unknown.Type, "ranking")
	is.True(strings.Contains(string(unknown.Raw), `"order"`))

	_, ok = resp.Noul("department")
	is.True(!ok)
	_, ok = resp.Noul("missing")
	is.True(!ok)
	is.Equal(len(resp.Nouls()), 2)
	is.Equal(len(resp.Choices())+len(resp.Scores()), 2)

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

func TestPointerAnswersAndZeroResponse(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	resp := &jev.Response{
		Answers: map[string]jev.Answer{"q": &jev.NoulAnswer{Noul: 0.9}, "nil": (*jev.NoulAnswer)(nil)},
	}
	noul, ok := resp.Noul("q")
	is.True(ok)
	is.Equal(noul.Noul, 0.9)
	_, ok = resp.Noul("nil")
	is.True(!ok)
	is.Equal(len(resp.Nouls()), 1)

	var zero jev.Response
	_, ok = zero.Noul("q")
	is.True(!ok)
	is.Equal(len(zero.Scores()), 0)
	is.Equal((jev.ScoreAnswer{}).Level(), 0)
	is.Equal(jev.ScoreAnswer{Probabilities: map[int]float64{0: 0.5, 1: 0.5, 2: 0}}.Level(), 0) // lowest on a tie
}

func TestResponseValidationErrors(t *testing.T) {
	t.Parallel()
	const prefix = `{"model":"m","answers":{"q":`
	const suffix = `},"usage":{"input_tokens":1,"output_tokens":1}}`
	const usage = `"usage":{"input_tokens":1,"output_tokens":1}`
	cases := map[string]struct{ body, path string }{
		"not json":        {`<html>`, ""},
		"missing model":   {`{"answers":{},` + usage + `}`, "model"},
		"missing answers": {`{"model":"m",` + usage + `}`, "answers"},
		"missing usage":   {`{"model":"m","answers":{}}`, "usage"},
		"empty usage": {
			`{"model":"m","answers":{"q":{"type":"noul","noul":0}},"usage":{}}`,
			"usage.input_tokens",
		},
		"null usage counter": {
			`{"model":"m","answers":{"q":{"type":"noul","noul":0}},"usage":{"input_tokens":null,"output_tokens":1}}`,
			"usage.input_tokens",
		},
		"wrong top type": {`{"model":5,"answers":{},` + usage + `}`, "model"},
		"missing answer": {`{"model":"m","answers":{},` + usage + `}`, "answers.q"},
		"wrong answer kind": {
			prefix + `{"type":"choice","choice":"a","confidence":1,"probabilities":{"a":1}}` + suffix,
			"answers.q.type",
		},
		"answer without type": {prefix + `{"noul":0.5}` + suffix, "answers.q.type"},
		"noul missing value":  {prefix + `{"type":"noul"}` + suffix, "answers.q.noul"},
		"noul null":           {prefix + `{"type":"noul","noul":null}` + suffix, "answers.q.noul"},
		"noul wrong type":     {prefix + `{"type":"noul","noul":"high"}` + suffix, "answers.q.noul"},
		"noul out of range":   {prefix + `{"type":"noul","noul":2}` + suffix, "answers.q.noul"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			is := is.New(t)
			client := transportClient(
				t,
				func(*http.Request) (*http.Response, error) { return reply(http.StatusOK, tc.body), nil },
			)
			_, err := client.SystemOne(t.Context(), jev.Request{State: "x", Questions: jev.Questions{"q": jev.Noul{}}})
			var verr *jev.ResponseValidationError
			is.True(errors.As(err, &verr))
			is.Equal(verr.Path, tc.path)
			is.Equal(verr.StatusCode, http.StatusOK)
			is.Equal(verr.RequestID, "req-123")
			is.Equal(verr.Attempts, 1)
			is.True(strings.Contains(verr.Error(), "invalid response data"))
		})
	}
}

func TestChoiceAndScoreValidation(t *testing.T) {
	t.Parallel()
	const prefix = `{"model":"m","answers":{"q":`
	const suffix = `},"usage":{"input_tokens":1,"output_tokens":1}}`
	choice := `{"type":"choice","choice":"a","confidence":0.9,"probabilities":{"a":0.9,"b":0.1}}`
	score := `{"type":"score","score":1,"confidence":1,"legend":{"0":"x","1":"y"},"probabilities":{"0":0,"1":1}}`
	cases := map[string]struct {
		question   jev.Question
		body, path string
	}{
		"choice ok": {jev.Choice{Criteria: map[string]any{"a": nil, "b": nil}}, choice, ""},
		"choice missing conf": {
			jev.Choice{Criteria: map[string]any{"a": nil}},
			`{"type":"choice","choice":"a","probabilities":{"a":1}}`,
			"answers.q.confidence",
		},
		"choice conf range": {
			jev.Choice{Criteria: map[string]any{"a": nil}},
			`{"type":"choice","choice":"a","confidence":1.5,"probabilities":{"a":1}}`,
			"answers.q.confidence",
		},
		"choice null probability": {
			jev.Choice{Criteria: map[string]any{"a": nil}},
			`{"type":"choice","choice":"a","confidence":1,"probabilities":{"a":null}}`,
			"answers.q.probabilities.a",
		},
		"choice not in probs": {
			jev.Choice{Criteria: map[string]any{"a": nil}},
			`{"type":"choice","choice":"z","confidence":1,"probabilities":{"a":1}}`,
			"answers.q.choice",
		},
		"score ok": {jev.Score{Criteria: []string{"x", "y"}}, score, ""},
		"score bad level key": {
			jev.Score{Criteria: []string{"x"}},
			`{"type":"score","score":0,"confidence":1,"legend":{"low":"x"},"probabilities":{"0":1}}`,
			"answers.q.legend.low",
		},
		"score padded key": {
			jev.Score{Criteria: []string{"x", "y"}},
			`{"type":"score","score":0,"confidence":1,"legend":{"0":"x","00":"y"},"probabilities":{"0":0.3,"00":0.7}}`,
			"answers.q.probabilities.00",
		},
		"score negative key": {
			jev.Score{Criteria: []string{"x"}},
			`{"type":"score","score":0,"confidence":1,"legend":{"-1":"x"},"probabilities":{"-1":1}}`,
			"answers.q.probabilities.-1",
		},
		"score null legend": {
			jev.Score{Criteria: []string{"x", "y"}},
			`{"type":"score","score":0,"confidence":1,"legend":{"0":null,"1":"y"},"probabilities":{"0":1,"1":0}}`,
			"answers.q.legend.0",
		},
		"score legend mismatch": {
			jev.Score{Criteria: []string{"x", "y"}},
			`{"type":"score","score":0,"confidence":1,"legend":{"0":"x"},"probabilities":{"0":1,"1":0}}`,
			"answers.q.legend",
		},
		"score missing probs": {
			jev.Score{Criteria: []string{"x"}},
			`{"type":"score","score":0,"confidence":1,"legend":{"0":"x"}}`,
			"answers.q.probabilities",
		},
		"score out of range": {
			jev.Score{Criteria: []string{"x", "y"}},
			`{"type":"score","score":3,"confidence":1,"legend":{"0":"x","1":"y"},"probabilities":{"0":0,"1":1}}`,
			"answers.q.score",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			is := is.New(t)
			client := transportClient(t, func(*http.Request) (*http.Response, error) {
				return reply(http.StatusOK, prefix+tc.body+suffix), nil
			})
			_, err := client.SystemOne(t.Context(), jev.Request{State: "x", Questions: jev.Questions{"q": tc.question}})
			if tc.path == "" {
				is.NoErr(err)
				return
			}
			var verr *jev.ResponseValidationError
			is.True(errors.As(err, &verr))
			is.Equal(verr.Path, tc.path)
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
			client := transportClient(t, func(*http.Request) (*http.Response, error) {
				return reply(tc.status, `{"error":"nope"}`), nil
			})
			_, err := client.SystemOne(t.Context(), basicRequest)
			is.True(errors.Is(err, tc.sentinel))
			var apiErr *jev.APIError
			is.True(errors.As(err, &apiErr))
			is.Equal(apiErr.StatusCode, tc.status)
			is.Equal(apiErr.RequestID, "req-123")
			is.Equal(apiErr.Message, "nope")
			is.Equal(apiErr.Attempts, 1)
			is.True(!errors.Is(err, jev.ErrConnection))
			is.Equal(errors.Is(err, jev.ErrRateLimit), tc.status == http.StatusTooManyRequests)
		})
	}
	t.Run("status without sentinel", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)
		client := transportClient(
			t,
			func(*http.Request) (*http.Response, error) { return reply(http.StatusTeapot, ""), nil },
		)
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
	long := strings.Repeat("x", 300)
	cases := map[string]struct{ body, want string }{
		"plain text":        {`plain failure`, "plain failure"},
		"error string":      {`{"error":"bad key"}`, "bad key"},
		"error object":      {`{"error":{"message":"nested"}}`, "nested"},
		"message":           {`{"message":"msg"}`, "msg"},
		"detail string":     {`{"detail":"det"}`, "det"},
		"detail object":     {`{"detail":{"message":"dm"}}`, "dm"},
		"unrecognized json": {`{"code":7}`, `{"code":7}`},
		"long text":         {long, long[:200] + "…"},
		"long message":      {`{"error":"` + long + `"}`, long[:200] + "…"},
		"validation list": {
			`{"detail":[{"loc":["body","questions","tone"],"msg":"field required"},{"msg":"other"}]}`,
			"questions.tone: field required; other",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			is := is.New(t)
			client := transportClient(t, func(*http.Request) (*http.Response, error) {
				return reply(http.StatusUnprocessableEntity, tc.body), nil
			})
			_, err := client.SystemOne(t.Context(), basicRequest)
			var apiErr *jev.APIError
			is.True(errors.As(err, &apiErr))
			is.Equal(apiErr.Message, tc.want)
			is.Equal(string(apiErr.Body), tc.body) // the raw body is not truncated
			is.True(strings.HasPrefix(err.Error(), "jev: POST https://unit.invalid/v1/systemone: 422"))
			is.True(strings.HasSuffix(err.Error(), "(request_id=req-123)"))
		})
	}
}

func TestRetryAfterParsing(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	huge := time.Duration(1<<63 - 1)
	cases := []struct {
		header, value string
		want          time.Duration
		ok            bool
	}{
		{"Retry-After", "0", 0, true},
		{"Retry-After", "2.5", 2500 * time.Millisecond, true},
		{"Retry-After", "-1", 0, false},
		{"Retry-After", "garbage", 0, false},
		{"Retry-After", "NaN", 0, false},
		{"Retry-After", "Inf", 0, false},
		{"Retry-After", "1e20", huge, true},
		{"Retry-After", "", 0, false},
		{"Retry-After", "Wed, 21 Oct 2015 07:28:00 GMT", 0, true},
		{"Retry-After-Ms", "1500", 1500 * time.Millisecond, true},
		{"Retry-After-Ms", "-5", 0, false},
		{"Retry-After-Ms", "1e20", huge, true},
	}
	for _, tc := range cases {
		got, ok := (&jev.APIError{Header: http.Header{tc.header: {tc.value}}}).RetryAfter()
		is.Equal(ok, tc.ok)    // tc.header + "=" + tc.value
		is.Equal(got, tc.want) // tc.header + "=" + tc.value
		is.True(got >= 0)
	}
	future := time.Now().Add(time.Minute).UTC().Format(http.TimeFormat)
	got, ok := (&jev.APIError{Header: http.Header{"Retry-After": {future}}}).RetryAfter()
	is.True(ok)
	is.True(got > 50*time.Second && got <= time.Minute)
	got, ok = (&jev.APIError{Header: http.Header{"Retry-After-Ms": {"garbage"}, "Retry-After": {"2"}}}).RetryAfter()
	is.True(ok)
	is.Equal(got, 2*time.Second)
	got, ok = (&jev.APIError{Header: http.Header{"Retry-After-Ms": {"5"}, "Retry-After": {"2"}}}).RetryAfter()
	is.True(ok)
	is.Equal(got, 5*time.Millisecond)
}

func TestHugeRetryAfterFallsBackToBackoff(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	var calls atomic.Int32
	policy := fastRetry()
	policy.MaxRetries = 1
	client := transportClient(t, func(*http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			res := reply(http.StatusTooManyRequests, "")
			res.Header.Set("Retry-After", "1e20")
			return res, nil
		}
		return reply(http.StatusOK, okBody), nil
	}, jev.WithRetryPolicy(policy))
	started := time.Now()
	_, err := client.SystemOne(t.Context(), basicRequest)
	is.NoErr(err)
	is.True(time.Since(started) < time.Second)
}

// recordingTracer records SystemOne start and end data.
type recordingTracer struct {
	mu     sync.Mutex
	starts []jev.SystemOneStartData
	ends   []jev.SystemOneEndData
}

type ctxKey struct{}

func (r *recordingTracer) TraceSystemOneStart(ctx context.Context, data jev.SystemOneStartData) context.Context {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.starts = append(r.starts, data)
	return context.WithValue(ctx, ctxKey{}, "traced")
}

func (r *recordingTracer) TraceSystemOneEnd(_ context.Context, data jev.SystemOneEndData) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ends = append(r.ends, data)
}

func TestRetryAfterHeaders(t *testing.T) {
	t.Parallel()
	t.Run("retry-after seconds then success", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)
		var calls atomic.Int32
		var retryCounts, bodies []string
		tracer := &recordingTracer{}
		client := newClient(t, func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			bodies = append(bodies, string(body))
			retryCounts = append(retryCounts, r.Header.Get("X-Typesafe-Retry-Count"))
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
		is.Equal(resp.Attempts, 3)
		is.Equal(retryCounts, []string{"", "1", "2"})
		is.True(bodies[0] == bodies[1] && bodies[1] == bodies[2]) // body re-sent on every attempt
		is.Equal(tracer.ends[0].Attempts, 3)
		is.NoErr(tracer.ends[0].Err)
	})
	t.Run("retry-after-ms is honored", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)
		var calls atomic.Int32
		client := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
			if calls.Add(1) == 1 {
				w.Header().Set("Retry-After-Ms", "60")
				writeJSON(w, http.StatusServiceUnavailable, "")
				return
			}
			writeJSON(w, http.StatusOK, okBody)
		})
		started := time.Now()
		_, err := client.SystemOne(t.Context(), basicRequest)
		is.NoErr(err)
		is.True(time.Since(started) >= 60*time.Millisecond)
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
		is.True(time.Since(started) < time.Second)
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
		is.Equal(calls.Load(), int32(3))
		var apiErr *jev.APIError
		is.True(errors.As(err, &apiErr))
		is.Equal(apiErr.Attempts, 3)
		is.Equal(tracer.ends[0].Attempts, 3)
		is.True(tracer.ends[0].Response == nil)
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
	t.Run("ShouldRetry extends the rules but not to validation", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)
		var calls, predicate atomic.Int32
		policy := fastRetry()
		policy.ShouldRetry = func(err error) bool {
			predicate.Add(1)
			return errors.Is(err, jev.ErrUnprocessableEntity)
		}
		client := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
			if calls.Add(1) == 1 {
				writeJSON(w, http.StatusUnprocessableEntity, "")
				return
			}
			writeJSON(w, http.StatusOK, `{}`)
		}, jev.WithRetryPolicy(policy))
		_, err := client.SystemOne(t.Context(), basicRequest)
		var verr *jev.ResponseValidationError
		is.True(errors.As(err, &verr))
		is.Equal(calls.Load(), int32(2))
		is.Equal(predicate.Load(), int32(1)) // consulted for the 422, not for the malformed 200
	})
	t.Run("WithMaxRetries keeps the rest of the policy", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)
		var calls atomic.Int32
		client := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			writeJSON(w, http.StatusInternalServerError, "")
		}, jev.WithMaxRetries(4))
		_, _ = client.SystemOne(t.Context(), basicRequest)
		is.Equal(calls.Load(), int32(5))
		_, _ = client.SystemOne(t.Context(), basicRequest, jev.WithMaxRetries(0))
		is.Equal(calls.Load(), int32(6))
		_, _ = client.SystemOne(t.Context(), basicRequest)
		is.Equal(calls.Load(), int32(11)) // per-call override did not leak
	})
	t.Run("total timeout skips a retry that would exceed it", func(t *testing.T) {
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
		is.Equal(calls.Load(), int32(1))
		is.True(time.Since(started) < 500*time.Millisecond)
	})
	t.Run("total timeout bounds a running attempt", func(t *testing.T) {
		t.Parallel()
		for _, firstFails := range []bool{false, true} {
			is := is.New(t)
			policy := fastRetry()
			policy.TotalTimeout = 30 * time.Millisecond
			var calls atomic.Int32
			client := transportClient(t, func(r *http.Request) (*http.Response, error) {
				if firstFails && calls.Add(1) == 1 {
					return reply(http.StatusServiceUnavailable, ""), nil
				}
				select {
				case <-time.After(500 * time.Millisecond):
					return reply(http.StatusOK, okBody), nil
				case <-r.Context().Done():
					return nil, r.Context().Err()
				}
			}, jev.WithRetryPolicy(policy), jev.WithTimeout(time.Second))
			started := time.Now()
			_, err := client.SystemOne(t.Context(), basicRequest)
			var connErr *jev.ConnectionError
			is.True(errors.As(err, &connErr))
			is.Equal(connErr.Timeout, 30*time.Millisecond)
			is.True(errors.Is(err, jev.ErrTimeout))
			is.True(time.Since(started) < 400*time.Millisecond)
		}
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

func TestSharedOptionsAreSafeForConcurrentUse(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	client := transportClient(
		t,
		func(*http.Request) (*http.Response, error) { return reply(http.StatusOK, okBody), nil },
	)
	retry := jev.WithRetryPolicy(jev.DefaultRetryPolicy())
	header := jev.WithHeader("X-Tenant", "t")
	var wg sync.WaitGroup
	var failures atomic.Int32
	for range 16 {
		wg.Go(func() {
			for range 20 {
				_, err := client.SystemOne(
					context.Background(),
					basicRequest,
					retry,
					header,
					jev.WithMaxRetries(1),
				) //nolint:usetesting // shared across goroutines.
				if err != nil {
					failures.Add(1)
				}
			}
		})
	}
	wg.Wait()
	is.Equal(failures.Load(), int32(0))
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
		resp, err := client.SystemOne(t.Context(), basicRequest)
		is.NoErr(err)
		is.Equal(resp.Attempts, 2)
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
		is.Equal(connErr.Attempts, 1)
		is.True(errors.Is(err, jev.ErrConnection))
		is.True(!errors.Is(err, jev.ErrTimeout))
		is.True(strings.Contains(err.Error(), "connection error after"))
	})
	t.Run("interrupted body keeps response metadata", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)
		client := transportClient(t, func(*http.Request) (*http.Response, error) {
			res := reply(http.StatusServiceUnavailable, "")
			res.Body = io.NopCloser(brokenReader{})
			return res, nil
		})
		_, err := client.SystemOne(t.Context(), basicRequest)
		var connErr *jev.ConnectionError
		is.True(errors.As(err, &connErr))
		is.True(errors.Is(err, io.ErrUnexpectedEOF))
		is.Equal(connErr.StatusCode, http.StatusServiceUnavailable)
		is.Equal(connErr.RequestID, "req-123")
	})
	t.Run("oversized body is rejected", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)
		client := transportClient(t, func(*http.Request) (*http.Response, error) {
			res := reply(http.StatusOK, "")
			res.Body = io.NopCloser(io.LimitReader(zeroReader{}, 17<<20))
			return res, nil
		})
		_, err := client.SystemOne(t.Context(), basicRequest)
		is.True(errors.Is(err, jev.ErrConnection))
		is.True(strings.Contains(err.Error(), "exceeds"))
	})
	t.Run("attempt timeout is classified and retried", func(t *testing.T) {
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
		is.True(connErr.Elapsed >= 30*time.Millisecond)
		is.True(errors.Is(err, jev.ErrTimeout))
		is.True(errors.Is(err, jev.ErrConnection))
		is.Equal(calls.Load(), int32(2))
		is.True(strings.Contains(err.Error(), "limit 30ms"))

		policy.Timeouts = false
		calls.Store(0)
		_, err = client.SystemOne(t.Context(), basicRequest, jev.WithRetryPolicy(policy))
		is.True(errors.Is(err, jev.ErrTimeout))
		is.Equal(calls.Load(), int32(1))
	})
	t.Run("transport timeout is not blamed on the SDK limit", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)
		server := httptest.NewServer(
			http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { waitForClient(r, 2*time.Second) }),
		)
		t.Cleanup(server.Close)
		client, err := jev.NewClient(jev.WithAPIKey(testKey), jev.WithBaseURL(server.URL),
			jev.WithHTTPClient(&http.Client{Timeout: 25 * time.Millisecond}), jev.WithTimeout(2*time.Second),
			jev.WithRetryPolicy(jev.RetryPolicy{}))
		is.NoErr(err)
		_, err = client.SystemOne(t.Context(), basicRequest)
		var connErr *jev.ConnectionError
		is.True(errors.As(err, &connErr))
		is.Equal(connErr.Timeout, time.Duration(0))
		is.True(connErr.Elapsed < time.Second)
		is.True(errors.Is(err, jev.ErrTimeout)) // still a timeout
		is.True(!strings.Contains(err.Error(), "limit"))
	})
}

type brokenReader struct{}

func (brokenReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

func TestCallerCancellation(t *testing.T) {
	t.Parallel()
	for _, phase := range []string{"headers", "body", "backoff"} {
		t.Run(phase, func(t *testing.T) {
			t.Parallel()
			is := is.New(t)
			started := make(chan struct{}, 1)
			var calls atomic.Int32
			client := newClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				started <- struct{}{}
				switch phase {
				case "headers":
					<-r.Context().Done()
				case "body":
					w.WriteHeader(http.StatusOK)
					w.(http.Flusher).Flush()
					<-r.Context().Done()
				case "backoff":
					w.Header().Set("Retry-After", "60")
					w.WriteHeader(http.StatusTooManyRequests)
				}
			})
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() {
				_, err := client.SystemOne(ctx, basicRequest)
				done <- err
			}()
			<-started
			if phase == "backoff" {
				time.Sleep(10 * time.Millisecond)
			}
			cancel()
			select {
			case err := <-done:
				is.True(errors.Is(err, context.Canceled))
				is.True(!errors.Is(err, jev.ErrTimeout))
				is.True(!errors.Is(err, jev.ErrConnection))
			case <-time.After(2 * time.Second):
				is.Fail() // cancellation did not complete
			}
			is.Equal(calls.Load(), int32(1))
		})
	}
	t.Run("deadline during request", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)
		client := newClient(t, func(_ http.ResponseWriter, r *http.Request) { waitForClient(r, 2*time.Second) })
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
		defer cancel()
		_, err := client.SystemOne(ctx, basicRequest)
		is.True(errors.Is(err, context.DeadlineExceeded))
		is.True(!errors.Is(err, jev.ErrTimeout))
	})
	t.Run("already canceled context sends nothing", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)
		var calls atomic.Int32
		tracer := &recordingTracer{}
		client := newClient(t, func(_ http.ResponseWriter, _ *http.Request) { calls.Add(1) }, jev.WithTracer(tracer))
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := client.SystemOne(ctx, basicRequest)
		is.True(errors.Is(err, context.Canceled))
		is.Equal(calls.Load(), int32(0))
		is.Equal(tracer.ends[0].Attempts, 0)
	})
}

func TestNoGoroutineLeakUnderTimeouts(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	before := runtime.NumGoroutine()
	client := transportClient(t, func(r *http.Request) (*http.Response, error) {
		<-r.Context().Done()
		return nil, r.Context().Err()
	}, jev.WithTimeout(time.Millisecond), jev.WithRetryPolicy(fastRetry()))
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 5 {
				_, err := client.SystemOne(
					context.Background(),
					basicRequest,
				) //nolint:usetesting // shared across goroutines.
				if !errors.Is(err, jev.ErrTimeout) {
					t.Error(err)
				}
			}
		})
	}
	wg.Wait()
	time.Sleep(20 * time.Millisecond)
	is.True(runtime.NumGoroutine() <= before+3)
}

func TestListModels(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	var got *http.Request
	client := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(context.Background()) //nolint:usetesting // detached copy for inspection.
		writeJSON(
			w,
			http.StatusOK,
			`{"models":[{"name":"jev-latest","description":"Latest","release_date":"2026-09-15"},`+
				`{"name":"jev-preview","description":"","release_date":"2026-09-10T18:38:01.391457+00:00"}]}`,
		)
	})
	resp, err := client.ListModels(t.Context())
	is.NoErr(err)
	is.Equal(got.Method, http.MethodGet)
	is.Equal(got.URL.Path, "/v1/models")
	is.Equal(got.Header.Get("Content-Type"), "")
	is.Equal(len(resp.Models), 2)
	is.Equal(resp.Models[0], jev.Model{Name: "jev-latest", Description: "Latest", ReleaseDate: "2026-09-15"})
	is.Equal(resp.RequestID, "req-123")
	is.Equal(resp.Attempts, 1)

	invalid := map[string]string{
		"models":                 `{"data":[]}`,
		"models[0].name":         `{"models":[{}]}`,
		"models[1].name":         `{"models":[{"name":"a","description":"","release_date":""},null]}`,
		"models[0].release_date": `{"models":[{"name":"a","description":""}]}`,
	}
	for path, body := range invalid {
		client := transportClient(
			t,
			func(*http.Request) (*http.Response, error) { return reply(http.StatusOK, body), nil },
		)
		_, err := client.ListModels(t.Context())
		var verr *jev.ResponseValidationError
		is.True(errors.As(err, &verr))
		is.Equal(verr.Path, path)
	}
}

func TestTracer(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	tracer := &recordingTracer{}
	var seen atomic.Value
	var wire map[string]any
	transport := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		value, _ := r.Context().Value(ctxKey{}).(string)
		seen.Store(value)
		_ = json.NewDecoder(r.Body).Decode(&wire)
		return reply(http.StatusOK, okBody), nil
	})
	client := transportClient(t, transport, jev.WithTracer(jev.MultiTracer(nil, tracer)))

	req := basicRequest
	req.Model = "jev-preview"
	req.Extra = map[string]any{"model": "wire-model"}
	resp, err := client.SystemOne(t.Context(), req)
	is.NoErr(err)
	is.Equal(seen.Load(), "traced") // the tracer's context reached the transport
	is.Equal(len(tracer.starts), 1)
	is.Equal(tracer.starts[0].Model, "wire-model") // the effective model, after Extra
	is.Equal(wire["model"], "wire-model")
	is.True(bytes.Contains(tracer.starts[0].Body, []byte(`"wire-model"`)))
	is.Equal(tracer.starts[0].Request.State, req.State)
	is.Equal(len(tracer.ends), 1)
	is.True(tracer.ends[0].Response == resp)
	is.Equal(tracer.ends[0].Attempts, 1)

	_, err = client.SystemOne(t.Context(), jev.Request{State: "x"})
	is.True(errors.Is(err, jev.ErrInvalidRequest))
	is.Equal(len(tracer.starts), 1) // invalid requests are not traced

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
		is.True(strings.Contains(out, want)) // want
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
	is.True(strings.Contains(buf.String(), "jev: response"))
	is.True(!strings.Contains(buf.String(), "jev: request "))

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
				Criteria:     map[string]string{"billing": "Payments", "technical": "Bugs", "sales": "Pricing"},
			},
			"severity": jev.Score{Instructions: "How severe is the reported issue?", Criteria: []map[string]string{
				{"what": "Cosmetic; no impact"},
				{"what": "Broken feature, workaround exists"},
				{"what": "Blocking; no workaround"},
			}},
			"single": jev.Score{Instructions: "Is this a bug report?", Criteria: []string{"yes"}},
		},
	})
	is.NoErr(err)
	t.Logf("model=%s request_id=%s usage=%+v attempts=%d", resp.Model, resp.RequestID, resp.Usage, resp.Attempts)
	for name, answer := range resp.Answers {
		t.Logf("%s: %#v", name, answer)
	}
	_, ok := resp.Choice("department")
	is.True(ok)
	severity, ok := resp.Score("severity")
	is.True(ok)
	is.Equal(len(severity.Legend), 3)
	t.Logf("severity level %d: %v", severity.Level(), severity.Legend[severity.Level()])
}
