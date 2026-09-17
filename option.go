package jev

import (
	"net/http"
	"time"
)

// Option configures a [Client] created by [NewClient].
type Option func(*config)

type config struct {
	apiKey     string
	baseURL    string
	httpClient *http.Client
	model      string
	maxRetries int
	retryWait  time.Duration
}

func defaultConfig() config {
	return config{
		baseURL:    DefaultBaseURL,
		httpClient: &http.Client{Timeout: 60 * time.Second},
		model:      ModelLatest,
		maxRetries: 3,
		retryWait:  500 * time.Millisecond,
	}
}

// WithAPIKey sets the bearer token used to authenticate requests. It is
// required.
func WithAPIKey(apiKey string) Option {
	return func(c *config) {
		c.apiKey = apiKey
	}
}

// WithBaseURL overrides the API base URL. The default is [DefaultBaseURL].
func WithBaseURL(baseURL string) Option {
	return func(c *config) {
		c.baseURL = baseURL
	}
}

// WithHTTPClient sets the HTTP client used for requests. The default client
// has a 60 second timeout.
func WithHTTPClient(httpClient *http.Client) Option {
	return func(c *config) {
		c.httpClient = httpClient
	}
}

// WithModel sets the model used for evaluations. The default is
// [ModelLatest].
func WithModel(model string) Option {
	return func(c *config) {
		c.model = model
	}
}

// WithMaxRetries sets how many times a request is retried after a network
// error or a retryable status (429, 529, or 5xx). The default is 3.
func WithMaxRetries(maxRetries int) Option {
	return func(c *config) {
		c.maxRetries = maxRetries
	}
}

// WithRetryWait sets the base delay for exponential backoff between retries.
// The default is 500 milliseconds.
func WithRetryWait(retryWait time.Duration) Option {
	return func(c *config) {
		c.retryWait = retryWait
	}
}
