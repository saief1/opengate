package provider

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// UpstreamError describes a failed upstream attempt.
type UpstreamError struct {
	Provider   string
	Status     int // 0 for network errors/timeouts
	Message    string
	RetryAfter time.Duration
	Err        error
}

func (e *UpstreamError) Error() string {
	if e.Status == 0 {
		return fmt.Sprintf("%s: %s", e.Provider, e.Message)
	}
	return fmt.Sprintf("%s: HTTP %d: %s", e.Provider, e.Status, e.Message)
}

func (e *UpstreamError) Unwrap() error { return e.Err }

// Retryable reports whether the same target may be retried (429, 5xx,
// network errors and timeouts).
func (e *UpstreamError) Retryable() bool {
	return e.Status == 0 || e.Status == http.StatusTooManyRequests || e.Status == http.StatusRequestTimeout || e.Status >= 500
}

// Failover reports whether the next target should be tried. In addition to
// retryable errors this covers auth/not-found problems that are specific to
// one provider (bad key, model missing there).
func (e *UpstreamError) Failover() bool {
	return e.Retryable() || e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden || e.Status == http.StatusNotFound
}

// errorFromResponse reads a non-2xx response into an UpstreamError.
func errorFromResponse(provider string, resp *http.Response, body []byte) *UpstreamError {
	e := &UpstreamError{Provider: provider, Status: resp.StatusCode, Message: extractMessage(body)}
	if e.Message == "" {
		e.Message = http.StatusText(resp.StatusCode)
	}
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if secs, err := strconv.Atoi(ra); err == nil {
			e.RetryAfter = time.Duration(secs) * time.Second
		}
	}
	return e
}

// extractMessage pulls a human-readable message out of the common provider
// error shapes ({"error":{"message":..}}, {"error":"..."}, {"message":..}).
func extractMessage(body []byte) string {
	var v struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
	}
	if json.Unmarshal(body, &v) == nil {
		var inner struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(v.Error, &inner) == nil && inner.Message != "" {
			return inner.Message
		}
		var s string
		if json.Unmarshal(v.Error, &s) == nil && s != "" {
			return s
		}
		if v.Message != "" {
			return v.Message
		}
	}
	if len(body) > 300 {
		body = body[:300]
	}
	return string(body)
}
