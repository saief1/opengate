package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"

	"github.com/saief1/opengate/internal/config"
	"github.com/saief1/opengate/internal/secure"
	"github.com/saief1/opengate/internal/store"
)

// browser is a cookie-carrying dashboard client.
type browser struct {
	t    *testing.T
	e    *env
	c    *http.Client
	csrf bool
}

func (e *env) browser() *browser {
	jar, _ := cookiejar.New(nil)
	return &browser{t: e.t, e: e, c: &http.Client{Jar: jar}, csrf: true}
}

func (b *browser) do(method, path string, body any, want int) map[string]any {
	b.t.Helper()
	var rd io.Reader
	if body != nil {
		j, _ := json.Marshal(body)
		rd = bytes.NewReader(j)
	}
	req, _ := http.NewRequest(method, b.e.gw.URL+path, rd)
	if b.csrf {
		req.Header.Set("X-Requested-With", "opengate")
	}
	resp, err := b.c.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != want {
		b.t.Fatalf("%s %s: %d, want %d: %v", method, path, resp.StatusCode, want, out)
	}
	return out
}

// adminOrg calls the admin API with the admin token acting on org.
func (e *env) adminOrg(org, method, path string, body any, want int) map[string]any {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		j, _ := json.Marshal(body)
		rd = bytes.NewReader(j)
	}
	req, _ := http.NewRequest(method, e.gw.URL+path, rd)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	req.Header.Set("X-OpenGate-Org", org)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != want {
		e.t.Fatalf("%s %s (org %s): %d, want %d: %v", method, path, org, resp.StatusCode, want, out)
	}
	return out
}

func (e *env) mkUser(email, pw, org, role string) {
	e.t.Helper()
	h, err := secure.HashPassword(pw)
	if err != nil {
		e.t.Fatal(err)
	}
	u, err := e.st.CreateUser(context.Background(), email, "", h)
	if err != nil {
		e.t.Fatal(err)
	}
	if err := e.st.AddMember(context.Background(), org, u.ID, role); err != nil {
		e.t.Fatal(err)
	}
}

func TestMultiTenantIsolation(t *testing.T) {
	e := setup(t)
	acme := e.admin("POST", "/admin/api/orgs", map[string]any{"name": "Acme"}, 201)["id"].(string)
	// Provider keys are per org: Acme brings its own.
	e.adminOrg(acme, "POST", "/admin/api/provider-keys", map[string]any{"provider": "openai", "key": "sk-acme-only-123456"}, 201)
	kr := e.adminOrg(acme, "POST", "/admin/api/keys", map[string]any{"name": "acme-bot"}, 201)
	acmeKey := kr["key"].(string)
	acmeKeyID := kr["virtual_key"].(map[string]any)["id"].(string)
	acmeProject := kr["virtual_key"].(map[string]any)["project_id"].(string)

	resp := e.chat(acmeKey, map[string]any{"model": "openai/gpt-4o", "messages": msgs("tenant hello")})
	decodeJSON(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("acme request: %d", resp.StatusCode)
	}
	// Default-org traffic.
	dk, _, _ := e.newKey("default-bot", "", nil)
	decodeJSON(t, e.chat(dk, map[string]any{"model": "openai/gpt-4o", "messages": msgs("hi")}))

	// Each org only sees its own projects, keys, logs, provider keys.
	for _, c := range []struct {
		org, path string
		n         int
	}{{acme, "/admin/api/keys", 1}, {acme, "/admin/api/logs", 1}, {acme, "/admin/api/provider-keys", 1}, {acme, "/admin/api/projects", 1}} {
		got := e.adminOrg(c.org, "GET", c.path, nil, 200)["data"].([]any)
		if len(got) != c.n {
			t.Fatalf("%s in acme: %d rows, want %d", c.path, len(got), c.n)
		}
	}
	for _, k := range e.admin("GET", "/admin/api/keys", nil, 200)["data"].([]any) {
		if k.(map[string]any)["id"] == acmeKeyID {
			t.Fatal("default org sees acme key")
		}
	}
	if pk := e.admin("GET", "/admin/api/provider-keys", nil, 200)["data"].([]any); len(pk) != 4 {
		t.Fatalf("default org provider keys %d", len(pk))
	}
	// Cross-org writes are refused as not found.
	e.admin("POST", "/admin/api/keys/"+acmeKeyID+"/kill", nil, 404)
	e.admin("PATCH", "/admin/api/projects/"+acmeProject, map[string]any{"log_bodies": true}, 404)
	e.admin("PUT", "/admin/api/limits", map[string]any{"scope": "key", "scope_id": acmeKeyID, "daily_usd": 1}, 404)
	e.admin("POST", "/admin/api/keys", map[string]any{"name": "sneaky", "project_id": acmeProject}, 404)
	ov := e.adminOrg(acme, "GET", "/admin/api/overview", nil, 200)
	if ov["today"].(map[string]any)["requests"].(float64) != 1 {
		t.Fatalf("acme overview: %v", ov["today"])
	}
	// Orgs without their own key can't silently use the operator's env keys.
	beta := e.admin("POST", "/admin/api/orgs", map[string]any{"name": "Beta"}, 201)["id"].(string)
	bk := e.adminOrg(beta, "POST", "/admin/api/keys", map[string]any{"name": "b"}, 201)["key"].(string)
	t.Setenv("OPENAI_API_KEY", "sk-operator-env-key-should-not-leak")
	resp = e.chat(bk, map[string]any{"model": "openai/gpt-4o", "messages": msgs("hi")})
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode == 200 {
		t.Fatal("beta used a key it does not own")
	}
}

func TestUsersSessionsRolesInvitesAudit(t *testing.T) {
	e := setup(t)
	e.mkUser("owner@example.com", "owner-password-1", store.DefaultOrgID, store.RoleOwner)

	cfg := e.browser().do("GET", "/admin/api/auth/config", nil, 200)
	if cfg["has_users"] != true || cfg["allow_signup"] != false {
		t.Fatalf("auth config: %v", cfg)
	}
	owner := e.browser()
	owner.do("POST", "/admin/api/auth/login", map[string]any{"email": "owner@example.com", "password": "wrong-password"}, 401)
	owner.do("POST", "/admin/api/auth/login", map[string]any{"email": "OWNER@example.com", "password": "owner-password-1"}, 200)
	me := owner.do("GET", "/admin/api/auth/me", nil, 200)
	if me["role"] != "owner" || me["kind"] != "user" {
		t.Fatalf("me: %v", me)
	}
	// Session works for the admin API; CSRF header is mandatory for writes.
	owner.do("GET", "/admin/api/overview", nil, 200)
	owner.csrf = false
	owner.do("POST", "/admin/api/projects", map[string]any{"name": "x"}, 403)
	owner.csrf = true
	owner.do("POST", "/admin/api/projects", map[string]any{"name": "team-proj"}, 201)

	// Invite a viewer and a member.
	inv := owner.do("POST", "/admin/api/invites", map[string]any{"email": "viewer@example.com", "role": "viewer"}, 201)
	tok := inv["token"].(string)
	if !strings.Contains(inv["path"].(string), tok) {
		t.Fatal("invite path")
	}
	viewer := e.browser()
	pv := viewer.do("POST", "/admin/api/auth/invites/preview", map[string]any{"token": tok}, 200)
	if pv["email"] != "viewer@example.com" || pv["existing_user"] != false {
		t.Fatalf("invite preview: %v", pv)
	}
	viewer.do("POST", "/admin/api/auth/invites/accept", map[string]any{"token": tok, "name": "Vi", "password": "short"}, 400)
	viewer.do("POST", "/admin/api/auth/invites/accept", map[string]any{"token": tok, "name": "Vi", "password": "viewer-password-1"}, 200)
	viewer.do("POST", "/admin/api/auth/invites/accept", map[string]any{"token": tok, "password": "viewer-password-1"}, 404) // single use
	// Viewer: read yes, write no.
	viewer.do("GET", "/admin/api/keys", nil, 200)
	viewer.do("POST", "/admin/api/keys", map[string]any{"name": "nope"}, 403)
	viewer.do("POST", "/admin/api/routing/preview", map[string]any{"request": map[string]any{"model": "openai/gpt-4o", "messages": msgs("x")}}, 200)

	mi := owner.do("POST", "/admin/api/invites", map[string]any{"email": "member@example.com", "role": "member"}, 201)
	member := e.browser()
	member.do("POST", "/admin/api/auth/invites/accept", map[string]any{"token": mi["token"], "password": "member-password-1"}, 200)
	kr := member.do("POST", "/admin/api/keys", map[string]any{"name": "member-key"}, 201)
	kid := kr["virtual_key"].(map[string]any)["id"].(string)
	member.do("POST", "/admin/api/keys/"+kid+"/kill", nil, 200) // kill switch: members allowed
	member.do("POST", "/admin/api/provider-keys", map[string]any{"provider": "openai", "key": "sk-x-1234567890"}, 403)
	member.do("POST", "/admin/api/invites", map[string]any{"email": "x@example.com", "role": "admin"}, 403)

	// Role management rules.
	members := owner.do("GET", "/admin/api/members", nil, 200)["data"].([]any)
	if len(members) != 3 {
		t.Fatalf("members: %d", len(members))
	}
	var ownerID, memberID string
	for _, m := range members {
		mm := m.(map[string]any)
		switch mm["email"] {
		case "owner@example.com":
			ownerID = mm["id"].(string)
		case "member@example.com":
			memberID = mm["id"].(string)
		}
	}
	owner.do("PATCH", "/admin/api/members/"+ownerID, map[string]any{"role": "admin"}, 400) // last owner
	owner.do("PATCH", "/admin/api/members/"+memberID, map[string]any{"role": "admin"}, 200)
	// The promoted admin cannot make itself owner.
	member.do("PATCH", "/admin/api/members/"+memberID, map[string]any{"role": "owner"}, 403)
	member.do("DELETE", "/admin/api/members/"+ownerID, nil, 403)

	// Audit log has the story, with actors.
	audit := owner.do("GET", "/admin/api/audit?limit=100", nil, 200)["data"].([]any)
	actions := map[string]string{}
	for _, a := range audit {
		am := a.(map[string]any)
		actions[am["action"].(string)] = am["actor_email"].(string)
	}
	for act, who := range map[string]string{"auth.login": "owner@example.com", "invite.create": "owner@example.com", "invite.accept": "viewer@example.com",
		"key.create": "member@example.com", "key.kill": "member@example.com", "member.role": "owner@example.com", "auth.login_failed": "owner@example.com"} {
		if actions[act] != who {
			t.Errorf("audit %s by %q, want %q (have %v)", act, actions[act], who, actions)
		}
	}
	// Denied attempts are audited with their status too.
	found403 := false
	for _, a := range audit {
		am := a.(map[string]any)
		if am["status"].(float64) == 403 && am["actor_email"] == "member@example.com" {
			found403 = true
		}
	}
	if !found403 {
		t.Error("forbidden mutation not audited")
	}

	// Logout invalidates the session server-side.
	owner.do("POST", "/admin/api/auth/logout", nil, 200)
	owner.do("GET", "/admin/api/overview", nil, 401)

	// Login lockout per email.
	locked := false
	for i := 0; i < e.cfg.AdminLockout.MaxFailures+1 && !locked; i++ {
		req, _ := http.NewRequest("POST", e.gw.URL+"/admin/api/auth/login", strings.NewReader(`{"email":"viewer@example.com","password":"bad-password-x"}`))
		req.Header.Set("X-Requested-With", "opengate")
		resp, _ := http.DefaultClient.Do(req)
		resp.Body.Close()
		locked = resp.StatusCode == 429
		if !locked && resp.StatusCode != 401 {
			t.Fatalf("login status %d", resp.StatusCode)
		}
	}
	e.browser().do("POST", "/admin/api/auth/login", map[string]any{"email": "viewer@example.com", "password": "viewer-password-1"}, 429)

	// Signup is off by default.
	e.browser().do("POST", "/admin/api/auth/signup", map[string]any{"email": "new@example.com", "password": "new-password-1"}, 403)
}

func TestSignupCreatesOrgAndSwitching(t *testing.T) {
	e := setupWith(t, func(c *config.Config) { c.Auth.AllowSignup = true })
	b := e.browser()
	out := b.do("POST", "/admin/api/auth/signup", map[string]any{"email": "founder@startup.io", "name": "Fo", "password": "founder-pass-1", "org_name": "Startup"}, 201)
	org := out["org"].(map[string]any)["id"].(string)
	me := b.do("GET", "/admin/api/auth/me", nil, 200)
	if me["org_id"] != org || me["role"] != "owner" {
		t.Fatalf("me: %v", me)
	}
	// New org starts empty and isolated, with its own default project.
	if ps := b.do("GET", "/admin/api/projects", nil, 200)["data"].([]any); len(ps) != 1 {
		t.Fatalf("projects: %v", ps)
	}
	second := b.do("POST", "/admin/api/orgs", map[string]any{"name": "Side project"}, 201)["id"].(string)
	b.do("POST", "/admin/api/auth/switch-org", map[string]any{"org_id": second}, 200)
	if b.do("GET", "/admin/api/org", nil, 200)["name"] != "Side project" {
		t.Fatal("switch failed")
	}
	b.do("POST", "/admin/api/auth/switch-org", map[string]any{"org_id": store.DefaultOrgID}, 403)
	b.do("POST", "/admin/api/auth/signup", map[string]any{"email": "founder@startup.io", "password": "founder-pass-1"}, 409)
}
