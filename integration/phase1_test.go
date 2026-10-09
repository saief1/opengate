package integration

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opengate-dev/opengate/internal/config"
	"github.com/opengate-dev/opengate/internal/pricing"
	"github.com/opengate-dev/opengate/internal/store"
)

func (e *env) logs(query string) []map[string]any {
	e.t.Helper()
	out := e.admin("GET", "/admin/api/logs?"+query, nil, 200)
	var rows []map[string]any
	for _, r := range out["data"].([]any) {
		rows = append(rows, r.(map[string]any))
	}
	return rows
}

func TestConcurrentTrafficCannotOvershootBudget(t *testing.T) {
	e := setup(t)
	// gpt-4o: $2.50 in / $10 out per 1M. Each request costs ~$0.0002.
	key, keyID, _ := e.newKey("swarm", "looper", map[string]any{"daily_usd": 0.001})
	var wg sync.WaitGroup
	var ok, denied atomic.Int64
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp := e.chat(key, map[string]any{"model": "openai/gpt-4o", "max_tokens": 20, "messages": msgs("concurrent hello")})
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			switch resp.StatusCode {
			case 200:
				ok.Add(1)
			case 429:
				denied.Add(1)
			default:
				t.Errorf("unexpected status %d", resp.StatusCode)
			}
		}()
	}
	wg.Wait()
	t.Logf("admitted %d, denied %d", ok.Load(), denied.Load())
	if ok.Load() == 0 || ok.Load() > 8 || denied.Load() < 30 {
		t.Fatalf("reservations should admit only a handful of 40 concurrent requests: ok=%d denied=%d", ok.Load(), denied.Load())
	}
	spent, _ := e.st.SpendSince(t.Context(), store.ScopeKey, keyID, time.Now().Add(-time.Hour))
	if spent > 0.001*1.5 {
		t.Fatalf("spend %.6f overshot the $0.001 budget by more than one request", spent)
	}
}

func TestModelAllowDenyListsAndMaxTokens(t *testing.T) {
	e := setup(t)
	key, keyID, projectID := e.newKey("restricted", "", nil)
	e.admin("PATCH", "/admin/api/keys/"+keyID, map[string]any{"allowed_models": []string{"openai/*", "smart"}, "max_tokens": 50}, 200)
	e.admin("PATCH", "/admin/api/projects/"+projectID, map[string]any{"denied_models": []string{"anthropic/*", "openai/o1*"}}, 200)

	resp := e.chat(key, map[string]any{"model": "gemini/gemini-2.5-flash", "messages": msgs("hi")})
	if m := decodeJSON(t, resp); resp.StatusCode != 403 || errCode(m) != "model_not_allowed" {
		t.Fatalf("not in allow-list: %d %v", resp.StatusCode, m)
	}
	resp = e.chat(key, map[string]any{"model": "openai/o1-preview", "messages": msgs("hi")})
	if m := decodeJSON(t, resp); resp.StatusCode != 403 || errCode(m) != "model_not_allowed" {
		t.Fatalf("project deny-list must win: %d %v", resp.StatusCode, m)
	}
	// Alias "smart" = [openai/gpt-fail, anthropic/claude-sonnet-4-5]; the
	// project denies anthropic/*, so failover to Anthropic must not happen.
	resp = e.chat(key, map[string]any{"model": "smart", "messages": msgs("hi")})
	decodeJSON(t, resp)
	if resp.StatusCode != 502 || e.mock.Hits("anthropic", "claude-sonnet-4-5") != 0 {
		t.Fatalf("denied failover target was used: status %d", resp.StatusCode)
	}
	resp = e.chat(key, map[string]any{"model": "openai/gpt-4o-mini", "max_tokens": 100, "messages": msgs("hi")})
	if m := decodeJSON(t, resp); resp.StatusCode != 400 || errCode(m) != "max_tokens_exceeded" {
		t.Fatalf("max_tokens cap: %d %v", resp.StatusCode, m)
	}
	resp = e.chat(key, map[string]any{"model": "openai/gpt-4o-mini", "messages": msgs("hi")})
	decodeJSON(t, resp)
	if resp.StatusCode != 200 || !strings.Contains(string(e.mock.LastBody("openai")), `"max_completion_tokens":50`) {
		t.Fatalf("cap must be injected when absent: %d %s", resp.StatusCode, e.mock.LastBody("openai"))
	}
	// /v1/models only lists what the key may call.
	req, _ := http.NewRequest("GET", e.gw.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	r2, _ := http.DefaultClient.Do(req)
	m := decodeJSON(t, r2)
	for _, d := range m["data"].([]any) {
		id := d.(map[string]any)["id"].(string)
		if !strings.HasPrefix(id, "openai/") && id != "smart" || strings.HasPrefix(id, "openai/o1") {
			t.Fatalf("/v1/models leaked disallowed model %s", id)
		}
	}
}

func TestPromptCacheTokensArePriced(t *testing.T) {
	e := setup(t)
	key, _, _ := e.newKey("cache", "", nil)
	resp := e.chat(key, map[string]any{"model": "anthropic/claude-sonnet-4-5-cached", "messages": msgs("long cached prompt")})
	m := decodeJSON(t, resp)
	u := m["usage"].(map[string]any)
	det := u["prompt_tokens_details"].(map[string]any)
	if det["cached_tokens"].(float64) != 9000 || det["cache_write_tokens"].(float64) != 1000 {
		t.Fatalf("anthropic cache usage not translated: %v", u)
	}
	l := e.logs("model=anthropic/claude-sonnet-4-5-cached")[0]
	tb, _ := pricing.Parse(pricing.DefaultYAML)
	want, _ := tb.CostTokens("anthropic", "claude-sonnet-4-5-cached", pricing.Tokens{Prompt: int(l["prompt_tokens"].(float64)),
		Completion: int(l["completion_tokens"].(float64)), CacheRead: 9000, CacheWrite: 1000})
	if l["cached_tokens"].(float64) != 9000 || l["cache_write_tokens"].(float64) != 1000 || abs(l["cost_usd"].(float64)-want) > 1e-12 {
		t.Fatalf("log: %v want cost %v", l, want)
	}
	naive := (u["prompt_tokens"].(float64)*3 + u["completion_tokens"].(float64)*15) / 1e6
	if l["cost_usd"].(float64) >= naive {
		t.Fatalf("cache reads should be cheaper than full input: %v >= %v", l["cost_usd"], naive)
	}
	// Streaming + OpenAI cached_tokens.
	resp = e.chat(key, map[string]any{"model": "openai/gpt-4o-cached", "stream": true, "messages": msgs("hi")})
	readSSE(t, resp)
	l = e.logs("model=openai/gpt-4o-cached")[0]
	if l["cached_tokens"].(float64) != 2000 {
		t.Fatalf("openai cached tokens: %v", l)
	}
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

func TestEmbeddingsEndpoint(t *testing.T) {
	e := setup(t)
	key, _, _ := e.newKey("emb", "", nil)
	resp := e.post("/v1/embeddings", key, map[string]any{"model": "openai/text-embedding-3-small", "input": []string{"a", "b"}, "dimensions": 4})
	m := decodeJSON(t, resp)
	if resp.StatusCode != 200 || len(m["data"].([]any)) != 2 || len(m["data"].([]any)[0].(map[string]any)["embedding"].([]any)) != 4 {
		t.Fatalf("openai embeddings: %d %v", resp.StatusCode, m)
	}
	resp = e.post("/v1/embeddings", key, map[string]any{"model": "gemini/gemini-embedding-2", "input": "hello world"})
	m = decodeJSON(t, resp)
	if resp.StatusCode != 200 || m["object"] != "list" || len(m["data"].([]any)[0].(map[string]any)["embedding"].([]any)) != 8 {
		t.Fatalf("gemini embeddings: %d %v", resp.StatusCode, m)
	}
	resp = e.post("/v1/embeddings", key, map[string]any{"model": "gemini/gemini-embedding-2", "input": "x", "encoding_format": "base64"})
	m = decodeJSON(t, resp)
	if _, ok := m["data"].([]any)[0].(map[string]any)["embedding"].(string); !ok {
		t.Fatalf("base64 encoding: %v", m)
	}
	resp = e.post("/v1/embeddings", key, map[string]any{"model": "anthropic/claude-sonnet-4-5", "input": "x"})
	if m := decodeJSON(t, resp); resp.StatusCode != 400 || errCode(m) != "unsupported_endpoint" {
		t.Fatalf("anthropic embeddings must be refused: %d %v", resp.StatusCode, m)
	}
	rows := e.logs("model=openai/text-embedding-3-small")
	if len(rows) != 1 || rows[0]["endpoint"] != "embeddings" || rows[0]["cost_usd"].(float64) <= 0 {
		t.Fatalf("embeddings log: %v", rows)
	}
}

func TestResponsesAPIPassthrough(t *testing.T) {
	e := setup(t)
	key, _, _ := e.newKey("resp", "", nil)
	resp := e.post("/v1/responses", key, map[string]any{"model": "openai/gpt-5-mini", "input": "say hi"})
	m := decodeJSON(t, resp)
	if resp.StatusCode != 200 || m["object"] != "response" || m["model"] != "gpt-5-mini" {
		t.Fatalf("responses: %d %v", resp.StatusCode, m)
	}
	resp = e.post("/v1/responses", key, map[string]any{"model": "openai/gpt-5-mini", "input": "stream please", "stream": true})
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	events := map[string]int{}
	for sc.Scan() {
		if ev, ok := strings.CutPrefix(sc.Text(), "event: "); ok {
			events[ev]++
		}
	}
	if events["response.created"] != 1 || events["response.output_text.delta"] == 0 || events["response.completed"] != 1 {
		t.Fatalf("stream events: %v", events)
	}
	time.Sleep(50 * time.Millisecond)
	rows := e.logs("model=openai/gpt-5-mini")
	if len(rows) != 2 {
		t.Fatalf("logs: %v", rows)
	}
	for _, r := range rows {
		if r["endpoint"] != "responses" || r["usage_estimated"] == true || r["completion_tokens"].(float64) == 0 || r["status"].(float64) != 200 {
			t.Fatalf("responses log: %v", r)
		}
	}
	resp = e.post("/v1/responses", key, map[string]any{"model": "anthropic/claude-sonnet-4-5", "input": "x"})
	if m := decodeJSON(t, resp); resp.StatusCode != 400 || errCode(m) != "unsupported_endpoint" {
		t.Fatalf("non-openai responses: %d %v", resp.StatusCode, m)
	}
}

func TestMetricsHealthAndReadiness(t *testing.T) {
	const metricsToken = "metrics-token-for-tests"
	e := setupWith(t, func(c *config.Config) { c.MetricsToken = metricsToken })
	key, _, _ := e.newKey("m", "", nil)
	decodeJSON(t, e.chat(key, map[string]any{"model": "openai/gpt-4o-mini", "messages": msgs("hi")}))
	get := func(path, tok string) (int, string) {
		req, _ := http.NewRequest("GET", e.gw.URL+path, nil)
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, _ := get("/metrics", ""); code != 401 {
		t.Fatalf("metrics must require a token, got %d", code)
	}
	// The admin token is not a metrics credential (security review L5).
	if code, _ := get("/metrics", adminToken); code != 401 {
		t.Fatalf("admin token must not unlock /metrics, got %d", code)
	}
	code, body := get("/metrics", metricsToken)
	if code != 200 || !strings.Contains(body, `opengate_requests_total{endpoint="chat",model="gpt-4o-mini",provider="openai",status="200"} 1`) &&
		!strings.Contains(body, `opengate_requests_total{endpoint="chat",provider="openai",model="gpt-4o-mini",status="200"} 1`) {
		t.Fatalf("metrics body:\n%s", body)
	}
	for _, want := range []string{"opengate_request_duration_seconds_bucket", "opengate_cost_usd_total", "opengate_tokens_total", "opengate_inflight_requests"} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %s", want)
		}
	}
	if code, body := get("/readyz", ""); code != 200 || !strings.Contains(body, `"ready"`) {
		t.Fatalf("readyz: %d %s", code, body)
	}
	if code, _ := get("/livez", ""); code != 200 {
		t.Fatal("livez")
	}
}

func TestAdminLockout(t *testing.T) {
	e := setupWith(t, func(c *config.Config) {
		c.AdminLockout = config.LockoutConfig{MaxFailures: 3, Window: time.Minute, Lockout: time.Minute}
	})
	try := func(tok string) int {
		req, _ := http.NewRequest("GET", e.gw.URL+"/admin/api/whoami", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, _ := http.DefaultClient.Do(req)
		resp.Body.Close()
		return resp.StatusCode
	}
	for i := 0; i < 3; i++ {
		if c := try("wrong-token-guess"); c != 401 {
			t.Fatalf("attempt %d: %d", i, c)
		}
	}
	// Further guesses are refused without being evaluated...
	if c := try("wrong-token-guess"); c != 429 {
		t.Fatalf("locked-out guesses must get 429, got %d", c)
	}
	// ...but the real token still works, so nobody can lock the operator out
	// (security review M5).
	if c := try(adminToken); c != 200 {
		t.Fatalf("correct token must work while guesses are locked out, got %d", c)
	}
}

func TestRateLimitStateSurvivesRestart(t *testing.T) {
	e := setup(t)
	key, _, _ := e.newKey("rl", "", map[string]any{"rpm": 2})
	for i := 0; i < 2; i++ {
		decodeJSON(t, e.chat(key, map[string]any{"model": "openai/gpt-4o-mini", "messages": msgs("hi")}))
	}
	// A fresh server instance on the same database (restart / second replica).
	_, h2 := e.newGateway()
	gw2 := httptest.NewServer(h2)
	defer gw2.Close()
	b, _ := json.Marshal(map[string]any{"model": "openai/gpt-4o-mini", "messages": msgs("hi")})
	req, _ := http.NewRequest("POST", gw2.URL+"/v1/chat/completions", strings.NewReader(string(b)))
	req.Header.Set("Authorization", "Bearer "+key)
	resp, _ := http.DefaultClient.Do(req)
	if m := decodeJSON(t, resp); resp.StatusCode != 429 || errCode(m) != "rate_limit_exceeded" {
		t.Fatalf("restarted instance must remember the bucket: %d %v", resp.StatusCode, m)
	}
}
