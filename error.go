package jev

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// ErrMissingAPIKey is returned by [NewClient] when no API key is configured.
var ErrMissingAPIKey = errors.New("jev: API key is required")

// ErrNilClient is returned when a request is built from a nil [*Client]. Use
// [NewClient] to obtain a client.
var ErrNilClient = errors.New("jev: client is nil; use jev.NewClient")

// ErrNilContext is returned by [Request.Send] when a nil context was passed to
// WithContext.
var ErrNilContext = errors.New("jev: context must not be nil")

// APIError is returned when the API responds with a non-2xx status code.
type APIError struct {
	// StatusCode is the HTTP status code.
	StatusCode int
	// Status is the HTTP status line, for example "422 Unprocessable Entity".
	Status string
	// Message is a best-effort message parsed from the response body.
	Message string
	// Body is the raw response body.
	Body []byte
	// RequestID is the API's request id (the x-typesafe-request-id response
	// header), useful for support. It is empty if the API did not send one.
	RequestID string
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("jev: API error %d %s: %s", e.StatusCode, e.Status, e.Message)
	}
	return fmt.Sprintf("jev: API error %d %s", e.StatusCode, e.Status)
}

// Unauthorized reports whether the request failed authentication (401).
func (e *APIError) Unauthorized() bool {
	return e.StatusCode == http.StatusUnauthorized
}

// Unprocessable reports whether the request failed validation (422).
func (e *APIError) Unprocessable() bool {
	return e.StatusCode == http.StatusUnprocessableEntity
}

// RateLimited reports whether the request exceeded the rate limit (429).
func (e *APIError) RateLimited() bool {
	return e.StatusCode == http.StatusTooManyRequests
}

// Overloaded reports whether the API is temporarily overloaded (529).
func (e *APIError) Overloaded() bool {
	return e.StatusCode == 529
}

func newAPIError(response *http.Response, body []byte) *APIError {
	apiErr := &APIError{
		StatusCode: response.StatusCode,
		Status:     response.Status,
		Body:       body,
		RequestID:  response.Header.Get("x-typesafe-request-id"),
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return apiErr
	}
	if message := decodeMessage(fields["message"]); message != "" {
		apiErr.Message = message
		return apiErr
	}
	apiErr.Message = decodeMessage(fields["error"])
	return apiErr
}

// decodeMessage extracts a message from a string, or from an object with a
// "message" field, tolerating either shape.
func decodeMessage(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	var nested struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &nested); err == nil {
		return nested.Message
	}
	return ""
}
