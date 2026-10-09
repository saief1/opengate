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
	// Webhook idempotency.
	if seen, _ := s.StripeEventSeen(ctx, "evt_1"); seen {
		t.Fatal("unseen event reported seen")
	}
	_ = s.MarkStripeEvent(ctx, "evt_1", "x")
	_ = s.MarkStripeEvent(ctx, "evt_1", "x")
	if seen, _ := s.StripeEventSeen(ctx, "evt_1"); !seen {
		t.Fatal("event not recorded")
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
