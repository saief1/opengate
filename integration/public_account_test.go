package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/saief1/opengate/internal/cloud"
	"github.com/saief1/opengate/internal/config"
	"github.com/saief1/opengate/internal/mailer"
	"github.com/saief1/opengate/internal/pricing"
	"github.com/saief1/opengate/internal/server"
	"github.com/saief1/opengate/internal/store"
	"github.com/saief1/opengate/web"
)

type captureMail struct {
	mu   sync.Mutex
	msgs []mailer.Message
}

func (c *captureMail) Configured() bool { return true }

func (c *captureMail) Send(_ context.Context, m mailer.Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.msgs = append(c.msgs, m)
	return nil
}

func (c *captureMail) last() mailer.Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.msgs) == 0 {
		return mailer.Message{}
	}
	return c.msgs[len(c.msgs)-1]
}

func (c *captureMail) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.msgs)
}

func gatewayWithMail(t *testing.T, e *env, m mailer.Sender) *httptest.Server {
	t.Helper()
	e.mail = m
	_, h := e.newGateway()
	gw := httptest.NewServer(h)
	t.Cleanup(gw.Close)
	e.gw = gw
	return gw
}

func publicClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}

func publicJSON(t *testing.T, c *http.Client, method, rawURL string, body any) (int, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, rawURL, rd)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Requested-With", "opengate")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func tokenFromMail(t *testing.T, body, path string) string {
	t.Helper()
	i := strings.Index(body, path)
	if i < 0 {
		t.Fatalf("email body has no %s: %q", path, body)
	}
	start := strings.LastIndex(body[:i], "http")
	if start < 0 {
		t.Fatalf("email body has no URL: %q", body)
	}
	end := i + len(path)
	for end < len(body) && body[end] != ' ' && body[end] != '\n' && body[end] != '\r' {
		end++
	}
	u, err := url.Parse(body[start:end])
	if err != nil {
		t.Fatal(err)
	}
	tok := u.Query().Get("token")
	if tok == "" {
		t.Fatalf("no token in %s", u)
	}
	return tok
}

func TestPublicSignupVerifyReset(t *testing.T) {
	cap := &captureMail{}
	e := setupWith(t, func(c *config.Config) { c.Auth.AllowSignup = true })
	gw := gatewayWithMail(t, e, cap)
	c := publicClient(t)

	if st, out := publicJSON(t, c, "POST", gw.URL+"/signup", map[string]any{
		"email": "ada@example.com", "name": "Ada", "password": "correct-horse", "org_name": "Analytical",
	}); st != 400 || errCode(out) != "terms_required" {
		t.Fatalf("terms: %d %v", st, out)
	}

	st, out := publicJSON(t, c, "POST", gw.URL+"/signup", map[string]any{
		"email": "ada@example.com", "name": "Ada", "password": "correct-horse", "org_name": "Analytical", "accept_terms": true,
	})
	if st != 201 || out["verification_required"] != true {
		t.Fatalf("signup: %d %v", st, out)
	}
	user := out["user"].(map[string]any)
	if user["email_verified"] == true {
		t.Fatal("configured mailer must leave the account unverified")
	}
	orgID := out["org"].(map[string]any)["id"].(string)
	base, _ := url.Parse(gw.URL)
	for _, ck := range c.Jar.Cookies(base) {
		if ck.Name == "og_session" {
			t.Fatal("signup must not start a session before verification")
		}
	}
	if st, out := publicJSON(t, c, "POST", gw.URL+"/login", map[string]any{"email": "ada@example.com", "password": "correct-horse"}); st != 403 || errCode(out) != "email_unverified" {
		t.Fatalf("login before verify: %d %v", st, out)
	}

	tok := tokenFromMail(t, cap.last().Text, "/verify-email?token=")
	if cap.last().To != "ada@example.com" || !strings.Contains(cap.last().Subject, "Verify") {
		t.Fatalf("verify mail: %+v", cap.last())
	}
	if st, out := publicJSON(t, c, "POST", gw.URL+"/verify-email", map[string]any{"token": tok}); st != 200 || out["status"] != "verified" {
		t.Fatalf("verify: %d %v", st, out)
	}
	if st, out := publicJSON(t, c, "GET", gw.URL+"/admin/api/auth/me", nil); st != 200 {
		t.Fatalf("me: %d %v", st, out)
	}
	ms, err := e.st.ListMembers(context.Background(), orgID)
	if err != nil || len(ms) != 1 || ms[0].Role != store.RoleOwner || ms[0].Email != "ada@example.com" {
		t.Fatalf("owner membership: %+v %v", ms, err)
	}
	if st, _ := publicJSON(t, c, "POST", gw.URL+"/verify-email", map[string]any{"token": tok}); st != 400 {
		t.Fatalf("replay verify: %d", st)
	}

	if st, out := publicJSON(t, c, "POST", gw.URL+"/logout", map[string]any{}); st != 200 || out["status"] != "signed_out" {
		t.Fatalf("logout: %d %v", st, out)
	}
	if st, _ := publicJSON(t, c, "GET", gw.URL+"/admin/api/auth/me", nil); st != 401 {
		t.Fatalf("me after logout: %d", st)
	}

	before := cap.len()
	if st, out := publicJSON(t, c, "POST", gw.URL+"/forgot-password", map[string]any{"email": "nobody@example.com"}); st != 200 {
		t.Fatalf("unknown forgot: %d %v", st, out)
	}
	if cap.len() != before {
		t.Fatal("unknown address must not send mail")
	}
	if st, out := publicJSON(t, c, "POST", gw.URL+"/forgot-password", map[string]any{"email": "ada@example.com"}); st != 200 {
		t.Fatalf("forgot: %d %v", st, out)
	}
	reset := tokenFromMail(t, cap.last().Text, "/reset-password?token=")
	if st, out := publicJSON(t, c, "POST", gw.URL+"/reset-password", map[string]any{"token": reset, "password": "new-password-1"}); st != 200 || out["status"] != "password_reset" {
		t.Fatalf("reset: %d %v", st, out)
	}
	if st, _ := publicJSON(t, c, "POST", gw.URL+"/reset-password", map[string]any{"token": reset, "password": "another-password"}); st != 400 {
		t.Fatalf("replay reset: %d", st)
	}
	if st, out := publicJSON(t, c, "POST", gw.URL+"/login", map[string]any{"email": "ada@example.com", "password": "correct-horse"}); st != 401 || errCode(out) != "invalid_credentials" {
		t.Fatalf("old password: %d %v", st, out)
	}
	st, out = publicJSON(t, c, "POST", gw.URL+"/login", map[string]any{"email": "ada@example.com", "password": "new-password-1"})
	if st != 200 || out["user"].(map[string]any)["email_verified"] != true {
		t.Fatalf("login after reset: %d %v", st, out)
	}

	page, err := http.Get(gw.URL + "/terms")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(page.Body)
	page.Body.Close()
	text := string(body)
	if page.StatusCode != 200 || !strings.Contains(text, "DRAFT") || !strings.Contains(text, "Quebec") || !strings.Contains(text, "OpenGate") {
		t.Fatalf("terms: %d %s", page.StatusCode, text)
	}
	if csp := page.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") || page.Header.Get("X-Frame-Options") != "DENY" {
		t.Fatalf("headers csp=%q frame=%q", csp, page.Header.Get("X-Frame-Options"))
	}
	for _, p := range []string{"/privacy", "/pricing", "/signup", "/login"} {
		resp, err := http.Get(gw.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.Contains(string(b), "/terms") {
			t.Fatalf("%s: %d", p, resp.StatusCode)
		}
	}
}

func TestPublicSignupRateLimit(t *testing.T) {
	e := setupWith(t, func(c *config.Config) { c.Auth.AllowSignup = true })
	c := publicClient(t)
	for i := 0; i < 5; i++ {
		st, out := publicJSON(t, c, "POST", e.gw.URL+"/signup", map[string]any{
			"email": "short@example.com", "password": "tiny", "accept_terms": true,
		})
		if st != 400 {
			t.Fatalf("attempt %d: %d %v", i, st, out)
		}
	}
	if st, out := publicJSON(t, c, "POST", e.gw.URL+"/signup", map[string]any{
		"email": "short@example.com", "password": "tiny", "accept_terms": true,
	}); st != 429 {
		t.Fatalf("rate limit: %d %v", st, out)
	}
}

func TestPublicSignupDisabledByDefault(t *testing.T) {
	e := setup(t)
	c := publicClient(t)
	st, out := publicJSON(t, c, "POST", e.gw.URL+"/signup", map[string]any{
		"email": "ada@example.com", "password": "correct-horse", "accept_terms": true,
	})
	if st != 403 || errCode(out) != "signup_disabled" {
		t.Fatalf("signup disabled: %d %v", st, out)
	}
}

func TestPlaygroundGating(t *testing.T) {
	e := setup(t)
	resp, err := http.Post(e.gw.URL+"/public/playground", "application/json", strings.NewReader(`{"preset":"simple"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("playground default: %d", resp.StatusCode)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	prices, _ := pricing.NewRegistry("")
	if _, err := server.New(context.Background(), server.Options{
		Config: e.cfg, Store: e.st, Cipher: e.cipher, Pricing: prices, Logger: log, WebFS: web.FS(),
		Demo: false, Playground: true, Mailer: &mailer.Log{Log: log},
	}); err == nil || !strings.Contains(err.Error(), "playground") {
		t.Fatalf("playground without demo: %v", err)
	}

	off, err := server.New(context.Background(), server.Options{
		Config: e.cfg, Store: e.st, Cipher: e.cipher, Pricing: prices, Logger: log, WebFS: web.FS(),
		Demo: true, Playground: false, Mailer: &mailer.Log{Log: log},
	})
	if err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(off.Handler())
	t.Cleanup(gw.Close)
	resp, err = http.Post(gw.URL+"/public/playground", "application/json", strings.NewReader(`{"preset":"simple"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("demo without playground flag: %d", resp.StatusCode)
	}

	cfg := *e.cfg
	providers := map[string]config.ProviderConfig{}
	for name, p := range e.cfg.Providers {
		providers[name] = p
	}
	providers["openai"] = config.ProviderConfig{Type: config.TypeOpenAI, BaseURL: "https://api.openai.com/v1"}
	cfg.Providers = providers
	live, err := server.New(context.Background(), server.Options{
		Config: &cfg, Store: e.st, Cipher: e.cipher, Pricing: prices, Logger: log, WebFS: web.FS(),
		Demo: true, Playground: true, Mailer: &mailer.Log{Log: log},
	})
	if err != nil {
		t.Fatal(err)
	}
	gw2 := httptest.NewServer(live.Handler())
	t.Cleanup(gw2.Close)
	resp, err = http.Post(gw2.URL+"/public/playground", "application/json", strings.NewReader(`{"preset":"simple"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out := decodeJSON(t, resp)
	if resp.StatusCode != http.StatusForbidden || errCode(out) != "playground_disabled" {
		t.Fatalf("non-mock playground: %d %v", resp.StatusCode, out)
	}
}

func TestCloudWebhookTamperedBody(t *testing.T) {
	e, m := setupCloud(t, nil)
	body, _ := m.Event("customer.subscription.updated", map[string]any{"id": "sub_missing"})
	sig := cloud.Sign(body, whsec, time.Now())
	body[len(body)/2] ^= 0x01
	if st, out := postWebhook(t, e, body, sig); st != 400 {
		t.Fatalf("tampered body: %d %v", st, out)
	}
}
