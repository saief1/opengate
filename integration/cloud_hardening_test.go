package integration

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/opengate-dev/opengate/internal/cloud"
	"github.com/opengate-dev/opengate/internal/config"
	"github.com/opengate-dev/opengate/internal/store"
	"github.com/opengate-dev/opengate/internal/stripemock"
)

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func auditActions(t *testing.T, b *browser) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, a := range b.do("GET", "/admin/api/audit", nil, 200)["data"].([]any) {
		out[a.(map[string]any)["action"].(string)]++
	}
	return out
}

func cloudPlanID(e *env, b *store.OrgBilling) string {
	return cloud.Effective(&e.cfg.Cloud, b, time.Now()).ID
}

func withPro(c *config.Config) {
	c.Cloud.Plans = append(c.Cloud.Plans, config.CloudPlan{ID: "pro", Name: "Pro", PriceUSD: 99, StripePriceID: "price_pro", MaxRequestsMonth: 1000, MaxProjects: 10, MaxMembers: 10})
	c.Cloud.Stripe.TrialDays, c.Cloud.Stripe.TrialWithoutCard = 14, true
}

// A1: at most one payable Checkout per org, adoption of a subscription the
// webhook missed, and cancellation of a duplicate live subscription.
func TestCloudNoDuplicateSubscriptions(t *testing.T) {
	e, m := setupCloud(t, withPro)
	b, org, _ := cloudSignup(t, e, "dup@x.test", "Dup")

	co1 := b.do("POST", "/admin/api/billing/checkout", map[string]any{"plan": "team"}, 200)
	// Trial without card on the first subscription.
	if days, pmc := m.SessionTrial(co1["id"].(string)); days != 14 || pmc != "if_required" {
		t.Fatalf("trial params: %d %q", days, pmc)
	}
	// Second click: same open session, not a new one.
	co1b := b.do("POST", "/admin/api/billing/checkout", map[string]any{"plan": "team"}, 200)
	if co1b["id"] != co1["id"] || co1b["reused"] != true {
		t.Fatalf("second click opened another session: %v vs %v", co1b, co1)
	}
	// Switching plan expires the old session before opening a new one.
	co2 := b.do("POST", "/admin/api/billing/checkout", map[string]any{"plan": "pro"}, 200)
	if co2["id"] == co1["id"] || m.SessionStatus(co1["id"].(string)) != "expired" {
		t.Fatalf("plan switch: old session %s is %s", co1["id"], m.SessionStatus(co1["id"].(string)))
	}
	if _, err := m.Pay(co1["id"].(string)); err == nil {
		t.Fatal("expired session could still be paid")
	}
	if _, err := m.Pay(co2["id"].(string)); err != nil {
		t.Fatal(err)
	}
	cur, _ := e.st.GetOrgBilling(context.Background(), org, "free")
	if cur.Plan != "pro" || cur.Status != "trialing" || cur.CheckoutSessionID != "" {
		t.Fatalf("after pay: %+v", cur)
	}

	// A second live subscription for the same customer (e.g. created in the
	// Dashboard or a raced Checkout) is canceled, not applied.
	m.AddSubscription("sub_dup", cur.StripeCustomerID, "active", "price_team", map[string]string{"org_id": org})
	if err := m.Deliver("customer.subscription.created", m.Subscription("sub_dup")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "duplicate canceled", func() bool { return m.Subscription("sub_dup")["status"] == "canceled" })
	after, _ := e.st.GetOrgBilling(context.Background(), org, "free")
	if after.StripeSubscriptionID != cur.StripeSubscriptionID || after.Plan != "pro" {
		t.Fatalf("duplicate replaced current: %+v", after)
	}
	if auditActions(t, b)["billing.subscription.duplicate_canceled"] != 1 {
		t.Fatal("duplicate cancellation not audited")
	}
	// The later customer.subscription.deleted for the duplicate is ignored.
	waitFor(t, "deleted event", func() bool {
		for _, ev := range m.Events() {
			if ev == "customer.subscription.deleted" {
				return true
			}
		}
		return false
	})
	if after, _ := e.st.GetOrgBilling(context.Background(), org, "free"); after.Plan != "pro" || after.Status != "trialing" {
		t.Fatalf("duplicate's deletion changed the org: %+v", after)
	}

	// Missed webhook: Stripe has a live subscription the org row doesn't
	// know about; Checkout adopts it and refuses to open another.
	b2, org2, _ := cloudSignup(t, e, "missed@x.test", "Missed")
	co := b2.do("POST", "/admin/api/billing/checkout", map[string]any{"plan": "team"}, 200)
	bil2, _ := e.st.GetOrgBilling(context.Background(), org2, "free")
	m.AddSubscription("sub_missed", bil2.StripeCustomerID, "active", "price_team", map[string]string{"org_id": org2})
	if out := b2.do("POST", "/admin/api/billing/checkout", map[string]any{"plan": "team"}, 409); errCode(out) != "subscription_exists" {
		t.Fatalf("missed webhook: %v", out)
	}
	if got, _ := e.st.GetOrgBilling(context.Background(), org2, "free"); got.Plan != "team" || got.StripeSubscriptionID != "sub_missed" {
		t.Fatalf("not adopted: %+v", got)
	}
	// ...and the stale open session can no longer be paid.
	if st := m.SessionStatus(co["id"].(string)); st != "expired" {
		t.Fatalf("stale session after adoption: %s", st)
	}
	_ = co
	// A failed first payment (incomplete) doesn't block a new Checkout; it
	// is canceled so it can't complete next to the new subscription.
	b4, org4, _ := cloudSignup(t, e, "incomplete@x.test", "Incomplete")
	b4.do("POST", "/admin/api/billing/checkout", map[string]any{"plan": "team"}, 200)
	bil4, _ := e.st.GetOrgBilling(context.Background(), org4, "free")
	m.AddSubscription("sub_incomplete", bil4.StripeCustomerID, "incomplete", "price_team", map[string]string{"org_id": org4})
	if err := m.Deliver("customer.subscription.created", m.Subscription("sub_incomplete")); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.st.GetOrgBilling(context.Background(), org4, "free"); got.Status != "incomplete" || cloudPlanID(e, got) != "free" {
		t.Fatalf("incomplete: %+v", got)
	}
	co4 := b4.do("POST", "/admin/api/billing/checkout", map[string]any{"plan": "team"}, 200)
	if co4["reused"] != true {
		t.Fatal("an incomplete subscription must not expire the open Checkout")
	}
	if m.Subscription("sub_incomplete")["status"] != "canceled" {
		t.Fatal("incomplete subscription left open")
	}
	if _, err := m.Pay(co4["id"].(string)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "org4 on team", func() bool {
		got, _ := e.st.GetOrgBilling(context.Background(), org4, "free")
		return got.Plan == "team" && got.Status == "trialing" && got.StripeSubscriptionID != "sub_incomplete"
	})

	// Cancel; a re-subscription gets no second trial.
	if err := m.Cancel("sub_missed"); err != nil {
		t.Fatal(err)
	}
	co3 := b2.do("POST", "/admin/api/billing/checkout", map[string]any{"plan": "team"}, 200)
	if days, _ := m.SessionTrial(co3["id"].(string)); days != 0 {
		t.Fatalf("second trial offered: %d days", days)
	}
	if co3["id"] == co["id"] {
		t.Fatal("reused a session from before the cancellation")
	}

	// A2: every Stripe call pinned the API version.
	for v, n := range m.Versions() {
		if v != config.StripeAPIVersion {
			t.Fatalf("%d calls with Stripe-Version %q", n, v)
		}
	}
}

// A3: disputes, refunds and early fraud warnings are audited, alerted and
// affect the plan.
func TestCloudRiskEvents(t *testing.T) {
	e, m := setupCloud(t, nil)
	b, org, key := cloudSignup(t, e, "risk@x.test", "Risk")
	co := b.do("POST", "/admin/api/billing/checkout", map[string]any{"plan": "team"}, 200)
	if _, err := m.Pay(co["id"].(string)); err != nil {
		t.Fatal(err)
	}
	bil, _ := e.st.GetOrgBilling(context.Background(), org, "free")
	ch := m.Charge(bil.StripeCustomerID, 4900)
	plan := func() (string, any) {
		out := b.do("GET", "/admin/api/billing", nil, 200)
		return out["effective_plan"].(map[string]any)["id"].(string), out["risk_status"]
	}

	// Open dispute: flagged, plan kept.
	dp, err := m.Dispute(ch, "fraudulent")
	if err != nil {
		t.Fatal(err)
	}
	if p, r := plan(); p != "team" || r != "dispute_open" {
		t.Fatalf("open dispute: %s %v", p, r)
	}
	// Won: flag cleared.
	if err := m.CloseDispute(dp, "won"); err != nil {
		t.Fatal(err)
	}
	if p, r := plan(); p != "team" || r != "" {
		t.Fatalf("won dispute: %s %v", p, r)
	}
	// Lost: paid plan suspended.
	dp2, _ := m.Dispute(ch, "product_not_received")
	if err := m.CloseDispute(dp2, "lost"); err != nil {
		t.Fatal(err)
	}
	if p, r := plan(); p != "free" || r != "dispute_lost" {
		t.Fatalf("lost dispute: %s %v", p, r)
	}
	for i := 0; i < 3; i++ {
		b.chatStatus(key)
	}
	if st, code := b.chatStatus(key); st != 429 || code != "plan_quota_exceeded" {
		t.Fatalf("suspended org still on paid quota: %d %s", st, code)
	}
	// Only the operator can lift it.
	b.do("POST", "/admin/api/billing/risk/clear", nil, 403)
	e.adminOrg(org, "POST", "/admin/api/billing/risk/clear", nil, 200)
	if p, r := plan(); p != "team" || r != "" {
		t.Fatalf("cleared: %s %v", p, r)
	}
	// Early fraud warning: suspended immediately.
	if err := m.EarlyFraudWarning(ch); err != nil {
		t.Fatal(err)
	}
	if p, r := plan(); p != "free" || r != "fraud_warning" {
		t.Fatalf("fraud warning: %s %v", p, r)
	}
	// A later open dispute doesn't downgrade the more severe flag.
	_, _ = m.Dispute(ch, "fraudulent")
	if _, r := plan(); r != "fraud_warning" {
		t.Fatalf("flag lowered: %v", r)
	}
	// Refund: audited.
	if err := m.Refund(ch); err != nil {
		t.Fatal(err)
	}
	acts := auditActions(t, b)
	for _, a := range []string{"billing.dispute.created", "billing.dispute.closed", "billing.early_fraud_warning.created", "billing.charge.refunded", "billing.risk.clear"} {
		if acts[a] == 0 {
			t.Fatalf("missing audit %s in %v", a, acts)
		}
	}
	// A charge of an unknown customer is acknowledged and ignored.
	other := m.Charge("cus_nobody", 100)
	if _, err := m.Dispute(other, "fraudulent"); err != nil {
		t.Fatalf("unknown customer dispute: %v", err)
	}
}

// A10 + A12: livemode must match the key; concurrent deliveries of one
// event are processed exactly once.
func TestCloudWebhookLivemodeAndClaims(t *testing.T) {
	e, m := setupCloud(t, nil)
	a, orgA, _ := cloudSignup(t, e, "lm@x.test", "LM")
	co := a.do("POST", "/admin/api/billing/checkout", map[string]any{"plan": "team"}, 200)
	if _, err := m.Pay(co["id"].(string)); err != nil {
		t.Fatal(err)
	}
	ba, _ := e.st.GetOrgBilling(context.Background(), orgA, "free")

	live := stripemock.New(m.BaseURL, "", whsec)
	live.Livemode = true
	body, sig := live.Event("customer.subscription.deleted", m.Subscription(ba.StripeSubscriptionID))
	if st, out := postWebhook(t, e, body, sig); st != 400 || errCode(out) != "livemode_mismatch" {
		t.Fatalf("live event on test deployment: %d %v", st, out)
	}

	m.SetStatus(ba.StripeSubscriptionID, "past_due")
	body, sig = m.Event("customer.subscription.updated", m.Subscription(ba.StripeSubscriptionID))
	var wg sync.WaitGroup
	var mu sync.Mutex
	codes := map[string]int{}
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st, out := postWebhook(t, e, body, sig)
			k := "processed"
			if out["duplicate"] == true {
				k = "duplicate"
			} else if st == 409 {
				k = "busy"
			} else if st != 200 {
				k = "error"
			}
			mu.Lock()
			codes[k]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	if codes["processed"] != 1 || codes["error"] != 0 {
		t.Fatalf("concurrent deliveries: %v", codes)
	}
	n := 0
	for _, a := range a.do("GET", "/admin/api/audit", nil, 200)["data"].([]any) {
		am := a.(map[string]any)
		if am["action"] == "billing.subscription" && am["detail"].(map[string]any)["status"] == "past_due" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("event applied %d times", n)
	}
}
