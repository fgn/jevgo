package jev_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"

	jev "github.com/fgn/jevgo"
)

func ExampleClient_SystemOne() {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"model":"jev-1.13","answers":{
			"department":{"type":"choice","choice":"billing","confidence":0.9,"probabilities":{"billing":0.9,"other":0.1}},
			"frustration":{"type":"score","score":1.4,"confidence":0.7,
				"legend":{"0":"Calm","1":"Frustrated","2":"Very angry"},"probabilities":{"0":0.1,"1":0.4,"2":0.5}}},
			"usage":{"input_tokens":100,"output_tokens":10}}`)
	}))
	defer server.Close()

	client, err := jev.NewClient(jev.WithAPIKey("key"), jev.WithBaseURL(server.URL))
	if err != nil {
		panic(err)
	}
	resp, err := client.SystemOne(context.Background(), jev.Request{
		State: "I was charged twice. Please fix this ASAP.",
		Questions: jev.Questions{
			"department": jev.Choice{
				Instructions: "Which team should handle this?",
				Criteria:     map[string]any{"billing": nil, "other": nil},
			},
			"frustration": jev.Score{
				Instructions: "How frustrated is the customer?",
				Criteria:     []string{"Calm", "Frustrated", "Very angry"},
			},
		},
	})
	if err != nil {
		panic(err)
	}
	department, _ := resp.Choice("department")
	frustration, _ := resp.Score("frustration")
	fmt.Println(department.Choice, department.Confidence)
	fmt.Println(frustration.Score, frustration.Legend[frustration.Level()])
	// Output:
	// billing 0.9
	// 1.4 Very angry
}

func ExampleAPIError() {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":"rate limit exceeded"}`)
	}))
	defer server.Close()

	client, err := jev.NewClient(jev.WithAPIKey("key"), jev.WithBaseURL(server.URL), jev.WithMaxRetries(0))
	if err != nil {
		panic(err)
	}
	_, err = client.SystemOne(context.Background(), jev.Request{
		State:     "hello",
		Questions: jev.Questions{"greeting": jev.Noul{Instructions: "Is this a greeting?"}},
	})
	var apiErr *jev.APIError
	if errors.As(err, &apiErr) {
		retryAfter, _ := apiErr.RetryAfter()
		fmt.Println(errors.Is(err, jev.ErrRateLimit), apiErr.StatusCode, apiErr.Message, retryAfter)
	}
	// Output:
	// true 429 rate limit exceeded 30s
}
