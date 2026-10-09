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

	"github.com/opengate-dev/opengate/internal/store"
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

// Enforcer checks and records spend. It caches period spend in memory
// (loaded from SQLite on first use) so checks are cheap.
type Enforcer struct {
	store   *store.Store
	alerter *Alerter
	rl      *RateLimiter
	log     *slog.Logger
	Now     func() time.Time

	mu     sync.Mutex
	limits map[Scope]store.Limit // keyed by Kind+ID only (Name empty)
	spend  map[spendKey]float64
}

// NewEnforcer builds an enforcer and loads limits.
func NewEnforcer(ctx context.Context, st *store.Store, alerter *Alerter, log *slog.Logger) (*Enforcer, error) {
	e := &Enforcer{store: st, alerter: alerter, rl: NewRateLimiter(), log: log, Now: time.Now, spend: map[spendKey]float64{}}
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
	return l, ok
}

// Spend returns the current spend of a scope in a period.
func (e *Enforcer) Spend(ctx context.Context, s Scope, period string) (float64, error) {
	start := PeriodStart(period, e.Now())
	k := spendKey{s.Kind, s.ID, period, start.UnixMilli()}
	e.mu.Lock()
	v, ok := e.spend[k]
	e.mu.Unlock()
	if ok {
		return v, nil
	}
	v, err := e.store.SpendSince(ctx, s.Kind, s.ID, start)
	if err != nil {
		return 0, err
	}
	e.mu.Lock()
	if cur, ok := e.spend[k]; ok { // raced with a concurrent loader/recorder
		v = cur
	} else {
		e.spend[k] = v
	}
	e.mu.Unlock()
	return v, nil
}

// Check returns a Denial if any scope is over budget or rate limited.
func (e *Enforcer) Check(ctx context.Context, scopes []Scope) (*Denial, error) {
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
			spent, err := e.Spend(ctx, s, period)
			if err != nil {
				return nil, err
			}
			if spent >= lim {
				d := &Denial{Status: http.StatusTooManyRequests, Type: "insufficient_quota", Code: "budget_exceeded",
					Message: fmt.Sprintf("OpenGate: %s has reached its %s budget ($%.4f of $%.2f). Raise the limit or wait for the period to reset.", s.Name, period, spent, lim)}
				if period != Total {
					d.RetryAfter = time.Until(nextPeriod(period, e.Now())).Round(time.Second)
				}
				return d, nil
			}
		}
	}
	// Rate limits last, so rejected-for-budget requests don't consume tokens.
	for _, s := range scopes {
		l, ok := e.limitFor(s)
		if !ok || l.RPM <= 0 {
			continue
		}
		if ok, wait := e.rl.Allow(s.Kind+":"+s.ID, l.RPM, e.Now()); !ok {
			return &Denial{Status: http.StatusTooManyRequests, Type: "rate_limit_error", Code: "rate_limit_exceeded", RetryAfter: wait,
				Message: fmt.Sprintf("OpenGate: rate limit for %s exceeded (%d requests/minute).", s.Name, l.RPM)}, nil
		}
	}
	return nil, nil
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
			k := spendKey{s.Kind, s.ID, period, PeriodStart(period, now).UnixMilli()}
			e.mu.Lock()
			if v, ok := e.spend[k]; ok {
				e.spend[k] = v + cost
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
}

// Status reports spend for a limit.
func (e *Enforcer) Status(ctx context.Context, l store.Limit) LimitStatus {
	s := Scope{Kind: l.Scope, ID: l.ScopeID}
	st := LimitStatus{Limit: l}
	st.DailySpent, _ = e.Spend(ctx, s, Daily)
	st.MonthlySpent, _ = e.Spend(ctx, s, Monthly)
	st.TotalSpent, _ = e.Spend(ctx, s, Total)
	return st
}
