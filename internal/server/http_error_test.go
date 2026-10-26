package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/saief1/opengate/internal/budget"
)

func TestHTTPErrorMetricAndDebouncedAlert(t *testing.T) {
	var mu sync.Mutex
	var got []map[string]any
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		mu.Lock()
		got = append(got, m)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(hook.Close)

	s := &Server{
		metrics:      newGatewayMetrics(),
		alerter:      budget.NewAlerter(hook.URL, nil, slog.New(slog.NewTextHandler(io.Discard, nil))),
		httpErrAlert: newIPLimiter(1, time.Minute),
	}
	r := httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil)
	r.Header.Set("X-OpenGate-Request-Id", "req_test")
	s.noteHTTP(r, 500)
	s.noteHTTP(r, 500)
	s.noteHTTP(httptest.NewRequest(http.MethodPost, "/signup", nil), 400)
	s.noteHTTP(httptest.NewRequest(http.MethodGet, "/healthz", nil), 200)

	if v := s.metrics.httpErrors.Value("5xx", "/v1"); v != 2 {
		t.Fatalf("5xx /v1 = %v", v)
	}
	if v := s.metrics.httpErrors.Value("4xx", "/account"); v != 1 {
		t.Fatalf("4xx /account = %v", v)
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n >= 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("webhook deliveries: %d, want 1 (5xx debounced, 4xx silent)", len(got))
	}
	ev := got[0]
	if ev["kind"] != "http_error" || ev["path"] != "/v1/chat/completions" || ev["method"] != "GET" || ev["request_id"] != "req_test" || ev["status"] != float64(500) {
		t.Fatalf("event: %v", ev)
	}
}
