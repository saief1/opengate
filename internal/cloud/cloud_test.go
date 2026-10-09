package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/opengate-dev/opengate/internal/config"
	"github.com/opengate-dev/opengate/internal/store"
)

func TestVerifySignature(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	body := []byte(`{"id":"evt_1"}`)
	h := Sign(body, "whsec_test", now)
	if err := VerifySignature(body, h, "whsec_test", 5*time.Minute, now.Add(time.Minute)); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	cases := map[string]struct {
		body   []byte
		header string
		secret string
		at     time.Time
	}{
		"wrong secret":      {body, h, "whsec_other", now},
		"tampered body":     {[]byte(`{"id":"evt_2"}`), h, "whsec_test", now},
		"too old (replay)":  {body, h, "whsec_test", now.Add(6 * time.Minute)},
		"from the future":   {body, h, "whsec_test", now.Add(-6 * time.Minute)},
		"missing v1":        {body, "t=1800000000", "whsec_test", now},
		"garbage":           {body, "nonsense", "whsec_test", now},
		"empty secret":      {body, h, "", now},
		"v0 scheme ignored": {body, strings.Replace(h, "v1=", "v0=", 1), "whsec_test", now},
	}
	for name, c := range cases {
		if err := VerifySignature(c.body, c.header, c.secret, 5*time.Minute, c.at); !errors.Is(err, ErrBadSignature) {
			t.Errorf("%s: want ErrBadSignature, got %v", name, err)
		}
	}
	// Secret rotation: Stripe sends several v1 signatures; any may match.
	multi := strings.Replace(Sign(body, "whsec_old", now), "t=1800000000,", "", 1)
	if err := VerifySignature(body, h+","+multi, "whsec_test", 5*time.Minute, now); err != nil {
		t.Fatalf("multi-signature header: %v", err)
	}
}

func cloudCfg() *config.CloudConfig {
	cc := &config.CloudConfig{DefaultPlan: "free", GracePeriod: 7 * 24 * time.Hour, Plans: config.DefaultCloudPlans()}
	for i := range cc.Plans {
		if cc.Plans[i].ID == "team" {
			cc.Plans[i].StripePriceID = "price_team"
		}
	}
	return cc
}

func sub(id, cust, status, price string) *Subscription {
	var s Subscription
	_ = json.Unmarshal([]byte(`{"id":"`+id+`","customer":"`+cust+`","status":"`+status+`","items":{"data":[{"current_period_end":1900000000,"price":{"id":"`+price+`"}}]}}`), &s)
	return &s
}

func TestApplySubscriptionAndEffective(t *testing.T) {
	cc, now := cloudCfg(), time.Now()
	b := &store.OrgBilling{OrgID: "org_a", Plan: "free", Status: "active"}
	if Effective(cc, b, now).ID != "free" {
		t.Fatal("default plan")
	}
	if ApplySubscription(cc, b, sub("sub_1", "cus_a", "active", "price_team"), now) != Applied || Effective(cc, b, now).ID != "team" {
		t.Fatalf("active team: %+v", b)
	}
	if b.CurrentPeriodEnd.Unix() != 1900000000 {
		t.Fatalf("period end from items: %v", b.CurrentPeriodEnd)
	}
	// Unknown price and a foreign customer are ignored.
	if ApplySubscription(cc, b, sub("sub_x", "cus_a", "active", "price_unknown"), now) != Ignored {
		t.Fatal("unknown price applied")
	}
	if ApplySubscription(cc, b, sub("sub_1", "cus_evil", "canceled", "price_team"), now) != Ignored || b.Plan != "team" {
		t.Fatal("other customer's subscription applied")
	}
	// An older, canceled subscription must not downgrade the current one.
	if ApplySubscription(cc, b, sub("sub_old", "cus_a", "canceled", "price_team"), now) != Ignored || b.Plan != "team" {
		t.Fatal("stale subscription applied")
	}
	// A1: a second live subscription is a duplicate and leaves the current one alone.
	for _, st := range []string{"active", "trialing", "past_due", "incomplete"} {
		if o := ApplySubscription(cc, b, sub("sub_dup", "cus_a", st, "price_team"), now); o != Duplicate || b.StripeSubscriptionID != "sub_1" || b.Status != "active" {
			t.Fatalf("duplicate %s: %v %+v", st, o, b)
		}
	}
	// Portal cancellation on current API versions: cancel_at, not cancel_at_period_end.
	sc := sub("sub_1", "cus_a", "active", "price_team")
	sc.CancelAt = 1900000000
	if ApplySubscription(cc, b, sc, now) != Applied || !b.CancelAtPeriodEnd {
		t.Fatalf("cancel_at not recognized: %+v", b)
	}
	ApplySubscription(cc, b, sub("sub_1", "cus_a", "active", "price_team"), now) // un-cancel
	if b.CancelAtPeriodEnd {
		t.Fatal("renewal not restored")
	}
	// Risk suspension: a lost dispute / fraud warning pauses the paid plan.
	b.RiskStatus = store.RiskDisputeOpen
	if Effective(cc, b, now).ID != "team" {
		t.Fatal("an open dispute alone must not suspend")
	}
	b.RiskStatus = store.RiskFraudWarning
	if Effective(cc, b, now).ID != "free" {
		t.Fatal("fraud warning must suspend the paid plan")
	}
	b.RiskStatus = ""
	// past_due keeps the plan during the grace period only.
	ApplySubscription(cc, b, sub("sub_1", "cus_a", "past_due", "price_team"), now)
	if Effective(cc, b, now).ID != "team" || Effective(cc, b, now.Add(8*24*time.Hour)).ID != "free" {
		t.Fatalf("grace period: %+v", b)
	}
	since := b.PastDueSince
	ApplySubscription(cc, b, sub("sub_1", "cus_a", "past_due", "price_team"), now.Add(time.Hour))
	if !b.PastDueSince.Equal(since) {
		t.Fatal("past_due_since must not move on repeated events")
	}
	ApplySubscription(cc, b, sub("sub_1", "cus_a", "active", "price_team"), now)
	if !b.PastDueSince.IsZero() {
		t.Fatal("recovery clears past_due_since")
	}
	// unpaid drops to the default limits immediately.
	ApplySubscription(cc, b, sub("sub_1", "cus_a", "unpaid", "price_team"), now)
	if Effective(cc, b, now).ID != "free" {
		t.Fatal("unpaid keeps paid limits")
	}
	// Cancellation returns the org to the default plan.
	ApplySubscription(cc, b, sub("sub_1", "cus_a", "canceled", "price_team"), now)
	if b.Plan != "free" || b.StripeSubscriptionID != "" || b.Status != "canceled" || b.StripeCustomerID != "cus_a" {
		t.Fatalf("canceled: %+v", b)
	}
	// Operator-assigned plans (no subscription) always apply.
	ent := &store.OrgBilling{Plan: "enterprise", Status: "active"}
	if Effective(cc, ent, now).ID != "enterprise" {
		t.Fatal("manual plan")
	}
	if Effective(cc, &store.OrgBilling{Plan: "removed-plan"}, now).ID != "free" {
		t.Fatal("unknown plan falls back to default")
	}
}

func TestIDAndInvoiceShapes(t *testing.T) {
	var s Session
	if err := json.Unmarshal([]byte(`{"customer":{"id":"cus_1","object":"customer"},"subscription":null}`), &s); err != nil || s.Customer != "cus_1" || s.Subscription != "" {
		t.Fatalf("expanded/null ids: %v %+v", err, s)
	}
	old, _ := InvoiceSubscription(json.RawMessage(`{"subscription":"sub_old","customer":"cus_1"}`))
	newer, cust := InvoiceSubscription(json.RawMessage(`{"customer":"cus_1","parent":{"subscription_details":{"subscription":"sub_new"}}}`))
	if old != "sub_old" || newer != "sub_new" || cust != "cus_1" {
		t.Fatalf("invoice shapes: %q %q %q", old, newer, cust)
	}
	var top Subscription
	_ = json.Unmarshal([]byte(`{"current_period_end":1700000000,"items":{"data":[]}}`), &top)
	if top.PeriodEnd().Unix() != 1700000000 || top.PriceID() != "" {
		t.Fatal("top-level period end")
	}
}

func TestReplacementAfterCancel(t *testing.T) {
	cc, now := cloudCfg(), time.Now()
	b := &store.OrgBilling{OrgID: "org_a", Plan: "team", Status: "canceled", StripeCustomerID: "cus_a", StripeSubscriptionID: "sub_old"}
	// The recorded subscription is no longer in force: a new live one replaces it.
	if o := ApplySubscription(cc, b, sub("sub_new", "cus_a", "active", "price_team"), now); o != Applied || b.StripeSubscriptionID != "sub_new" {
		t.Fatalf("replacement: %v %+v", o, b)
	}
}

func TestLivemodeFromKey(t *testing.T) {
	for k, want := range map[string]bool{"sk_live_x": true, "rk_live_x": true, "sk_test_x": false, "rk_test_x": false, "mock": false} {
		if (&Stripe{APIKey: k}).Livemode() != want {
			t.Fatalf("%s", k)
		}
	}
}

func TestClientPinsAPIVersion(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get("Stripe-Version"))
		_, _ = w.Write([]byte(`{"id":"sub_1","status":"active","data":[]}`))
	}))
	defer srv.Close()
	c := &Stripe{BaseURL: srv.URL, APIKey: "sk_test_x", APIVersion: config.StripeAPIVersion, Client: srv.Client()}
	_, _ = c.GetSubscription(context.Background(), "sub_1")
	_, _ = c.ListSubscriptions(context.Background(), "cus_1")
	_ = c.ExpireCheckoutSession(context.Background(), "cs_1")
	if len(got) != 3 {
		t.Fatalf("calls %d", len(got))
	}
	for _, v := range got {
		if v != "2026-09-30.endive" {
			t.Fatalf("Stripe-Version %q", v)
		}
	}
	// Path injection through ids is refused before any request.
	if err := c.ExpireCheckoutSession(context.Background(), "cs_1/../../v1/customers"); err == nil {
		t.Fatal("bad id accepted")
	}
}
