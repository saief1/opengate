package integration

import (
	"context"
	"testing"

	"github.com/saief1/opengate/internal/config"
	"github.com/saief1/opengate/internal/store"
)

func (e *env) chatWant(key string, want int, code string) {
	e.t.Helper()
	r := e.chat(key, map[string]any{"model": "openai/gpt-4o-mini", "messages": msgs("x")})
	m := decodeJSON(e.t, r)
	if r.StatusCode != want || (code != "" && errCode(m) != code) {
		e.t.Fatalf("want %d %s, got %d %v", want, code, r.StatusCode, m)
	}
}

func TestFailoverHopsLogged(t *testing.T) {
	e := setup(t)
	key, _, _ := e.newKey("fo", "", nil)
	resp := e.chat(key, map[string]any{"model": "smart", "messages": msgs("failover")})
	resp.Body.Close()
	id := resp.Header.Get("X-OpenGate-Request-Id")
	if resp.StatusCode != 200 || id == "" {
		t.Fatalf("failover: %d", resp.StatusCode)
	}
	l := e.admin("GET", "/admin/api/logs/"+id, nil, 200)
	hops, _ := l["hops"].([]any)
	if len(hops) != 3 {
		t.Fatalf("want 3 hops, got %v", l["hops"])
	}
	h0, h2 := hops[0].(map[string]any), hops[2].(map[string]any)
	if h0["provider"] != "openai" || h0["model"] != "gpt-fail" || h0["status"].(float64) < 500 || h0["error"] == "" {
		t.Fatalf("first hop: %v", h0)
	}
	if h2["provider"] != "anthropic" || h2["status"].(float64) != 200 || h2["error"] != nil {
		t.Fatalf("last hop: %v", h2)
	}
	// Streaming failover records hops too.
	resp = e.chat(key, map[string]any{"model": "smart", "stream": true, "messages": msgs("s")})
	readSSE(t, resp)
	sid := resp.Header.Get("X-OpenGate-Request-Id")
	var got *store.RequestLog
	waitFor(t, "stream log", func() bool { got, _ = e.st.GetLog(context.Background(), sid); return got != nil })
	if len(got.Hops) != 3 || got.Hops[2].Provider != "anthropic" {
		t.Fatalf("stream hops: %+v", got.Hops)
	}

	// Measured performance: openai failed every hop, anthropic succeeded.
	p := e.admin("GET", "/admin/api/performance?hours=1", nil, 200)
	succ := map[string]float64{}
	for _, x := range p["providers"].([]any) {
		m := x.(map[string]any)
		succ[m["provider"].(string)] = m["success_pct"].(float64)
	}
	if succ["openai"] != 0 || succ["anthropic"] != 100 {
		t.Fatalf("success: %v", p["providers"])
	}
	found := false
	for _, x := range p["models"].([]any) {
		m := x.(map[string]any)
		if m["model"] == "anthropic/claude-sonnet-4-5" && m["requests"].(float64) == 2 {
			found = true
		}
		if m["model"] == "openai/gpt-fail" {
			t.Fatal("failed-only model must not get a latency")
		}
	}
	if !found {
		t.Fatalf("models: %v", p["models"])
	}
}

func TestGlobalAgentCustomerKill(t *testing.T) {
	e := setup(t)
	a, _, proj := e.newKey("a", "planner", nil)
	b, bID, _ := e.newKey("b", "other", nil)
	e.admin("PATCH", "/admin/api/keys/"+bID, map[string]any{"customer_id": "acme"}, 200)

	e.admin("POST", "/admin/api/kill-switch", map[string]any{"scope": "global", "killed": true}, 200)
	e.chatWant(a, 403, "org_killed")
	e.chatWant(b, 403, "org_killed")
	ks := e.admin("GET", "/admin/api/kill-switch", nil, 200)
	if ks["global"] != true {
		t.Fatalf("global: %v", ks)
	}
	e.admin("POST", "/admin/api/kill-switch", map[string]any{"scope": "global", "killed": false}, 200)
	e.chatWant(a, 200, "")

	e.admin("POST", "/admin/api/kill-switch", map[string]any{"scope": "agent", "scope_id": proj + "/planner", "killed": true}, 200)
	e.chatWant(a, 403, "agent_killed")
	e.chatWant(b, 200, "")
	e.admin("POST", "/admin/api/kill-switch", map[string]any{"scope": "agent", "scope_id": proj + "/planner", "killed": false}, 200)

	e.admin("POST", "/admin/api/kill-switch", map[string]any{"scope": "customer", "scope_id": "acme", "killed": true}, 200)
	e.chatWant(b, 403, "customer_killed")
	e.chatWant(a, 200, "")

	e.admin("POST", "/admin/api/kill-switch", map[string]any{"scope": "key", "scope_id": "x", "killed": true}, 400)
	e.admin("POST", "/admin/api/kill-switch", map[string]any{"scope": "agent", "scope_id": "prj_nope/x", "killed": true}, 404)

	ks = e.admin("GET", "/admin/api/kill-switch", nil, 200)
	if sw := ks["switches"].([]any); len(sw) != 1 || sw[0].(map[string]any)["scope_id"] != "acme" {
		t.Fatalf("switches: %v", ks["switches"])
	}
	if h := ks["history"].([]any); len(h) != 5 || h[0].(map[string]any)["action"] != "killswitch.engage" {
		t.Fatalf("history: %v", h)
	}
}

func TestOrgSpendCap(t *testing.T) {
	e := setup(t)
	key, _, _ := e.newKey("k", "", nil)
	e.chatWant(key, 200, "")
	c := e.admin("GET", "/admin/api/org/spend-cap", nil, 200)
	if c["source"] != "" || c["daily_spent"].(float64) <= 0 {
		t.Fatalf("cap before: %v", c)
	}
	c = e.admin("PUT", "/admin/api/org/spend-cap", map[string]any{"daily_usd": 0.0000001, "monthly_usd": 100}, 200)
	if c["source"] != "org" || c["daily_usd"].(float64) != 0.0000001 {
		t.Fatalf("cap set: %v", c)
	}
	e.chatWant(key, 429, "budget_exceeded")
	e.admin("PUT", "/admin/api/org/spend-cap", map[string]any{"daily_usd": -1}, 400)
	e.admin("PUT", "/admin/api/org/spend-cap", map[string]any{"daily_usd": 0, "monthly_usd": 0}, 200)
	e.chatWant(key, 200, "")
	// The org scope can't be edited through the generic limits API.
	e.admin("PUT", "/admin/api/limits", map[string]any{"scope": "org", "scope_id": "org_default", "daily_usd": 1}, 400)
	acts := auditActionsStore(t, e)
	if acts["org.spend_cap.set"] != 1 || acts["org.spend_cap.clear"] != 1 {
		t.Fatalf("audit: %v", acts)
	}
}

func TestOrgSpendCapConfigDefault(t *testing.T) {
	e := setupWith(t, func(c *config.Config) { c.Budgets.OrgDailyUSD = 0.0000001 })
	key, _, _ := e.newKey("k", "", nil)
	e.chatWant(key, 200, "")
	e.chatWant(key, 429, "budget_exceeded")
	if c := e.admin("GET", "/admin/api/org/spend-cap", nil, 200); c["source"] != "config" {
		t.Fatalf("source: %v", c)
	}
	// An explicit org cap overrides the config default.
	e.admin("PUT", "/admin/api/org/spend-cap", map[string]any{"daily_usd": 1000}, 200)
	e.chatWant(key, 200, "")
}

func auditActionsStore(t *testing.T, e *env) map[string]int {
	rows, _, err := e.st.ListAudit(context.Background(), store.DefaultOrgID, 500, 0)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]int{}
	for _, a := range rows {
		m[a.Action]++
	}
	return m
}
