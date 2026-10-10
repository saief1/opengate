package budget

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/saief1/opengate/internal/store"
	"github.com/saief1/opengate/internal/storetest"
)

func setup(t *testing.T) (*store.Store, *Enforcer, []Scope, *Alerter) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(storetest.DSN(t))
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
	st, _, _, _ := setup(t)
	ctx := context.Background()
	for name, r := range map[string]RateStore{"memory": NewRateLimiter(), "db": NewDBRates(st)} {
		now := time.Now()
		for i := 0; i < 3; i++ {
			if ok, _, err := r.Allow(ctx, "k", 3, now); !ok || err != nil {
				t.Fatalf("%s: burst should be allowed (%v)", name, err)
			}
		}
		ok, wait, _ := r.Allow(ctx, "k", 3, now)
		if ok || wait <= 0 {
			t.Fatalf("%s: 4th request should be limited", name)
		}
		if ok, _, _ := r.Allow(ctx, "k", 3, now.Add(21*time.Second)); !ok {
			t.Fatalf("%s: token should refill after 20s at 3 rpm", name)
		}
	}
}

func TestDBRateLimiterSurvivesRestartAndIsShared(t *testing.T) {
	st, _, _, _ := setup(t)
	ctx := context.Background()
	now := time.Now()
	a, b := NewDBRates(st), NewDBRates(st) // two "replicas" / a restart
	for i := 0; i < 2; i++ {
		if ok, _, _ := a.Allow(ctx, "shared", 2, now); !ok {
			t.Fatal("burst should be allowed")
		}
	}
	if ok, _, _ := b.Allow(ctx, "shared", 2, now); ok {
		t.Fatal("second limiter must see the first one's consumption")
	}
}

func TestDBRateLimiterConcurrent(t *testing.T) {
	st, _, _, _ := setup(t)
	ctx := context.Background()
	r := NewDBRates(st)
	now := time.Now()
	var wg sync.WaitGroup
	var allowed atomic.Int64
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, _, err := r.Allow(ctx, "c", 10, now); err == nil && ok {
				allowed.Add(1)
			} else if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if allowed.Load() != 10 {
		t.Fatalf("exactly 10 of 40 concurrent requests should pass a 10 rpm bucket, got %d", allowed.Load())
	}
}

func TestReservationBlocksConcurrentOvershoot(t *testing.T) {
	ctx := context.Background()
	st, e, scopes, _ := setup(t)
	st.UpsertLimit(ctx, store.Limit{Scope: store.ScopeKey, ScopeID: scopes[0].ID, DailyUSD: 1.0})
	e.ReloadLimits(ctx)
	// 50 concurrent requests, each estimated (and actually costing) $0.10.
	// Without reservations all 50 would pass the check against $0 spend.
	var wg sync.WaitGroup
	var admitted atomic.Int64
	var mu sync.Mutex
	var held []*Reservation
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, d, err := e.Reserve(ctx, scopes, 0.10)
			if err != nil {
				t.Error(err)
				return
			}
			if d == nil {
				admitted.Add(1)
				mu.Lock()
				held = append(held, r)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if n := admitted.Load(); n != 10 {
		t.Fatalf("expected exactly 10 admissions for $1.00 / $0.10, got %d", n)
	}
	if got := e.Reserved(scopes[0], Daily); got < 0.999 || got > 1.001 {
		t.Fatalf("reserved = %v", got)
	}
	// Commit actual (cheaper) costs: reservations convert into spend.
	for _, r := range held {
		spendLog(t, st, scopes, 0.05)
		r.Commit(ctx, 0.05)
	}
	if got := e.Reserved(scopes[0], Daily); got != 0 {
		t.Fatalf("reservations not released: %v", got)
	}
	if sp, _ := e.Spend(ctx, scopes[0], Daily); sp < 0.499 || sp > 0.501 {
		t.Fatalf("spend = %v", sp)
	}
	// Budget left again ($0.50), so new requests are admitted.
	if _, d, _ := e.Reserve(ctx, scopes, 0.10); d != nil {
		t.Fatalf("should admit after commit: %s", d.Message)
	}
}

func TestReservationReleaseAndStrict(t *testing.T) {
	ctx := context.Background()
	st, _, scopes, al := setup(t)
	st.UpsertLimit(ctx, store.Limit{Scope: store.ScopeProject, ScopeID: scopes[2].ID, MonthlyUSD: 1.0})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	e, _ := NewEnforcer(ctx, st, al, log, Options{Strict: true})
	if _, d, _ := e.Reserve(ctx, scopes, 1.5); d == nil {
		t.Fatal("strict mode must refuse a request whose worst case exceeds the limit")
	}
	r, d, _ := e.Reserve(ctx, scopes, 0.6)
	if d != nil {
		t.Fatal(d.Message)
	}
	if _, d, _ := e.Reserve(ctx, scopes, 0.6); d == nil {
		t.Fatal("second reservation would exceed the limit")
	}
	r.Release()
	r.Release() // idempotent
	if _, d, _ := e.Reserve(ctx, scopes, 0.6); d != nil {
		t.Fatal("released reservation must free budget")
	}
}

func TestSpendSyncAcrossReplicas(t *testing.T) {
	ctx := context.Background()
	st, _, scopes, al := setup(t)
	st.UpsertLimit(ctx, store.Limit{Scope: store.ScopeKey, ScopeID: scopes[0].ID, DailyUSD: 1.0})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a, _ := NewEnforcer(ctx, st, al, log, Options{SyncInterval: time.Millisecond})
	if _, d, _ := a.Reserve(ctx, scopes, 0); d != nil {
		t.Fatal("unexpected denial")
	}
	// Another replica writes $2 of spend directly to the shared database.
	spendLog(t, st, scopes, 2)
	time.Sleep(5 * time.Millisecond)
	if _, d, _ := a.Reserve(ctx, scopes, 0); d == nil {
		t.Fatal("replica must pick up spend recorded elsewhere after the sync interval")
	}
}

var logSeq atomic.Int64

func spendLog(t *testing.T, st *store.Store, scopes []Scope, cost float64) {
	t.Helper()
	err := st.InsertLog(context.Background(), &store.RequestLog{ID: fmt.Sprintf("l%d", logSeq.Add(1)), TS: time.Now(),
		ProjectID: scopes[len(scopes)-1].ID, KeyID: scopes[0].ID, Agent: "planner", CostUSD: cost, Status: 200})
	if err != nil {
		t.Fatal(err)
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
