// Package budget enforces spend limits, rate limits and threshold alerts.
package budget

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/saief1/opengate/internal/store"
)

// Periods for spend limits. All periods are calendar periods in UTC.
const (
	Daily   = "daily"
	Monthly = "monthly"
	Total   = "total"
)

// Scope identifies something a limit can be attached to.
type Scope struct {
	Kind string // store.ScopeKey | store.ScopeAgent | store.ScopeProject
	ID   string
	Name string // human label for messages/alerts
}

// ScopesFor returns the scopes that apply to a request made with key k.
func ScopesFor(k *store.VirtualKey, p *store.Project) []Scope {
	s := []Scope{{Kind: store.ScopeKey, ID: k.ID, Name: fmt.Sprintf("key %q", k.Name)}}
	if k.Agent != "" {
		s = append(s, Scope{Kind: store.ScopeAgent, ID: k.ProjectID + "/" + k.Agent, Name: fmt.Sprintf("agent %q", k.Agent)})
	}
	s = append(s, Scope{Kind: store.ScopeProject, ID: k.ProjectID, Name: fmt.Sprintf("project %q", p.Name)})
	if p.OrgID != "" {
		s = append(s, Scope{Kind: store.ScopeOrg, ID: p.OrgID, Name: "organization"})
	}
	return s
}

// Denial explains why a request was rejected.
type Denial struct {
	Status     int
	Type       string
	Code       string
	Message    string
	RetryAfter time.Duration
}

// PeriodStart returns the start of the period containing t (UTC).
func PeriodStart(period string, t time.Time) time.Time {
	t = t.UTC()
	switch period {
	case Daily:
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	case Monthly:
		return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	default:
		return time.Unix(0, 0).UTC()
	}
}

// periodLimit returns the USD limit for period (0 = none).
func periodLimit(l store.Limit, period string) float64 {
	switch period {
	case Daily:
		return l.DailyUSD
	case Monthly:
		return l.MonthlyUSD
	default:
		return l.TotalUSD
	}
}

type spendKey struct {
	kind, id, period string
	start            int64
}

type spendEntry struct {
	v      float64
	loaded time.Time
}

// Options tune an Enforcer.
type Options struct {
	// Rates is the rate-limit backend (default: in-memory).
	Rates RateStore
	// SyncInterval re-reads period spend from the database at most this
	// often, so several replicas sharing PostgreSQL converge on the same
	// totals. 0 disables refreshing (single-instance SQLite).
	SyncInterval time.Duration
	// Strict denies a request whose reserved worst-case cost would push a
	// scope past its limit. When false (default) a request is admitted as
	// long as committed+reserved spend is still below the limit, which caps
	// overshoot at roughly one request's cost even under heavy concurrency.
	Strict bool
	// OrgDefault is the org-wide spend cap applied to every organization
	// without its own org limit (config budgets.org_daily_usd /
	// org_monthly_usd). Zero values mean no default cap.
	OrgDefault store.Limit
}

// Enforcer checks, reserves and records spend. Period spend is cached in
// memory (loaded from the database on first use) so checks are cheap, and
// in-flight requests hold reservations so concurrent traffic cannot all
// pass a check against the same stale total.
type Enforcer struct {
	store   *store.Store
	alerter *Alerter
	rates   RateStore
	opts    Options
	log     *slog.Logger
	Now     func() time.Time

	mu       sync.Mutex
	limits   map[Scope]store.Limit // keyed by Kind+ID only (Name empty)
	spend    map[spendKey]spendEntry
	reserved map[spendKey]float64
}

// NewEnforcer builds an enforcer and loads limits.
func NewEnforcer(ctx context.Context, st *store.Store, alerter *Alerter, log *slog.Logger, opts ...Options) (*Enforcer, error) {
	var o Options
	if len(opts) > 0 {
		o = opts[0]
	}
	if o.Rates == nil {
		o.Rates = NewRateLimiter()
	}
	e := &Enforcer{store: st, alerter: alerter, rates: o.Rates, opts: o, log: log, Now: time.Now,
		spend: map[spendKey]spendEntry{}, reserved: map[spendKey]float64{}}
	return e, e.ReloadLimits(ctx)
}

// ReloadLimits refreshes the in-memory limit table (call after edits).
func (e *Enforcer) ReloadLimits(ctx context.Context) error {
	ls, err := e.store.ListLimits(ctx)
	if err != nil {
		return err
	}
	m := make(map[Scope]store.Limit, len(ls))
	for _, l := range ls {
		m[Scope{Kind: l.Scope, ID: l.ScopeID}] = l
	}
	e.mu.Lock()
	e.limits = m
	e.mu.Unlock()
	return nil
}

func (e *Enforcer) limitFor(s Scope) (store.Limit, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	l, ok := e.limits[Scope{Kind: s.Kind, ID: s.ID}]
	if !ok && s.Kind == store.ScopeOrg && (e.opts.OrgDefault.DailyUSD > 0 || e.opts.OrgDefault.MonthlyUSD > 0) {
		l = store.Limit{Scope: store.ScopeOrg, ScopeID: s.ID, DailyUSD: e.opts.OrgDefault.DailyUSD, MonthlyUSD: e.opts.OrgDefault.MonthlyUSD}
		return l, true
	}
	return l, ok
}

// OrgCap returns the effective org-wide cap for orgID and where it comes
// from: "org" (set via the admin API), "config" (server default) or "".
func (e *Enforcer) OrgCap(orgID string) (store.Limit, string) {
	e.mu.Lock()
	l, ok := e.limits[Scope{Kind: store.ScopeOrg, ID: orgID}]
	e.mu.Unlock()
	if ok {
		return l, "org"
	}
	if l, ok := e.limitFor(Scope{Kind: store.ScopeOrg, ID: orgID}); ok {
		return l, "config"
	}
	return store.Limit{Scope: store.ScopeOrg, ScopeID: orgID}, ""
}

func (e *Enforcer) key(s Scope, period string, now time.Time) spendKey {
	return spendKey{s.Kind, s.ID, period, PeriodStart(period, now).UnixMilli()}
}

// Spend returns the committed spend of a scope in a period.
func (e *Enforcer) Spend(ctx context.Context, s Scope, period string) (float64, error) {
	now := e.Now()
	k := e.key(s, period, now)
	e.mu.Lock()
	v, ok := e.spend[k]
	e.mu.Unlock()
	if ok && (e.opts.SyncInterval <= 0 || time.Since(v.loaded) < e.opts.SyncInterval) {
		return v.v, nil
	}
	fresh, err := e.store.SpendSince(ctx, s.Kind, s.ID, PeriodStart(period, now))
	if err != nil {
		return 0, err
	}
	e.mu.Lock()
	if cur, ok2 := e.spend[k]; ok2 && !ok {
		fresh = cur.v // raced with a concurrent first load
	}
	e.spend[k] = spendEntry{v: fresh, loaded: time.Now()}
	e.mu.Unlock()
	return fresh, nil
}

// Reserved returns the amount currently held by in-flight requests.
func (e *Enforcer) Reserved(s Scope, period string) float64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.reserved[e.key(s, period, e.Now())]
}

// Reservation holds estimated cost for an in-flight request. Exactly one of
// Commit or Release must be called.
type Reservation struct {
	e      *Enforcer
	scopes []Scope
	keys   []spendKey
	amount float64
	once   sync.Once
}

// Check is Reserve with a zero estimate (kept for callers that only need a
// yes/no answer); the returned reservation is released immediately.
func (e *Enforcer) Check(ctx context.Context, scopes []Scope) (*Denial, error) {
	r, d, err := e.Reserve(ctx, scopes, 0)
	if r != nil {
		r.Release()
	}
	return d, err
}

// Reserve atomically checks every applicable limit against committed spend
// plus outstanding reservations and, if the request is admitted, reserves
// estimate USD on all scopes. Rate limits are applied afterwards.
func (e *Enforcer) Reserve(ctx context.Context, scopes []Scope, estimate float64) (*Reservation, *Denial, error) {
	if estimate < 0 {
		estimate = 0
	}
	now := e.Now()
	type check struct {
		s      Scope
		period string
		lim    float64
		k      spendKey
	}
	var checks []check
	for _, s := range scopes {
		l, ok := e.limitFor(s)
		if !ok {
			continue
		}
		for _, period := range []string{Daily, Monthly, Total} {
			lim := periodLimit(l, period)
			if lim <= 0 {
				continue
			}
			// Make sure the committed spend is loaded (and fresh) before locking.
			if _, err := e.Spend(ctx, s, period); err != nil {
				return nil, nil, err
			}
			checks = append(checks, check{s, period, lim, e.key(s, period, now)})
		}
	}
	res := &Reservation{e: e, scopes: scopes, amount: estimate}
	e.mu.Lock()
	for _, c := range checks {
		spent := e.spend[c.k].v
		held := e.reserved[c.k]
		over := spent+held >= c.lim-1e-9 // epsilon: float sums of reservations
		if e.opts.Strict && estimate > 0 {
			over = spent+held+estimate > c.lim+1e-9
		}
		if over {
			e.mu.Unlock()
			msg := fmt.Sprintf("OpenGate: %s has reached its %s budget ($%.4f of $%.2f", c.s.Name, c.period, spent, c.lim)
			if held > 0 {
				msg += fmt.Sprintf(", plus $%.4f reserved by in-flight requests", held)
			}
			msg += "). Raise the limit or wait for the period to reset."
			d := &Denial{Status: http.StatusTooManyRequests, Type: "insufficient_quota", Code: "budget_exceeded", Message: msg}
			if c.period != Total {
				d.RetryAfter = time.Until(nextPeriod(c.period, now)).Round(time.Second)
			}
			return nil, d, nil
		}
	}
	for _, s := range scopes {
		for _, period := range []string{Daily, Monthly, Total} {
			k := e.key(s, period, now)
			res.keys = append(res.keys, k)
			e.reserved[k] += estimate
		}
	}
	e.mu.Unlock()

	// Rate limits last, so rejected-for-budget requests don't consume tokens.
	for _, s := range scopes {
		l, ok := e.limitFor(s)
		if !ok || l.RPM <= 0 {
			continue
		}
		allowed, wait, err := e.rates.Allow(ctx, s.Kind+":"+s.ID, l.RPM, now)
		if err != nil {
			res.Release()
			return nil, nil, err
		}
		if !allowed {
			res.Release()
			return nil, &Denial{Status: http.StatusTooManyRequests, Type: "rate_limit_error", Code: "rate_limit_exceeded", RetryAfter: wait,
				Message: fmt.Sprintf("OpenGate: rate limit for %s exceeded (%d requests/minute).", s.Name, l.RPM)}, nil
		}
	}
	return res, nil, nil
}

func (r *Reservation) release() {
	r.e.mu.Lock()
	for _, k := range r.keys {
		v := r.e.reserved[k] - r.amount
		if v <= 1e-12 {
			delete(r.e.reserved, k)
		} else {
			r.e.reserved[k] = v
		}
	}
	r.e.mu.Unlock()
}

// Release drops the reservation without recording spend (request failed
// before any billable upstream usage).
func (r *Reservation) Release() {
	if r == nil {
		return
	}
	r.once.Do(r.release)
}

// Commit replaces the reservation with the actual cost (the log row must
// already be written) and fires threshold alerts.
func (r *Reservation) Commit(ctx context.Context, actual float64) {
	if r == nil {
		return
	}
	r.once.Do(func() {
		r.release()
		r.e.Record(ctx, r.scopes, actual)
	})
}

func nextPeriod(period string, t time.Time) time.Time {
	s := PeriodStart(period, t)
	if period == Daily {
		return s.AddDate(0, 0, 1)
	}
	return s.AddDate(0, 1, 0)
}

// Record adds cost to all scopes (the log row must already be written) and
// fires threshold alerts.
func (e *Enforcer) Record(ctx context.Context, scopes []Scope, cost float64) {
	if cost <= 0 {
		return
	}
	now := e.Now()
	for _, s := range scopes {
		for _, period := range []string{Daily, Monthly, Total} {
			k := e.key(s, period, now)
			e.mu.Lock()
			if v, ok := e.spend[k]; ok {
				v.v += cost
				e.spend[k] = v
			}
			e.mu.Unlock()
		}
		e.checkThresholds(ctx, s, cost)
	}
}

func (e *Enforcer) checkThresholds(ctx context.Context, s Scope, cost float64) {
	if e.alerter == nil {
		return
	}
	l, ok := e.limitFor(s)
	if !ok {
		return
	}
	for _, period := range []string{Daily, Monthly, Total} {
		lim := periodLimit(l, period)
		if lim <= 0 {
			continue
		}
		spent, err := e.Spend(ctx, s, period)
		if err != nil {
			continue
		}
		before := spent - cost
		for _, th := range e.alerter.Thresholds {
			mark := lim * th
			if spent >= mark && before < mark+1e-12 {
				start := PeriodStart(period, e.Now())
				fresh, err := e.store.MarkAlertSent(ctx, s.Kind, s.ID, period, start, th)
				if err != nil || !fresh {
					continue
				}
				e.alerter.Send(Event{
					Kind: "budget_threshold", Scope: s.Kind, ScopeID: s.ID, ScopeName: s.Name, Period: period,
					Threshold: th, SpentUSD: round(spent), LimitUSD: lim,
					Text: fmt.Sprintf(":rotating_light: OpenGate: %s reached %.0f%% of its %s budget ($%.2f / $%.2f).%s",
						s.Name, th*100, period, spent, lim, map[bool]string{true: " Further requests are now blocked.", false: ""}[th >= 1]),
				})
			}
		}
	}
}

func round(v float64) float64 { return math.Round(v*1e6) / 1e6 }

// LimitStatus is a limit plus current spend, for the dashboard.
type LimitStatus struct {
	store.Limit
	DailySpent   float64 `json:"daily_spent"`
	MonthlySpent float64 `json:"monthly_spent"`
	TotalSpent   float64 `json:"total_spent"`
	Reserved     float64 `json:"reserved"`
}

// Status reports spend for a limit.
func (e *Enforcer) Status(ctx context.Context, l store.Limit) LimitStatus {
	s := Scope{Kind: l.Scope, ID: l.ScopeID}
	st := LimitStatus{Limit: l}
	st.DailySpent, _ = e.Spend(ctx, s, Daily)
	st.MonthlySpent, _ = e.Spend(ctx, s, Monthly)
	st.TotalSpent, _ = e.Spend(ctx, s, Total)
	st.Reserved = e.Reserved(s, Total)
	return st
}
