package jev

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
)

// ErrInvalidRequest is wrapped by errors for requests rejected before they
// are sent.
var ErrInvalidRequest = errors.New("jev: invalid request")

// Request is the input to [Client.SystemOne]. It is not the wire body;
// SystemOne validates and encodes it.
type Request struct {
	// State is the content every question refers to: a string, or a JSON
	// object or array such as a map, slice, or struct with json tags.
	State any
	// Questions is the nonempty set of named questions. Answers are returned
	// under the same names.
	Questions Questions
	// Model overrides the client's default model when non-empty.
	Model string
	// Extra adds top-level fields this SDK version does not model. Keys are
	// merged last-write-wins after validation, so they can replace state,
	// model, and questions on the wire.
	Extra map[string]any
}

// Questions maps question names to questions.
type Questions map[string]Question

// Question is one of [Noul], [Choice], [Score], or [RawQuestion].
type Question interface {
	json.Marshaler
	question()
}

// Noul asks a yes/no question. The answer is the probability of yes.
type Noul struct {
	// Instructions is a string, or a JSON object or array.
	Instructions any
	// Criteria optionally describes what yes and no mean.
	Criteria *NoulCriteria
}

// NoulCriteria describes the outcomes of a [Noul]; nil fields are omitted.
type NoulCriteria struct {
	True  any `json:"true,omitempty"`
	False any `json:"false,omitempty"`
}

// Choice selects one option from a defined set.
type Choice struct {
	// Instructions is a string, or a JSON object or array.
	Instructions any
	// Criteria is a JSON object mapping each option to its description, such
	// as a map[string]string or map[string]any. A nil description leaves the
	// option undescribed.
	Criteria any
}

// Score rates the state against ordered levels. The answer is a
// probability-weighted position along them.
type Score struct {
	// Instructions is a string, or a JSON object or array.
	Instructions any
	// Criteria is a nonempty JSON array of level descriptions from lowest to
	// highest, such as a []string or []any.
	Criteria any
}

// RawQuestion is sent as-is, for fields or kinds this SDK version does not
// model. It must contain a non-empty string "type"; known kinds are
// validated like their typed forms.
type RawQuestion map[string]any

func (Noul) question()        {}
func (Choice) question()      {}
func (Score) question()       {}
func (RawQuestion) question() {}

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

// MarshalJSON encodes the underlying map.
func (q RawQuestion) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]any(q))
}

// encodedRequest is a validated wire body with what tracing and decoding
// need to know about it.
type encodedRequest struct {
	body  []byte
	model string
	// kinds maps each question name to its wire type.
	kinds map[string]string
}

func (r Request) encode(defaultModel string) (encodedRequest, error) {
	if len(r.Questions) == 0 {
		return encodedRequest{}, fmt.Errorf("%w: at least one question is required", ErrInvalidRequest)
	}
	questions := make(map[string]json.RawMessage, len(r.Questions))
	kinds := make(map[string]string, len(r.Questions))
	for _, name := range slices.Sorted(maps.Keys(r.Questions)) {
		question := r.Questions[name]
		if isNilQuestion(question) {
			return encodedRequest{}, fmt.Errorf("%w: question %q is nil", ErrInvalidRequest, name)
		}
		raw, err := json.Marshal(question)
		if err != nil {
			return encodedRequest{}, fmt.Errorf("%w: question %q: %w", ErrInvalidRequest, name, err)
		}
		kind, err := validateQuestion(name, raw)
		if err != nil {
			return encodedRequest{}, err
		}
		questions[name] = raw
		kinds[name] = kind
	}
	model := r.Model
	if model == "" {
		model = defaultModel
	}
	if model == "" {
		return encodedRequest{}, fmt.Errorf("%w: model is required", ErrInvalidRequest)
	}
	body := map[string]any{"state": r.State, "model": model, "questions": questions}
	maps.Copy(body, r.Extra)
	if override, ok := body["model"].(string); ok {
		model = override
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return encodedRequest{}, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	return encodedRequest{body: encoded, model: model, kinds: kinds}, nil
}

// isNilQuestion also catches typed nil pointers and nil maps, which satisfy
// the interface but panic or encode as null.
func isNilQuestion(q Question) bool {
	if q == nil {
		return true
	}
	v := reflect.ValueOf(q)
	return (v.Kind() == reflect.Pointer || v.Kind() == reflect.Map) && v.IsNil()
}

// validateQuestion checks the encoded JSON rather than Go types, so raw and
// typed questions follow the same rules.
func validateQuestion(name string, raw []byte) (string, error) {
	var wire struct {
		Type     string          `json:"type"`
		Criteria json.RawMessage `json:"criteria"`
	}
	err := json.Unmarshal(raw, &wire)
	if err != nil {
		return "", fmt.Errorf("%w: question %q: %w", ErrInvalidRequest, name, err)
	}
	if wire.Type == "" {
		return "", fmt.Errorf("%w: question %q needs a non-empty string \"type\"", ErrInvalidRequest, name)
	}
	criteria := bytes.TrimSpace(wire.Criteria)
	switch wire.Type {
	case "choice":
		if !bytes.HasPrefix(criteria, []byte("{")) {
			return "", fmt.Errorf("%w: question %q: choice criteria must be a JSON object", ErrInvalidRequest, name)
		}
	case "score":
		var levels []json.RawMessage
		if json.Unmarshal(criteria, &levels) != nil || len(levels) == 0 {
			return "", fmt.Errorf("%w: question %q: score criteria must be a nonempty JSON array", ErrInvalidRequest, name)
		}
	}
	return wire.Type, nil
}
