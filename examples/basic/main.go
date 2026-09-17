// Command basic asks the three question types about a support ticket.
// It reads TYPESAFE_API_KEY from the environment.
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	jev "github.com/fgn/jevgo"
)

func main() {
	ticket := "I was charged twice for my subscription and need a refund before tomorrow."
	if len(os.Args) > 1 {
		ticket = os.Args[1]
	}

	client, err := jev.NewClient()
	if err != nil {
		log.Fatal(err)
	}
	resp, err := client.SystemOne(context.Background(), jev.Request{
		State: ticket,
		Questions: jev.Questions{
			"urgent": jev.Noul{
				Instructions: "Does this convey urgency?",
				Criteria:     &jev.NoulCriteria{True: "Explicitly time-sensitive", False: "No urgency expressed"},
			},
			"department": jev.Choice{
				Instructions: "Which team should handle this?",
				Criteria: map[string]any{
					"billing":   "Payments, invoicing, refunds",
					"technical": "Bugs, outages, integrations",
					"sales":     "Pricing, upgrades, new accounts",
				},
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

	urgent, _ := resp.Noul("urgent")
	department, _ := resp.Choice("department")
	frustration, _ := resp.Score("frustration")
	fmt.Printf("model:       %s (request %s, %d input tokens)\n", resp.Model, resp.RequestID, resp.Usage.InputTokens)
	fmt.Printf("urgent:      %.2f\n", urgent.Noul)
	fmt.Printf("department:  %s (confidence %.2f, probabilities %v)\n",
		department.Choice, department.Confidence, department.Probabilities)
	fmt.Printf("frustration: %.2f of %d (confidence %.2f)\n",
		frustration.Score, len(frustration.Legend)-1, frustration.Confidence)
}
