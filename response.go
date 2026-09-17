package jev

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
)

// Response is the result of [Client.SystemOne].
type Response struct {
	ResponseMetadata

	// Model is the model that answered the request.
	Model string
	// Answers holds one answer per question under the question's name.
	Answers map[string]Answer
	Usage   Usage
}

// ResponseMetadata carries transport details of a successful response.
type ResponseMetadata struct {
	// RequestID is the X-Typesafe-Request-Id header, or empty when absent.
	RequestID  string
	StatusCode int
	Header     http.Header
	// RawBody is the complete response body, including answers of kinds this
	// SDK version does not model.
	RawBody json.RawMessage
	// Attempts is the number of HTTP attempts made, including retries.
	Attempts int
}

// Usage is the token usage for a request.
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
	// Confidence is the model's certainty in Choice, from 0 to 1.
	Confidence float64 `json:"confidence"`
	// Probabilities maps every option to its probability.
	Probabilities map[string]float64 `json:"probabilities"`
}

// ScoreAnswer answers a [Score].
type ScoreAnswer struct {
	// Score is the probability-weighted position along the levels, from 0 to
	// the highest level. It usually falls between levels; see [ScoreAnswer.Level].
	Score float64 `json:"score"`
	// Confidence is the model's certainty in Score, from 0 to 1.
	Confidence float64 `json:"confidence"`
	// Legend maps each level to the description sent in the request.
	Legend map[int]any `json:"legend"`
	// Probabilities maps every level to its probability.
	Probabilities map[int]float64 `json:"probabilities"`
}

// UnknownAnswer is an answer of a kind this SDK version does not model.
type UnknownAnswer struct {
	Type string
	// Raw is the complete answer object.
	Raw json.RawMessage
}

func (NoulAnswer) answer()    {}
func (ChoiceAnswer) answer()  {}
func (ScoreAnswer) answer()   {}
func (UnknownAnswer) answer() {}

// Level returns the highest-probability level, the lowest on a tie. Use it
// instead of truncating Score, which is a weighted average.
func (a ScoreAnswer) Level() int {
	best, bestProbability := 0, -1.0
	for level, probability := range a.Probabilities {
		if probability > bestProbability || (probability == bestProbability && level < best) {
			best, bestProbability = level, probability
		}
	}
	return best
}

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
func (r *Response) Noul(name string) (NoulAnswer, bool) { return answerAs[NoulAnswer](r, name) }

// Choice returns the answer to the named [Choice] question.
func (r *Response) Choice(name string) (ChoiceAnswer, bool) { return answerAs[ChoiceAnswer](r, name) }

// Score returns the answer to the named [Score] question.
func (r *Response) Score(name string) (ScoreAnswer, bool) { return answerAs[ScoreAnswer](r, name) }

// Nouls returns every [NoulAnswer] by question name.
func (r *Response) Nouls() map[string]NoulAnswer { return answersOf[NoulAnswer](r) }

// Choices returns every [ChoiceAnswer] by question name.
func (r *Response) Choices() map[string]ChoiceAnswer { return answersOf[ChoiceAnswer](r) }

// Scores returns every [ScoreAnswer] by question name.
func (r *Response) Scores() map[string]ScoreAnswer { return answersOf[ScoreAnswer](r) }

// answerAs accepts both value and pointer forms so hand-built responses in
// tests behave like decoded ones.
func answerAs[T Answer](r *Response, name string) (T, bool) {
	var zero T
	if r == nil {
		return zero, false
	}
	v, ok := deref(r.Answers[name]).(T)
	if !ok {
		return zero, false
	}
	return v, true
}

func deref(a Answer) Answer {
	switch v := a.(type) {
	case *NoulAnswer:
		if v != nil {
			return *v
		}
	case *ChoiceAnswer:
		if v != nil {
			return *v
		}
	case *ScoreAnswer:
		if v != nil {
			return *v
		}
	case *UnknownAnswer:
		if v != nil {
			return *v
		}
	default:
		return a
	}
	return nil
}

func answersOf[T Answer](r *Response) map[string]T {
	out := make(map[string]T)
	if r == nil {
		return out
	}
	for name := range r.Answers {
		if typed, ok := answerAs[T](r, name); ok {
			out[name] = typed
		}
	}
	return out
}

// ModelsResponse is the result of [Client.ListModels].
type ModelsResponse struct {
	ResponseMetadata

	Models []Model
}

// Model describes an available model.
type Model struct {
	// Name is the model name or alias to send as [Request.Model].
	Name        string `json:"name"`
	Description string `json:"description"`
	// ReleaseDate is kept as sent: the API documents YYYY-MM-DD and has
	// returned RFC 3339 timestamps.
	ReleaseDate string `json:"release_date"`
}

var (
	errMissingField = errors.New("missing required field")
	errNullValue    = errors.New("null value")
	errOutOfRange   = errors.New("value out of range")
)

func metadataOf(res result) ResponseMetadata {
	return ResponseMetadata{
		RequestID:  res.header.Get(requestIDHeader),
		StatusCode: res.status,
		Header:     res.header,
		RawBody:    res.body,
		Attempts:   res.attempts,
	}
}

// decodeSystemOne validates the body against the API contract and against
// the questions that were asked.
func decodeSystemOne(res result, kinds map[string]string) (*Response, error) {
	var wire struct {
		Model   *string                    `json:"model"`
		Answers map[string]json.RawMessage `json:"answers"`
		Usage   *struct {
			InputTokens  *int `json:"input_tokens"`
			OutputTokens *int `json:"output_tokens"`
		} `json:"usage"`
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
	case wire.Usage.InputTokens == nil:
		return nil, newResponseValidationError(res, "usage.input_tokens", errMissingField)
	case wire.Usage.OutputTokens == nil:
		return nil, newResponseValidationError(res, "usage.output_tokens", errMissingField)
	}
	answers := make(map[string]Answer, len(wire.Answers))
	for name, raw := range wire.Answers {
		answer, path, err := decodeAnswer(raw)
		if err != nil {
			return nil, newResponseValidationError(res, joinPath("answers."+name, path), err)
		}
		answers[name] = answer
	}
	for name, kind := range kinds {
		answer, ok := answers[name]
		if !ok {
			return nil, newResponseValidationError(res, "answers."+name, errMissingField)
		}
		if got := answerKind(answer); isKnownKind(kind) && got != kind {
			return nil, newResponseValidationError(res, "answers."+name+".type",
				fmt.Errorf("expected %q, got %q", kind, got))
		}
	}
	return &Response{
		ResponseMetadata: metadataOf(res),
		Model:            *wire.Model,
		Answers:          answers,
		Usage:            Usage{InputTokens: *wire.Usage.InputTokens, OutputTokens: *wire.Usage.OutputTokens},
	}, nil
}

func isKnownKind(kind string) bool { return kind == "noul" || kind == "choice" || kind == "score" }

func answerKind(a Answer) string {
	switch v := a.(type) {
	case NoulAnswer:
		return "noul"
	case ChoiceAnswer:
		return "choice"
	case ScoreAnswer:
		return "score"
	case UnknownAnswer:
		return v.Type
	default:
		return ""
	}
}

// decodeAnswer reports the offending field relative to the answer.
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
		return decodeNoul(raw)
	case "choice":
		return decodeChoice(raw)
	case "score":
		return decodeScore(raw)
	default:
		return UnknownAnswer{Type: *tag.Type, Raw: append(json.RawMessage(nil), raw...)}, "", nil
	}
}

func decodeNoul(raw json.RawMessage) (Answer, string, error) {
	var w struct {
		Noul *float64 `json:"noul"`
	}
	err := json.Unmarshal(raw, &w)
	switch {
	case err != nil:
		return nil, jsonErrorPath(err), err
	case w.Noul == nil:
		return nil, "noul", errMissingField
	case !inUnitRange(*w.Noul):
		return nil, "noul", errOutOfRange
	}
	return NoulAnswer{Noul: *w.Noul}, "", nil
}

func decodeChoice(raw json.RawMessage) (Answer, string, error) {
	var w struct {
		Choice        *string             `json:"choice"`
		Confidence    *float64            `json:"confidence"`
		Probabilities map[string]*float64 `json:"probabilities"`
	}
	err := json.Unmarshal(raw, &w)
	switch {
	case err != nil:
		return nil, jsonErrorPath(err), err
	case w.Choice == nil:
		return nil, "choice", errMissingField
	case w.Confidence == nil:
		return nil, "confidence", errMissingField
	case !inUnitRange(*w.Confidence):
		return nil, "confidence", errOutOfRange
	case w.Probabilities == nil:
		return nil, "probabilities", errMissingField
	}
	probabilities, path, err := probabilityMap(w.Probabilities)
	if err != nil {
		return nil, "probabilities" + path, err
	}
	if _, ok := probabilities[*w.Choice]; !ok {
		return nil, "choice", fmt.Errorf("%q is not among the probabilities", *w.Choice)
	}
	return ChoiceAnswer{Choice: *w.Choice, Confidence: *w.Confidence, Probabilities: probabilities}, "", nil
}

func decodeScore(raw json.RawMessage) (Answer, string, error) {
	var w struct {
		Score         *float64                   `json:"score"`
		Confidence    *float64                   `json:"confidence"`
		Legend        map[string]json.RawMessage `json:"legend"`
		Probabilities map[string]*float64        `json:"probabilities"`
	}
	err := json.Unmarshal(raw, &w)
	switch {
	case err != nil:
		return nil, jsonErrorPath(err), err
	case w.Score == nil:
		return nil, "score", errMissingField
	case w.Confidence == nil:
		return nil, "confidence", errMissingField
	case !inUnitRange(*w.Confidence):
		return nil, "confidence", errOutOfRange
	case w.Legend == nil:
		return nil, "legend", errMissingField
	case w.Probabilities == nil:
		return nil, "probabilities", errMissingField
	}
	probabilities, path, err := probabilityMap(w.Probabilities)
	if err != nil {
		return nil, "probabilities" + path, err
	}
	levelProbabilities, path, err := levelKeys(probabilities)
	if err != nil {
		return nil, "probabilities" + path, err
	}
	legendValues := make(map[string]any, len(w.Legend))
	for key, value := range w.Legend {
		var description any
		err := json.Unmarshal(value, &description)
		if err != nil || description == nil {
			return nil, "legend." + key, errNullValue
		}
		legendValues[key] = description
	}
	legend, path, err := levelKeys(legendValues)
	if err != nil {
		return nil, "legend" + path, err
	}
	if len(legend) != len(levelProbabilities) {
		return nil, "legend", errors.New("levels differ from probabilities")
	}
	for level := range legend {
		if _, ok := levelProbabilities[level]; !ok {
			return nil, "legend." + strconv.Itoa(level), errors.New("level missing from probabilities")
		}
	}
	if *w.Score < -1e-6 || *w.Score > float64(len(legend)-1)+1e-6 {
		return nil, "score", errOutOfRange
	}
	return ScoreAnswer{
		Score:         *w.Score,
		Confidence:    *w.Confidence,
		Legend:        legend,
		Probabilities: levelProbabilities,
	}, "", nil
}

func inUnitRange(v float64) bool { return v >= -1e-6 && v <= 1+1e-6 }

func probabilityMap(in map[string]*float64) (map[string]float64, string, error) {
	out := make(map[string]float64, len(in))
	for key, value := range in {
		switch {
		case value == nil:
			return nil, "." + key, errNullValue
		case !inUnitRange(*value):
			return nil, "." + key, errOutOfRange
		}
		out[key] = *value
	}
	return out, "", nil
}

// levelKeys converts string keys to levels, requiring canonical nonnegative
// integers so distinct keys cannot merge.
func levelKeys[V any](in map[string]V) (map[int]V, string, error) {
	out := make(map[int]V, len(in))
	for key, value := range in {
		level, err := strconv.Atoi(key)
		if err != nil || level < 0 || strconv.Itoa(level) != key {
			return nil, "." + key, errors.New("key is not a level index")
		}
		out[level] = value
	}
	return out, "", nil
}

func decodeModels(res result) (*ModelsResponse, error) {
	var wire struct {
		Models []json.RawMessage `json:"models"`
	}
	err := json.Unmarshal(res.body, &wire)
	if err != nil {
		return nil, newResponseValidationError(res, jsonErrorPath(err), err)
	}
	if wire.Models == nil {
		return nil, newResponseValidationError(res, "models", errMissingField)
	}
	models := make([]Model, 0, len(wire.Models))
	for i, raw := range wire.Models {
		var w struct {
			Name        *string `json:"name"`
			Description *string `json:"description"`
			ReleaseDate *string `json:"release_date"`
		}
		path := "models[" + strconv.Itoa(i) + "]"
		err := json.Unmarshal(raw, &w)
		switch {
		case err != nil:
			return nil, newResponseValidationError(res, joinPath(path, jsonErrorPath(err)), err)
		case w.Name == nil || *w.Name == "":
			return nil, newResponseValidationError(res, path+".name", errMissingField)
		case w.Description == nil:
			return nil, newResponseValidationError(res, path+".description", errMissingField)
		case w.ReleaseDate == nil:
			return nil, newResponseValidationError(res, path+".release_date", errMissingField)
		}
		models = append(models, Model{Name: *w.Name, Description: *w.Description, ReleaseDate: *w.ReleaseDate})
	}
	return &ModelsResponse{ResponseMetadata: metadataOf(res), Models: models}, nil
}

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
