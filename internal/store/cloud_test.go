package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestOrgBillingUsageAndRetention(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	a, _ := s.CreateOrg(ctx, "A")
	b, _ := s.CreateOrg(ctx, "B")
	if got, _ := s.GetOrgBilling(ctx, a.ID, "free"); got.Plan != "free" || got.Status != "active" {
		t.Fatalf("default billing: %+v", got)
	}
	ob := &OrgBilling{OrgID: a.ID, Plan: "team", Status: "active", StripeCustomerID: "cus_a", StripeSubscriptionID: "sub_a", CurrentPeriodEnd: time.Now().Add(time.Hour)}
	if err := s.PutOrgBilling(ctx, ob); err != nil {
		t.Fatal(err)
	}
	if got, err := s.OrgBillingByCustomer(ctx, "cus_a"); err != nil || got.OrgID != a.ID || got.PastDueSince.IsZero() == false {
		t.Fatalf("by customer: %+v %v", got, err)
	}
	// A Stripe customer belongs to one org only.
	if err := s.PutOrgBilling(ctx, &OrgBilling{OrgID: b.ID, Plan: "free", Status: "active", StripeCustomerID: "cus_a"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate customer: %v", err)
	}
	// Usage counters are atomic under concurrency.
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = s.IncrOrgUsage(ctx, a.ID, "2026-10") }()
	}
	wg.Wait()
	if n, _ := s.OrgUsage(ctx, a.ID, "2026-10"); n != 20 {
		t.Fatalf("usage: %d", n)
	}
	if n, _ := s.OrgUsage(ctx, b.ID, "2026-10"); n != 0 {
		t.Fatalf("other org usage: %d", n)
	}
	// Webhook idempotency: atomic claims.
	if seen, _ := s.StripeEventSeen(ctx, "evt_1"); seen {
		t.Fatal("unseen event reported seen")
	}
	if c, err := s.ClaimStripeEvent(ctx, "evt_1", "x", time.Minute); err != nil || c != EventClaimed {
		t.Fatalf("first claim: %d %v", c, err)
	}
	if c, _ := s.ClaimStripeEvent(ctx, "evt_1", "x", time.Minute); c != EventBusy {
		t.Fatalf("concurrent claim: %d", c)
	}
	if seen, _ := s.StripeEventSeen(ctx, "evt_1"); seen {
		t.Fatal("in-flight event reported done")
	}
	_ = s.FinishStripeEvent(ctx, "evt_1")
	if c, _ := s.ClaimStripeEvent(ctx, "evt_1", "x", time.Minute); c != EventDone {
		t.Fatalf("claim after done: %d", c)
	}
	// A failed delivery releases its claim so the retry can process it.
	_, _ = s.ClaimStripeEvent(ctx, "evt_2", "x", time.Minute)
	_ = s.ReleaseStripeEvent(ctx, "evt_2")
	if c, _ := s.ClaimStripeEvent(ctx, "evt_2", "x", time.Minute); c != EventClaimed {
		t.Fatalf("claim after release: %d", c)
	}
	// A stale claim (crashed process) is taken over.
	if c, _ := s.ClaimStripeEvent(ctx, "evt_2", "x", -time.Second); c != EventClaimed {
		t.Fatalf("stale takeover: %d", c)
	}
	// Exactly one of many concurrent deliveries wins.
	var won sync.Map
	var wg2 sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg2.Add(1)
		go func(i int) {
			defer wg2.Done()
			if c, err := s.ClaimStripeEvent(ctx, "evt_race", "x", time.Minute); err == nil && c == EventClaimed {
				won.Store(i, true)
			}
		}(i)
	}
	wg2.Wait()
	n := 0
	won.Range(func(any, any) bool { n++; return true })
	if n != 1 {
		t.Fatalf("concurrent claims won: %d", n)
	}
	// Risk and checkout columns round-trip.
	ob.RiskStatus, ob.RiskDetail, ob.CheckoutSessionID, ob.CheckoutPlan = RiskFraudWarning, "issfr_1", "cs_1", "team"
	_ = s.PutOrgBilling(ctx, ob)
	if got, _ := s.GetOrgBilling(ctx, a.ID, "free"); !got.Suspended() || got.CheckoutSessionID != "cs_1" || got.CheckoutPlan != "team" || got.RiskDetail != "issfr_1" {
		t.Fatalf("risk/checkout round-trip: %+v", got)
	}
	// Retention prunes only the given org's old logs.
	pa, _ := s.CreateProjectInOrg(ctx, a.ID, "pa", false)
	pb, _ := s.CreateProjectInOrg(ctx, b.ID, "pb", false)
	old := time.Now().Add(-40 * 24 * time.Hour)
	for i, l := range []*RequestLog{{ID: "l1", TS: old, ProjectID: pa.ID}, {ID: "l2", TS: time.Now(), ProjectID: pa.ID}, {ID: "l3", TS: old, ProjectID: pb.ID}} {
		l.KeyID, l.Model, l.Endpoint = "k", "m", "chat"
		if err := s.InsertLog(ctx, l); err != nil {
			t.Fatal(i, err)
		}
	}
	if n, err := s.PruneOrgLogs(ctx, a.ID, time.Now().Add(-30*24*time.Hour)); err != nil || n != 1 {
		t.Fatalf("prune: %d %v", n, err)
	}
	if _, err := s.GetLog(ctx, "l3"); err != nil {
		t.Fatal("other org's log pruned")
	}
	if _, err := s.GetLog(ctx, "l2"); err != nil {
		t.Fatal("recent log pruned")
	}
	if p, _ := s.CountProjects(ctx, a.ID); p != 2 {
		t.Fatalf("projects: %d", p)
	}
}

func TestJobLease(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	if ok, err := s.AcquireLease(ctx, "job", "a", time.Minute); err != nil || !ok {
		t.Fatalf("a acquire: %v %v", ok, err)
	}
	if ok, _ := s.AcquireLease(ctx, "job", "b", time.Minute); ok {
		t.Fatal("b stole a live lease")
	}
	if ok, _ := s.AcquireLease(ctx, "job", "a", time.Minute); !ok {
		t.Fatal("a could not renew")
	}
	_ = s.ReleaseLease(ctx, "job", "b") // not b's: no effect
	if ok, _ := s.AcquireLease(ctx, "job", "b", time.Minute); ok {
		t.Fatal("foreign release freed the lease")
	}
	_ = s.ReleaseLease(ctx, "job", "a")
	if ok, _ := s.AcquireLease(ctx, "job", "b", time.Minute); !ok {
		t.Fatal("b could not take a released lease")
	}
	// Expired leases are taken over.
	_, _ = s.AcquireLease(ctx, "job2", "a", -time.Second)
	if ok, _ := s.AcquireLease(ctx, "job2", "b", time.Minute); !ok {
		t.Fatal("expired lease not taken over")
	}
}
