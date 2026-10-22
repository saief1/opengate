package server

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/saief1/opengate/internal/budget"
	"github.com/saief1/opengate/internal/store"
)

// Kill switch page, org-wide spend cap and measured performance.
func (s *Server) registerAdminControl(h func(string, http.HandlerFunc)) {
	h("GET /admin/api/kill-switch", s.adminKillSwitch)
	h("POST /admin/api/kill-switch", s.adminSetKillSwitch)
	h("GET /admin/api/org/spend-cap", s.adminOrgSpendCap)
	h("PUT /admin/api/org/spend-cap", s.adminPutOrgSpendCap)
	h("GET /admin/api/performance", s.adminPerformance)
}

func actorLabel(r *http.Request) string {
	if p := principalOf(r); p != nil && p.User != nil {
		return p.User.Email
	}
	return "admin token"
}

// killHistoryPrefixes are the audit actions shown as kill-switch history.
var killHistoryPrefixes = []string{"killswitch.", "key.kill", "key.unkill", "project.kill", "project.unkill"}

func (s *Server) adminKillSwitch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	org := orgID(r)
	sw, err := s.store.ListKillSwitches(ctx, org)
	if err != nil {
		storeErr(w, err)
		return
	}
	var global *store.KillSwitch
	scoped := []store.KillSwitch{}
	for i := range sw {
		if sw[i].Scope == store.KillGlobal {
			global = &sw[i]
		} else {
			scoped = append(scoped, sw[i])
		}
	}
	var hist []*store.AuditEntry
	for _, p := range killHistoryPrefixes {
		rows, _, err := s.store.ListAuditFiltered(ctx, store.AuditFilter{OrgID: org, Action: p, Limit: 100})
		if err != nil {
			storeErr(w, err)
			return
		}
		hist = append(hist, rows...)
	}
	sort.Slice(hist, func(i, j int) bool { return hist[i].TS.After(hist[j].TS) })
	if len(hist) > 100 {
		hist = hist[:100]
	}
	if hist == nil {
		hist = []*store.AuditEntry{}
	}
	writeJSON(w, 200, map[string]any{"global": global != nil, "global_switch": global, "switches": scoped, "history": hist})
}

func (s *Server) adminSetKillSwitch(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Scope   string `json:"scope"`
		ScopeID string `json:"scope_id"`
		Killed  bool   `json:"killed"`
	}
	if err := decode(r, &in); err != nil {
		bad(w, err)
		return
	}
	if !store.ValidKillScope(in.Scope) {
		bad(w, fmt.Errorf("scope must be global, agent or customer (use /keys/{id}/kill or /projects/{id}/kill for keys and projects)"))
		return
	}
	in.ScopeID = strings.TrimSpace(in.ScopeID)
	org := orgID(r)
	if in.Scope == store.KillAgent {
		proj, agent, ok := strings.Cut(in.ScopeID, "/")
		if !ok || agent == "" {
			bad(w, fmt.Errorf("agent scope_id must be project_id/agent"))
			return
		}
		if _, ok := s.projectInOrg(w, r, proj); !ok {
			return
		}
	}
	if in.Scope == store.KillGlobal {
		in.ScopeID = ""
	}
	target := in.Scope
	if in.ScopeID != "" {
		target += ":" + in.ScopeID
	}
	was := false
	if cur, err := s.store.ListKillSwitches(r.Context(), org); err == nil {
		for _, k := range cur {
			if k.Scope == in.Scope && k.ScopeID == in.ScopeID {
				was = true
			}
		}
	}
	s.auditChange(r, map[bool]string{true: "killswitch.engage", false: "killswitch.release"}[in.Killed], target, map[string]bool{"killed": was}, map[string]bool{"killed": in.Killed})
	if err := s.store.SetKillSwitch(r.Context(), org, in.Scope, in.ScopeID, actorLabel(r), in.Killed); err != nil {
		bad(w, err)
		return
	}
	aborted := 0
	if in.Killed {
		aborted = s.inflight.killWhere(func(f flight) bool {
			if f.orgID != org {
				return false
			}
			switch in.Scope {
			case store.KillGlobal:
				return true
			case store.KillAgent:
				return f.agentID == in.ScopeID
			default:
				return f.customerID == in.ScopeID
			}
		})
	}
	what := map[string]string{store.KillGlobal: "every request in the organization", store.KillAgent: "agent " + strconv.Quote(in.ScopeID), store.KillCustomer: "customer " + strconv.Quote(in.ScopeID)}[in.Scope]
	s.alerter.Send(budget.Event{Kind: "kill_switch", Scope: "org", ScopeID: org, ScopeName: target,
		Text: fmt.Sprintf(":octagonal_sign: OpenGate: kill switch for %s was %s by an operator (%d in-flight requests aborted).", what, map[bool]string{true: "ENGAGED", false: "released"}[in.Killed], aborted)})
	writeJSON(w, 200, map[string]any{"scope": in.Scope, "scope_id": in.ScopeID, "killed": in.Killed, "aborted_inflight": aborted})
}

// orgCapView is the org-wide cap with current spend and the sum of project
// caps (what charts fall back to when no org cap is set).
func (s *Server) orgCapView(r *http.Request) map[string]any {
	ctx := r.Context()
	org := orgID(r)
	l, src := s.enforcer.OrgCap(org)
	sc := budget.Scope{Kind: store.ScopeOrg, ID: org}
	daySpent, _ := s.enforcer.Spend(ctx, sc, budget.Daily)
	monSpent, _ := s.enforcer.Spend(ctx, sc, budget.Monthly)
	var pd, pm float64
	if ps, err := s.store.ListProjectsInOrg(ctx, org); err == nil {
		in := map[string]bool{}
		for _, p := range ps {
			in[p.ID] = true
		}
		if ls, err := s.store.ListLimits(ctx); err == nil {
			for _, x := range ls {
				if x.Scope == store.ScopeProject && in[x.ScopeID] {
					pd += x.DailyUSD
					pm += x.MonthlyUSD
				}
			}
		}
	}
	return map[string]any{"daily_usd": l.DailyUSD, "monthly_usd": l.MonthlyUSD, "source": src,
		"daily_spent": daySpent, "monthly_spent": monSpent,
		"project_caps": map[string]float64{"daily_usd": pd, "monthly_usd": pm}}
}

func (s *Server) adminOrgSpendCap(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.orgCapView(r))
}

func (s *Server) adminPutOrgSpendCap(w http.ResponseWriter, r *http.Request) {
	var in struct {
		DailyUSD   float64 `json:"daily_usd"`
		MonthlyUSD float64 `json:"monthly_usd"`
	}
	if err := decode(r, &in); err != nil {
		bad(w, err)
		return
	}
	if in.DailyUSD < 0 || in.MonthlyUSD < 0 {
		bad(w, fmt.Errorf("caps must not be negative"))
		return
	}
	org := orgID(r)
	before := s.currentLimit(r.Context(), store.ScopeOrg, org)
	var err error
	if in.DailyUSD == 0 && in.MonthlyUSD == 0 {
		err = s.store.DeleteLimit(r.Context(), store.ScopeOrg, org)
		if err == store.ErrNotFound {
			err = nil
		}
		s.auditChange(r, "org.spend_cap.clear", org, before, nil)
	} else {
		err = s.store.UpsertLimit(r.Context(), store.Limit{Scope: store.ScopeOrg, ScopeID: org, DailyUSD: in.DailyUSD, MonthlyUSD: in.MonthlyUSD})
		s.auditChange(r, "org.spend_cap.set", org, before, map[string]float64{"daily_usd": in.DailyUSD, "monthly_usd": in.MonthlyUSD})
	}
	if err != nil {
		storeErr(w, err)
		return
	}
	if err := s.enforcer.ReloadLimits(r.Context()); err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, 200, s.orgCapView(r))
}

func (s *Server) adminPerformance(w http.ResponseWriter, r *http.Request) {
	hours := atoiDefault(r.URL.Query().Get("hours"), 24)
	if hours <= 0 || hours > 24*90 {
		hours = 24
	}
	since := time.Now().UTC().Add(-time.Duration(hours) * time.Hour)
	models, provs, err := s.store.Performance(r.Context(), store.StatsFilter{OrgID: orgID(r)}, since)
	if err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"hours": hours, "since": since, "models": models, "providers": provs,
		"note": "Measured from request logs: p50/p95 over successful non-cached requests per serving model (streams: time to completion); success % per upstream attempt."})
}
