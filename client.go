// Package jev provides a Go client for TypeSafe's System One JEV model.
package jev

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	mrand "math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultBaseURL is the default System One API base URL.
	DefaultBaseURL = "https://api.typesafe.ai"
	// ModelLatest is TypeSafe's flagship model alias. It points to the most
	// recent stable release and moves when a new release ships.
	ModelLatest = "jev-latest"
	// ModelPreview points to the most recent release, including previews. It
	// moves ahead of ModelLatest when a preview build is available.
	ModelPreview = "jev-preview"

	endpointPath     = "/v1/systemone"
	modelsPath       = "/v1/models"
	maxResponseBytes = 10 << 20
	maxBackoff       = 30 * time.Second
	maxRetryAfter    = 5 * time.Minute
)

// Client evaluates typed questions against a state using the System One API.
// Create one with [NewClient]. A Client is immutable after creation and safe
// for concurrent use.
type Client struct {
	apiKey         string
	endpoint       string
	modelsEndpoint string
	httpClient     *http.Client
	model          string
	maxRetries     int
	retryWait      time.Duration
}

// NewClient builds a [*Client]. WithAPIKey is required; all other options have
// sensible defaults.
func NewClient(options ...Option) (*Client, error) {
	cfg := defaultConfig()
	for _, option := range options {
		if option != nil {
			option(&cfg)
		}
	}

	if strings.TrimSpace(cfg.apiKey) == "" {
		return nil, ErrMissingAPIKey
	}
	if cfg.httpClient == nil {
		return nil, errors.New("jev: HTTP client must not be nil")
	}
	if strings.TrimSpace(cfg.model) == "" {
		return nil, errors.New("jev: model must not be empty")
	}
	if cfg.maxRetries < 0 {
		cfg.maxRetries = 0
	}
	if cfg.retryWait < 0 {
		cfg.retryWait = 0
	}

	baseURL, err := url.Parse(cfg.baseURL)
	if err != nil {
		return nil, fmt.Errorf("jev: invalid base URL: %w", err)
	}
	if baseURL.Scheme == "" || baseURL.Host == "" {
		return nil, fmt.Errorf("jev: invalid base URL %q", cfg.baseURL)
	}
	basePath := strings.TrimRight(baseURL.Path, "/")
	if before, ok := strings.CutSuffix(basePath, endpointPath); ok {
		basePath = before
	}
	baseURL.Path = basePath
	root := baseURL.String()

	return &Client{
		apiKey:         strings.TrimSpace(cfg.apiKey),
		endpoint:       root + endpointPath,
		modelsEndpoint: root + modelsPath,
		httpClient:     cfg.httpClient,
		model:          strings.TrimSpace(cfg.model),
		maxRetries:     cfg.maxRetries,
		retryWait:      cfg.retryWait,
	}, nil
}

// Request is a fluent builder for a single System One request. It is created
// by [Client.NewRequest]. A Request is immutable: every method returns a new
// Request, so a partially built request can be reused or branched without the
// branches affecting each other, and it can be shared across goroutines.
type Request struct {
	client    *Client
	ctx       context.Context
	state     Value
	model     string
	modelSet  bool
	questions []Question
	err       error
}

// NewRequest begins a request. Add a context with [Request.WithContext], set
// the state with [Request.State], add questions with [Request.Question], and
// send it with [Request.Send]:
//
//	result, err := client.NewRequest().
//		WithContext(ctx).
//		State("Hello!").
//		Question(question).
//		Send()
func (c *Client) NewRequest() *Request {
	if c == nil {
		return &Request{err: ErrNilClient}
	}
	return &Request{client: c, ctx: context.Background()}
}

// WithContext sets the context used for cancellation and deadlines. A nil
// context is rejected and reported by [Request.Send].
func (r Request) WithContext(ctx context.Context) *Request {
	if ctx == nil {
		if r.err == nil {
			r.err = ErrNilContext
		}
		return &r
	}
	if errors.Is(r.err, ErrNilContext) {
		r.err = nil
	}
	r.ctx = ctx
	return &r
}

// State sets the content to evaluate. Pass a string for text or a [Value]
// built with [Text], [JSON], or [MustJSON]. State is required before
// [Request.Send].
func (r Request) State[S StateValue](state S) *Request {
	r.state = normalize(state)
	return &r
}

// Model overrides the model for this request. It defaults to the client's
// model. A model id, alias, or versioned id is accepted.
func (r Request) Model(model string) *Request {
	r.model = strings.TrimSpace(model)
	r.modelSet = true
	return &r
}

// Question adds one or more questions to the request. Every question must be
// created by [Noul], [Choice], or [Score] and have a unique, non-empty id.
func (r Request) Question(questions ...Question) *Request {
	if len(questions) == 0 {
		return &r
	}
	merged := make([]Question, 0, len(r.questions)+len(questions))
	merged = append(merged, r.questions...)
	merged = append(merged, questions...)
	r.questions = merged
	return &r
}

// Send sends the request and returns one answer per question. Every requested
// question is guaranteed to be present in the result, so [Result.Get] and the
// Answer methods cannot fail on a result returned here. Send retries transient
// failures and returns an [*APIError] for non-2xx responses.
func (r Request) Send() (*Result, error) {
	if r.err != nil {
		return nil, r.err
	}
	c := r.client
	if c == nil {
		return nil, ErrNilClient
	}
	if strings.TrimSpace(c.apiKey) == "" {
		return nil, ErrMissingAPIKey
	}
	if err := validateValue(r.state, "state"); err != nil {
		return nil, err
	}
	if len(r.questions) == 0 {
		return nil, errors.New("jev: at least one question is required")
	}

	specs := make(map[string]questionSpec, len(r.questions))
	byID := make(map[string]Question, len(r.questions))
	for _, question := range r.questions {
		if question == nil || isNilValue(question) {
			return nil, errors.New("jev: question must not be nil")
		}
		id := question.questionID()
		if _, duplicate := specs[id]; duplicate {
			return nil, fmt.Errorf("jev: duplicate question id %q", id)
		}
		if err := question.Validate(); err != nil {
			return nil, fmt.Errorf("jev: question %q: %w", id, err)
		}
		specs[id] = question.questionSpec()
		byID[id] = question
	}

	model := c.model
	if r.modelSet {
		if r.model == "" {
			return nil, errors.New("jev: model must not be empty")
		}
		model = r.model
	}

	payload, err := json.Marshal(systemOneRequest{
		State:     r.state,
		Model:     model,
		Questions: specs,
	})
	if err != nil {
		return nil, fmt.Errorf("jev: encode request: %w", err)
	}

	ctx := r.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	body, header, err := c.do(ctx, http.MethodPost, c.endpoint, payload)
	if err != nil {
		return nil, err
	}

	var response systemOneResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("jev: decode response: %w", err)
	}
	for id := range response.Answers {
		if _, ok := byID[id]; !ok {
			return nil, fmt.Errorf("jev: response contains an unexpected answer for question %q", id)
		}
	}

	answers := make(map[token]any, len(byID))
	for id, question := range byID {
		raw, ok := response.Answers[id]
		if !ok {
			return nil, fmt.Errorf("jev: response is missing an answer for question %q", id)
		}
		decoded, err := question.decode(raw)
		if err != nil {
			return nil, fmt.Errorf("jev: decode answer %q: %w", id, err)
		}
		answers[question.questionToken()] = decoded
	}

	usage := Usage{}
	if response.Usage != nil {
		usage = *response.Usage
		usage.Reported = true
	}
	return &Result{
		model:     response.Model,
		usage:     usage,
		requestID: header.Get("x-typesafe-request-id"),
		byToken:   answers,
	}, nil
}

// Model describes a model or alias available to the account.
type Model struct {
	// Name is the model id or alias, as accepted by [WithModel].
	Name string `json:"name"`
	// Description says what the model is for.
	Description string `json:"description"`
	// ReleaseDate is when the model or alias was released.
	ReleaseDate string `json:"release_date"`
}

// ModelsResult is the result of [Client.Models].
type ModelsResult struct {
	models    []Model
	requestID string
}

// Models returns the models and aliases available to the account.
func (r *ModelsResult) Models() []Model {
	if r == nil {
		return nil
	}
	out := make([]Model, len(r.models))
	copy(out, r.models)
	return out
}

// RequestID returns the API's request id (the x-typesafe-request-id response
// header), useful for support. It is empty if the API did not send one.
func (r *ModelsResult) RequestID() string {
	if r == nil {
		return ""
	}
	return r.requestID
}

// Models lists the models and aliases available to the account via
// GET /v1/models. Versioned model ids are accepted by [WithModel] whether or
// not they appear in the list.
func (c *Client) Models(ctx context.Context) (*ModelsResult, error) {
	if c == nil {
		return nil, ErrNilClient
	}
	if strings.TrimSpace(c.apiKey) == "" {
		return nil, ErrMissingAPIKey
	}
	if ctx == nil {
		return nil, ErrNilContext
	}
	body, header, err := c.do(ctx, http.MethodGet, c.modelsEndpoint, nil)
	if err != nil {
		return nil, err
	}
	var response struct {
		Models []Model `json:"models"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("jev: decode models: %w", err)
	}
	return &ModelsResult{
		models:    response.Models,
		requestID: header.Get("x-typesafe-request-id"),
	}, nil
}

type systemOneRequest struct {
	State     Value                   `json:"state"`
	Model     string                  `json:"model"`
	Questions map[string]questionSpec `json:"questions"`
}

type systemOneResponse struct {
	Model   string                     `json:"model"`
	Answers map[string]json.RawMessage `json:"answers"`
	Usage   *Usage                     `json:"usage"`
}

func (c *Client) do(ctx context.Context, method, endpoint string, payload []byte) ([]byte, http.Header, error) {
	idempotencyKey := newIdempotencyKey()
	for attempt := 0; ; attempt++ {
		var body io.Reader
		if payload != nil {
			body = bytes.NewReader(payload)
		}
		request, err := http.NewRequestWithContext(ctx, method, endpoint, body)
		if err != nil {
			return nil, nil, fmt.Errorf("jev: build request: %w", err)
		}
		request.Header.Set("Authorization", "Bearer "+c.apiKey)
		request.Header.Set("Accept", "application/json")
		if payload != nil {
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Idempotency-Key", idempotencyKey)
		}

		response, err := c.httpClient.Do(request)
		if err != nil {
			if !c.retryable(attempt, 0) {
				return nil, nil, fmt.Errorf("jev: request failed: %w", err)
			}
			if err := c.backoff(ctx, attempt, 0); err != nil {
				return nil, nil, err
			}
			continue
		}

		limited := &io.LimitedReader{R: response.Body, N: maxResponseBytes + 1}
		responseBody, readErr := io.ReadAll(limited)
		response.Body.Close()
		if readErr != nil {
			return nil, nil, fmt.Errorf("jev: read response: %w", readErr)
		}
		if int64(len(responseBody)) > maxResponseBytes {
			return nil, nil, fmt.Errorf("jev: response exceeds %d bytes", maxResponseBytes)
		}

		if response.StatusCode >= 200 && response.StatusCode < 300 {
			return responseBody, response.Header, nil
		}

		apiErr := newAPIError(response, responseBody)
		if !c.retryable(attempt, response.StatusCode) {
			return nil, nil, apiErr
		}
		retryAfter := parseRetryAfter(response.Header.Get("Retry-After"))
		if retryAfter == 0 {
			retryAfter = parseRetryAfterMilliseconds(response.Header.Get("Retry-After-Ms"))
		}
		if err := c.backoff(ctx, attempt, retryAfter); err != nil {
			return nil, nil, err
		}
	}
}

func (c *Client) retryable(attempt, statusCode int) bool {
	if attempt >= c.maxRetries {
		return false
	}
	switch {
	case statusCode == 0:
		return true
	case statusCode == http.StatusTooManyRequests, statusCode == 529:
		return true
	case statusCode >= 500 && statusCode <= 599:
		return true
	default:
		return false
	}
}

func (c *Client) backoff(ctx context.Context, attempt int, retryAfter time.Duration) error {
	var delay time.Duration
	if c.retryWait > 0 {
		delay = c.retryWait
		for i := 0; i < attempt && delay < maxBackoff; i++ {
			delay *= 2
		}
		if delay > maxBackoff {
			delay = maxBackoff
		}
	}
	limit := maxBackoff
	if retryAfter > 0 {
		delay = retryAfter
		limit = maxRetryAfter
	}
	if delay > 0 {
		delay += time.Duration(mrand.Int64N(int64(delay)/2 + 1))
		if delay > limit {
			delay = limit
		}
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// newIdempotencyKey returns a fresh key for one logical request. The key is
// reused across retries of the same request so a retried request is not
// evaluated twice by the server.
func newIdempotencyKey() string {
	var buf [16]byte
	if _, err := cryptorand.Read(buf[:]); err != nil {
		return "jev-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	return hex.EncodeToString(buf[:])
}

func parseRetryAfter(value string) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	seconds, err := strconv.ParseInt(value, 10, 64)
	switch {
	case err == nil:
		if seconds <= 0 {
			return 0
		}
		if seconds > int64(maxRetryAfter/time.Second) {
			return maxRetryAfter
		}
		return time.Duration(seconds) * time.Second
	case errors.Is(err, strconv.ErrRange):
		return maxRetryAfter
	}
	if when, err := http.ParseTime(value); err == nil {
		if delay := time.Until(when); delay > 0 {
			if delay > maxRetryAfter {
				return maxRetryAfter
			}
			return delay
		}
	}
	return 0
}

// parseRetryAfterMilliseconds reads the retry-after-ms header used by some SDKs.
func parseRetryAfterMilliseconds(value string) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	millis, err := strconv.ParseInt(value, 10, 64)
	if err != nil || millis <= 0 {
		return 0
	}
	if millis > int64(maxRetryAfter/time.Millisecond) {
		return maxRetryAfter
	}
	return time.Duration(millis) * time.Millisecond
}
