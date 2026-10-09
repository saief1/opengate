package budget

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/opengate-dev/opengate/internal/store"
)

func setup(t *testing.T) (*store.Store, *Enforcer, []Scope, *Alerter) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	p, _ := st.EnsureProject(ctx, "demo")
	_, k, _ := st.CreateKey(ctx, p.ID, "bot", "planner", "")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	al := NewAlerter("", []float64{0.8, 1.0}, log)
	e, err := NewEnforcer(ctx, st, al, log)
	if err != nil {
		t.Fatal(err)
	}
	return st, e, ScopesFor(k, p), al
}

func spend(t *testing.T, st *store.Store, e *Enforcer, scopes []Scope, cost float64, ts time.Time) {
	t.Helper()
	k := scopes[0].ID
	proj := scopes[len(scopes)-1].ID
	if err := st.InsertLog(context.Background(), &store.RequestLog{ID: time.Now().String() + k, TS: ts, ProjectID: proj, KeyID: k, Agent: "planner", CostUSD: cost, Status: 200}); err != nil {
		t.Fatal(err)
	}
	e.Record(context.Background(), scopes, cost)
}

func TestScopes(t *testing.T) {
	_, _, scopes, _ := setup(t)
	if len(scopes) != 3 || scopes[0].Kind != store.ScopeKey || scopes[1].Kind != store.ScopeAgent || scopes[2].Kind != store.ScopeProject {
		t.Fatalf("scopes: %+v", scopes)
	}
}

func TestBudgetCutoffPerScope(t *testing.T) {
	ctx := context.Background()
	for _, idx := range []int{0, 1, 2} { // key, agent, project
		st, e, scopes, _ := setup(t)
		s := scopes[idx]
		st.UpsertLimit(ctx, store.Limit{Scope: s.Kind, ScopeID: s.ID, DailyUSD: 1.0})
		e.ReloadLimits(ctx)
		if d, _ := e.Check(ctx, scopes); d != nil {
			t.Fatalf("%s: unexpected denial %v", s.Kind, d.Message)
		}
		spend(t, st, e, scopes, 0.6, time.Now())
		if d, _ := e.Check(ctx, scopes); d != nil {
			t.Fatalf("%s: denied under limit", s.Kind)
		}
		spend(t, st, e, scopes, 0.5, time.Now())
		d, _ := e.Check(ctx, scopes)
		if d == nil || d.Code != "budget_exceeded" || d.Status != 429 || d.RetryAfter <= 0 {
			t.Fatalf("%s: expected budget denial, got %+v", s.Kind, d)
		}
	}
}

func TestPeriodsAreIndependent(t *testing.T) {
	ctx := context.Background()
	st, e, scopes, _ := setup(t)
	st.UpsertLimit(ctx, store.Limit{Scope: store.ScopeKey, ScopeID: scopes[0].ID, DailyUSD: 1, MonthlyUSD: 100, TotalUSD: 2})
	e.ReloadLimits(ctx)
	// Spend from 40 days ago counts toward total only.
	spend(t, st, e, scopes, 1.5, time.Now().AddDate(0, 0, -40))
	if d, _ := e.Check(ctx, scopes); d != nil {
		t.Fatalf("old spend must not count for daily: %s", d.Message)
	}
	spend(t, st, e, scopes, 0.6, time.Now())
	d, _ := e.Check(ctx, scopes)
	if d == nil || d.RetryAfter != 0 { // total period never resets
		t.Fatalf("expected total budget denial: %+v", d)
	}
}

func TestPeriodStart(t *testing.T) {
	ts := time.Date(2026, 3, 15, 13, 4, 5, 0, time.UTC)
	if PeriodStart(Daily, ts) != time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC) ||
		PeriodStart(Monthly, ts) != time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC) ||
		!PeriodStart(Total, ts).Equal(time.Unix(0, 0)) {
		t.Fatal("period start")
	}
}

func TestRateLimiter(t *testing.T) {
	r := NewRateLimiter()
	now := time.Now()
	for i := 0; i < 3; i++ {
		if ok, _ := r.Allow("k", 3, now); !ok {
			t.Fatal("burst should be allowed")
		}
	}
	ok, wait := r.Allow("k", 3, now)
	if ok || wait <= 0 {
		t.Fatal("4th request should be limited")
	}
	if ok, _ := r.Allow("k", 3, now.Add(21*time.Second)); !ok {
		t.Fatal("token should refill after 20s at 3 rpm")
	}
}

func TestRPMEnforced(t *testing.T) {
	ctx := context.Background()
	st, e, scopes, _ := setup(t)
	st.UpsertLimit(ctx, store.Limit{Scope: store.ScopeKey, ScopeID: scopes[0].ID, RPM: 2})
	e.ReloadLimits(ctx)
	e.Check(ctx, scopes)
	e.Check(ctx, scopes)
	d, _ := e.Check(ctx, scopes)
	if d == nil || d.Code != "rate_limit_exceeded" {
		t.Fatalf("expected rate limit: %+v", d)
	}
}

func TestThresholdAlertsOncePerPeriod(t *testing.T) {
	ctx := context.Background()
	st, e, scopes, _ := setup(t)
	got := make(chan Event, 10)
	sendHook = func(ev Event) { got <- ev }
	t.Cleanup(func() { sendHook = nil })
	st.UpsertLimit(ctx, store.Limit{Scope: store.ScopeKey, ScopeID: scopes[0].ID, DailyUSD: 10})
	e.ReloadLimits(ctx)
	spend(t, st, e, scopes, 5, time.Now())   // 50%: nothing
	spend(t, st, e, scopes, 3.5, time.Now()) // 85%: 80% alert
	spend(t, st, e, scopes, 0.1, time.Now()) // still 86%: no duplicate
	spend(t, st, e, scopes, 2, time.Now())   // 106%: 100% alert
	close(got)
	var ths []float64
	for ev := range got {
		ths = append(ths, ev.Threshold)
	}
	if len(ths) != 2 || ths[0] != 0.8 || ths[1] != 1.0 {
		t.Fatalf("thresholds fired: %v", ths)
	}
}
