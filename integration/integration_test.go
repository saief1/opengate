// Package integration runs OpenGate end-to-end against a mock provider HTTP
// server: no real API keys or network access are needed.
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
	"sync"
	"testing"
	"time"

	"github.com/saief1/opengate/internal/config"
	"github.com/saief1/opengate/internal/mailer"
	"github.com/saief1/opengate/internal/mockprovider"
	"github.com/saief1/opengate/internal/pricing"
	"github.com/saief1/opengate/internal/secure"
	"github.com/saief1/opengate/internal/server"
	"github.com/saief1/opengate/internal/store"
	"github.com/saief1/opengate/internal/storetest"
	"github.com/saief1/opengate/web"
)

const adminToken = "test-admin-token-0123456789"

type env struct {
	t        *testing.T
	gw       *httptest.Server
	mock     *mockprovider.Mock
	st       *store.Store
	cfg      *config.Config
	cipher   *secure.Cipher
	srv      *server.Server
	mu       sync.Mutex
	webhooks []map[string]any
	mail     mailer.Sender
}

func setup(t *testing.T) *env { return setupWith(t, nil) }

// setupWith starts a gateway (SQLite, or PostgreSQL when
// OPENGATE_TEST_POSTGRES_DSN is set) in front of the mock provider.
func setupWith(t *testing.T, mutate func(*config.Config)) *env {
	t.Helper()
	e := &env{t: t}
	e.mock = mockprovider.New(mockprovider.Options{ChunkDelay: time.Millisecond})
	up := httptest.NewServer(e.mock)
	t.Cleanup(up.Close)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		json.NewDecoder(r.Body).Decode(&m)
		e.mu.Lock()
		e.webhooks = append(e.webhooks, m)
		e.mu.Unlock()
	}))
	t.Cleanup(hook.Close)

	cfg := config.Default()
	cfg.AdminToken = adminToken
	cfg.Providers = map[string]config.ProviderConfig{
		"openai":     {Type: config.TypeOpenAI, BaseURL: up.URL + "/openai/v1"},
		"anthropic":  {Type: config.TypeAnthropic, BaseURL: up.URL + "/anthropic"},
		"gemini":     {Type: config.TypeGemini, BaseURL: up.URL + "/gemini"},
		"openrouter": {Type: config.TypeOpenRouter, BaseURL: up.URL + "/openrouter/v1"},
	}
	cfg.Routes = map[string][]string{
		"smart":    {"openai/gpt-fail", "anthropic/claude-sonnet-4-5"},
		"all-down": {"openai/gpt-fail", "gemini/gemini-fail"},
	}
	cfg.Retry = config.RetryConfig{MaxAttempts: 2, InitialBackoff: time.Millisecond, MaxBackoff: 10 * time.Millisecond}
	cfg.Alerts.WebhookURL = hook.URL
	cfg.Database = storetest.DSN(t)
	if mutate != nil {
		mutate(cfg)
	}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.Database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e.st, e.cfg = st, cfg
	mk, _ := secure.GenerateMasterKey()
	kb, _ := secure.ParseMasterKey(mk)
	e.cipher, _ = secure.NewCipher(kb)
	var gw http.Handler
	e.srv, gw = e.newGateway()
	e.gw = httptest.NewServer(gw)
	t.Cleanup(e.gw.Close)
	// Bring-your-own provider keys, stored encrypted through the admin API.
	for _, p := range []string{"openai", "anthropic", "gemini", "openrouter"} {
		e.admin("POST", "/admin/api/provider-keys", map[string]any{"provider": p, "key": "sk-test-" + p + "-123456"}, 201)
	}
	return e
}

// newGateway builds another server instance on the same database (a
// restart or a second replica).
func (e *env) newGateway() (*server.Server, http.Handler) {
	e.t.Helper()
	prices, _ := pricing.NewRegistry("")
	mail := e.mail
	if mail == nil {
		mail = &mailer.Log{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	}
	srv, err := server.New(context.Background(), server.Options{Config: e.cfg, Store: e.st, Cipher: e.cipher, Pricing: prices,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), WebFS: web.FS(), Mailer: mail})
	if err != nil {
		e.t.Fatal(err)
	}
	return srv, srv.Handler()
}

// post sends a JSON request to a gateway endpoint with a virtual key.
func (e *env) post(path, key string, body any) *http.Response {
	e.t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", e.gw.URL+path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	return resp
}

func (e *env) admin(method, path string, body any, want int) map[string]any {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.gw.URL+path, rd)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != want {
		e.t.Fatalf("%s %s: status %d, want %d: %v", method, path, resp.StatusCode, want, out)
	}
	return out
}

// newKey creates a virtual key through the admin API.
func (e *env) newKey(name, agent string, limits map[string]any) (key, id, project string) {
	e.t.Helper()
	body := map[string]any{"name": name, "agent": agent}
	if limits != nil {
		body["limits"] = limits
	}
	out := e.admin("POST", "/admin/api/keys", body, 201)
	vk := out["virtual_key"].(map[string]any)
	return out["key"].(string), vk["id"].(string), vk["project_id"].(string)
}

func (e *env) chat(key string, body map[string]any) *http.Response {
	e.t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", e.gw.URL+"/v1/chat/completions", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	return resp
}

func msgs(text string) []map[string]any {
	return []map[string]any{{"role": "system", "content": "be brief"}, {"role": "user", "content": text}}
}

func decodeJSON(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer resp.Body.Close()
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatal(err)
	}
	return m
}

func errCode(m map[string]any) string {
	if e, ok := m["error"].(map[string]any); ok {
		c, _ := e["code"].(string)
		return c
	}
	return ""
}

// readSSE collects chunks from an SSE response.
func readSSE(t *testing.T, resp *http.Response) (content string, chunks []map[string]any, done bool) {
	t.Helper()
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type %q", ct)
	}
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		if data == "[DONE]" {
			done = true
			continue
		}
		var c map[string]any
		if err := json.Unmarshal([]byte(data), &c); err != nil {
			t.Fatalf("bad chunk %q", data)
		}
		chunks = append(chunks, c)
		if ch, ok := c["choices"].([]any); ok && len(ch) > 0 {
			if d, ok := ch[0].(map[string]any)["delta"].(map[string]any); ok {
				if s, ok := d["content"].(string); ok {
					content += s
				}
			}
		}
	}
	return
}

func TestAuthAndDashboard(t *testing.T) {
	e := setup(t)
	resp := e.chat("", map[string]any{"model": "openai/gpt-4o-mini", "messages": msgs("hi")})
	if resp.StatusCode != 401 || errCode(decodeJSON(t, resp)) != "missing_api_key" {
		t.Fatal("missing key must be 401 missing_api_key")
	}
	resp = e.chat("og-"+strings.Repeat("0", 48), map[string]any{"model": "openai/gpt-4o-mini", "messages": msgs("hi")})
	if resp.StatusCode != 401 || errCode(decodeJSON(t, resp)) != "invalid_api_key" {
		t.Fatal("unknown key must be 401")
	}
	r, _ := http.Get(e.gw.URL + "/admin/api/keys")
	if r.StatusCode != 401 {
		t.Fatal("admin API must require token")
	}
	r, _ = http.Get(e.gw.URL + "/ui/")
	b, _ := io.ReadAll(r.Body)
	if r.StatusCode != 200 || !strings.Contains(string(b), "OpenGate") || !strings.Contains(r.Header.Get("Content-Security-Policy"), "default-src 'self'") {
		t.Fatalf("dashboard: %d %s", r.StatusCode, r.Header)
	}
	for asset, ctype := range map[string]string{
		"/ui/app.js": "text/javascript", "/ui/style.css": "text/css", "/ui/favicon.ico": "image/x-icon", "/favicon.ico": "image/x-icon",
		"/ui/favicon.svg": "image/svg+xml", "/ui/favicon-transparent.svg": "image/svg+xml", "/ui/apple-touch-icon.png": "image/png",
		"/ui/android-chrome-192.png": "image/png", "/ui/android-chrome-512.png": "image/png",
		"/ui/site.webmanifest": "application/manifest+json", "/ui/brand/opengate-lockup-dark.svg": "image/svg+xml",
	} {
		r, err := http.Get(e.gw.URL + asset)
		if err != nil || r.StatusCode != 200 || !strings.HasPrefix(r.Header.Get("Content-Type"), ctype) {
			t.Fatalf("asset %s: %v %d %q (want %s)", asset, err, r.StatusCode, r.Header.Get("Content-Type"), ctype)
		}
		r.Body.Close()
	}
	key, _, _ := e.newKey("models", "", nil)
	req, _ := http.NewRequest("GET", e.gw.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	mr, _ := http.DefaultClient.Do(req)
	m := decodeJSON(t, mr)
	ids := ""
	for _, d := range m["data"].([]any) {
		ids += d.(map[string]any)["id"].(string) + " "
	}
	if !strings.Contains(ids, "smart") || !strings.Contains(ids, "openai/gpt-4o-mini") {
		t.Fatalf("models: %s", ids)
	}
}

func TestNonStreamingAllProviders(t *testing.T) {
	e := setup(t)
	key, keyID, _ := e.newKey("bot", "planner", nil)
	for _, model := range []string{"openai/gpt-4o-mini", "anthropic/claude-sonnet-4-5", "gemini/gemini-2.5-flash", "openrouter/meta-llama/llama-3.3-70b-instruct"} {
		resp := e.chat(key, map[string]any{"model": model, "messages": msgs("ping " + model)})
		if resp.StatusCode != 200 {
			t.Fatalf("%s: status %d %v", model, resp.StatusCode, decodeJSON(t, resp))
		}
		prov := strings.SplitN(model, "/", 2)[0]
		if resp.Header.Get("X-OpenGate-Provider") != prov || resp.Header.Get("X-OpenGate-Cost-USD") == "" {
			t.Fatalf("%s: headers %v", model, resp.Header)
		}
		m := decodeJSON(t, resp)
		if m["object"] != "chat.completion" {
			t.Fatalf("%s: object %v", model, m["object"])
		}
		content := m["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"].(string)
		if !strings.Contains(content, "mock "+prov) || !strings.Contains(content, "ping") {
			t.Fatalf("%s: content %q", model, content)
		}
		u := m["usage"].(map[string]any)
		if u["prompt_tokens"].(float64) <= 0 || u["completion_tokens"].(float64) <= 0 {
			t.Fatalf("%s: usage %v", model, u)
		}
	}
	logs, _, _ := e.st.ListLogs(context.Background(), store.LogFilter{KeyID: keyID})
	if len(logs) != 4 {
		t.Fatalf("want 4 logs, got %d", len(logs))
	}
	for _, l := range logs {
		if l.Status != 200 || l.CostUSD <= 0 || l.PromptTokens == 0 || l.UsageEstimated || l.Agent != "planner" || l.RequestBody != "" {
			t.Fatalf("log: %+v", l)
		}
	}
}

func TestStreamingAllProviders(t *testing.T) {
	e := setup(t)
	key, keyID, _ := e.newKey("streamer", "", nil)
	for _, model := range []string{"openai/gpt-4o-mini", "anthropic/claude-haiku-4-5", "gemini/gemini-2.5-flash"} {
		for _, includeUsage := range []bool{false, true} {
			body := map[string]any{"model": model, "stream": true, "messages": msgs("stream please")}
			if includeUsage {
				body["stream_options"] = map[string]any{"include_usage": true}
			}
			resp := e.chat(key, body)
			if resp.StatusCode != 200 {
				t.Fatalf("%s: %d", model, resp.StatusCode)
			}
			content, chunks, done := readSSE(t, resp)
			if !done || len(chunks) < 5 || !strings.Contains(content, "Hello from the OpenGate mock") {
				t.Fatalf("%s: done=%v chunks=%d content=%q", model, done, len(chunks), content)
			}
			sawUsage, sawFinish := false, false
			for _, c := range chunks {
				if c["object"] != "chat.completion.chunk" {
					t.Fatalf("%s: chunk object %v", model, c["object"])
				}
				if _, ok := c["usage"].(map[string]any); ok {
					sawUsage = true
				}
				if ch, _ := c["choices"].([]any); len(ch) > 0 {
					if fr, _ := ch[0].(map[string]any)["finish_reason"].(string); fr == "stop" {
						sawFinish = true
					}
				}
			}
			if sawUsage != includeUsage || !sawFinish {
				t.Fatalf("%s include_usage=%v: sawUsage=%v sawFinish=%v", model, includeUsage, sawUsage, sawFinish)
			}
		}
	}
	logs, _, _ := e.st.ListLogs(context.Background(), store.LogFilter{KeyID: keyID})
	for _, l := range logs {
		if !l.Stream || l.Status != 200 || l.CompletionTokens == 0 || l.CostUSD <= 0 || l.UsageEstimated {
			t.Fatalf("stream log: %+v", l)
		}
	}
}

func TestFailoverAndRetries(t *testing.T) {
	e := setup(t)
	key, _, _ := e.newKey("fo", "", nil)

	// Alias "smart": openai/gpt-fail (500) is retried, then fails over to anthropic.
	resp := e.chat(key, map[string]any{"model": "smart", "messages": msgs("failover")})
	if resp.StatusCode != 200 || resp.Header.Get("X-OpenGate-Provider") != "anthropic" || resp.Header.Get("X-OpenGate-Attempts") != "3" {
		t.Fatalf("failover: %d %v", resp.StatusCode, resp.Header)
	}
	resp.Body.Close()
	if h := e.mock.Hits("openai", "gpt-fail"); h != 2 {
		t.Fatalf("expected 2 attempts on failing target, got %d", h)
	}

	// Streaming failover too.
	resp = e.chat(key, map[string]any{"model": "smart", "stream": true, "messages": msgs("failover stream")})
	content, _, done := readSSE(t, resp)
	if !done || !strings.Contains(content, "mock anthropic") {
		t.Fatalf("stream failover: %q", content)
	}

	// Flaky upstream: first attempt 503, retry succeeds on the same provider.
	resp = e.chat(key, map[string]any{"model": "openai/gpt-flaky", "messages": msgs("retry")})
	if resp.StatusCode != 200 || resp.Header.Get("X-OpenGate-Attempts") != "2" || resp.Header.Get("X-OpenGate-Provider") != "openai" {
		t.Fatalf("retry: %d %v", resp.StatusCode, resp.Header)
	}
	resp.Body.Close()

	// 429 from upstream is retried.
	resp = e.chat(key, map[string]any{"model": "openai/gpt-ratelimit", "messages": msgs("x")})
	m := decodeJSON(t, resp)
	if resp.StatusCode != 429 || errCode(m) != "upstream_error" || e.mock.Hits("openai", "gpt-ratelimit") != 2 {
		t.Fatalf("429 retry: %d %v hits=%d", resp.StatusCode, m, e.mock.Hits("openai", "gpt-ratelimit"))
	}

	// Everything down: clean OpenAI-format 502.
	resp = e.chat(key, map[string]any{"model": "all-down", "messages": msgs("x")})
	m = decodeJSON(t, resp)
	if resp.StatusCode != 502 || errCode(m) != "upstream_error" {
		t.Fatalf("all down: %d %v", resp.StatusCode, m)
	}

	// Unknown provider: 404 model_not_found.
	resp = e.chat(key, map[string]any{"model": "nope/model", "messages": msgs("x")})
	if resp.StatusCode != 404 || errCode(decodeJSON(t, resp)) != "model_not_found" {
		t.Fatal("unknown provider should be 404 model_not_found")
	}
}

func (e *env) waitWebhooks(n int) []map[string]any {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		e.mu.Lock()
		got := append([]map[string]any(nil), e.webhooks...)
		e.mu.Unlock()
		if len(got) >= n {
			return got
		}
		time.Sleep(20 * time.Millisecond)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.webhooks
}

func TestBudgetCutoffAndAlerts(t *testing.T) {
	e := setup(t)
	// gpt-4o: $2.50/$10 per 1M tokens; one mock request costs ~ $0.0002.
	key, keyID, _ := e.newKey("capped", "looping-agent", map[string]any{"daily_usd": 0.0003})
	var spent int
	for i := 0; i < 10; i++ {
		resp := e.chat(key, map[string]any{"model": "openai/gpt-4o", "messages": msgs("loop forever")})
		if resp.StatusCode == 200 {
			spent++
			resp.Body.Close()
			continue
		}
		m := decodeJSON(t, resp)
		if resp.StatusCode != 429 || errCode(m) != "budget_exceeded" || resp.Header.Get("Retry-After") == "" {
			t.Fatalf("expected budget_exceeded, got %d %v", resp.StatusCode, m)
		}
		if typ := m["error"].(map[string]any)["type"]; typ != "insufficient_quota" {
			t.Fatalf("error type %v", typ)
		}
		break
	}
	if spent < 1 || spent > 3 {
		t.Fatalf("expected cutoff after 1-3 requests, got %d", spent)
	}
	// Streaming requests are blocked too.
	resp := e.chat(key, map[string]any{"model": "openai/gpt-4o", "stream": true, "messages": msgs("x")})
	if resp.StatusCode != 429 {
		t.Fatalf("stream should be blocked, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	// The rejected request was logged as blocked with zero cost.
	logs, _, _ := e.st.ListLogs(context.Background(), store.LogFilter{KeyID: keyID, ErrorsOnly: true})
	if len(logs) == 0 || logs[0].ErrorCode != "budget_exceeded" || logs[0].CostUSD != 0 {
		t.Fatalf("blocked log: %+v", logs)
	}
	// Slack-compatible webhooks fired for 80% and 100%.
	hooks := e.waitWebhooks(2)
	var ths []float64
	for _, h := range hooks {
		if h["kind"] == "budget_threshold" {
			if !strings.Contains(h["text"].(string), "capped") {
				t.Fatalf("webhook text: %v", h["text"])
			}
			ths = append(ths, h["threshold"].(float64))
		}
	}
	if len(ths) < 1 || ths[len(ths)-1] != 1.0 {
		t.Fatalf("expected threshold alerts ending at 100%%, got %v", ths)
	}
	// Agent-level budget: a second key with the same agent label shares the agent budget.
	_, _, proj := e.newKey("other", "shared-agent", nil)
	e.admin("PUT", "/admin/api/limits", map[string]any{"scope": "agent", "scope_id": proj + "/shared-agent", "total_usd": 0.0001}, 200)
	k2, _, _ := e.newKey("other2", "shared-agent", nil)
	r1 := e.chat(k2, map[string]any{"model": "openai/gpt-4o", "messages": msgs("x")})
	r1.Body.Close()
	r2 := e.chat(k2, map[string]any{"model": "openai/gpt-4o", "messages": msgs("x")})
	if r1.StatusCode != 200 || r2.StatusCode != 429 {
		t.Fatalf("agent budget: %d then %d", r1.StatusCode, r2.StatusCode)
	}
	r2.Body.Close()
	// Raising the limit restores access immediately.
	e.admin("PUT", "/admin/api/limits", map[string]any{"scope": "agent", "scope_id": proj + "/shared-agent", "total_usd": 100}, 200)
	r3 := e.chat(k2, map[string]any{"model": "openai/gpt-4o", "messages": msgs("x")})
	if r3.StatusCode != 200 {
		t.Fatalf("after raising limit: %d", r3.StatusCode)
	}
	r3.Body.Close()
	lim := e.admin("GET", "/admin/api/limits", nil, 200)
	if len(lim["data"].([]any)) != 2 {
		t.Fatalf("limits: %v", lim)
	}
}

func TestRateLimit(t *testing.T) {
	e := setup(t)
	key, _, _ := e.newKey("rl", "", map[string]any{"rpm": 2})
	codes := []int{}
	for i := 0; i < 3; i++ {
		r := e.chat(key, map[string]any{"model": "openai/gpt-4o-mini", "messages": msgs("x")})
		codes = append(codes, r.StatusCode)
		if i == 2 && errCode(decodeJSON(t, r)) != "rate_limit_exceeded" {
			t.Fatal("expected rate_limit_exceeded")
		}
	}
	if codes[0] != 200 || codes[1] != 200 || codes[2] != 429 {
		t.Fatalf("codes %v", codes)
	}
}

func TestKillSwitch(t *testing.T) {
	e := setup(t)
	key, keyID, proj := e.newKey("runaway", "agent-x", nil)
	ok := func(want int, code string) {
		t.Helper()
		r := e.chat(key, map[string]any{"model": "openai/gpt-4o-mini", "messages": msgs("x")})
		m := decodeJSON(t, r)
		if r.StatusCode != want || (code != "" && errCode(m) != code) {
			t.Fatalf("want %d %s, got %d %v", want, code, r.StatusCode, m)
		}
	}
	ok(200, "")
	e.admin("POST", "/admin/api/keys/"+keyID+"/kill", nil, 200)
	ok(403, "key_killed")
	e.admin("POST", "/admin/api/keys/"+keyID+"/unkill", nil, 200)
	ok(200, "")
	e.admin("POST", "/admin/api/projects/"+proj+"/kill", nil, 200)
	ok(403, "project_killed")
	e.admin("POST", "/admin/api/projects/"+proj+"/unkill", nil, 200)
	ok(200, "")

	// Killing a key aborts its in-flight stream.
	resp := e.chat(key, map[string]any{"model": "openai/gpt-long", "stream": true, "messages": msgs("generate forever")})
	if resp.StatusCode != 200 {
		t.Fatalf("long stream: %d", resp.StatusCode)
	}
	sc := bufio.NewScanner(resp.Body)
	n := 0
	for sc.Scan() && n < 5 {
		if strings.HasPrefix(sc.Text(), "data: ") {
			n++
		}
	}
	start := time.Now()
	out := e.admin("POST", "/admin/api/keys/"+keyID+"/kill", nil, 200)
	if out["aborted_inflight"].(float64) != 1 {
		t.Fatalf("aborted_inflight: %v", out["aborted_inflight"])
	}
	rest, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(rest), "kill switch") || strings.Contains(string(rest), "[DONE]") || time.Since(start) > 2*time.Second {
		t.Fatalf("stream not aborted cleanly: %q", rest)
	}
	// The stream's log row is written when its handler returns; poll for it
	// rather than relying on ordering by start timestamp.
	var killedLog *store.RequestLog
	for deadline := time.Now().Add(3 * time.Second); killedLog == nil && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		logs, _, _ := e.st.ListLogs(context.Background(), store.LogFilter{KeyID: keyID})
		for _, l := range logs {
			if l.Stream {
				killedLog = l
			}
		}
	}
	if killedLog == nil || killedLog.ErrorCode != "killed_mid_stream" || killedLog.CompletionTokens == 0 {
		t.Fatalf("killed stream log: %+v", killedLog)
	}
	// Kill-switch webhooks were sent.
	found := false
	for _, h := range e.waitWebhooks(5) {
		if h["kind"] == "kill_switch" {
			found = true
		}
	}
	if !found {
		t.Fatal("no kill_switch webhook")
	}
}

func TestBodyLoggingOptIn(t *testing.T) {
	e := setup(t)
	key, keyID, proj := e.newKey("logs", "", nil)
	r := e.chat(key, map[string]any{"model": "openai/gpt-4o-mini", "messages": msgs("secret-ish prompt")})
	r.Body.Close()
	e.admin("PATCH", "/admin/api/projects/"+proj, map[string]any{"log_bodies": true}, 200)
	r = e.chat(key, map[string]any{"model": "anthropic/claude-haiku-4-5", "stream": true, "messages": msgs("now logged")})
	readSSE(t, r)
	list := e.admin("GET", "/admin/api/logs?key_id="+keyID, nil, 200)
	data := list["data"].([]any)
	if len(data) != 2 {
		t.Fatalf("logs: %v", list)
	}
	newest := e.admin("GET", "/admin/api/logs/"+data[0].(map[string]any)["id"].(string), nil, 200)
	oldest := e.admin("GET", "/admin/api/logs/"+data[1].(map[string]any)["id"].(string), nil, 200)
	if _, has := oldest["request_body"]; has {
		t.Fatal("body logged without opt-in")
	}
	if !strings.Contains(newest["request_body"].(string), "now logged") || !strings.Contains(newest["response_body"].(string), "mock anthropic") {
		t.Fatalf("opt-in body logging: %v", newest)
	}
	ov := e.admin("GET", "/admin/api/overview?days=7", nil, 200)
	if ov["today"].(map[string]any)["requests"].(float64) != 2 {
		t.Fatalf("overview: %v", ov["today"])
	}
}
