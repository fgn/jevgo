// Command basic triages a support ticket with TypeSafe and records the call
// as a Langfuse generation. It reads TYPESAFE_API_KEY and the LANGFUSE_*
// variables from the environment.
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/fgn/go-langfuse"

	jev "github.com/fgn/jevgo"
	jevlangfuse "github.com/fgn/jevgo/contrib/langfuse"
)

func main() {
	if err := run(context.Background()); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context) error {
	lf, err := langfuse.New(ctx, langfuse.ConfigFromEnv())
	if err != nil {
		return fmt.Errorf("create Langfuse client: %w", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := lf.Shutdown(shutdownCtx); err != nil {
			log.Printf("shut down Langfuse client: %v", err)
		}
	}()

	client, err := jev.NewClient(jev.WithTracer(jevlangfuse.NewTracer(lf)))
	if err != nil {
		return err
	}

	ticket := "I was charged twice for my subscription and need a refund before tomorrow."
	ctx = lf.WithTraceAttributes(ctx, langfuse.TraceAttributes{Name: "triage-ticket", Tags: []string{"support"}})
	ctx, root := lf.StartObservation(ctx, "triage-ticket", langfuse.TypeSpan,
		langfuse.ObservationAttributes{Input: ticket})
	defer root.End()

	resp, err := client.SystemOne(ctx, jev.Request{
		State: ticket,
		Questions: jev.Questions{
			"urgent": jev.Noul{Instructions: "Does this convey urgency?"},
			"department": jev.Choice{
				Instructions: "Which team should handle this?",
				Criteria:     map[string]any{"billing": nil, "technical": nil, "sales": nil},
			},
		},
	})
	if err != nil {
		root.RecordError(err)
		return err
	}
	department, _ := resp.Choice("department")
	urgent, _ := resp.Noul("urgent")
	root.Update(langfuse.ObservationAttributes{Output: map[string]any{
		"department": department.Choice, "urgent": urgent.Noul,
	}})
	fmt.Printf("department=%s (confidence %.2f) urgent=%.2f request_id=%s\n",
		department.Choice, department.Confidence, urgent.Noul, resp.RequestID)
	return nil
}
