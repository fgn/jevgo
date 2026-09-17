package jev

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// Response is the result of [Client.SystemOne].
type Response struct {
	ResponseMetadata

	// Model is the model that answered the request.
	Model string
	// Answers holds one answer per question under the question's name.
	Answers map[string]Answer
	// Usage is the token usage for the request.
	Usage Usage
}

// ResponseMetadata carries transport details of a successful response.
type ResponseMetadata struct {
	// RequestID is the x-typesafe-request-id header, or empty when absent.
	RequestID string
	// StatusCode is the HTTP status code.
	StatusCode int
	// Header holds the HTTP response headers.
	Header http.Header
	// RawBody is the complete response body, including answers of kinds this
	// SDK version does not model.
	RawBody json.RawMessage
}

// Usage is token usage for a request.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Answer is one of [NoulAnswer], [ChoiceAnswer], [ScoreAnswer], or
// [UnknownAnswer].
type Answer interface {
	json.Marshaler
	answer()
}

// NoulAnswer answers a [Noul].
type NoulAnswer struct {
	// Noul is the probability of yes, from 0 to 1.
	Noul float64 `json:"noul"`
}

// ChoiceAnswer answers a [Choice].
type ChoiceAnswer struct {
	// Choice is the highest-probability option.
	Choice string `json:"choice"`
	// Confidence is the model's certainty, derived from Probabilities.
	Confidence float64 `json:"confidence"`
	// Probabilities maps every option to its probability; they sum to 1.
	Probabilities map[string]float64 `json:"probabilities"`
}

// ScoreAnswer answers a [Score].
type ScoreAnswer struct {
	// Score is the probability-weighted position along the levels, from 0 to
	// the number of levels minus one; it can fall between levels.
	Score float64 `json:"score"`
	// Confidence is the model's certainty, derived from Probabilities.
	Confidence float64 `json:"confidence"`
	// Legend maps each level index to the description sent in the request.
	Legend map[int]any `json:"legend"`
	// Probabilities maps every level index to its probability; they sum to 1.
	Probabilities map[int]float64 `json:"probabilities"`
}

// UnknownAnswer is an answer of a kind this SDK version does not model.
type UnknownAnswer struct {
	// Type is the answer's type field.
	Type string
	// Raw is the complete answer object.
	Raw json.RawMessage
}

func (NoulAnswer) answer()    {}
func (ChoiceAnswer) answer()  {}
func (ScoreAnswer) answer()   {}
func (UnknownAnswer) answer() {}

// MarshalJSON encodes the answer in the API wire format.
func (a NoulAnswer) MarshalJSON() ([]byte, error) {
	type wire NoulAnswer
	return json.Marshal(struct {
		wire
		Type string `json:"type"`
	}{wire(a), "noul"})
}

// MarshalJSON encodes the answer in the API wire format.
func (a ChoiceAnswer) MarshalJSON() ([]byte, error) {
	type wire ChoiceAnswer
	return json.Marshal(struct {
		wire
		Type string `json:"type"`
	}{wire(a), "choice"})
}

// MarshalJSON encodes the answer in the API wire format.
func (a ScoreAnswer) MarshalJSON() ([]byte, error) {
	type wire ScoreAnswer
	return json.Marshal(struct {
		wire
		Type string `json:"type"`
	}{wire(a), "score"})
}

// MarshalJSON returns the raw answer object.
func (a UnknownAnswer) MarshalJSON() ([]byte, error) {
	if a.Raw == nil {
		return json.Marshal(map[string]string{"type": a.Type})
	}
	return a.Raw, nil
}

// Noul returns the answer to the named [Noul] question.
func (r *Response) Noul(name string) (NoulAnswer, bool) {
	a, ok := r.Answers[name].(NoulAnswer)
	return a, ok
}

// Choice returns the answer to the named [Choice] question.
func (r *Response) Choice(name string) (ChoiceAnswer, bool) {
	a, ok := r.Answers[name].(ChoiceAnswer)
	return a, ok
}

// Score returns the answer to the named [Score] question.
func (r *Response) Score(name string) (ScoreAnswer, bool) {
	a, ok := r.Answers[name].(ScoreAnswer)
	return a, ok
}

// Nouls returns every [NoulAnswer] by question name.
func (r *Response) Nouls() map[string]NoulAnswer {
	return answersOf[NoulAnswer](r)
}

// Choices returns every [ChoiceAnswer] by question name.
func (r *Response) Choices() map[string]ChoiceAnswer {
	return answersOf[ChoiceAnswer](r)
}

// Scores returns every [ScoreAnswer] by question name.
func (r *Response) Scores() map[string]ScoreAnswer {
	return answersOf[ScoreAnswer](r)
}

func answersOf[T Answer](r *Response) map[string]T {
	out := make(map[string]T)
	for name, answer := range r.Answers {
		if typed, ok := answer.(T); ok {
			out[name] = typed
		}
	}
	return out
}

// ModelsResponse is the result of [Client.ListModels].
type ModelsResponse struct {
	ResponseMetadata

	// Models are the models available to the account.
	Models []Model
}

// Model describes an available model.
type Model struct {
	// Name is the model name or alias to send as [Request.Model].
	Name string `json:"name"`
	// Description summarizes the model.
	Description string `json:"description"`
	// ReleaseDate is when the model was released.
	ReleaseDate time.Time `json:"release_date"`
}

func metadataOf(res result) ResponseMetadata {
	return ResponseMetadata{
		RequestID:  res.header.Get(requestIDHeader),
		StatusCode: res.status,
		Header:     res.header,
		RawBody:    res.body,
	}
}

func decodeSystemOne(res result) (*Response, error) {
	var wire struct {
		Model   *string                    `json:"model"`
		Answers map[string]json.RawMessage `json:"answers"`
		Usage   *Usage                     `json:"usage"`
	}
	err := json.Unmarshal(res.body, &wire)
	if err != nil {
		return nil, newResponseValidationError(res, jsonErrorPath(err), err)
	}
	switch {
	case wire.Model == nil:
		return nil, newResponseValidationError(res, "model", errMissingField)
	case wire.Answers == nil:
		return nil, newResponseValidationError(res, "answers", errMissingField)
	case wire.Usage == nil:
		return nil, newResponseValidationError(res, "usage", errMissingField)
	}
	answers := make(map[string]Answer, len(wire.Answers))
	for name, raw := range wire.Answers {
		answer, path, err := decodeAnswer(raw)
		if err != nil {
			return nil, newResponseValidationError(res, joinPath("answers."+name, path), err)
		}
		answers[name] = answer
	}
	return &Response{
		Model:            *wire.Model,
		Answers:          answers,
		Usage:            *wire.Usage,
		ResponseMetadata: metadataOf(res),
	}, nil
}

var errMissingField = errors.New("missing required field")

// decodeAnswer decodes one answer; on failure it reports the offending field
// relative to the answer.
func decodeAnswer(raw json.RawMessage) (Answer, string, error) {
	var tag struct {
		Type *string `json:"type"`
	}
	err := json.Unmarshal(raw, &tag)
	if err != nil {
		return nil, jsonErrorPath(err), err
	}
	if tag.Type == nil {
		return nil, "type", errMissingField
	}
	switch *tag.Type {
	case "noul":
		var w struct {
			Noul *float64 `json:"noul"`
		}
		err := json.Unmarshal(raw, &w)
		if err != nil {
			return nil, jsonErrorPath(err), err
		}
		if w.Noul == nil {
			return nil, "noul", errMissingField
		}
		return NoulAnswer{Noul: *w.Noul}, "", nil
	case "choice":
		var w struct {
			Choice        *string            `json:"choice"`
			Confidence    *float64           `json:"confidence"`
			Probabilities map[string]float64 `json:"probabilities"`
		}
		err := json.Unmarshal(raw, &w)
		if err != nil {
			return nil, jsonErrorPath(err), err
		}
		switch {
		case w.Choice == nil:
			return nil, "choice", errMissingField
		case w.Confidence == nil:
			return nil, "confidence", errMissingField
		case w.Probabilities == nil:
			return nil, "probabilities", errMissingField
		}
		return ChoiceAnswer{Choice: *w.Choice, Confidence: *w.Confidence, Probabilities: w.Probabilities}, "", nil
	case "score":
		var w struct {
			Score         *float64           `json:"score"`
			Confidence    *float64           `json:"confidence"`
			Legend        map[string]any     `json:"legend"`
			Probabilities map[string]float64 `json:"probabilities"`
		}
		err := json.Unmarshal(raw, &w)
		if err != nil {
			return nil, jsonErrorPath(err), err
		}
		switch {
		case w.Score == nil:
			return nil, "score", errMissingField
		case w.Confidence == nil:
			return nil, "confidence", errMissingField
		case w.Legend == nil:
			return nil, "legend", errMissingField
		case w.Probabilities == nil:
			return nil, "probabilities", errMissingField
		}
		legend, err := intKeys(w.Legend)
		if err != nil {
			return nil, "legend", err
		}
		probabilities, err := intKeys(w.Probabilities)
		if err != nil {
			return nil, "probabilities", err
		}
		return ScoreAnswer{Score: *w.Score, Confidence: *w.Confidence, Legend: legend, Probabilities: probabilities}, "", nil
	default:
		return UnknownAnswer{Type: *tag.Type, Raw: append(json.RawMessage(nil), raw...)}, "", nil
	}
}

// intKeys converts the string level keys of a score map to integers.
func intKeys[V any](in map[string]V) (map[int]V, error) {
	out := make(map[int]V, len(in))
	for key, value := range in {
		level, err := strconv.Atoi(key)
		if err != nil {
			return nil, fmt.Errorf("level %q is not an integer", key)
		}
		out[level] = value
	}
	return out, nil
}

func decodeModels(res result) (*ModelsResponse, error) {
	var wire struct {
		Models []Model `json:"models"`
	}
	err := json.Unmarshal(res.body, &wire)
	if err != nil {
		return nil, newResponseValidationError(res, jsonErrorPath(err), err)
	}
	if wire.Models == nil {
		return nil, newResponseValidationError(res, "models", errMissingField)
	}
	return &ModelsResponse{Models: wire.Models, ResponseMetadata: metadataOf(res)}, nil
}

// jsonErrorPath extracts the field path from an encoding/json error.
func jsonErrorPath(err error) string {
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		return typeErr.Field
	}
	return ""
}

func joinPath(prefix, suffix string) string {
	if suffix == "" {
		return prefix
	}
	return prefix + "." + suffix
}
