// Package jev is a Go client for the TypeSafe AI System One API and its
// flagship model, Jev: send a state and typed questions, get typed answers
// with probabilities.
//
//	client, err := jev.NewClient() // reads TYPESAFE_API_KEY
//	...
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
//				Criteria:     []string{"Calm", "Frustrated", "Very angry"},
//			},
//		},
//	})
//	...
//	if department, ok := resp.Choice("department"); ok {
//		fmt.Println(department.Choice, department.Confidence)
//	}
//
// Answers are a sealed interface: type switch on [NoulAnswer],
// [ChoiceAnswer], [ScoreAnswer], and [UnknownAnswer], or use the typed
// getters on [Response]. See the README for configuration, errors, retries,
// and instrumentation.
package jev
