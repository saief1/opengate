package integration

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/saief1/opengate/internal/config"
	"github.com/saief1/opengate/internal/pricing"
	"github.com/saief1/opengate/internal/server"
	"github.com/saief1/opengate/web"
)

// setupDemo starts a second gateway on the same database with demo mode on
// (no traffic generator) and the seeded demo projects in place.
func setupDemo(t *testing.T) (*env, *httptest.Server, map[string]string) {
	t.Helper()
	e := setupWith(t, func(c *config.Config) {
		c.SmartRouting.Auto = config.AutoConfig{Enabled: true, Models: []config.AutoModel{
			{Model: "openai/gpt-4o-mini", Tier: 1}, {Model: "anthropic/claude-sonnet-4-5", Tier: 3}}}
		c.MasterKey = "test-master-key"
	})
	projects := map[string]string{}
	for _, n := range []string{"research-agents", "support-bot"} {
		projects[n] = e.admin("POST", "/admin/api/projects", map[string]any{"name": n, "log_bodies": true}, 201)["id"].(string)
	}
	e.admin("PUT", "/admin/api/projects/"+projects["support-bot"]+"/guardrails", map[string]any{"pii": "redact", "injection": "block"}, 200)
	prices, _ := pricing.NewRegistry("")
	srv, err := server.New(context.Background(), server.Options{Config: e.cfg, Store: e.st, Cipher: e.cipher, Pricing: prices,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), WebFS: web.FS(), Demo: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go srv.Run(ctx)
	gw := httptest.NewServer(srv.Handler())
	t.Cleanup(gw.Close)
	// wait for the demo keys
	for i := 0; ; i++ {
		req, _ := http.NewRequest("GET", gw.URL+"/admin/api/plan", nil)
		req.Header.Set("Authorization", "Bearer "+adminToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if m := decodeJSON(t, resp); m["demo"] == true {
			break
		}
		if i > 100 {
			t.Fatal("demo keys never became ready")
		}
		time.Sleep(50 * time.Millisecond)
	}
	return e, gw, projects
}

func TestPublicEndpointsOffOutsideDemo(t *testing.T) {
	e := setup(t)
	for _, p := range []string{"/public/live"} {
		resp, _ := http.Get(e.gw.URL + p)
		resp.Body.Close()
		if resp.StatusCode == 200 {
			t.Fatalf("%s must not be served outside demo mode", p)
		}
	}
	resp, _ := http.Post(e.gw.URL+"/public/playground", "application/json", strings.NewReader(`{"preset":"simple"}`))
	resp.Body.Close()
	if resp.StatusCode == 200 {
		t.Fatal("playground must not be served outside demo mode")
	}
	// Try it is demo-only too.
	e.admin("POST", "/admin/api/demo/try", map[string]any{}, 404)
}

func TestStreamRequiresAuth(t *testing.T) {
	e := setup(t)
	for _, auth := range []string{"", "Bearer wrong-token-0000000000"} {
		req, _ := http.NewRequest("GET", e.gw.URL+"/admin/api/stream", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Fatalf("auth %q: status %d, want 401", auth, resp.StatusCode)
		}
		if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
			t.Fatal("no stream before auth")
		}
	}
}

type sseEvent struct{ typ, data string }

func readEvents(t *testing.T, body io.Reader) <-chan sseEvent {
	ch := make(chan sseEvent, 64)
	go func() {
		defer close(ch)
		sc := bufio.NewScanner(body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		var ev sseEvent
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				ev.typ = line[7:]
			case strings.HasPrefix(line, "data: "):
				ev.data = line[6:]
			case line == "" && ev.typ != "":
				ch <- ev
				ev = sseEvent{}
			}
		}
	}()
	return ch
}

func waitEvent(t *testing.T, ch <-chan sseEvent, typ string, match func(string) bool) string {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatalf("stream closed waiting for %s", typ)
			}
			if ev.typ == typ && (match == nil || match(ev.data)) {
				return ev.data
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s event", typ)
		}
	}
}

func TestStreamEmitsEvents(t *testing.T) {
	e := setup(t)
	key, keyID, _ := e.newKey("stream-key", "streamer", nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", e.gw.URL+"/admin/api/stream", nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("stream: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	ch := readEvents(t, resp.Body)
	waitEvent(t, ch, "hello", nil)
	waitEvent(t, ch, "tick", func(d string) bool { return strings.Contains(d, `"today"`) })

	r := e.chat(key, map[string]any{"model": "openai/gpt-4o-mini", "messages": msgs("stream me SECRET-BODY-123")})
	decodeJSON(t, r)
	d := waitEvent(t, ch, "request", func(d string) bool { return strings.Contains(d, keyID) })
	var row map[string]any
	if err := json.Unmarshal([]byte(d), &row); err != nil {
		t.Fatal(err)
	}
	if row["model"] != "openai/gpt-4o-mini" || row["agent"] != "streamer" {
		t.Fatalf("request event: %v", row)
	}
	if strings.Contains(d, "SECRET-BODY-123") {
		t.Fatal("stream must not carry request bodies")
	}
	e.admin("POST", "/admin/api/keys/"+keyID+"/kill", nil, 200)
	waitEvent(t, ch, "kill", func(d string) bool { return strings.Contains(d, keyID) })
}

func TestDemoPublicLiveLeaksNothing(t *testing.T) {
	e, gw, projects := setupDemo(t)
	key, keyID, _ := e.newKey("customer-key", "secret-agent-name", nil)
	e.admin("PATCH", "/admin/api/projects/"+projects["support-bot"], map[string]any{"trust_customer_header": true}, 200)
	const secretPrompt = "TOP-SECRET-PROMPT jane.doe@example.com sk-live-abcdef0123456789"
	r := e.chatH(key, map[string]any{"model": "openai/gpt-4o-mini", "messages": msgs(secretPrompt)}, map[string]string{"X-OpenGate-Customer": "acme-secret-customer"})
	decodeJSON(t, r)
	// Dashboard "Try it" goes through a demo key.
	try := map[string]any{}
	{
		req, _ := http.NewRequest("POST", gw.URL+"/admin/api/demo/try", strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer "+adminToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		try = decodeJSON(t, resp)
		if resp.StatusCode != 200 || try["status"].(float64) != 200 || try["request_id"] == "" {
			t.Fatalf("try it: %d %v", resp.StatusCode, try)
		}
	}

	resp, err := http.Get(gw.URL + "/public/live")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("public live: %d %s", resp.StatusCode, b)
	}
	var live struct {
		Counters map[string]float64 `json:"counters"`
		Recent   []map[string]any   `json:"recent"`
	}
	if err := json.Unmarshal(b, &live); err != nil {
		t.Fatal(err)
	}
	if live.Counters["requests"] < 2 || len(live.Recent) < 2 {
		t.Fatalf("public live data: %s", b)
	}
	body := string(b)
	for _, bad := range []string{"TOP-SECRET", "jane.doe", "sk-live", "og-", "secret-agent-name", "acme-secret-customer", keyID, "key_", "prj_", "org_", "req_",
		try["request_id"].(string), projects["support-bot"], "planner", "error_message", adminToken} {
		if bad != "" && strings.Contains(body, bad) {
			t.Fatalf("/public/live leaks %q: %s", bad, body)
		}
	}
	allowed := map[string]bool{"ts": true, "model": true, "routed_to": true, "route": true, "latency_ms": true, "cost_usd": true, "saved_usd": true, "status": true, "badges": true}
	for _, row := range live.Recent {
		for k := range row {
			if !allowed[k] {
				t.Fatalf("unexpected public field %q", k)
			}
		}
	}

	play := func(body string) (int, map[string]any) {
		resp, err := http.Post(gw.URL+"/public/playground", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, decodeJSON(t, resp)
	}
	code, m := play(`{"preset":"injection"}`)
	if code != 200 || m["blocked"] != true || !strings.Contains(m["block_reason"].(string), "injection") {
		t.Fatalf("injection: %d %v", code, m)
	}
	code, m = play(`{"preset":"pii"}`)
	if code != 200 || m["redacted"] != true || m["status"].(float64) != 200 || m["model"] == "" {
		t.Fatalf("pii: %d %v", code, m)
	}
	if _, ok := m["request_id"]; ok {
		t.Fatal("playground must not return request ids")
	}
	code, m = play(`{"prompt":"Translate thank you to Spanish"}`)
	if code != 200 || m["requested_model"] != "auto" || m["model"] != "openai/gpt-4o-mini" {
		t.Fatalf("auto: %d %v", code, m)
	}
	if code, _ := play(`{"prompt":""}`); code != 400 {
		t.Fatalf("empty prompt: %d", code)
	}
	if r, _ := http.Post(gw.URL+"/public/playground", "text/plain", bytes.NewReader([]byte(`{"preset":"simple"}`))); r.StatusCode != 415 {
		t.Fatalf("non-JSON playground: %d", r.StatusCode)
	}
	// Per-IP rate limit.
	limited := false
	for i := 0; i < 12 && !limited; i++ {
		c, _ := play(`{"preset":"simple"}`)
		limited = c == 429
	}
	if !limited {
		t.Fatal("playground is not rate limited")
	}
	limited = false
	for i := 0; i < 70 && !limited; i++ {
		r, _ := http.Get(gw.URL + "/public/live")
		r.Body.Close()
		limited = r.StatusCode == 429
	}
	if !limited {
		t.Fatal("/public/live is not rate limited")
	}
}

func TestPruneKeyRowsCaps(t *testing.T) {
	e := setup(t)
	key, keyID, _ := e.newKey("prune", "pruner", nil)
	for i := 0; i < 5; i++ {
		decodeJSON(t, e.chat(key, map[string]any{"model": "openai/gpt-4o-mini", "messages": msgs("hi")}))
		time.Sleep(2 * time.Millisecond)
	}
	n, err := e.st.PruneKeyRows(context.Background(), []string{keyID}, time.Now().Add(-time.Hour), 2)
	if err != nil || n != 3 {
		t.Fatalf("pruned %d, err %v", n, err)
	}
	out := e.admin("GET", "/admin/api/logs?key_id="+keyID, nil, 200)
	if out["total"].(float64) != 2 {
		t.Fatalf("kept %v rows", out["total"])
	}
}
