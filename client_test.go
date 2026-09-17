package jev

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const noulOnlyAnswer = `{"model":"jev-latest","answers":{"is_urgent":{"type":"noul","noul":0.92}},"usage":{}}`

const choiceOnlyAnswer = `{"model":"jev-latest","answers":{"department":{"type":"choice","choice":"technical",
	"probabilities":{"billing":0.08,"technical":0.85,"sales":0.07},"confidence":0.82}},"usage":{}}`

const validAnswers = `{
	"model": "jev-latest",
	"answers": {
		"is_urgent": {"type": "noul", "noul": 0.92},
		"department": {"type": "choice", "choice": "technical",
			"probabilities": {"billing": 0.08, "technical": 0.85, "sales": 0.07}, "confidence": 0.82},
		"frustration": {"type": "score", "score": 1.6,
			"legend": {"0": "Calm", "1": "Frustrated", "2": "Very angry"},
			"probabilities": {"0": 0.05, "1": 0.3, "2": 0.65}, "confidence": 0.78}
	},
	"usage": {"input_tokens": 312, "output_tokens": 48}
}`

func TestEvalEncodesRequestAndDecodesAnswers(t *testing.T) {
	var gotAuth, gotContentType, gotIdempotency string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		gotIdempotency = r.Header.Get("Idempotency-Key")
		if r.URL.Path != "/v1/systemone" {
			t.Errorf("path = %q, want /v1/systemone", r.URL.Path)
		}

		var body struct {
			State     json.RawMessage            `json:"state"`
			Model     string                     `json:"model"`
			Questions map[string]json.RawMessage `json:"questions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if string(body.State) != `"hello"` {
			t.Errorf("state = %s, want \"hello\"", body.State)
		}
		if body.Model != ModelLatest {
			t.Errorf("model = %q, want %q", body.Model, ModelLatest)
		}
		if len(body.Questions) != 3 {
			t.Errorf("questions = %d, want 3", len(body.Questions))
		}
		var noul struct {
			Type         string            `json:"type"`
			Instructions string            `json:"instructions"`
			Criteria     map[string]string `json:"criteria"`
		}
		if err := json.Unmarshal(body.Questions["is_urgent"], &noul); err != nil {
			t.Fatalf("decode question: %v", err)
		}
		if noul.Type != "noul" || noul.Instructions != "Does this convey urgency?" {
			t.Errorf("noul question = %+v", noul)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(validAnswers))
	}))
	defer server.Close()

	client := newTestClient(t, server.URL)

	isUrgent := Noul("is_urgent", "Does this convey urgency?").True("yes").False("no")
	department := Choice("department", "Which team?").
		Option("billing", "Payments").
		Option("technical", "Bugs").
		Option("sales", "Pricing")
	frustration := Score("frustration", "How frustrated?").
		Level("Calm").
		Level("Frustrated").
		Level("Very angry")

	result, err := client.NewRequest().WithContext(context.Background()).State("hello").
		Question(isUrgent).
		Question(department).
		Question(frustration).
		Send()
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	if gotAuth != "Bearer test-key" {
		t.Errorf("authorization = %q", gotAuth)
	}
	if gotContentType != "application/json" {
		t.Errorf("content-type = %q", gotContentType)
	}
	if gotIdempotency == "" {
		t.Error("missing Idempotency-Key header")
	}
	if result.Model() != "jev-latest" {
		t.Errorf("model = %q", result.Model())
	}
	usage := result.Usage()
	input, inputOK := usage.Input()
	output, outputOK := usage.Output()
	if !inputOK || !outputOK || input != 312 || output != 48 {
		t.Errorf("usage = %+v", usage)
	}
	if got := mustGet(t, result, isUrgent).Yes; got != 0.92 {
		t.Errorf("noul = %v, want 0.92", got)
	}
	if got := mustGet(t, result, department); got.Choice != "technical" || got.Confidence != 0.82 {
		t.Errorf("choice = %+v", got)
	}
	if got := mustGet(t, result, frustration); got.Score != 1.6 || got.Legend["2"].String() != "Very angry" {
		t.Errorf("score = %+v", got)
	}
}

func TestEvalReturnsAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"message": "questions must not be empty"}`))
	}))
	defer server.Close()

	client := newTestClient(t, server.URL)
	_, err := client.NewRequest().State("state").Question(Noul("q", "Is it?")).Send()
	if err == nil {
		t.Fatal("expected error")
	}

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %T, want *APIError", err)
	}
	if !apiErr.Unprocessable() || apiErr.Message != "questions must not be empty" {
		t.Errorf("apiErr = %+v", apiErr)
	}
}

func TestEvalRetriesWithStableIdempotencyKey(t *testing.T) {
	var calls int
	var keys []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		if calls == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"message": "slow down"}`))
			return
		}
		_, _ = w.Write([]byte(`{"model": "jev-latest", "answers": {"q": {"type": "noul", "noul": 1}}, "usage": {}}`))
	}))
	defer server.Close()

	client, err := NewClient(
		WithAPIKey("test-key"),
		WithBaseURL(server.URL),
		WithRetryWait(time.Millisecond),
	)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	question := Noul("q", "Is it?")
	result, err := client.NewRequest().State("state").Question(question).Send()
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}
	if len(keys) != 2 || keys[0] == "" || keys[0] != keys[1] {
		t.Errorf("idempotency keys = %v, want two equal non-empty keys", keys)
	}
	if got := mustGet(t, result, question).Yes; got != 1 {
		t.Errorf("noul = %v, want 1", got)
	}
}

func TestEvalValidatesQuestions(t *testing.T) {
	client := newTestClient(t, "http://127.0.0.1:1")

	tests := []struct {
		name      string
		questions []Question
	}{
		{"no questions", nil},
		{"empty id", []Question{Noul("", "Is it?")}},
		{"blank instructions", []Question{Noul("q", "   ")}},
		{"duplicate id", []Question{Noul("q", "Is it?"), Noul("q", "Is that?")}},
		{"choice without options", []Question{Choice("c", "Which?")}},
		{"duplicate choice option", []Question{Choice("c", "Which?").Option("a").Option("a")}},
		{"score with one level", []Question{Score("s", "How much?").Level("Only")}},
		{"zero-value question", []Question{NoulQuestion{}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := client.NewRequest().State("state").Question(tt.questions...).Send(); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestStructuredValueEncodesInPlace(t *testing.T) {
	question := Noul("q", MustJSON(map[string]any{"ask": "Is it urgent?"}))
	if err := question.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	raw, err := json.Marshal(question.questionSpec().Instructions)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(raw) != `{"ask":"Is it urgent?"}` {
		t.Errorf("instructions = %s", raw)
	}
}

func TestNewClientRequiresAPIKey(t *testing.T) {
	if _, err := NewClient(); !errors.Is(err, ErrMissingAPIKey) {
		t.Fatalf("err = %v, want ErrMissingAPIKey", err)
	}
}

func TestValueRejectsNilAndTypedNil(t *testing.T) {
	var pointer *int
	var mapping map[string]int
	var slice []int
	for name, value := range map[string]any{
		"untyped nil":       nil,
		"typed nil pointer": pointer,
		"typed nil map":     mapping,
		"typed nil slice":   slice,
	} {
		if _, err := JSON(value); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestValueSnapshotsInput(t *testing.T) {
	mapping := map[string]any{"n": 1}
	value, err := JSON(mapping)
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}
	mapping["n"] = 2
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(raw) != `{"n":1}` {
		t.Errorf("value aliased input: %s", raw)
	}
}

func TestZeroValueRejected(t *testing.T) {
	if _, err := json.Marshal(Value{}); !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("err = %v, want ErrInvalidValue", err)
	}
	if _, err := (&Client{}).NewRequest().State("s").Question(Noul("q", "Is it?")).Send(); err == nil {
		t.Fatal("zero-value state should be rejected")
	}
}

func TestGetReportsQuestionNotInResult(t *testing.T) {
	question := Noul("q", "Is it?")
	result := &Result{byToken: map[token]any{}}
	if _, err := result.Get(question); err == nil {
		t.Fatal("expected error for question not in result")
	}
}

func TestResponseRejectsTypeMismatch(t *testing.T) {
	body := `{"model":"jev-latest","answers":{"q":{"type":"choice","choice":"x"}},"usage":{}}`
	server := jsonServer(t, body)
	defer server.Close()

	_, err := newTestClient(t, server.URL).NewRequest().State("s").Question(Noul("q", "Is it?")).Send()
	if err == nil || !strings.Contains(err.Error(), "expected type") {
		t.Fatalf("err = %v, want type mismatch", err)
	}
}

func TestResponseRejectsUnexpectedAndMissingAnswers(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"unexpected", `{"answers":{"q":{"type":"noul","noul":1},"extra":{"type":"noul","noul":1}},"usage":{}}`},
		{"missing", `{"answers":{},"usage":{}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := jsonServer(t, tt.body)
			defer server.Close()
			_, err := newTestClient(t, server.URL).NewRequest().State("s").Question(Noul("q", "Is it?")).Send()
			if err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestChoiceDecodeRejectsUnknownOption(t *testing.T) {
	body := `{"answers":{"c":{"type":"choice","choice":"a",
		"probabilities":{"a":0.5,"x":0.5},"confidence":0.5}},"usage":{}}`
	server := jsonServer(t, body)
	defer server.Close()

	_, err := newTestClient(t, server.URL).NewRequest().State("s").
		Question(Choice("c", "Which?").Option("a").Option("b")).
		Send()
	if err == nil || !strings.Contains(err.Error(), "not a declared option") {
		t.Fatalf("err = %v, want undeclared option", err)
	}
}

func TestBuilderIsImmutable(t *testing.T) {
	base := newTestClient(t, "http://127.0.0.1:1").NewRequest().State("s")
	first := base.Question(Noul("a", "Is it?"))
	second := first.Question(Noul("b", "Is it?"))

	if len(first.questions) != 1 {
		t.Errorf("first has %d questions, want 1", len(first.questions))
	}
	if len(second.questions) != 2 {
		t.Errorf("second has %d questions, want 2", len(second.questions))
	}
	if first == second {
		t.Error("Question returned the same request")
	}
}

func TestNilContextRejected(t *testing.T) {
	client := newTestClient(t, "http://127.0.0.1:1")
	_, err := client.NewRequest().WithContext(t.Context()).State("s").Question(Noul("q", "Is it?")).Send()
	if !errors.Is(err, ErrNilContext) {
		t.Fatalf("err = %v, want ErrNilContext", err)
	}
}

func TestNilClientRejected(t *testing.T) {
	var client *Client
	if _, err := client.NewRequest().State("s").Send(); !errors.Is(err, ErrNilClient) {
		t.Fatalf("State err = %v, want ErrNilClient", err)
	}
	if _, err := client.NewRequest().WithContext(context.Background()).Send(); !errors.Is(err, ErrNilClient) {
		t.Fatalf("WithContext err = %v, want ErrNilClient", err)
	}
}

func TestStateRequired(t *testing.T) {
	client := newTestClient(t, "http://127.0.0.1:1")
	_, err := client.NewRequest().WithContext(context.Background()).Question(Noul("q", "Is it?")).Send()
	if err == nil || !strings.Contains(err.Error(), "state is required") {
		t.Fatalf("err = %v, want state required", err)
	}
}

func TestResultAnswersAreIsolated(t *testing.T) {
	server := jsonServer(t, choiceOnlyAnswer)
	defer server.Close()

	department := Choice("department", "Which team?").
		Option("billing").
		Option("technical").
		Option("sales")
	result, err := newTestClient(t, server.URL).NewRequest().State("s").Question(department).Send()
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	first := mustGet(t, result, department)
	first.Probabilities["billing"] = 999

	second := mustGet(t, result, department)
	if second.Probabilities["billing"] == 999 {
		t.Error("returned answer aliases the Result's internal maps")
	}
}

func TestConcurrentReadsAreSafe(t *testing.T) {
	server := jsonServer(t, noulOnlyAnswer)
	defer server.Close()

	question := Noul("is_urgent", "Is it urgent?")
	result, err := newTestClient(t, server.URL).NewRequest().State("s").Question(question).Send()
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			answer, err := result.Get(question)
			if err != nil {
				t.Errorf("Get: %v", err)
				return
			}
			if answer.Yes != 0.92 {
				t.Errorf("noul = %v", answer.Yes)
			}
		})
	}
	wg.Wait()
}

func jsonServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
}

func mustGet[T any](t *testing.T, result *Result, question Typed[T]) T {
	t.Helper()
	value, err := result.Get(question)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return value
}

func newTestClient(t *testing.T, baseURL string) *Client {
	t.Helper()
	client, err := NewClient(WithAPIKey("test-key"), WithBaseURL(baseURL))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return client
}
