// Command structured evaluates an order against a return policy using
// structured state, instructions, and criteria, and shows how to type
// switch over answers. It reads TYPESAFE_API_KEY from the environment.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"

	jev "github.com/fgn/jevgo"
)

type order struct {
	DaysSinceDelivery int  `json:"days_since_delivery"`
	Opened            bool `json:"opened"`
	FinalSale         bool `json:"final_sale"`
}

func main() {
	client, err := jev.NewClient()
	if err != nil {
		log.Fatal(err)
	}
	resp, err := client.SystemOne(context.Background(), jev.Request{
		State: order{DaysSinceDelivery: 10, Opened: false, FinalSale: false},
		Questions: jev.Questions{
			"eligible": jev.Choice{
				Instructions: map[string]any{
					"question": "Is this order eligible for a return?",
					"policy": map[string]any{
						"return_window_days": 30,
						"must_be_unopened":   true,
						"exclude_final_sale": true,
					},
				},
				Criteria: map[string]any{
					"eligible":   map[string]any{"rule": "All return policy conditions are satisfied"},
					"ineligible": map[string]any{"rule": "At least one return policy condition is violated"},
				},
			},
			"needs_review": jev.Noul{Instructions: "Is the outcome ambiguous enough to need a human check?"},
		},
	})
	if err != nil {
		log.Fatal(err)
	}

	for name, answer := range resp.Answers {
		switch a := answer.(type) {
		case jev.ChoiceAnswer:
			fmt.Printf("%s: %s (confidence %.2f)\n", name, a.Choice, a.Confidence)
		case jev.NoulAnswer:
			fmt.Printf("%s: %.2f\n", name, a.Noul)
		case jev.ScoreAnswer:
			fmt.Printf("%s: %.2f\n", name, a.Score)
		case jev.UnknownAnswer:
			fmt.Printf("%s: unknown answer type %q\n", name, a.Type)
		}
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(resp.Answers)
}
