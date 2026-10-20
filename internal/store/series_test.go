package store

import (
	"context"
	"testing"
	"time"
)

func TestGroupSeriesLatencyAndAlerts(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	p, err := s.EnsureProject(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	logs := []*RequestLog{
		{ID: "r1", TS: now.Add(-30 * time.Minute), ProjectID: p.ID, KeyID: "k1", Agent: "planner", Provider: "openai", CustomerID: "acme", CostUSD: 0.5, LatencyMS: 100, Status: 200},
		{ID: "r2", TS: now.Add(-20 * time.Minute), ProjectID: p.ID, KeyID: "k1", Agent: "planner", Provider: "openai", CostUSD: 0.25, LatencyMS: 300, Status: 200},
		{ID: "r3", TS: now.Add(-10 * time.Minute), ProjectID: p.ID, KeyID: "k2", Agent: "", Provider: "", Status: 403, ErrorCode: "budget_exceeded"},
		{ID: "r4", TS: now.Add(-48 * time.Hour), ProjectID: p.ID, KeyID: "k1", Agent: "planner", Provider: "openai", CostUSD: 9, LatencyMS: 9000, Status: 200},
	}
	for _, l := range logs {
		if err := s.InsertLog(ctx, l); err != nil {
			t.Fatal(err)
		}
	}
	since := now.Add(-24 * time.Hour)
	sum := func(pts []GroupPoint, name string) (c float64, n int) {
		for _, p := range pts {
			if p.Name == name {
				c, n = c+p.CostUSD, n+p.Requests
			}
		}
		return
	}
	pts, err := s.GroupSeries(ctx, StatsFilter{}, "provider", since, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if c, n := sum(pts, "openai"); n != 2 || c != 0.75 {
		t.Fatalf("openai: %v %d", c, n)
	}
	if _, n := sum(pts, "(rejected)"); n != 1 {
		t.Fatalf("rejected: %d", n)
	}
	if pts, _ = s.GroupSeries(ctx, StatsFilter{}, "agent", since, time.Hour); func() int { _, n := sum(pts, "(none)"); return n }() != 1 {
		t.Fatal("agent (none) missing")
	}
	if pts, _ = s.GroupSeries(ctx, StatsFilter{}, "customer", since, time.Hour); len(pts) != 1 || pts[0].Name != "acme" {
		t.Fatalf("customer: %+v", pts)
	}
	if pts, _ = s.GroupSeries(ctx, StatsFilter{}, "blocked", since, time.Hour); len(pts) != 1 || pts[0].Name != "budget_exceeded" {
		t.Fatalf("blocked: %+v", pts)
	}
	if _, err := s.GroupSeries(ctx, StatsFilter{}, "nope", since, time.Hour); err == nil {
		t.Fatal("expected bad group error")
	}
	p50, p95, n, err := s.LatencyPercentiles(ctx, StatsFilter{}, since)
	if err != nil || n != 2 || p50 != 100 || p95 != 100 {
		t.Fatalf("latency: %v %v %d %v", p50, p95, n, err)
	}
	if _, err := s.MarkAlertSent(ctx, "key", "k1", "daily", now.Truncate(24*time.Hour), 0.8); err != nil {
		t.Fatal(err)
	}
	al, err := s.RecentAlerts(ctx, 10)
	if err != nil || len(al) != 1 || al[0].ScopeID != "k1" || al[0].Threshold != 0.8 {
		t.Fatalf("alerts: %+v %v", al, err)
	}
}
