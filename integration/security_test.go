package integration

// Regression tests for findings of the internal pre-release security reviews
// (org scoping, token caps, passthrough spend, customer attribution,
// lockout, cross-org routing, invite handling).

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/saief1/opengate/internal/config"
)

// H1: every object-by-id admin route answers 404 for another org's IDs, and
// list / aggregate routes never include another org's rows.
func TestCrossOrgIDsAre404(t *testing.T) {
	stripe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"object":"billing.meter_event"}`)
	}))
	defer stripe.Close()
	e := setupWith(t, func(c *config.Config) {
		c.Billing.Stripe = config.StripeConfig{Enabled: true, BaseURL: stripe.URL, Lag: time.Nanosecond, Interval: time.Hour, Window: time.Second}
		c.StripeAPIKey = "sk_test_mock_only"
	})
	acme := e.admin("POST", "/admin/api/orgs", map[string]any{"name": "Acme"}, 201)["id"].(string)
	pk := e.adminOrg(acme, "POST", "/admin/api/provider-keys", map[string]any{"provider": "openai", "key": "sk-acme-only-123456"}, 201)
	kr := e.adminOrg(acme, "POST", "/admin/api/keys", map[string]any{"name": "acme-bot"}, 201)
	acmeKey := kr["key"].(string)
	keyID := kr["virtual_key"].(map[string]any)["id"].(string)
	proj := kr["virtual_key"].(map[string]any)["project_id"].(string)
	e.adminOrg(acme, "PATCH", "/admin/api/projects/"+proj, map[string]any{"log_bodies": true, "trust_customer_header": true}, 200)
	e.adminOrg(acme, "PUT", "/admin/api/projects/"+proj+"/cache", map[string]any{"ttl_seconds": 60}, 200)
	e.adminOrg(acme, "PUT", "/admin/api/limits", map[string]any{"scope": "key", "scope_id": keyID, "daily_usd": 5}, 200)
	e.adminOrg(acme, "PUT", "/admin/api/customers/"+proj+"/acme-cust", map[string]any{"name": "Acme customer", "stripe_customer_id": "cus_acme"}, 200)
	rule := e.adminOrg(acme, "POST", "/admin/api/routing/rules", map[string]any{"name": "r", "match": map[string]any{"tags": []string{"never"}},
		"target": "openai/gpt-4o-mini"}, 200)["id"].(string)
	markup := e.adminOrg(acme, "POST", "/admin/api/billing/markups", map[string]any{"markup_pct": 20}, 201)["id"].(string)
	invite := e.adminOrg(acme, "POST", "/admin/api/invites", map[string]any{"email": "new@acme.test", "role": "member"}, 201)["invite"].(map[string]any)["id"].(string)
	e.mkUser("member@acme.test", "correct horse battery staple", acme, "member")
	var memberID string
	for _, m := range e.adminOrg(acme, "GET", "/admin/api/members", nil, 200)["data"].([]any) {
		if mm := m.(map[string]any); mm["email"] == "member@acme.test" {
			memberID = mm["id"].(string)
		}
	}
	resp := e.chatH(acmeKey, map[string]any{"model": "openai/gpt-4o", "messages": msgs("acme secret prompt")}, map[string]string{"X-OpenGate-Customer": "acme-cust"})
	decodeJSON(t, resp)
	logs := e.adminOrg(acme, "GET", "/admin/api/logs", nil, 200)["data"].([]any)
	logID := logs[0].(map[string]any)["id"].(string)

	// Default org must get 404 on every Acme ID.
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{"GET", "/admin/api/logs/" + logID, nil},
		{"PATCH", "/admin/api/keys/" + keyID, map[string]any{"max_tokens": 1}},
		{"DELETE", "/admin/api/keys/" + keyID, nil},
		{"POST", "/admin/api/keys/" + keyID + "/kill", nil},
		{"POST", "/admin/api/keys/" + keyID + "/unkill", nil},
		{"POST", "/admin/api/keys", map[string]any{"name": "sneaky", "project_id": proj}},
		{"PATCH", "/admin/api/projects/" + proj, map[string]any{"log_bodies": false}},
		{"POST", "/admin/api/projects/" + proj + "/kill", nil},
		{"POST", "/admin/api/projects/" + proj + "/unkill", nil},
		{"PUT", "/admin/api/projects/" + proj + "/cache", map[string]any{"ttl_seconds": 0}},
		{"PUT", "/admin/api/projects/" + proj + "/guardrails", map[string]any{"pii": "off"}},
		{"PUT", "/admin/api/limits", map[string]any{"scope": "key", "scope_id": keyID, "daily_usd": 1000}},
		{"PUT", "/admin/api/limits", map[string]any{"scope": "project", "scope_id": proj, "daily_usd": 1000}},
		{"DELETE", "/admin/api/limits?scope=key&scope_id=" + keyID, nil},
		{"DELETE", "/admin/api/provider-keys/" + pk["id"].(string), nil},
		{"PUT", "/admin/api/routing/rules/" + rule, map[string]any{"name": "x", "target": "openai/gpt-4o"}},
		{"DELETE", "/admin/api/routing/rules/" + rule, nil},
		{"DELETE", "/admin/api/billing/markups/" + markup, nil},
		{"DELETE", "/admin/api/invites/" + invite, nil},
		{"PATCH", "/admin/api/members/" + memberID, map[string]any{"role": "owner"}},
		{"DELETE", "/admin/api/members/" + memberID, nil},
		{"PUT", "/admin/api/customers/" + proj + "/acme-cust", map[string]any{"name": "pwned"}},
		{"GET", "/admin/api/cache?project_id=" + proj, nil},
		{"DELETE", "/admin/api/cache?project_id=" + proj, nil},
	} {
		e.admin(c.method, c.path, c.body, 404)
	}

	// Aggregates and lists exclude Acme.
	for _, l := range e.admin("GET", "/admin/api/logs", nil, 200)["data"].([]any) {
		if l.(map[string]any)["id"] == logID {
			t.Fatal("default org lists acme log")
		}
	}
	for _, l := range e.admin("GET", "/admin/api/limits", nil, 200)["data"].([]any) {
		if l.(map[string]any)["scope_id"] == keyID {
			t.Fatal("default org lists acme limit")
		}
	}
	for _, path := range []string{"/admin/api/customers", "/admin/api/billing/markups", "/admin/api/routing/rules", "/admin/api/invites", "/admin/api/projects", "/admin/api/keys"} {
		b, _ := json.Marshal(e.admin("GET", path, nil, 200))
		for _, id := range []string{proj, keyID, rule, markup, invite, "acme-cust"} {
			if strings.Contains(string(b), id) {
				t.Fatalf("%s in default org leaks acme id %s", path, id)
			}
		}
	}
	if st := e.admin("GET", "/admin/api/cache", nil, 200)["stats"].(map[string]any); st["entries"].(float64) != 0 {
		t.Fatalf("default org sees acme cache entries: %v", st)
	}
	if st := e.adminOrg(acme, "GET", "/admin/api/cache", nil, 200)["stats"].(map[string]any); st["entries"].(float64) != 1 {
		t.Fatalf("acme cache stats: %v", st)
	}
	if ov := e.admin("GET", "/admin/api/overview", nil, 200)["today"].(map[string]any); ov["requests"].(float64) != 0 {
		t.Fatalf("default overview counts acme traffic: %v", ov)
	}
	// Stripe sync only touches the caller's org.
	time.Sleep(1100 * time.Millisecond) // let the 1s reporting window close
	if s := e.admin("POST", "/admin/api/billing/stripe/sync", nil, 200); s["customers"].(float64) != 0 {
		t.Fatalf("default org synced acme customers: %v", s)
	}
	if s := e.adminOrg(acme, "POST", "/admin/api/billing/stripe/sync", nil, 200); s["customers"].(float64) != 1 {
		t.Fatalf("acme sync: %v", s)
	}
	// And the key still works: nothing above changed Acme's state.
	resp = e.chat(acmeKey, map[string]any{"model": "openai/gpt-4o-mini", "messages": msgs("still alive")})
	decodeJSON(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("acme key affected by cross-org calls: %d", resp.StatusCode)
	}
}

// M1: a cap can't be dodged by sending both max-token fields; upstream sees
// exactly one field with the larger value.
func TestMaxTokensBothFieldsNormalized(t *testing.T) {
	e := setup(t)
	key, keyID, _ := e.newKey("capped", "", nil)
	e.admin("PATCH", "/admin/api/keys/"+keyID, map[string]any{"max_tokens": 100}, 200)
	r := e.chat(key, map[string]any{"model": "openai/gpt-4o", "messages": msgs("x"), "max_completion_tokens": 10, "max_tokens": 200000})
	if m := decodeJSON(t, r); r.StatusCode != 400 || errCode(m) != "max_tokens_exceeded" {
		t.Fatalf("bypass not caught: %d %v", r.StatusCode, m)
	}
	r = e.chat(key, map[string]any{"model": "openai/gpt-4o", "messages": msgs("x"), "max_completion_tokens": 10, "max_tokens": 50})
	if decodeJSON(t, r); r.StatusCode != 200 {
		t.Fatalf("status %d", r.StatusCode)
	}
	var up map[string]any
	json.Unmarshal(e.mock.LastBody("openai"), &up)
	if up["max_completion_tokens"] != float64(50) || up["max_tokens"] != nil {
		t.Fatalf("openai upstream body not normalized: %v", up)
	}
	r = e.chat(key, map[string]any{"model": "openrouter/meta/llama", "messages": msgs("x"), "max_completion_tokens": 10, "max_tokens": 50})
	if decodeJSON(t, r); r.StatusCode != 200 {
		t.Fatalf("openrouter status %d", r.StatusCode)
	}
	up = nil
	json.Unmarshal(e.mock.LastBody("openrouter"), &up)
	if up["max_tokens"] != float64(50) || up["max_completion_tokens"] != nil {
		t.Fatalf("openrouter upstream body not normalized: %v", up)
	}
}

// M2: passthrough features with unbudgetable cost are opt-in per project, and
// n>1 reserves the worst case for every choice.
func TestPassthroughFeatureAllowlist(t *testing.T) {
	e := setupWith(t, func(c *config.Config) { c.Budgets.Strict = true })
	key, _, proj := e.newKey("feat", "", map[string]any{"total_usd": 0.05})
	deny := func(path string, body map[string]any) {
		t.Helper()
		r := e.post(path, key, body)
		if m := decodeJSON(t, r); r.StatusCode != 400 || errCode(m) != "feature_not_allowed" {
			t.Fatalf("%s %v: want feature_not_allowed, got %d %v", path, body, r.StatusCode, m)
		}
	}
	chat := func(extra map[string]any) map[string]any {
		b := map[string]any{"model": "openai/gpt-4o", "messages": msgs("x"), "max_tokens": 1000}
		for k, v := range extra {
			b[k] = v
		}
		return b
	}
	deny("/v1/chat/completions", chat(map[string]any{"n": 3}))
	deny("/v1/chat/completions", chat(map[string]any{"web_search_options": map[string]any{}}))
	deny("/v1/chat/completions", chat(map[string]any{"tools": []any{map[string]any{"type": "web_search"}}}))
	deny("/v1/chat/completions", chat(map[string]any{"store": true}))
	deny("/v1/responses", map[string]any{"model": "openai/gpt-4o", "input": "x", "background": true})
	deny("/v1/responses", map[string]any{"model": "openai/gpt-4o", "input": "x", "tools": []any{map[string]any{"type": "code_interpreter"}}})
	// Client-executed function tools are always fine.
	r := e.chat(key, chat(map[string]any{"tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "f"}}}}))
	if decodeJSON(t, r); r.StatusCode != 200 {
		t.Fatalf("function tool: %d", r.StatusCode)
	}
	e.admin("PATCH", "/admin/api/projects/"+proj, map[string]any{"allowed_features": []string{"n", "builtin_tools", "background"}}, 200)
	e.admin("PATCH", "/admin/api/projects/"+proj, map[string]any{"allowed_features": []string{"bogus"}}, 400)
	r = e.chat(key, chat(map[string]any{"n": 2}))
	if decodeJSON(t, r); r.StatusCode != 200 {
		t.Fatalf("allowed n=2: %d", r.StatusCode)
	}
	// Worst case for n=10 x 1000 tokens of gpt-4o exceeds the $0.05 budget
	// in strict mode, although a single choice fits.
	r = e.chat(key, chat(map[string]any{"n": 10}))
	if m := decodeJSON(t, r); r.StatusCode == 200 {
		t.Fatalf("n=10 must reserve 10x the output: %v", m)
	}
	r = e.post("/v1/responses", key, map[string]any{"model": "openai/gpt-4o", "input": "x", "tools": []any{map[string]any{"type": "web_search"}}})
	if decodeJSON(t, r); r.StatusCode != 200 {
		t.Fatalf("allowed builtin tool: %d", r.StatusCode)
	}
}

// M4: customer attribution can't be asserted by arbitrary key holders.
func TestCustomerAttributionControls(t *testing.T) {
	e := setup(t)
	key, keyID, proj := e.newKey("cust", "", nil)
	cust := func(headers map[string]string, body map[string]any) (int, string) {
		t.Helper()
		b := map[string]any{"model": "openai/gpt-4o-mini", "messages": msgs("x")}
		for k, v := range body {
			b[k] = v
		}
		r := e.chatH(key, b, headers)
		m := decodeJSON(t, r)
		if r.StatusCode != 200 {
			return r.StatusCode, errCode(m)
		}
		id, _ := e.logs("limit=1")[0]["customer_id"].(string)
		return 200, id
	}
	// Untrusted project (default): header and user field are ignored.
	if c, id := cust(map[string]string{"X-OpenGate-Customer": "victim"}, map[string]any{"user": "victim2"}); c != 200 || id != "" {
		t.Fatalf("untrusted header honored: %d %q", c, id)
	}
	// Trusted project: honored.
	e.admin("PATCH", "/admin/api/projects/"+proj, map[string]any{"trust_customer_header": true}, 200)
	if _, id := cust(map[string]string{"X-OpenGate-Customer": "acme"}, nil); id != "acme" {
		t.Fatalf("trusted header: %q", id)
	}
	// Key bound to a customer: always attributed, conflicting header refused.
	e.admin("PATCH", "/admin/api/keys/"+keyID, map[string]any{"customer_id": "bound-co"}, 200)
	if _, id := cust(nil, map[string]any{"user": "someone-else"}); id != "bound-co" {
		t.Fatalf("bound key attribution: %q", id)
	}
	if c, code := cust(map[string]string{"X-OpenGate-Customer": "victim"}, nil); c != 403 || code != "customer_mismatch" {
		t.Fatalf("conflicting header: %d %s", c, code)
	}
	time.Sleep(2 * time.Millisecond) // log rows are ordered by ms timestamp
	if c, id := cust(map[string]string{"X-OpenGate-Customer": "bound-co"}, nil); id != "bound-co" {
		t.Fatalf("matching header must be accepted: %d %q", c, id)
	}
	e.admin("PATCH", "/admin/api/keys/"+keyID, map[string]any{"customer_id": "bad\u0007id"}, 400)
}

// M4 / L2: cache entries are per key and per customer; clients can't enable
// caching on their own; hits still count against rate limits.
func TestCacheScopingAndOptIn(t *testing.T) {
	e := setup(t)
	k1, _, proj := e.newKey("c1", "", map[string]any{"rpm": 3})
	body := map[string]any{"model": "openai/gpt-4o", "messages": msgs("cache scope"), "temperature": 0}
	// Client TTL header alone does not enable caching.
	for i := 0; i < 2; i++ {
		r := e.chatH(k1, body, map[string]string{"X-OpenGate-Cache-TTL": "60"})
		decodeJSON(t, r)
		if r.Header.Get("X-OpenGate-Cache") != "" {
			t.Fatal("client enabled caching on a project without cache_ttl_seconds")
		}
	}
	e.admin("PUT", "/admin/api/projects/"+proj+"/cache", map[string]any{"ttl_seconds": 60}, 200)
	kr := e.admin("POST", "/admin/api/keys", map[string]any{"name": "c2", "project_id": proj}, 201)
	k2 := kr["key"].(string)
	r := e.chat(k2, body)
	decodeJSON(t, r)
	r2 := e.chat(k2, body)
	decodeJSON(t, r2)
	r3 := e.chat(k1, body) // k1 has used 2 of 3 rpm
	decodeJSON(t, r3)
	if r.Header.Get("X-OpenGate-Cache") != "MISS" || r2.Header.Get("X-OpenGate-Cache") != "HIT" || r3.Header.Get("X-OpenGate-Cache") != "MISS" {
		t.Fatalf("per-key cache: %q %q %q", r.Header.Get("X-OpenGate-Cache"), r2.Header.Get("X-OpenGate-Cache"), r3.Header.Get("X-OpenGate-Cache"))
	}
	// k1 now has a cached entry but no rpm left: hits are rate limited too.
	r4 := e.chat(k1, body)
	if decodeJSON(t, r4); r4.StatusCode != 429 {
		t.Fatalf("cache hit bypassed rpm: %d %q", r4.StatusCode, r4.Header.Get("X-OpenGate-Cache"))
	}
}

// M3: a kill issued through another replica aborts this replica's stream.
func TestKillPropagatesAcrossReplicas(t *testing.T) {
	e := setup(t)
	key, keyID, _ := e.newKey("replica", "", nil)
	srvA, hA := e.newGateway()
	gwA := httptest.NewServer(hA)
	defer gwA.Close()
	req, _ := http.NewRequest("POST", gwA.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"openai/gpt-long","stream":true,"messages":[{"role":"user","content":"go"}]}`))
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("stream on A: %v %v", err, resp)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	for n := 0; n < 3 && sc.Scan(); {
		if strings.HasPrefix(sc.Text(), "data: ") {
			n++
		}
	}
	// Kill through the default gateway (replica B): nothing in flight there.
	if out := e.admin("POST", "/admin/api/keys/"+keyID+"/kill", nil, 200); out["aborted_inflight"].(float64) != 0 {
		t.Fatalf("B aborted %v", out["aborted_inflight"])
	}
	if n := srvA.SweepKills(context.Background()); n != 1 {
		t.Fatalf("sweep on A aborted %d", n)
	}
	rest, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(rest), "kill switch") || strings.Contains(string(rest), "[DONE]") {
		t.Fatalf("A's stream not aborted: %q", rest)
	}
}

// L8: optional instance-wide in-flight cap.
func TestMaxInflight(t *testing.T) {
	e := setupWith(t, func(c *config.Config) { c.MaxInflight = 1 })
	key, _, _ := e.newKey("busy", "", nil)
	resp := e.chat(key, map[string]any{"model": "openai/gpt-long", "stream": true, "messages": msgs("hold")})
	defer resp.Body.Close()
	bufio.NewReader(resp.Body).ReadString('\n')
	r := e.chat(key, map[string]any{"model": "openai/gpt-4o-mini", "messages": msgs("x")})
	if m := decodeJSON(t, r); r.StatusCode != 503 || errCode(m) != "gateway_overloaded" {
		t.Fatalf("want 503, got %d %v", r.StatusCode, m)
	}
}

// M5: per account+IP login lockout; X-Forwarded-For rightmost-hop handling.
func TestLoginLockoutPerAccountAndIP(t *testing.T) {
	e := setupWith(t, func(c *config.Config) {
		c.TrustProxyHeaders = true // test peer plays the reverse proxy
		c.AdminLockout = config.LockoutConfig{MaxFailures: 3, Window: time.Minute, Lockout: time.Minute}
	})
	e.mkUser("victim@example.test", "victim-password-123", "org_default", "owner")
	login := func(xff, pw string) int {
		req, _ := http.NewRequest("POST", e.gw.URL+"/admin/api/auth/login",
			strings.NewReader(`{"email":"victim@example.test","password":"`+pw+`"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Requested-With", "opengate")
		req.Header.Set("X-Forwarded-For", xff)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	// The attacker rotates a spoofed leftmost hop; the proxy-appended
	// rightmost hop is what counts, so the lockout still triggers.
	for i := 0; i < 3; i++ {
		if c := login("10.9.9."+string(rune('1'+i))+", 198.51.100.1", "guess"); c != 401 {
			t.Fatalf("guess %d: %d", i, c)
		}
	}
	if c := login("10.9.9.9, 198.51.100.1", "guess"); c != 429 {
		t.Fatalf("attacker IP should be locked: %d", c)
	}
	// The victim from a different IP is unaffected.
	if c := login("198.51.100.2", "victim-password-123"); c != 200 {
		t.Fatalf("victim locked out by attacker: %d", c)
	}
}

// Section 4: invites can't grant more than the inviter holds, and are
// single-use.
func TestInviteRoleCapAndSingleUse(t *testing.T) {
	e := setup(t)
	e.mkUser("admin@example.test", "admin-password-123", "org_default", "admin")
	b := e.browser()
	b.do("POST", "/admin/api/auth/login", map[string]any{"email": "admin@example.test", "password": "admin-password-123"}, 200)
	b.do("POST", "/admin/api/invites", map[string]any{"email": "boss@example.test", "role": "owner"}, 403)
	inv := b.do("POST", "/admin/api/invites", map[string]any{"email": "new@example.test", "role": "admin"}, 201)
	tok := inv["token"].(string)
	nb := e.browser()
	nb.do("POST", "/admin/api/auth/invites/accept", map[string]any{"token": tok, "name": "New", "password": "new-password-1234"}, 200)
	e.browser().do("POST", "/admin/api/auth/invites/accept", map[string]any{"token": tok, "name": "Again", "password": "new-password-1234"}, 404)
}

// v1.0.1 N1: another org's catch-all rule must not shadow this org's rules
// (evaluation, not just the admin API, is org-scoped).
func TestRoutingRulesAreOrgScoped(t *testing.T) {
	e := setup(t)
	acme := e.admin("POST", "/admin/api/orgs", map[string]any{"name": "Acme"}, 201)["id"].(string)
	e.adminOrg(acme, "POST", "/admin/api/provider-keys", map[string]any{"provider": "openai", "key": "sk-acme-only-123456"}, 201)
	e.adminOrg(acme, "POST", "/admin/api/routing/rules", map[string]any{"name": "acme-catch-all", "priority": 0,
		"target": "openai/gpt-4o-mini"}, 200)
	e.admin("POST", "/admin/api/routing/rules", map[string]any{"name": "default-rule", "priority": 10,
		"match": map[string]any{"models": []string{"openai/gpt-4o"}}, "target": "openai/gpt-4.1-nano"}, 200)
	key, _, _ := e.newKey("default-bot", "", nil)
	r := e.chat(key, map[string]any{"model": "openai/gpt-4o", "messages": msgs("hi")})
	decodeJSON(t, r)
	if r.StatusCode != 200 || r.Header.Get("X-OpenGate-Route") != "rule:default-rule" {
		t.Fatalf("default org routing shadowed by acme rule: %d route=%q", r.StatusCode, r.Header.Get("X-OpenGate-Route"))
	}
	ak := e.adminOrg(acme, "POST", "/admin/api/keys", map[string]any{"name": "acme-bot"}, 201)["key"].(string)
	r = e.chat(ak, map[string]any{"model": "openai/gpt-4o", "messages": msgs("hi")})
	decodeJSON(t, r)
	if r.Header.Get("X-OpenGate-Route") != "rule:acme-catch-all" {
		t.Fatalf("acme rule not applied to acme: %q", r.Header.Get("X-OpenGate-Route"))
	}
	pv := e.admin("POST", "/admin/api/routing/preview", map[string]any{"request": map[string]any{"model": "openai/gpt-4o", "messages": msgs("x")}}, 200)
	if pv["route"] != "rule:default-rule" {
		t.Fatalf("preview uses other org's rules: %v", pv["route"])
	}
}

// v1.0.1 N2/N4: invite preview takes the token in the body only, and
// accepting an invite never demotes an existing member (incl. the owner).
func TestInvitePreviewAndNoRoleOverwrite(t *testing.T) {
	e := setup(t)
	e.mkUser("owner@example.test", "owner-password-123", "org_default", "owner")
	e.mkUser("admin2@example.test", "admin2-password-123", "org_default", "admin")
	admin := e.browser()
	admin.do("POST", "/admin/api/auth/login", map[string]any{"email": "admin2@example.test", "password": "admin2-password-123"}, 200)
	tok := admin.do("POST", "/admin/api/invites", map[string]any{"email": "owner@example.test", "role": "viewer"}, 201)["token"].(string)
	// The old GET-with-token-in-path route is gone.
	req, _ := http.NewRequest("GET", e.gw.URL+"/admin/api/auth/invites/"+tok, nil)
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode == 200 {
		t.Fatal("token-in-URL preview still served")
	}
	ob := e.browser()
	pv := ob.do("POST", "/admin/api/auth/invites/preview", map[string]any{"token": tok}, 200)
	if pv["role"] != "viewer" || pv["existing_user"] != true {
		t.Fatalf("preview: %v", pv)
	}
	acc := ob.do("POST", "/admin/api/auth/invites/accept", map[string]any{"token": tok, "password": "owner-password-123"}, 200)
	if acc["role"] != "owner" {
		t.Fatalf("owner demoted by invite: %v", acc)
	}
	for _, m := range e.admin("GET", "/admin/api/members", nil, 200)["data"].([]any) {
		if mm := m.(map[string]any); mm["email"] == "owner@example.test" && mm["role"] != "owner" {
			t.Fatalf("membership changed: %v", mm)
		}
	}
}
