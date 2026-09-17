package jev

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync/atomic"
	"unicode/utf8"
)

const (
	questionTypeNoul   = "noul"
	questionTypeChoice = "choice"
	questionTypeScore  = "score"

	// probabilityTolerance bounds rounding error in server-supplied
	// probability distributions. It grows with the number of categories.
	probabilityTolerance = 0.01
	// rangeTolerance absorbs floating-point error at the edges of [0, 1].
	rangeTolerance = 1e-9
)

// token gives every question value a unique identity. It is assigned by the
// constructors and preserved across copies, so a [Result] can prove it came
// from the exact question that was sent, even if another question reuses the
// same id. Builder methods assign a fresh token, so a derived question is
// distinct from the one it was derived from. The zero token is invalid.
type token struct{ id uint64 }

var tokenCounter atomic.Uint64

func newToken() token { return token{id: tokenCounter.Add(1)} }

// Question is a single typed judgment to evaluate against a state. Only the
// implementations in this package (NoulQuestion, ChoiceQuestion, and
// ScoreQuestion) satisfy it. A question must be created by [Noul], [Choice],
// or [Score]; a zero-value question is rejected by [Request.Send].
type Question interface {
	questionID() string
	questionToken() token
	questionSpec() questionSpec
	// Validate reports whether the question is well formed.
	Validate() error
	// decode parses and validates the API response payload for this question.
	decode(json.RawMessage) (any, error)
}

// Typed is a [Question] whose answer type is T. The compiler enforces the
// link between a question and its answer, so an answer can never be misread
// as the wrong type. Read it with [Result.Get].
type Typed[T any] interface {
	Question
	answer(*Result) (T, error)
}

// questionSpec is the wire representation shared by all question types. Each
// field may be a string or a structured JSON value.
type questionSpec struct {
	Type         string `json:"type"`
	Instructions Value  `json:"instructions"`
	Criteria     any    `json:"criteria,omitempty"`
}

func validateBase(t token, id string, instructions Value) error {
	if t.id == 0 {
		return errors.New("question was not created by jev.Noul, jev.Choice, or jev.Score")
	}
	if strings.TrimSpace(id) == "" {
		return errors.New("question id must not be empty")
	}
	if !utf8.ValidString(id) {
		return errors.New("question id must be valid UTF-8")
	}
	if !instructions.valid {
		return errors.New("instructions is required")
	}
	if instructions.isNull {
		// Null is allowed: the API documents instructions as optional, and
		// [Null] is the explicit way to omit them.
		return nil
	}
	if instructions.isText && strings.TrimSpace(instructions.text) == "" {
		return errors.New("instructions must not be empty")
	}
	return nil
}

// entryValue normalizes a description. A blank text description is treated as
// unset (nil), matching an omitted description.
func entryValue[E EntryValue](description E) *Value {
	value := normalize(description)
	if !value.valid || (value.isText && strings.TrimSpace(value.text) == "") {
		return nil
	}
	return &value
}

func probabilityToleranceFor(categories int) float64 {
	tolerance := probabilityTolerance
	if scaled := 0.005 * float64(categories); scaled > tolerance {
		tolerance = scaled
	}
	return tolerance + rangeTolerance
}

func clamp01(v float64) float64 {
	switch {
	case v < 0:
		return 0
	case v > 1:
		return 1
	default:
		return v
	}
}

// ---- Noul -----------------------------------------------------------------

// NoulQuestion is a yes/no question that returns the probability the answer
// is yes.
type NoulQuestion struct {
	id           string
	token        token
	instructions Value

	trueCriteria  *Value
	falseCriteria *Value
}

// Noul builds a [NoulQuestion] with the given id and instructions. Pass a
// string or a [Value] for structured instructions.
func Noul[I EntryValue](id string, instructions I) NoulQuestion {
	return NoulQuestion{id: id, token: newToken(), instructions: normalize(instructions)}
}

// True describes what a yes (value near 1) means. The description may be a
// string or a structured [Value]. It returns a distinct question: a result
// answers either the original or the derived question, not both.
func (q NoulQuestion) True[E EntryValue](description E) NoulQuestion {
	q.trueCriteria = entryValue(description)
	q.token = newToken()
	return q
}

// False describes what a no (value near 0) means. The description may be a
// string or a structured [Value].
func (q NoulQuestion) False[E EntryValue](description E) NoulQuestion {
	q.falseCriteria = entryValue(description)
	q.token = newToken()
	return q
}

func (q NoulQuestion) questionID() string   { return q.id }
func (q NoulQuestion) questionToken() token { return q.token }

func (q NoulQuestion) questionSpec() questionSpec {
	spec := questionSpec{Type: questionTypeNoul, Instructions: q.instructions}
	criteria := map[string]Value{}
	if q.trueCriteria != nil {
		criteria["true"] = *q.trueCriteria
	}
	if q.falseCriteria != nil {
		criteria["false"] = *q.falseCriteria
	}
	if len(criteria) > 0 {
		spec.Criteria = criteria
	}
	return spec
}

// Validate reports whether the id and instructions are well formed.
// [Request.Send] calls it before sending a request.
func (q NoulQuestion) Validate() error {
	if err := validateBase(q.token, q.id, q.instructions); err != nil {
		return err
	}
	if q.trueCriteria != nil && !q.trueCriteria.valid {
		return errors.New("true criteria is invalid")
	}
	if q.falseCriteria != nil && !q.falseCriteria.valid {
		return errors.New("false criteria is invalid")
	}
	return nil
}

func (q NoulQuestion) decode(raw json.RawMessage) (any, error) {
	var payload struct {
		Type string   `json:"type"`
		Noul *float64 `json:"noul"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	if payload.Type != questionTypeNoul {
		return nil, fmt.Errorf("expected type %q, got %q", questionTypeNoul, payload.Type)
	}
	if payload.Noul == nil {
		return nil, errors.New("missing noul value")
	}
	if *payload.Noul < -rangeTolerance || *payload.Noul > 1+rangeTolerance {
		return nil, fmt.Errorf("noul value %v is out of range [0, 1]", *payload.Noul)
	}
	return NoulAnswer{Yes: clamp01(*payload.Noul)}, nil
}

// ---- Choice ---------------------------------------------------------------

// ChoiceQuestion picks one option from a set. It returns the chosen option
// and the full probability distribution.
type ChoiceQuestion struct {
	id           string
	token        token
	instructions Value
	options      []choiceOption
}

type choiceOption struct {
	key         string
	description *Value
	extra       bool
}

// Choice builds a [ChoiceQuestion] with the given id and instructions. Pass a
// string or a [Value] for structured instructions. Add options with
// [ChoiceQuestion.Option] or [ChoiceQuestion.OptionValue].
func Choice[I EntryValue](id string, instructions I) ChoiceQuestion {
	return ChoiceQuestion{id: id, token: newToken(), instructions: normalize(instructions)}
}

// Option adds an option with an optional text description. Omit the
// description when the option needs none (it is sent as null). For a
// structured description, use [ChoiceQuestion.OptionValue]. Option keys must
// be unique; [Request.Send] rejects duplicates.
func (q ChoiceQuestion) Option(key string, description ...string) ChoiceQuestion {
	options := make([]choiceOption, len(q.options), len(q.options)+1)
	copy(options, q.options)
	option := choiceOption{key: key, extra: len(description) > 1}
	if len(description) > 0 {
		value := Text(description[0])
		option.description = &value
	}
	q.options = append(options, option)
	q.token = newToken()
	return q
}

// OptionValue adds an option with a structured description, such as a rubric
// object. Use [JSON] or [MustJSON] to build the description.
func (q ChoiceQuestion) OptionValue(key string, description Value) ChoiceQuestion {
	options := make([]choiceOption, len(q.options), len(q.options)+1)
	copy(options, q.options)
	option := choiceOption{key: key}
	if description.valid {
		option.description = &description
	} else {
		option.extra = true
	}
	q.options = append(options, option)
	q.token = newToken()
	return q
}

func (q ChoiceQuestion) questionID() string   { return q.id }
func (q ChoiceQuestion) questionToken() token { return q.token }

func (q ChoiceQuestion) questionSpec() questionSpec {
	criteria := make(map[string]*Value, len(q.options))
	for _, option := range q.options {
		criteria[option.key] = option.description
	}
	return questionSpec{Type: questionTypeChoice, Instructions: q.instructions, Criteria: criteria}
}

// Validate reports whether the id, instructions, and options are well formed.
// [Request.Send] calls it before sending a request.
func (q ChoiceQuestion) Validate() error {
	if err := validateBase(q.token, q.id, q.instructions); err != nil {
		return err
	}
	if len(q.options) == 0 {
		return errors.New("choice requires at least one option")
	}
	seen := make(map[string]struct{}, len(q.options))
	for _, option := range q.options {
		if strings.TrimSpace(option.key) == "" {
			return errors.New("choice option key must not be empty")
		}
		if _, duplicate := seen[option.key]; duplicate {
			return fmt.Errorf("duplicate choice option %q", option.key)
		}
		if option.extra {
			return fmt.Errorf("choice option %q accepts at most one description", option.key)
		}
		seen[option.key] = struct{}{}
	}
	return nil
}

func (q ChoiceQuestion) decode(raw json.RawMessage) (any, error) {
	var payload struct {
		Type          string             `json:"type"`
		Choice        *string            `json:"choice"`
		Probabilities map[string]float64 `json:"probabilities"`
		Confidence    *float64           `json:"confidence"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	if payload.Type != questionTypeChoice {
		return nil, fmt.Errorf("expected type %q, got %q", questionTypeChoice, payload.Type)
	}
	if payload.Choice == nil || *payload.Choice == "" {
		return nil, errors.New("missing choice")
	}
	allowed := make(map[string]struct{}, len(q.options))
	for _, option := range q.options {
		allowed[option.key] = struct{}{}
	}
	if _, ok := allowed[*payload.Choice]; !ok {
		return nil, fmt.Errorf("choice %q is not one of the declared options", *payload.Choice)
	}
	if payload.Probabilities == nil {
		return nil, errors.New("missing probabilities")
	}
	tolerance := probabilityToleranceFor(len(allowed))
	probabilities := make(map[string]float64, len(payload.Probabilities))
	for key, probability := range payload.Probabilities {
		if _, ok := allowed[key]; !ok {
			return nil, fmt.Errorf("probability key %q is not a declared option", key)
		}
		if probability < -rangeTolerance || probability > 1+rangeTolerance {
			return nil, fmt.Errorf("probability for %q is out of range [0, 1]", key)
		}
		probabilities[key] = clamp01(probability)
	}
	for key := range allowed {
		if _, ok := probabilities[key]; !ok {
			return nil, fmt.Errorf("missing probability for option %q", key)
		}
	}
	sum, maxProbability := 0.0, 0.0
	for _, probability := range probabilities {
		sum += probability
		if probability > maxProbability {
			maxProbability = probability
		}
	}
	if math.Abs(sum-1) > tolerance {
		return nil, fmt.Errorf("probabilities sum to %v, want 1", sum)
	}
	if probabilities[*payload.Choice] < maxProbability-tolerance {
		return nil, fmt.Errorf("choice %q is not the highest-probability option", *payload.Choice)
	}
	if payload.Confidence == nil {
		return nil, errors.New("missing confidence")
	}
	if *payload.Confidence < -rangeTolerance || *payload.Confidence > 1+rangeTolerance {
		return nil, fmt.Errorf("confidence %v is out of range [0, 1]", *payload.Confidence)
	}
	return ChoiceAnswer{
		Choice:        *payload.Choice,
		Probabilities: probabilities,
		Confidence:    clamp01(*payload.Confidence),
	}, nil
}

// ---- Score ----------------------------------------------------------------

// ScoreQuestion rates the state along an ordered rubric and returns a
// probability-weighted value across the levels.
type ScoreQuestion struct {
	id           string
	token        token
	instructions Value
	levels       []Value
}

// Score builds a [ScoreQuestion] with the given id and instructions. Pass a
// string or a [Value] for structured instructions. Add ordered levels with
// [ScoreQuestion.Level]; at least two are required.
func Score[I EntryValue](id string, instructions I) ScoreQuestion {
	return ScoreQuestion{id: id, token: newToken(), instructions: normalize(instructions)}
}

// Level appends an ordered rubric level. The description may be a string or a
// structured [Value]. At least two levels are required.
func (q ScoreQuestion) Level[E EntryValue](description E) ScoreQuestion {
	levels := make([]Value, len(q.levels), len(q.levels)+1)
	copy(levels, q.levels)
	q.levels = append(levels, normalize(description))
	q.token = newToken()
	return q
}

func (q ScoreQuestion) questionID() string   { return q.id }
func (q ScoreQuestion) questionToken() token { return q.token }

func (q ScoreQuestion) questionSpec() questionSpec {
	levels := make([]Value, len(q.levels))
	copy(levels, q.levels)
	return questionSpec{Type: questionTypeScore, Instructions: q.instructions, Criteria: levels}
}

// Validate reports whether the id, instructions, and levels are well formed.
// [Request.Send] calls it before sending a request.
func (q ScoreQuestion) Validate() error {
	if err := validateBase(q.token, q.id, q.instructions); err != nil {
		return err
	}
	if len(q.levels) < 2 {
		return errors.New("score requires at least two levels")
	}
	for i, level := range q.levels {
		if !level.valid {
			return fmt.Errorf("level %d is invalid", i)
		}
		if level.isText && strings.TrimSpace(level.text) == "" {
			return fmt.Errorf("level %d must not be empty", i)
		}
	}
	return nil
}

func (q ScoreQuestion) decode(raw json.RawMessage) (any, error) {
	var payload struct {
		Type          string             `json:"type"`
		Score         *float64           `json:"score"`
		Legend        map[string]Value   `json:"legend"`
		Probabilities map[string]float64 `json:"probabilities"`
		Confidence    *float64           `json:"confidence"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	if payload.Type != questionTypeScore {
		return nil, fmt.Errorf("expected type %q, got %q", questionTypeScore, payload.Type)
	}
	if payload.Score == nil {
		return nil, errors.New("missing score")
	}
	maxIndex := float64(len(q.levels) - 1)
	if *payload.Score < -rangeTolerance || *payload.Score > maxIndex+rangeTolerance {
		return nil, fmt.Errorf("score %v is out of range [0, %v]", *payload.Score, maxIndex)
	}
	if payload.Probabilities == nil {
		return nil, errors.New("missing probabilities")
	}
	expected := make(map[string]struct{}, len(q.levels))
	for i := range q.levels {
		expected[strconv.Itoa(i)] = struct{}{}
	}
	probabilities := make(map[string]float64, len(payload.Probabilities))
	for key, probability := range payload.Probabilities {
		if _, ok := expected[key]; !ok {
			return nil, fmt.Errorf("probability key %q is not a valid level index", key)
		}
		if probability < -rangeTolerance || probability > 1+rangeTolerance {
			return nil, fmt.Errorf("probability for %q is out of range [0, 1]", key)
		}
		probabilities[key] = clamp01(probability)
	}
	sum := 0.0
	for key := range expected {
		probability, ok := probabilities[key]
		if !ok {
			return nil, fmt.Errorf("missing probability for level %q", key)
		}
		sum += probability
	}
	if math.Abs(sum-1) > probabilityToleranceFor(len(expected)) {
		return nil, fmt.Errorf("probabilities sum to %v, want 1", sum)
	}
	if payload.Legend == nil {
		return nil, errors.New("missing legend")
	}
	for key := range payload.Legend {
		if _, ok := expected[key]; !ok {
			return nil, fmt.Errorf("legend key %q is not a valid level index", key)
		}
	}
	for key := range expected {
		index, _ := strconv.Atoi(key)
		value, present := payload.Legend[key]
		if q.levels[index].IsNull() {
			// An undescribed level may be echoed as null or omitted.
			continue
		}
		if !present || value.IsNull() {
			return nil, fmt.Errorf("legend is missing level %q", key)
		}
	}
	if payload.Confidence == nil {
		return nil, errors.New("missing confidence")
	}
	if *payload.Confidence < -rangeTolerance || *payload.Confidence > 1+rangeTolerance {
		return nil, fmt.Errorf("confidence %v is out of range [0, 1]", *payload.Confidence)
	}
	return ScoreAnswer{
		Score:         math.Min(math.Max(*payload.Score, 0), maxIndex),
		Legend:        payload.Legend,
		Probabilities: probabilities,
		Confidence:    clamp01(*payload.Confidence),
	}, nil
}

var (
	_ Question            = NoulQuestion{}
	_ Question            = ChoiceQuestion{}
	_ Question            = ScoreQuestion{}
	_ Typed[NoulAnswer]   = NoulQuestion{}
	_ Typed[ChoiceAnswer] = ChoiceQuestion{}
	_ Typed[ScoreAnswer]  = ScoreQuestion{}
)
