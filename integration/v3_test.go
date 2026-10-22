package integration

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/saief1/opengate/internal/config"
	"github.com/saief1/opengate/internal/store"
)

// mockMCP is a tiny streamable-HTTP MCP server.
type mockMCP struct {
	mu    sync.Mutex
	auths []string
	srv   *httptest.Server
}

func newMockMCP(t *testing.T) *mockMCP {
	m := &mockMCP{}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.auths = append(m.auths, r.Header.Get("Authorization")+"|"+r.Header.Get("X-Api-Key"))
		m.mu.Unlock()
		if r.Method == http.MethodGet { // legacy HTTP+SSE transport
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "event: endpoint\ndata: /messages?sessionId=abc\n\n")
			return
		}
		var msg map[string]any
		json.NewDecoder(r.Body).Decode(&msg)
		id := msg["id"]
		var result any
		switch msg["method"] {
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "search"}, map[string]any{"name": "delete_repo"}, map[string]any{"name": "read_file"}}}
		case "tools/call":
			p := msg["params"].(map[string]any)
			if p["name"] == "broken" {
				result = map[string]any{"isError": true, "content": []any{}}
			} else {
				result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "ok:" + p["name"].(string)}}}
			}
		default:
			result = map[string]any{}
		}
		out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
		w.Header().Set("Mcp-Session-Id", "sess-1")
		if strings.Contains(r.Header.Get("Accept"), "text/event-stream") && msg["method"] == "tools/list" {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "event: message\ndata: "+string(out)+"\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(out)
	}))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *mockMCP) lastAuth() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.auths[len(m.auths)-1]
}

func (e *env) mcp(server, key string, msg map[string]any, accept string) (int, map[string]any, string) {
	e.t.Helper()
	b, _ := json.Marshal(msg)
	req, _ := http.NewRequest("POST", e.gw.URL+"/mcp/"+server, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	json.Unmarshal(raw, &out)
	return resp.StatusCode, out, string(raw)
}

func call(id int, tool string, args map[string]any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call", "params": map[string]any{"name": tool, "arguments": args}}
}

func TestMCPGateway(t *testing.T) {
	e := setup(t)
	up := newMockMCP(t)
	srv := e.admin("POST", "/admin/api/mcp/servers", map[string]any{"name": "github", "url": up.srv.URL + "/mcp", "secret": "Bearer server-secret-ABCD1234"}, 201)
	sid := srv["id"].(string)
	if srv["has_secret"] != true || srv["secret_hint"] != "…1234" {
		t.Fatalf("server: %v", srv)
	}
	list := e.admin("GET", "/admin/api/mcp/servers", nil, 200)
	if b, _ := json.Marshal(list); strings.Contains(string(b), "server-secret") {
		t.Fatal("secret leaked by the API")
	}
	e.admin("POST", "/admin/api/mcp/policies", map[string]any{"server_id": sid, "tool": "delete_*", "action": "deny"}, 201)
	e.admin("POST", "/admin/api/mcp/policies", map[string]any{"server_id": sid, "tool": "search", "action": "allow", "rpm": 2}, 201)
	// An agent-specific allow beats the all-agents deny.
	e.admin("POST", "/admin/api/mcp/policies", map[string]any{"server_id": sid, "agent": "admin-bot", "tool": "delete_repo", "action": "allow"}, 201)
	key, keyID, _ := e.newKey("mcp-key", "bot", nil)
	adminKey, _, _ := e.newKey("mcp-admin", "admin-bot", nil)

	// tools/list is filtered per agent (JSON and SSE responses).
	st, out, _ := e.mcp("github", key, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"}, "application/json")
	if st != 200 || strings.Contains(mustJSON(out), "delete_repo") || !strings.Contains(mustJSON(out), "read_file") {
		t.Fatalf("tools/list: %d %v", st, out)
	}
	_, _, raw := e.mcp("github", key, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"}, "application/json, text/event-stream")
	if !strings.Contains(raw, "data: ") || strings.Contains(raw, "delete_repo") {
		t.Fatalf("SSE tools/list not filtered: %s", raw)
	}
	_, out, _ = e.mcp("github", adminKey, map[string]any{"jsonrpc": "2.0", "id": 3, "method": "tools/list"}, "application/json")
	if !strings.Contains(mustJSON(out), "delete_repo") {
		t.Fatalf("admin-bot should see delete_repo: %v", out)
	}

	// Allowed call: server credential injected; arguments redacted in the log.
	st, out, _ = e.mcp("github", key, call(4, "search", map[string]any{"q": "mail jane.doe@example.com", "token": "tok_live_value"}), "")
	if st != 200 || !strings.Contains(mustJSON(out), "ok:search") {
		t.Fatalf("search: %d %v", st, out)
	}
	if a := up.lastAuth(); a != "Bearer server-secret-ABCD1234|" {
		t.Fatalf("upstream auth = %q", a)
	}
	// Per-agent credential overrides the server secret.
	e.admin("PUT", "/admin/api/mcp/servers/"+sid+"/credentials", map[string]any{"agent": "bot", "header": "X-Api-Key", "secret": "agent-secret-9999"}, 200)
	e.mcp("github", key, call(5, "search", map[string]any{"q": "x"}), "")
	if a := up.lastAuth(); a != "|agent-secret-9999" {
		t.Fatalf("agent credential not injected: %q", a)
	}
	// Rate limit: search is limited to 2/min.
	if st, out, _ = e.mcp("github", key, call(6, "search", nil), ""); st != 429 {
		t.Fatalf("rate limit: %d %v", st, out)
	}
	// Denied tool.
	st, out, _ = e.mcp("github", key, call(7, "delete_repo", map[string]any{"repo": "x"}), "")
	if st != 403 || out["error"].(map[string]any)["code"].(float64) != -32001 {
		t.Fatalf("deny: %d %v", st, out)
	}
	if st, _, _ = e.mcp("github", adminKey, call(8, "delete_repo", nil), ""); st != 200 {
		t.Fatalf("admin-bot delete_repo: %d", st)
	}
	// Tool errors are recorded.
	e.mcp("github", key, call(9, "broken", nil), "")
	// Dry run.
	chk := e.admin("POST", "/admin/api/mcp/check", map[string]any{"server_id": sid, "agent": "bot", "tool": "delete_x"}, 200)
	if chk["allowed"] != false {
		t.Fatalf("check: %v", chk)
	}

	calls := e.admin("GET", "/admin/api/mcp/calls?agent=bot", nil, 200)
	rows := calls["data"].([]any)
	if len(rows) < 5 {
		t.Fatalf("calls logged: %d", len(rows))
	}
	all := mustJSON(calls)
	if strings.Contains(all, "jane.doe@example.com") || strings.Contains(all, "tok_live_value") || !strings.Contains(all, "[REDACTED_EMAIL]") {
		t.Fatalf("args not redacted: %s", all)
	}
	codes := map[string]bool{}
	for _, r := range rows {
		c, _ := r.(map[string]any)["error_code"].(string)
		codes[c] = true
	}
	for _, c := range []string{"tool_not_allowed", "rate_limit_exceeded", "tool_error", ""} {
		if !codes[c] {
			t.Errorf("missing call with error_code %q in %v", c, codes)
		}
	}

	// Legacy SSE transport: the endpoint event is rewritten through OpenGate.
	req, _ := http.NewRequest("GET", e.gw.URL+"/mcp/github", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(resp.Body)
	found := false
	for sc.Scan() {
		if sc.Text() == "data: /mcp/github/messages?sessionId=abc" {
			found = true
		}
	}
	resp.Body.Close()
	if !found {
		t.Fatal("endpoint event not rewritten")
	}

	// Kill switch blocks MCP too.
	e.admin("POST", "/admin/api/keys/"+keyID+"/kill", nil, 200)
	if st, out, _ = e.mcp("github", key, call(10, "read_file", nil), ""); st != 403 {
		t.Fatalf("killed key: %d %v", st, out)
	}
	// Unknown server / bad key.
	if st, _, _ = e.mcp("nope", adminKey, call(1, "x", nil), ""); st != 404 {
		t.Fatalf("unknown server: %d", st)
	}
	if st, _, _ = e.mcp("github", "og-bad", call(1, "x", nil), ""); st != 401 {
		t.Fatalf("bad key: %d", st)
	}
	// Audit trail of MCP config changes.
	aud := mustJSON(e.admin("GET", "/admin/api/audit?action=mcp.", nil, 200))
	for _, a := range []string{"mcp.server.create", "mcp.policy.add", "mcp.credential.set"} {
		if !strings.Contains(aud, a) {
			t.Errorf("audit missing %s", a)
		}
	}
	if strings.Contains(aud, "server-secret") || strings.Contains(aud, "agent-secret") {
		t.Fatal("secret in audit log")
	}
	// Disabling a server makes it unreachable.
	e.admin("PATCH", "/admin/api/mcp/servers/"+sid, map[string]any{"enabled": false}, 200)
	if st, _, _ = e.mcp("github", adminKey, call(11, "search", nil), ""); st != 404 {
		t.Fatalf("disabled server: %d", st)
	}
}

func TestMCPBudget(t *testing.T) {
	e := setup(t)
	up := newMockMCP(t)
	e.admin("POST", "/admin/api/mcp/servers", map[string]any{"name": "tools", "url": up.srv.URL}, 201)
	key, _, _ := e.newKey("tiny", "spender", map[string]any{"total_usd": 0.0000001})
	resp := e.chat(key, map[string]any{"model": "openai/gpt-4o", "messages": msgs("hello")})
	resp.Body.Close()
	st, out, _ := e.mcp("tools", key, call(1, "search", nil), "")
	if st != 429 || !strings.Contains(mustJSON(out), "budget_exceeded") {
		t.Fatalf("over-budget agent kept calling tools: %d %v", st, out)
	}
}

func TestAuditLogBeforeAfterFiltersCSV(t *testing.T) {
	e := setup(t)
	_, keyID, _ := e.newKey("audited", "a1", nil)
	e.admin("POST", "/admin/api/keys/"+keyID+"/kill", nil, 200)
	e.admin("PUT", "/admin/api/limits", map[string]any{"scope": "key", "scope_id": keyID, "daily_usd": 5}, 200)
	e.admin("PUT", "/admin/api/limits", map[string]any{"scope": "key", "scope_id": keyID, "daily_usd": 9}, 200)
	out := e.admin("GET", "/admin/api/audit?action=key.kill", nil, 200)
	rows := out["data"].([]any)
	if len(rows) != 1 {
		t.Fatalf("filter: %v", out)
	}
	a := rows[0].(map[string]any)
	if a["before"].(map[string]any)["killed"] != false || a["after"].(map[string]any)["killed"] != true {
		t.Fatalf("before/after: %v", a)
	}
	var lim map[string]any
	for _, x := range e.admin("GET", "/admin/api/audit?action=limit.set", nil, 200)["data"].([]any) {
		if x.(map[string]any)["before"] != nil {
			lim = x.(map[string]any)
		}
	}
	if lim == nil || lim["before"].(map[string]any)["daily_usd"].(float64) != 5 || lim["after"].(map[string]any)["daily_usd"].(float64) != 9 {
		t.Fatalf("limit before/after: %v", lim)
	}
	if n := len(e.admin("GET", "/admin/api/audit?since=2999-01-01", nil, 200)["data"].([]any)); n != 0 {
		t.Fatalf("since filter: %d", n)
	}
	req, _ := http.NewRequest("GET", e.gw.URL+"/admin/api/audit/export.csv?action=key.", nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(string(b), "id,ts,actor_type") || !strings.Contains(string(b), "key.kill") || strings.Contains(string(b), "limit.set") {
		t.Fatalf("csv: %d %s", resp.StatusCode, b)
	}
}

func TestRolesReadOnlyAndMember(t *testing.T) {
	e := setup(t)
	e.mkUser("ro@example.com", "readonly-password-1", store.DefaultOrgID, "read-only")
	e.mkUser("mem@example.com", "member-password-1", store.DefaultOrgID, "member")
	_, keyID, _ := e.newKey("k", "a", nil)
	ro := e.browser()
	ro.do("POST", "/admin/api/auth/login", map[string]any{"email": "ro@example.com", "password": "readonly-password-1"}, 200)
	if p := ro.do("GET", "/admin/api/plan", nil, 200); p["role"] != "viewer" {
		t.Fatalf("plan role: %v", p)
	}
	ro.do("GET", "/admin/api/keys", nil, 200)
	ro.do("GET", "/admin/api/audit", nil, 200)
	ro.do("POST", "/admin/api/keys", map[string]any{"name": "x"}, 403)
	ro.do("POST", "/admin/api/keys/"+keyID+"/kill", nil, 403)
	ro.do("POST", "/admin/api/mcp/servers", map[string]any{"name": "x", "url": "https://example.com"}, 403)
	mem := e.browser()
	mem.do("POST", "/admin/api/auth/login", map[string]any{"email": "mem@example.com", "password": "member-password-1"}, 200)
	mem.do("POST", "/admin/api/keys", map[string]any{"name": "mine"}, 201)
	mem.do("POST", "/admin/api/keys/"+keyID+"/kill", nil, 200)
	mem.do("PUT", "/admin/api/limits", map[string]any{"scope": "key", "scope_id": keyID, "daily_usd": 1}, 403)
	mem.do("POST", "/admin/api/mcp/policies", map[string]any{"server_id": "x", "tool": "*", "action": "deny"}, 403)
	// The member's kill is attributed in the audit log.
	aud := mustJSON(e.admin("GET", "/admin/api/audit?actor=mem@example.com", nil, 200))
	if !strings.Contains(aud, "key.kill") {
		t.Fatalf("member action not audited: %s", aud)
	}
}

func TestPlanGatingFree(t *testing.T) {
	e := setupWith(t, func(c *config.Config) { c.Plan = "free" })
	p := e.admin("GET", "/admin/api/plan", nil, 200)
	if p["plan"] != "free" || p["features"].(map[string]any)["audit_log"] != false {
		t.Fatalf("plan: %v", p)
	}
	if len(p["coming_soon"].([]any)) != 4 {
		t.Fatalf("coming soon: %v", p)
	}
	e.admin("GET", "/admin/api/audit", nil, 402)
	e.admin("POST", "/admin/api/billing/markups", map[string]any{"markup_pct": 10}, 402)
	e.admin("POST", "/admin/api/invites", map[string]any{"email": "x@example.com", "role": "viewer"}, 402)
	// Core gateway features keep working on the free plan, and the audit
	// trail is still recorded (just not viewable until upgrade).
	key, _, _ := e.newKey("free-key", "f", nil)
	resp := e.chat(key, map[string]any{"model": "openai/gpt-4o", "messages": msgs("hi")})
	if resp.StatusCode != 200 {
		t.Fatalf("chat on free plan: %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestPerTokenMarkup(t *testing.T) {
	e := setup(t)
	e.admin("POST", "/admin/api/billing/markups", map[string]any{"markup_pct": 0, "per_mtok_usd": 1000}, 201)
	key, _, _ := e.newKey("billed", "b", nil)
	resp := e.chat(key, map[string]any{"model": "openai/gpt-4o", "messages": msgs("hello there")})
	resp.Body.Close()
	l := e.logs("limit=1")[0]
	tokens := l["prompt_tokens"].(float64) + l["completion_tokens"].(float64)
	want := l["cost_usd"].(float64) + tokens*1000/1e6
	if got := l["billed_usd"].(float64); abs(got-want) > 1e-9 {
		t.Fatalf("billed %v want %v (%v tokens)", got, want, tokens)
	}
}

func TestCORSGatewayOnly(t *testing.T) {
	e := setupWith(t, func(c *config.Config) { c.CORSOrigins = []string{"https://app.example.com"} })
	pre := func(path, origin string) http.Header {
		req, _ := http.NewRequest("OPTIONS", e.gw.URL+path, nil)
		req.Header.Set("Origin", origin)
		req.Header.Set("Access-Control-Request-Method", "POST")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.Header
	}
	if h := pre("/v1/chat/completions", "https://app.example.com"); h.Get("Access-Control-Allow-Origin") != "https://app.example.com" {
		t.Fatalf("gateway preflight: %v", h)
	}
	if h := pre("/v1/chat/completions", "https://evil.example"); h.Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("foreign origin allowed")
	}
	if h := pre("/admin/api/keys", "https://app.example.com"); h.Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("admin API must never send CORS headers")
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestCloudFreeOrgAuditGated(t *testing.T) {
	e, _ := setupCloud(t, nil)
	b, _, _ := cloudSignup(t, e, "free@x.test", "FreeCo")
	b.do("GET", "/admin/api/audit", nil, 402)
	if p := b.do("GET", "/admin/api/plan", nil, 200); p["features"].(map[string]any)["roles"] != false {
		t.Fatalf("plan: %v", p)
	}
	org := b.do("GET", "/admin/api/org", nil, 200)["id"].(string)
	e.adminOrg(org, "GET", "/admin/api/audit", nil, 200)
}
