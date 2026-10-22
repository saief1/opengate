package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/saief1/opengate/internal/billing"
	"github.com/saief1/opengate/internal/guard"
	"github.com/saief1/opengate/internal/provider"
	"github.com/saief1/opengate/internal/store"
)

func (s *Server) registerAdminPhase2(h func(string, http.HandlerFunc)) {
	h("GET /admin/api/routing/rules", s.adminRoutingRules)
	h("POST /admin/api/routing/rules", s.adminSaveRoutingRule)
	h("PUT /admin/api/routing/rules/{id}", s.adminSaveRoutingRule)
	h("DELETE /admin/api/routing/rules/{id}", s.adminDeleteRoutingRule)
	h("GET /admin/api/routing/auto", s.adminAutoConfig)
	h("POST /admin/api/routing/preview", s.adminRoutingPreview)

	h("GET /admin/api/guardrails", s.adminGuardrails)
	h("PUT /admin/api/projects/{id}/guardrails", s.adminPutGuardrails)
	h("POST /admin/api/guardrails/test", s.adminGuardTest)

	h("GET /admin/api/cache", s.adminCache)
	h("PUT /admin/api/projects/{id}/cache", s.adminPutCache)
	h("DELETE /admin/api/cache", s.adminPurgeCache)

	h("GET /admin/api/customers", s.adminCustomers)
	h("PUT /admin/api/customers/{project}/{id}", s.adminPutCustomer)
	h("GET /admin/api/billing/markups", s.adminMarkups)
	h("POST /admin/api/billing/markups", s.gated(FeatureCustomerBilling, s.adminAddMarkup))
	h("DELETE /admin/api/billing/markups/{id}", s.adminDeleteMarkup)
	h("GET /admin/api/billing/export.csv", s.adminExportCSV)
	h("GET /admin/api/billing/stripe", s.adminStripeStatus)
	h("POST /admin/api/billing/stripe/sync", s.gated(FeatureCustomerBilling, s.adminStripeSync))
}

// projectInOrg loads a project and checks it belongs to the caller's org.
func (s *Server) projectInOrg(w http.ResponseWriter, r *http.Request, id string) (*store.Project, bool) {
	p, err := s.store.GetProject(r.Context(), id)
	if err != nil || p.OrgID != orgID(r) {
		writeError(w, http.StatusNotFound, "invalid_request_error", "not_found", "project not found")
		return nil, false
	}
	return p, true
}

// ---------- routing ----------

func (s *Server) adminRoutingRules(w http.ResponseWriter, r *http.Request) {
	rules, err := s.store.ListRoutingRules(r.Context(), orgID(r))
	if err != nil {
		storeErr(w, err)
		return
	}
	since := time.Now().UTC().AddDate(0, 0, -30)
	stats, _ := s.store.BreakdownSince(r.Context(), store.StatsFilter{OrgID: orgID(r)}, "route", since, 50)
	writeJSON(w, 200, map[string]any{"data": rules, "stats_30d": stats})
}

func (s *Server) validateTarget(name string) error {
	if s.autoEnabled(name) {
		return nil
	}
	_, err := s.resolve(name)
	return err
}

func (s *Server) adminSaveRoutingRule(w http.ResponseWriter, r *http.Request) {
	var body struct {
		store.RoutingRule
		Enabled *bool `json:"enabled"` // default true
	}
	if err := decode(r, &body); err != nil {
		bad(w, err)
		return
	}
	in := body.RoutingRule
	in.Enabled = body.Enabled == nil || *body.Enabled
	in.ID, in.OrgID = r.PathValue("id"), orgID(r)
	if in.ProjectID != "" {
		if _, ok := s.projectInOrg(w, r, in.ProjectID); !ok {
			return
		}
	}
	for _, t := range append([]string{in.Target}, in.Fallbacks...) {
		if err := s.validateTarget(strings.TrimSpace(t)); err != nil {
			bad(w, fmt.Errorf("target %q: %w", t, err))
			return
		}
	}
	if err := s.store.UpsertRoutingRule(r.Context(), &in); err != nil {
		if err == store.ErrNotFound {
			storeErr(w, err)
			return
		}
		bad(w, err)
		return
	}
	s.routingRules.invalidate()
	s.audit(r, "routing_rule.save", in.ID, map[string]any{"name": in.Name, "target": in.Target})
	writeJSON(w, 200, in)
}

func (s *Server) adminDeleteRoutingRule(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteRoutingRule(r.Context(), orgID(r), r.PathValue("id")); err != nil {
		storeErr(w, err)
		return
	}
	s.routingRules.invalidate()
	s.audit(r, "routing_rule.delete", r.PathValue("id"), nil)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) adminAutoConfig(w http.ResponseWriter, r *http.Request) {
	a := s.cfg.SmartRouting.Auto
	type m struct {
		Model        string  `json:"model"`
		Tier         int     `json:"tier"`
		InputPer1M   float64 `json:"input_per_1m"`
		OutputPer1M  float64 `json:"output_per_1m"`
		BlendedPrice float64 `json:"blended"`
	}
	out := []m{}
	for _, x := range a.Models {
		ts, _ := parseTarget(x.Model)
		p, _ := s.pricing.Table().Lookup(ts.Provider, ts.Model)
		out = append(out, m{x.Model, x.Tier, p.Input, p.Output, s.blendedPrice(x.Model)})
	}
	writeJSON(w, 200, map[string]any{"enabled": a.Enabled, "alias": a.Alias, "baseline": s.autoBaseline(), "models": out})
}

// adminRoutingPreview dry-runs routing for a sample request.
func (s *Server) adminRoutingPreview(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ProjectID string            `json:"project_id"`
		Agent     string            `json:"agent"`
		Headers   map[string]string `json:"headers"`
		Request   json.RawMessage   `json:"request"`
	}
	if err := decode(r, &in); err != nil {
		bad(w, err)
		return
	}
	var req provider.ChatRequest
	var raw map[string]json.RawMessage
	if json.Unmarshal(in.Request, &req) != nil || json.Unmarshal(in.Request, &raw) != nil || req.Model == "" {
		bad(w, fmt.Errorf("request must be a chat completion body with a model"))
		return
	}
	pid := in.ProjectID
	if pid == "" {
		if p, err := s.store.ProjectByNameInOrg(r.Context(), orgID(r), "default"); err == nil {
			pid = p.ID
		}
	}
	proj, ok := s.projectInOrg(w, r, pid)
	if !ok {
		return
	}
	hr, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, "/", nil)
	for k, v := range in.Headers {
		hr.Header.Set(k, v)
	}
	rs := &reqState{r: hr, raw: raw, proj: proj, key: &store.VirtualKey{Agent: in.Agent}, log: &store.RequestLog{}}
	prompt := estimateTokens(promptChars(&req))
	out := req.MaxOutputTokens()
	if out <= 0 {
		out = s.cfg.DefaultMaxTokens
	}
	plan, err := s.planChat(rs, &req, prompt, out)
	if err != nil {
		bad(w, err)
		return
	}
	targets := make([]string, len(plan.targets))
	for i, t := range plan.targets {
		targets[i] = t.String()
	}
	reqCost := 0.0
	if ts, err := s.resolve(plan.baseline); err == nil {
		reqCost = s.estimateCost(ts[:1], prompt, out)
	}
	routedCost := 0.0
	if len(plan.targets) > 0 {
		routedCost = s.estimateCost(plan.targets[:1], prompt, out)
	}
	writeJSON(w, 200, map[string]any{"route": plan.route, "targets": targets, "classification": plan.class,
		"prompt_tokens_est": prompt, "baseline": plan.baseline, "est_cost_baseline_usd": reqCost, "est_cost_routed_usd": routedCost})
}

// ---------- guardrails ----------

func (s *Server) adminGuardrails(w http.ResponseWriter, r *http.Request) {
	ps, err := s.store.ListProjectsInOrg(r.Context(), orgID(r))
	if err != nil {
		storeErr(w, err)
		return
	}
	type row struct {
		ProjectID string              `json:"project_id"`
		Name      string              `json:"name"`
		Override  bool                `json:"override"`
		Effective store.GuardSettings `json:"effective"`
	}
	rows := []row{}
	for _, p := range ps {
		rows = append(rows, row{p.ID, p.Name, p.Guardrails != nil, s.guardSettings(p)})
	}
	d := s.cfg.Guardrails
	since := time.Now().UTC().AddDate(0, 0, -30)
	flagged, _, _ := s.store.ListLogs(r.Context(), store.LogFilter{OrgID: orgID(r), GuardOnly: true, Limit: 25})
	sum, _ := s.store.SummarySince(r.Context(), store.StatsFilter{OrgID: orgID(r)}, since)
	writeJSON(w, 200, map[string]any{
		"defaults":   store.GuardSettings{PII: d.PII, Categories: d.Categories, Responses: d.Responses, Injection: d.Injection},
		"categories": guard.AllCategories, "projects": rows, "recent": flagged, "flagged_30d": sum.Flagged,
	})
}

func validGuard(g *store.GuardSettings) error {
	switch g.PII {
	case "", guard.ModeOff, guard.ModeLog, guard.ModeRedact, guard.ModeBlock:
	default:
		return fmt.Errorf("pii: want off|log|redact|block")
	}
	switch g.Injection {
	case "", guard.ModeOff, guard.ModeLog, guard.ModeBlock:
	default:
		return fmt.Errorf("injection: want off|log|block")
	}
	for _, c := range g.Categories {
		if !guard.ValidCategory(c) {
			return fmt.Errorf("unknown category %q (valid: %s)", c, strings.Join(guard.AllCategories, ", "))
		}
	}
	return nil
}

func (s *Server) adminPutGuardrails(w http.ResponseWriter, r *http.Request) {
	p, ok := s.projectInOrg(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	var in *store.GuardSettings // JSON null or empty body = inherit server defaults
	if r.ContentLength != 0 {
		if err := decode(r, &in); err != nil && !errors.Is(err, io.EOF) {
			bad(w, err)
			return
		}
	}
	if in != nil {
		if err := validGuard(in); err != nil {
			bad(w, err)
			return
		}
	}
	if err := s.store.SetProjectGuardrails(r.Context(), p.ID, in); err != nil {
		storeErr(w, err)
		return
	}
	s.audit(r, "guardrails.update", p.ID, in)
	p, _ = s.store.GetProject(r.Context(), p.ID)
	writeJSON(w, 200, map[string]any{"project": p, "effective": s.guardSettings(p)})
}

func (s *Server) adminGuardTest(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Text       string   `json:"text"`
		Categories []string `json:"categories"`
	}
	if err := decode(r, &in); err != nil {
		bad(w, err)
		return
	}
	out, f := guard.NewScanner(in.Categories).Scan(in.Text, true)
	score, hits := guard.InjectionScore(in.Text)
	writeJSON(w, 200, map[string]any{"redacted": out, "findings": f, "injection_score": score, "injection_signals": hits,
		"injection_flagged": score >= guard.InjectionThreshold})
}

// ---------- cache ----------

func (s *Server) adminCache(w http.ResponseWriter, r *http.Request) {
	var ids []string
	if pid := r.URL.Query().Get("project_id"); pid != "" {
		if _, ok := s.projectInOrg(w, r, pid); !ok {
			return
		}
		ids = []string{pid}
	} else {
		_, ids = s.orgProjects(r)
	}
	st, err := s.store.CacheStatsFor(r.Context(), ids)
	if err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"enabled": !s.cfg.Cache.Disabled, "max_ttl_seconds": int(s.cfg.Cache.MaxTTL.Seconds()), "stats": st})
}

func (s *Server) adminPutCache(w http.ResponseWriter, r *http.Request) {
	p, ok := s.projectInOrg(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	var in struct {
		TTLSeconds int `json:"ttl_seconds"`
	}
	if err := decode(r, &in); err != nil {
		bad(w, err)
		return
	}
	if err := s.store.SetProjectCacheTTL(r.Context(), p.ID, in.TTLSeconds); err != nil {
		bad(w, err)
		return
	}
	s.audit(r, "cache.update", p.ID, in)
	p, _ = s.store.GetProject(r.Context(), p.ID)
	writeJSON(w, 200, p)
}

func (s *Server) adminPurgeCache(w http.ResponseWriter, r *http.Request) {
	pid := r.URL.Query().Get("project_id")
	if pid == "" {
		bad(w, fmt.Errorf("project_id is required"))
		return
	}
	if _, ok := s.projectInOrg(w, r, pid); !ok {
		return
	}
	n, err := s.store.PurgeCache(r.Context(), pid)
	if err != nil {
		storeErr(w, err)
		return
	}
	s.audit(r, "cache.purge", pid, map[string]any{"entries": n})
	writeJSON(w, 200, map[string]any{"deleted": n})
}

// ---------- customers & billing ----------

func (s *Server) orgProjects(r *http.Request) (map[string]string, []string) {
	names, ids := map[string]string{}, []string{}
	ps, _ := s.store.ListProjectsInOrg(r.Context(), orgID(r))
	for _, p := range ps {
		names[p.ID] = p.Name
		ids = append(ids, p.ID)
	}
	return names, ids
}

func (s *Server) adminCustomers(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	from, to := time.Now().UTC().AddDate(0, 0, -30), time.Now().UTC().Add(time.Minute)
	if m := q.Get("month"); m != "" {
		var err error
		if from, to, err = billing.MonthRange(m); err != nil {
			bad(w, err)
			return
		}
	}
	usage, err := s.store.CustomerUsageBetween(r.Context(), store.StatsFilter{OrgID: orgID(r), ProjectID: q.Get("project_id")}, from, to)
	if err != nil {
		storeErr(w, err)
		return
	}
	names, ids := s.orgProjects(r)
	metas, _ := s.store.ListCustomerMeta(r.Context(), ids)
	meta := map[string]*store.Customer{}
	for _, c := range metas {
		meta[c.ProjectID+"\x00"+c.ID] = c
	}
	type row struct {
		*store.CustomerUsage
		Project          string `json:"project"`
		Name             string `json:"name"`
		StripeCustomerID string `json:"stripe_customer_id"`
	}
	rows := []row{}
	seen := map[string]bool{}
	for _, u := range usage {
		k := u.ProjectID + "\x00" + u.CustomerID
		seen[k] = true
		rr := row{CustomerUsage: u, Project: names[u.ProjectID]}
		if m := meta[k]; m != nil {
			rr.Name, rr.StripeCustomerID = m.Name, m.StripeCustomerID
		}
		rows = append(rows, rr)
	}
	for k, m := range meta { // customers with metadata but no usage in range
		if !seen[k] && (q.Get("project_id") == "" || q.Get("project_id") == m.ProjectID) {
			rows = append(rows, row{CustomerUsage: &store.CustomerUsage{ProjectID: m.ProjectID, CustomerID: m.ID}, Project: names[m.ProjectID], Name: m.Name, StripeCustomerID: m.StripeCustomerID})
		}
	}
	writeJSON(w, 200, map[string]any{"from": from, "to": to, "data": rows, "header": s.cfg.Billing.CustomerHeader})
}

func (s *Server) adminPutCustomer(w http.ResponseWriter, r *http.Request) {
	p, ok := s.projectInOrg(w, r, r.PathValue("project"))
	if !ok {
		return
	}
	var in struct {
		Name             string `json:"name"`
		StripeCustomerID string `json:"stripe_customer_id"`
	}
	if err := decode(r, &in); err != nil {
		bad(w, err)
		return
	}
	if in.StripeCustomerID != "" && !strings.HasPrefix(in.StripeCustomerID, "cus_") {
		bad(w, fmt.Errorf("stripe_customer_id must look like cus_..."))
		return
	}
	c := &store.Customer{ProjectID: p.ID, ID: r.PathValue("id"), Name: in.Name, StripeCustomerID: in.StripeCustomerID}
	if !validCustomerID(c.ID) {
		bad(w, fmt.Errorf("invalid customer id"))
		return
	}
	if err := s.store.UpsertCustomer(r.Context(), c); err != nil {
		bad(w, err)
		return
	}
	s.audit(r, "customer.update", p.ID+"/"+c.ID, in)
	writeJSON(w, 200, c)
}

func (s *Server) adminMarkups(w http.ResponseWriter, r *http.Request) {
	rs, err := s.store.ListMarkupRules(r.Context(), orgID(r))
	if err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"data": rs})
}

func (s *Server) adminAddMarkup(w http.ResponseWriter, r *http.Request) {
	var in store.MarkupRule
	if err := decode(r, &in); err != nil {
		bad(w, err)
		return
	}
	in.OrgID = orgID(r)
	if in.ProjectID != "" {
		if _, ok := s.projectInOrg(w, r, in.ProjectID); !ok {
			return
		}
	}
	if err := s.store.AddMarkupRule(r.Context(), &in); err != nil {
		bad(w, err)
		return
	}
	s.markups.Invalidate()
	s.auditChange(r, "markup.add", in.ID, nil, in)
	writeJSON(w, http.StatusCreated, in)
}

func (s *Server) adminDeleteMarkup(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteMarkupRule(r.Context(), orgID(r), r.PathValue("id")); err != nil {
		storeErr(w, err)
		return
	}
	s.markups.Invalidate()
	s.audit(r, "markup.delete", r.PathValue("id"), nil)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) adminExportCSV(w http.ResponseWriter, r *http.Request) {
	month := r.URL.Query().Get("month")
	if month == "" {
		month = time.Now().UTC().Format("2006-01")
	}
	from, to, err := billing.MonthRange(month)
	if err != nil {
		bad(w, err)
		return
	}
	usage, err := s.store.CustomerUsageBetween(r.Context(), store.StatsFilter{OrgID: orgID(r), ProjectID: r.URL.Query().Get("project_id")}, from, to)
	if err != nil {
		storeErr(w, err)
		return
	}
	names, ids := s.orgProjects(r)
	metas, _ := s.store.ListCustomerMeta(r.Context(), ids)
	meta := map[string]*store.Customer{}
	for _, c := range metas {
		meta[c.ProjectID+"\x00"+c.ID] = c
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="opengate-usage-%s.csv"`, month))
	_ = billing.WriteCSV(w, month, usage, names, meta)
}

func (s *Server) adminStripeStatus(w http.ResponseWriter, r *http.Request) {
	st := s.cfg.Billing.Stripe
	states, _ := s.store.StripeSyncStates(r.Context())
	_, ids := s.orgProjects(r)
	in := map[string]bool{}
	for _, id := range ids {
		in[id] = true
	}
	mine := []*store.StripeSyncState{}
	for _, x := range states {
		if in[x.ProjectID] {
			mine = append(mine, x)
		}
	}
	writeJSON(w, 200, map[string]any{"enabled": s.stripe != nil, "event_name": st.EventName, "unit": st.Unit,
		"interval_seconds": int(st.Interval.Seconds()), "lag_seconds": int(st.Lag.Seconds()), "customers": mine})
}

func (s *Server) adminStripeSync(w http.ResponseWriter, r *http.Request) {
	if s.stripe == nil {
		writeError(w, http.StatusConflict, "invalid_request_error", "stripe_disabled", "Stripe sync is disabled (billing.stripe.enabled + STRIPE_API_KEY).")
		return
	}
	_, ids := s.orgProjects(r)
	res := s.stripe.SyncProjects(r.Context(), ids)
	s.audit(r, "stripe.sync", "", res)
	writeJSON(w, 200, res)
}
