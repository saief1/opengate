package cloud

import (
	"encoding/json"
	"errors"
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
	if !ApplySubscription(cc, b, sub("sub_1", "cus_a", "active", "price_team"), now) || Effective(cc, b, now).ID != "team" {
		t.Fatalf("active team: %+v", b)
	}
	if b.CurrentPeriodEnd.Unix() != 1900000000 {
		t.Fatalf("period end from items: %v", b.CurrentPeriodEnd)
	}
	// Unknown price and a foreign customer are ignored.
	if ApplySubscription(cc, b, sub("sub_x", "cus_a", "active", "price_unknown"), now) {
		t.Fatal("unknown price applied")
	}
	if ApplySubscription(cc, b, sub("sub_1", "cus_evil", "canceled", "price_team"), now) || b.Plan != "team" {
		t.Fatal("other customer's subscription applied")
	}
	// An older, canceled subscription must not downgrade the current one.
	if ApplySubscription(cc, b, sub("sub_old", "cus_a", "canceled", "price_team"), now) || b.Plan != "team" {
		t.Fatal("stale subscription applied")
	}
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
