package jev

import (
	"errors"
	"fmt"
	"maps"
)

// NoulAnswer is the answer to a [NoulQuestion]. Yes is the probability the
// answer is yes, on a scale from 0 (no) to 1 (yes). Noul answers have no
// separate confidence.
type NoulAnswer struct {
	Yes float64
}

// ChoiceAnswer is the answer to a [ChoiceQuestion].
type ChoiceAnswer struct {
	// Choice is the highest-probability option.
	Choice string
	// Probabilities maps every option to its probability; values sum to 1.
	Probabilities map[string]float64
	// Confidence is how certain the model is, derived from Probabilities.
	Confidence float64
}

// ScoreAnswer is the answer to a [ScoreQuestion].
type ScoreAnswer struct {
	// Score is the probability-weighted answer across the levels; it can land
	// between levels.
	Score float64
	// Legend maps each level number back to its description. A description may
	// be a string or structured; use [Value.Text] or [Value.Decode] to read it.
	Legend map[string]Value
	// Probabilities maps each level (as a string key) to its probability;
	// values sum to 1.
	Probabilities map[string]float64
	// Confidence is how certain the model is, derived from Probabilities.
	Confidence float64
}

// Usage reports token usage for a request. Each field is nil when the API did
// not report it; use [Usage.Input] and [Usage.Output] for a convenience
// two-value read.
type Usage struct {
	// Reported is true when the API included a usage object.
	Reported     bool `json:"-"`
	InputTokens  *int `json:"input_tokens"`
	OutputTokens *int `json:"output_tokens"`
}

// Input returns the input token count and whether the API reported it.
func (u Usage) Input() (int, bool) {
	if u.InputTokens == nil {
		return 0, false
	}
	return *u.InputTokens, true
}

// Output returns the output token count and whether the API reported it.
func (u Usage) Output() (int, bool) {
	if u.OutputTokens == nil {
		return 0, false
	}
	return *u.OutputTokens, true
}

// Result holds the answers to the questions passed to [Request.Question],
// along with the model, token usage, and request id. Read answers with
// [Result.Get]. A Result is read-only and safe for concurrent use: its state
// is never exposed for mutation, and answers are copied on read.
type Result struct {
	model     string
	usage     Usage
	requestID string
	byToken   map[token]any
}

// Model returns the model that performed the evaluation. It is the versioned
// model id, which may differ from the requested alias.
func (r *Result) Model() string {
	if r == nil {
		return ""
	}
	return r.model
}

// Usage returns the token usage for the request. Check [Usage.Reported] to
// distinguish an unreported usage from zero tokens.
func (r *Result) Usage() Usage {
	if r == nil {
		return Usage{}
	}
	return r.usage
}

// RequestID returns the API's request id (the x-typesafe-request-id response
// header), useful for logging and support. It is empty if the API did not send
// one.
func (r *Result) RequestID() string {
	if r == nil {
		return ""
	}
	return r.requestID
}

// Get returns the typed answer for q. It reports an error if the result did
// not come from a request that included q, rather than returning a zero value.
// The answer type is inferred from q, so an answer can never be misread as the
// wrong type.
func (r *Result) Get[T any](q Typed[T]) (T, error) {
	var zero T
	if q == nil || isNilValue(q) {
		return zero, errors.New("jev: question must not be nil")
	}
	return q.answer(r)
}

func (q NoulQuestion) answer(r *Result) (NoulAnswer, error) {
	return lookup[NoulAnswer](r, q)
}

func (q ChoiceQuestion) answer(r *Result) (ChoiceAnswer, error) {
	return lookup[ChoiceAnswer](r, q)
}

func (q ScoreQuestion) answer(r *Result) (ScoreAnswer, error) {
	return lookup[ScoreAnswer](r, q)
}

// lookup finds the answer stored under q's token and asserts it to T.
func lookup[T any](r *Result, q Question) (T, error) {
	var zero T
	if r == nil {
		return zero, errors.New("jev: result is nil")
	}
	token := q.questionToken()
	if token.id == 0 {
		return zero, errors.New("jev: question was not created by jev.Noul, jev.Choice, or jev.Score")
	}
	stored, ok := r.byToken[token]
	if !ok {
		return zero, fmt.Errorf("jev: result does not include question %q", q.questionID())
	}
	typed, ok := cloneAnswer(stored).(T)
	if !ok {
		return zero, fmt.Errorf("jev: answer for question %q has an unexpected type", q.questionID())
	}
	return typed, nil
}

// cloneAnswer returns a copy of a stored answer so callers can never mutate
// the Result's internal state through returned maps.
func cloneAnswer(stored any) any {
	switch answer := stored.(type) {
	case ChoiceAnswer:
		answer.Probabilities = cloneFloat64Map(answer.Probabilities)
		return answer
	case ScoreAnswer:
		answer.Probabilities = cloneFloat64Map(answer.Probabilities)
		answer.Legend = cloneValueMap(answer.Legend)
		return answer
	default:
		return stored
	}
}

func cloneFloat64Map(m map[string]float64) map[string]float64 {
	if m == nil {
		return nil
	}
	out := make(map[string]float64, len(m))
	maps.Copy(out, m)
	return out
}

func cloneValueMap(m map[string]Value) map[string]Value {
	if m == nil {
		return nil
	}
	out := make(map[string]Value, len(m))
	maps.Copy(out, m)
	return out
}
