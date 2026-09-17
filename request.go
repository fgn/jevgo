package jev

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"sort"
)

// ErrInvalidRequest is wrapped by every error for a request rejected before
// it is sent: no questions, a Score without at least two levels, a Choice
// without criteria, or a RawQuestion without a type.
var ErrInvalidRequest = errors.New("jev: invalid request")

// Request is the input to [Client.SystemOne].
type Request struct {
	// State is the content every question refers to: a string, or a JSON
	// object or array such as a map, slice, or struct with json tags.
	State any
	// Questions is the nonempty set of named questions. Answers are returned
	// under the same names.
	Questions Questions
	// Model overrides the client's default model when non-empty.
	Model string
	// Extra adds top-level request fields this SDK version does not model.
	// Keys are shallow-merged last-write-wins over state, model, and
	// questions.
	Extra map[string]any
}

// Questions maps question names to questions.
type Questions map[string]Question

// Question is one of [Noul], [Choice], [Score], or [RawQuestion].
type Question interface {
	json.Marshaler
	validate(name string) error
}

// Noul asks a yes/no question. The answer is the probability of yes.
type Noul struct {
	// Instructions is the question: a string, or a JSON object or array.
	Instructions any
	// Criteria optionally describes what yes and no mean.
	Criteria *NoulCriteria
}

// NoulCriteria describes the outcomes of a [Noul]. Nil fields are omitted.
type NoulCriteria struct {
	True  any `json:"true,omitempty"`
	False any `json:"false,omitempty"`
}

// Choice selects one option from a defined set. The answer is the selected
// option with a probability for every option and a confidence.
type Choice struct {
	// Instructions is the question: a string, or a JSON object or array.
	Instructions any
	// Criteria maps each option to its description; a nil value leaves the
	// option undescribed. At least one option is required.
	Criteria map[string]any
}

// Score rates the state against ordered levels. The answer is a
// probability-weighted position along the levels with a probability for
// every level and a confidence.
type Score struct {
	// Instructions is the question: a string, or a JSON object or array.
	Instructions any
	// Criteria lists the levels from lowest to highest, each a string or a
	// JSON object or array. At least two levels are required.
	Criteria []any
}

// RawQuestion is a question sent as-is, for fields or types this SDK version
// does not model. It must contain a non-empty string "type".
type RawQuestion map[string]any

type wireQuestion struct {
	Type         string `json:"type"`
	Instructions any    `json:"instructions,omitempty"`
	Criteria     any    `json:"criteria,omitempty"`
}

// MarshalJSON encodes the question in the API wire format.
func (q Noul) MarshalJSON() ([]byte, error) {
	w := wireQuestion{Type: "noul", Instructions: q.Instructions}
	if q.Criteria != nil {
		w.Criteria = q.Criteria
	}
	return json.Marshal(w)
}

// MarshalJSON encodes the question in the API wire format.
func (q Choice) MarshalJSON() ([]byte, error) {
	criteria := q.Criteria
	if criteria == nil {
		criteria = map[string]any{}
	}
	return json.Marshal(wireQuestion{Type: "choice", Instructions: q.Instructions, Criteria: criteria})
}

// MarshalJSON encodes the question in the API wire format.
func (q Score) MarshalJSON() ([]byte, error) {
	criteria := q.Criteria
	if criteria == nil {
		criteria = []any{}
	}
	return json.Marshal(wireQuestion{Type: "score", Instructions: q.Instructions, Criteria: criteria})
}

// MarshalJSON encodes the question as the underlying map.
func (q RawQuestion) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]any(q))
}

func (q Noul) validate(string) error { return nil }

func (q Choice) validate(name string) error {
	if len(q.Criteria) == 0 {
		return fmt.Errorf("%w: question %q: choice criteria need at least one option", ErrInvalidRequest, name)
	}
	return nil
}

func (q Score) validate(name string) error {
	if len(q.Criteria) < 2 {
		return fmt.Errorf("%w: question %q: score criteria need at least two levels, got %d",
			ErrInvalidRequest, name, len(q.Criteria))
	}
	return nil
}

func (q RawQuestion) validate(name string) error {
	kind, _ := q["type"].(string)
	if kind == "" {
		return fmt.Errorf("%w: question %q: raw question needs a non-empty string \"type\"", ErrInvalidRequest, name)
	}
	if kind == "score" {
		levels, ok := q["criteria"].([]any)
		if !ok || len(levels) < 2 {
			return fmt.Errorf("%w: question %q: score criteria need at least two levels", ErrInvalidRequest, name)
		}
	}
	return nil
}

// marshal validates the request and encodes it with the resolved model.
func (r Request) marshal(model string) ([]byte, error) {
	if len(r.Questions) == 0 {
		return nil, fmt.Errorf("%w: at least one question is required", ErrInvalidRequest)
	}
	names := make([]string, 0, len(r.Questions))
	for name := range r.Questions {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		question := r.Questions[name]
		if question == nil {
			return nil, fmt.Errorf("%w: question %q is nil", ErrInvalidRequest, name)
		}
		err := question.validate(name)
		if err != nil {
			return nil, err
		}
	}
	if model == "" {
		return nil, fmt.Errorf("%w: model is required", ErrInvalidRequest)
	}
	body := map[string]any{
		"state":     r.State,
		"model":     model,
		"questions": r.Questions,
	}
	maps.Copy(body, r.Extra)
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("%w: encode request: %w", ErrInvalidRequest, err)
	}
	return encoded, nil
}
