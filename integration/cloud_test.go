package integration

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/saief1/opengate/internal/cloud"
	"github.com/saief1/opengate/internal/config"
	"github.com/saief1/opengate/internal/stripemock"
)

const whsec = "whsec_test_secret_for_integration"

// setupCloud starts a cloud-mode gateway wired to a mock Stripe whose
// webhooks are delivered back to the gateway.
func setupCloud(t *testing.T, mutate func(*config.Config)) (*env, *stripemock.Mock) {
	t.Helper()
	m := stripemock.New("", "", whsec)
	m.APIKey = "sk_test_mock"
	ms := httptest.NewServer(m.Handler())
	t.Cleanup(ms.Close)
	m.BaseURL = ms.URL
	e := setupWith(t, func(c *config.Config) {
		c.Cloud.Enabled = true
		c.Cloud.PublicURL = "http://127.0.0.1:1"
		c.Cloud.Stripe.BaseURL = ms.URL
		c.StripeAPIKey, c.StripeWebhookSecret = "sk_test_mock", whsec
		c.Cloud.Plans = config.DefaultCloudPlans()
		for i := range c.Cloud.Plans {
			switch c.Cloud.Plans[i].ID {
			case "free":
				c.Cloud.Plans[i].MaxRequestsMonth, c.Cloud.Plans[i].MaxProjects, c.Cloud.Plans[i].MaxMembers = 3, 2, 2
			case "team":
				c.Cloud.Plans[i].StripePriceID = "price_team"
			}
		}
		if mutate != nil {
			mutate(c)
		}
	})
	m.WebhookURL = e.gw.URL + "/cloud/stripe/webhook"
	return e, m
}

func (b *browser) chatStatus(key string) (int, string) {
	b.t.Helper()
	r := b.e.chat(key, map[string]any{"model": "openai/gpt-4o-mini", "messages": msgs("hi")})
	return r.StatusCode, errCode(decodeJSON(b.t, r))
}

func cloudSignup(t *testing.T, e *env, email, org string) (*browser, string, string) {
	t.Helper()
	b := e.browser()
	out := b.do("POST", "/admin/api/auth/signup", map[string]any{"email": email, "name": "Founder", "password": "founder-pass-123", "org_name": org}, 201)
	orgID := out["org"].(map[string]any)["id"].(string)
	b.do("POST", "/admin/api/provider-keys", map[string]any{"provider": "openai", "key": "sk-tenant-own-key-123"}, 201)
	key := b.do("POST", "/admin/api/keys", map[string]any{"name": "app"}, 201)["key"].(string)
	return b, orgID, key
}

func TestCloudSignupCheckoutLifecycle(t *testing.T) {
	e, m := setupCloud(t, nil)
	if cfg := decodeJSON(t, mustGet(t, e.gw.URL+"/admin/api/auth/config")); cfg["cloud"] != true || cfg["allow_signup"] != true {
		t.Fatalf("auth config: %v", cfg)
	}
	b, org, key := cloudSignup(t, e, "founder@startup.test", "Startup")

	bill := b.do("GET", "/admin/api/billing", nil, 200)
	if bill["effective_plan"].(map[string]any)["id"] != "free" || bill["can_manage"] != true {
		t.Fatalf("new org billing: %v", bill)
	}
	selfServe := map[string]bool{}
	for _, p := range bill["plans"].([]any) {
		pm := p.(map[string]any)
		selfServe[pm["id"].(string)] = pm["self_serve"].(bool)
		if _, leaked := pm["stripe_price_id"]; leaked {
			t.Fatal("price ids must not be exposed")
		}
	}
	if !selfServe["team"] || selfServe["enterprise"] || selfServe["free"] {
		t.Fatalf("self-serve flags: %v", selfServe)
	}

	// Free plan: 3 requests / month, then a clear quota error.
	for i := 0; i < 3; i++ {
		if st, code := b.chatStatus(key); st != 200 {
			t.Fatalf("request %d: %d %s", i, st, code)
		}
	}
	if st, code := b.chatStatus(key); st != 429 || code != "plan_quota_exceeded" {
		t.Fatalf("over quota: %d %s", st, code)
	}
	if u := b.do("GET", "/admin/api/billing", nil, 200)["usage"].(map[string]any); u["requests"].(float64) != 4 {
		t.Fatalf("usage: %v", u)
	}
	// Free plan: 2 projects (the default one + one more).
	b.do("POST", "/admin/api/projects", map[string]any{"name": "second"}, 201)
	if out := b.do("POST", "/admin/api/projects", map[string]any{"name": "third"}, 402); errCode(out) != "plan_limit_reached" {
		t.Fatalf("project limit: %v", out)
	}

	// Checkout.
	b.do("POST", "/admin/api/billing/checkout", map[string]any{"plan": "enterprise"}, 400)
	b.do("POST", "/admin/api/billing/portal", nil, 409) // no billing account yet
	co := b.do("POST", "/admin/api/billing/checkout", map[string]any{"plan": "team"}, 200)
	url := co["url"].(string)
	if !strings.HasPrefix(url, m.BaseURL+"/checkout/cs_test_") {
		t.Fatalf("checkout url: %s", url)
	}
	if r := mustGet(t, url); r.StatusCode != 200 {
		t.Fatalf("hosted checkout page: %d", r.StatusCode)
	}
	// A second click before paying reuses the same Stripe customer.
	b.do("POST", "/admin/api/billing/checkout", map[string]any{"plan": "team"}, 200)
	redirect, err := m.Pay(co["id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(redirect, "http://127.0.0.1:1/ui/#billing?checkout=success&session_id=cs_test_") {
		t.Fatalf("success redirect: %s", redirect)
	}
	n := 0
	for _, c := range m.Calls() {
		if c == "POST /v1/customers" {
			n++
		}
	}
	bill = b.do("GET", "/admin/api/billing", nil, 200)
	if bill["plan"] != "team" || bill["status"] != "active" || bill["effective_plan"].(map[string]any)["id"] != "team" || bill["has_subscription"] != true {
		t.Fatalf("after checkout: %v", bill)
	}
	cust, err := e.st.GetOrgBilling(context.Background(), org, "free")
	if err != nil || !strings.HasPrefix(cust.StripeCustomerID, "cus_") {
		t.Fatalf("customer link: %+v %v", cust, err)
	}
	// Two checkout clicks before the first payment: the customer is created
	// once, then reused (stored on the org; idempotency key at Stripe).
	if n != 1 {
		t.Fatalf("stripe customers created: %d", n)
	}
	if st, code := b.chatStatus(key); st != 200 {
		t.Fatalf("paid plan request: %d %s", st, code)
	}
	b.do("POST", "/admin/api/projects", map[string]any{"name": "third"}, 201)
	b.do("POST", "/admin/api/billing/checkout", map[string]any{"plan": "team"}, 409)

	// Portal + lifecycle events.
	if p := b.do("POST", "/admin/api/billing/portal", nil, 200); !strings.HasPrefix(p["url"].(string), m.BaseURL+"/portal/") {
		t.Fatalf("portal: %v", p)
	}
	sub := m.SubscriptionFor(cust.StripeCustomerID)
	if err := m.FailPayment(sub); err != nil {
		t.Fatal(err)
	}
	bill = b.do("GET", "/admin/api/billing", nil, 200)
	if bill["status"] != "past_due" || bill["effective_plan"].(map[string]any)["id"] != "team" {
		t.Fatalf("past_due within grace keeps the plan: %v", bill)
	}
	if err := m.RecoverPayment(sub); err != nil {
		t.Fatal(err)
	}
	if err := m.CancelAtPeriodEnd(sub); err != nil {
		t.Fatal(err)
	}
	bill = b.do("GET", "/admin/api/billing", nil, 200)
	if bill["status"] != "active" || bill["cancel_at_period_end"] != true {
		t.Fatalf("cancel at period end: %v", bill)
	}
	if err := m.Cancel(sub); err != nil {
		t.Fatal(err)
	}
	bill = b.do("GET", "/admin/api/billing", nil, 200)
	if bill["plan"] != "free" || bill["status"] != "canceled" || bill["has_subscription"] != false {
		t.Fatalf("canceled: %v", bill)
	}
	if st, code := b.chatStatus(key); st != 429 || code != "plan_quota_exceeded" {
		t.Fatalf("back on free: %d %s", st, code)
	}
	// Plan changes are in the org's audit log, attributed to Stripe.
	audit := b.do("GET", "/admin/api/audit", nil, 200)["data"].([]any)
	stripeEntries := 0
	for _, a := range audit {
		if am := a.(map[string]any); am["actor_type"] == "stripe" && am["action"] == "billing.subscription" {
			stripeEntries++
		}
	}
	if stripeEntries < 4 {
		t.Fatalf("expected billing audit entries, got %d", stripeEntries)
	}
	// Resubscribing after cancellation works and reuses the customer.
	co = b.do("POST", "/admin/api/billing/checkout", map[string]any{"plan": "team"}, 200)
	if _, err := m.Pay(co["id"].(string)); err != nil {
		t.Fatal(err)
	}
	if after, _ := e.st.GetOrgBilling(context.Background(), org, "free"); after.Plan != "team" || after.StripeCustomerID != cust.StripeCustomerID {
		t.Fatalf("resubscribe: %+v", after)
	}
}

func mustGet(t *testing.T, url string) *http.Response {
	t.Helper()
	r, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func postWebhook(t *testing.T, e *env, body []byte, sig string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("POST", e.gw.URL+"/cloud/stripe/webhook", bytes.NewReader(body))
	if sig != "" {
		req.Header.Set("Stripe-Signature", sig)
	}
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return r.StatusCode, decodeJSON(t, r)
}

func TestCloudWebhookSecurity(t *testing.T) {
	e, m := setupCloud(t, nil)
	a, orgA, _ := cloudSignup(t, e, "a@a.test", "A")
	_, orgB, _ := cloudSignup(t, e, "b@b.test", "B")
	co := a.do("POST", "/admin/api/billing/checkout", map[string]any{"plan": "team"}, 200)
	if _, err := m.Pay(co["id"].(string)); err != nil {
		t.Fatal(err)
	}
	ba, _ := e.st.GetOrgBilling(context.Background(), orgA, "free")
	subA := ba.StripeSubscriptionID

	// Signatures: missing, wrong secret, stale timestamp are all rejected.
	body, sig := m.Event("customer.subscription.updated", m.Subscription(subA))
	if st, _ := postWebhook(t, e, body, ""); st != 400 {
		t.Fatalf("unsigned: %d", st)
	}
	if st, _ := postWebhook(t, e, body, cloud.Sign(body, "whsec_wrong", time.Now())); st != 400 {
		t.Fatalf("wrong secret: %d", st)
	}
	if st, _ := postWebhook(t, e, body, cloud.Sign(body, whsec, time.Now().Add(-10*time.Minute))); st != 400 {
		t.Fatalf("stale: %d", st)
	}
	// A valid event is processed once; the replay is acknowledged as a duplicate.
	if st, out := postWebhook(t, e, body, sig); st != 200 || out["duplicate"] == true {
		t.Fatalf("valid: %d %v", st, out)
	}
	if st, out := postWebhook(t, e, body, sig); st != 200 || out["duplicate"] != true {
		t.Fatalf("replay: %d %v", st, out)
	}

	// A subscription of another customer that claims org A in its metadata
	// must not touch org A.
	m.AddSubscription("sub_foreign", "cus_someone_else", "active", "price_team", map[string]string{"org_id": orgA})
	if err := m.Deliver("customer.subscription.updated", m.Subscription("sub_foreign")); err != nil {
		t.Fatal(err)
	}
	// A checkout session completed for org B carrying org A's subscription is ignored too.
	if err := m.Deliver("checkout.session.completed", map[string]any{"id": "cs_x", "mode": "subscription", "client_reference_id": orgB,
		"customer": ba.StripeCustomerID, "subscription": subA}); err != nil {
		t.Fatal(err)
	}
	// Unknown org and unknown price are acknowledged and ignored.
	m.AddSubscription("sub_ghost", "cus_ghost", "active", "price_team", map[string]string{"org_id": "org_does_not_exist"})
	m.AddSubscription("sub_oddprice", "cus_odd", "active", "price_unknown", map[string]string{"org_id": orgB})
	for _, s := range []string{"sub_ghost", "sub_oddprice"} {
		if err := m.Deliver("customer.subscription.created", m.Subscription(s)); err != nil {
			t.Fatal(err)
		}
	}
	if after, _ := e.st.GetOrgBilling(context.Background(), orgA, "free"); after.Plan != "team" || after.Status != "active" || after.StripeSubscriptionID != subA {
		t.Fatalf("org A changed by foreign events: %+v", after)
	}
	if after, _ := e.st.GetOrgBilling(context.Background(), orgB, "free"); after.Plan != "free" || after.StripeCustomerID != "" {
		t.Fatalf("org B changed: %+v", after)
	}

	// Out-of-order delivery: an old "active" payload after the subscription
	// was canceled can't resurrect the plan (the webhook re-fetches state).
	stale := m.Subscription(subA)
	m.SetStatus(subA, "canceled")
	if err := m.Deliver("customer.subscription.updated", stale); err != nil {
		t.Fatal(err)
	}
	if after, _ := e.st.GetOrgBilling(context.Background(), orgA, "free"); after.Plan != "free" {
		t.Fatalf("stale payload resurrected plan: %+v", after)
	}
}

func TestCloudRolesAndLimits(t *testing.T) {
	e, _ := setupCloud(t, func(c *config.Config) { c.Cloud.MaxOrgsPerUser = 2 })
	owner, org, _ := cloudSignup(t, e, "owner@x.test", "X")

	// Seats: free plan has 2 (owner + 1 pending invite fills it).
	tok := owner.do("POST", "/admin/api/invites", map[string]any{"email": "admin@x.test", "role": "admin"}, 201)["token"].(string)
	if out := owner.do("POST", "/admin/api/invites", map[string]any{"email": "third@x.test"}, 402); errCode(out) != "plan_limit_reached" {
		t.Fatalf("seat limit: %v", out)
	}
	adm := e.browser()
	adm.do("POST", "/admin/api/auth/invites/accept", map[string]any{"token": tok, "name": "Ad", "password": "admin-pass-1234"}, 200)

	// Only owners manage billing; admins can view it.
	if out := adm.do("GET", "/admin/api/billing", nil, 200); out["can_manage"] != false {
		t.Fatalf("admin can_manage: %v", out)
	}
	adm.do("POST", "/admin/api/billing/checkout", map[string]any{"plan": "team"}, 403)
	adm.do("POST", "/admin/api/billing/portal", nil, 403)
	// Plan assignment is operator-only.
	owner.do("PUT", "/admin/api/billing/plan", map[string]any{"plan": "enterprise"}, 403)
	if out := e.adminOrg(org, "PUT", "/admin/api/billing/plan", map[string]any{"plan": "enterprise"}, 200); out["plan"] != "enterprise" {
		t.Fatalf("operator plan: %v", out)
	}
	if out := owner.do("GET", "/admin/api/billing", nil, 200); out["effective_plan"].(map[string]any)["id"] != "enterprise" {
		t.Fatalf("enterprise not effective: %v", out)
	}
	owner.do("POST", "/admin/api/invites", map[string]any{"email": "third@x.test"}, 201) // unlimited seats now

	// Org ownership cap per user.
	owner.do("POST", "/admin/api/orgs", map[string]any{"name": "Second"}, 201)
	if out := owner.do("POST", "/admin/api/orgs", map[string]any{"name": "Third"}, 403); errCode(out) != "org_limit_reached" {
		t.Fatalf("org cap: %v", out)
	}
	// The operator's default org is never plan-limited.
	for i := 0; i < 4; i++ {
		e.admin("POST", "/admin/api/projects", map[string]any{"name": "op" + string(rune('a'+i))}, 201)
	}
	if out := e.admin("GET", "/admin/api/billing", nil, 200); out["effective_plan"].(map[string]any)["id"] != "operator" {
		t.Fatalf("default org plan: %v", out)
	}
}

func TestCloudSignupThrottlePerIP(t *testing.T) {
	e, _ := setupCloud(t, func(c *config.Config) { c.AdminLockout.MaxFailures = 3 })
	for i := 0; i < 3; i++ {
		e.browser().do("POST", "/admin/api/auth/signup", map[string]any{"email": "u" + string(rune('a'+i)) + "@spam.test", "password": "spam-password-123"}, 201)
	}
	e.browser().do("POST", "/admin/api/auth/signup", map[string]any{"email": "ud@spam.test", "password": "spam-password-123"}, 429)
}
