package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/opengate-dev/opengate/internal/budget"
	"github.com/opengate-dev/opengate/internal/secure"
	"github.com/opengate-dev/opengate/internal/store"
)

func (s *Server) registerAdmin(mux *http.ServeMux) {
	h := func(pattern string, fn http.HandlerFunc) { mux.Handle(pattern, s.adminAuth(fn)) }
	h("GET /admin/api/whoami", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"status": "ok", "version": Version})
	})
	h("GET /admin/api/overview", s.adminOverview)
	h("GET /admin/api/logs", s.adminLogs)
	h("GET /admin/api/logs/{id}", s.adminLog)
	h("GET /admin/api/projects", s.adminProjects)
	h("POST /admin/api/projects", s.adminCreateProject)
	h("PATCH /admin/api/projects/{id}", s.adminPatchProject)
	h("POST /admin/api/projects/{id}/kill", s.adminKillProject(true))
	h("POST /admin/api/projects/{id}/unkill", s.adminKillProject(false))
	h("GET /admin/api/keys", s.adminKeys)
	h("POST /admin/api/keys", s.adminCreateKey)
	h("POST /admin/api/keys/{id}/kill", s.adminKillKey(true))
	h("POST /admin/api/keys/{id}/unkill", s.adminKillKey(false))
	h("DELETE /admin/api/keys/{id}", s.adminRevokeKey)
	h("PATCH /admin/api/keys/{id}", s.adminPatchKey)
	h("GET /admin/api/limits", s.adminLimits)
	h("PUT /admin/api/limits", s.adminPutLimit)
	h("DELETE /admin/api/limits", s.adminDeleteLimit)
	h("GET /admin/api/providers", s.adminProviders)
	h("GET /admin/api/provider-keys", s.adminProviderKeys)
	h("POST /admin/api/provider-keys", s.adminAddProviderKey)
	h("DELETE /admin/api/provider-keys/{id}", s.adminDeleteProviderKey)
	h("GET /admin/api/pricing", s.adminPricing)
	h("POST /admin/api/pricing/reload", s.adminReloadPricing)
	s.registerAdminPhase2(h)
	mux.Handle("/admin/", s.adminAuth(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "invalid_request_error", "not_found", "unknown admin endpoint")
	}))
}

func (s *Server) adminAuth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		ip := s.clientIP(r)
		if locked, d := s.lockout.locked(ip); locked {
			w.Header().Set("Retry-After", strconv.Itoa(int(d.Seconds())+1))
			writeError(w, http.StatusTooManyRequests, "rate_limit_error", "admin_locked_out",
				"Too many failed admin authentications from this address; try again later.")
			return
		}
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || s.cfg.AdminToken == "" || !secure.ConstantTimeEqual(strings.TrimSpace(tok), s.cfg.AdminToken) {
			s.metrics.adminFails.Inc()
			if s.lockout.fail(ip) {
				s.log.Warn("admin lockout", "remote_ip", ip, "duration", s.cfg.AdminLockout.Lockout.String())
			}
			writeError(w, http.StatusUnauthorized, "invalid_request_error", "invalid_admin_token", "admin token required")
			return
		}
		s.lockout.success(ip)
		next(w, r)
	})
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return err
		}
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	return nil
}

func bad(w http.ResponseWriter, err error) {
	writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_request", err.Error())
}

func storeErr(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "invalid_request_error", "not_found", "not found")
		return
	}
	writeError(w, http.StatusInternalServerError, "server_error", "internal_error", err.Error())
}

func (s *Server) adminOverview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	if days <= 0 || days > 365 {
		days = 30
	}
	now := time.Now().UTC()
	today := budget.PeriodStart(budget.Daily, now)
	month := budget.PeriodStart(budget.Monthly, now)
	since := today.AddDate(0, 0, -(days - 1))
	sToday, err := s.store.SummarySince(ctx, store.StatsFilter{}, today)
	if err != nil {
		storeErr(w, err)
		return
	}
	sMonth, _ := s.store.SummarySince(ctx, store.StatsFilter{}, month)
	sRange, _ := s.store.SummarySince(ctx, store.StatsFilter{}, since)
	series, _ := s.store.SpendSeries(ctx, store.StatsFilter{}, since, 24*time.Hour)
	agents, _ := s.store.BreakdownSince(ctx, store.StatsFilter{}, "agent", since, 8)
	models, _ := s.store.BreakdownSince(ctx, store.StatsFilter{}, "model", since, 8)
	projects, _ := s.store.BreakdownSince(ctx, store.StatsFilter{}, "project", since, 8)
	routes, _ := s.store.BreakdownSince(ctx, store.StatsFilter{}, "route", since, 8)
	customers, _ := s.store.BreakdownSince(ctx, store.StatsFilter{}, "customer", since, 8)
	names := map[string]string{}
	if ps, err := s.store.ListProjects(ctx); err == nil {
		for _, p := range ps {
			names[p.ID] = p.Name
		}
	}
	for i := range projects {
		if n := names[projects[i].Name]; n != "" {
			projects[i].Name = n
		}
	}
	writeJSON(w, 200, map[string]any{
		"days": days, "since": since, "today": sToday, "month": sMonth, "range": sRange,
		"series": series, "top_agents": agents, "top_models": models, "top_projects": projects,
		"top_routes": routes, "top_customers": customers,
	})
}

func (s *Server) adminLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	logs, total, err := s.store.ListLogs(r.Context(), store.LogFilter{ProjectID: q.Get("project_id"), KeyID: q.Get("key_id"),
		Agent: q.Get("agent"), Model: q.Get("model"), ErrorsOnly: q.Get("errors") == "1", Limit: limit, Offset: offset})
	if err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"data": logs, "total": total})
}

func (s *Server) adminLog(w http.ResponseWriter, r *http.Request) {
	l, err := s.store.GetLog(r.Context(), r.PathValue("id"))
	if err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, 200, l)
}

func (s *Server) adminProjects(w http.ResponseWriter, r *http.Request) {
	ps, err := s.store.ListProjects(r.Context())
	if err != nil {
		storeErr(w, err)
		return
	}
	type row struct {
		*store.Project
		Keys       int     `json:"keys"`
		SpendToday float64 `json:"spend_today"`
		SpendMonth float64 `json:"spend_month"`
	}
	keys, _ := s.store.ListKeys(r.Context(), "")
	count := map[string]int{}
	for _, k := range keys {
		if !k.Revoked {
			count[k.ProjectID]++
		}
	}
	out := []row{}
	for _, p := range ps {
		sc := budget.Scope{Kind: store.ScopeProject, ID: p.ID}
		d, _ := s.enforcer.Spend(r.Context(), sc, budget.Daily)
		m, _ := s.enforcer.Spend(r.Context(), sc, budget.Monthly)
		out = append(out, row{Project: p, Keys: count[p.ID], SpendToday: d, SpendMonth: m})
	}
	writeJSON(w, 200, map[string]any{"data": out})
}

func (s *Server) adminCreateProject(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name      string `json:"name"`
		LogBodies bool   `json:"log_bodies"`
	}
	if err := decode(r, &in); err != nil {
		bad(w, err)
		return
	}
	p, err := s.store.CreateProject(r.Context(), in.Name, in.LogBodies)
	if err != nil {
		bad(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

func (s *Server) adminPatchProject(w http.ResponseWriter, r *http.Request) {
	var in struct {
		LogBodies *bool `json:"log_bodies"`
		policyInput
	}
	if err := decode(r, &in); err != nil {
		bad(w, err)
		return
	}
	id := r.PathValue("id")
	if in.policyInput.set() {
		cur, err := s.store.GetProject(r.Context(), id)
		if err != nil {
			storeErr(w, err)
			return
		}
		if err := s.store.SetProjectPolicy(r.Context(), id, in.policyInput.merge(cur.ModelPolicy)); err != nil {
			bad(w, err)
			return
		}
	}
	if in.LogBodies != nil {
		if err := s.store.SetProjectLogBodies(r.Context(), id, *in.LogBodies); err != nil {
			storeErr(w, err)
			return
		}
	}
	p, err := s.store.GetProject(r.Context(), id)
	if err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, 200, p)
}

func (s *Server) adminKillProject(kill bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := s.store.SetProjectKilled(r.Context(), id, kill); err != nil {
			storeErr(w, err)
			return
		}
		p, _ := s.store.GetProject(r.Context(), id)
		aborted := 0
		if kill {
			aborted = s.inflight.kill("", id)
		}
		s.alerter.Send(budget.Event{Kind: "kill_switch", Scope: store.ScopeProject, ScopeID: id, ScopeName: p.Name,
			Text: fmt.Sprintf(":octagonal_sign: OpenGate: project %q was %s by an operator (%d in-flight requests aborted).", p.Name, map[bool]string{true: "KILLED", false: "re-enabled"}[kill], aborted)})
		writeJSON(w, 200, map[string]any{"project": p, "aborted_inflight": aborted})
	}
}

func (s *Server) adminKeys(w http.ResponseWriter, r *http.Request) {
	ks, err := s.store.ListKeys(r.Context(), r.URL.Query().Get("project_id"))
	if err != nil {
		storeErr(w, err)
		return
	}
	type row struct {
		*store.VirtualKey
		SpendToday float64 `json:"spend_today"`
		SpendTotal float64 `json:"spend_total"`
	}
	out := []row{}
	for _, k := range ks {
		if k.Revoked && r.URL.Query().Get("include_revoked") != "1" {
			continue
		}
		sc := budget.Scope{Kind: store.ScopeKey, ID: k.ID}
		d, _ := s.enforcer.Spend(r.Context(), sc, budget.Daily)
		t, _ := s.enforcer.Spend(r.Context(), sc, budget.Total)
		out = append(out, row{VirtualKey: k, SpendToday: d, SpendTotal: t})
	}
	writeJSON(w, 200, map[string]any{"data": out})
}

type limitInput struct {
	DailyUSD   float64 `json:"daily_usd"`
	MonthlyUSD float64 `json:"monthly_usd"`
	TotalUSD   float64 `json:"total_usd"`
	RPM        int     `json:"rpm"`
}

func (s *Server) adminCreateKey(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ProjectID string      `json:"project_id"`
		Name      string      `json:"name"`
		Agent     string      `json:"agent"`
		User      string      `json:"user"`
		Limits    *limitInput `json:"limits"`
	}
	if err := decode(r, &in); err != nil {
		bad(w, err)
		return
	}
	if in.ProjectID == "" {
		p, err := s.store.EnsureProject(r.Context(), "default")
		if err != nil {
			storeErr(w, err)
			return
		}
		in.ProjectID = p.ID
	}
	plain, k, err := s.store.CreateKey(r.Context(), in.ProjectID, in.Name, in.Agent, in.User)
	if err != nil {
		bad(w, err)
		return
	}
	if in.Limits != nil {
		l := store.Limit{Scope: store.ScopeKey, ScopeID: k.ID, DailyUSD: in.Limits.DailyUSD, MonthlyUSD: in.Limits.MonthlyUSD, TotalUSD: in.Limits.TotalUSD, RPM: in.Limits.RPM}
		if err := s.store.UpsertLimit(r.Context(), l); err != nil {
			bad(w, err)
			return
		}
		_ = s.enforcer.ReloadLimits(r.Context())
	}
	writeJSON(w, http.StatusCreated, map[string]any{"key": plain, "virtual_key": k,
		"note": "Store this key now; OpenGate only keeps its hash and cannot show it again."})
}

func (s *Server) adminKillKey(kill bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := s.store.SetKeyKilled(r.Context(), id, kill); err != nil {
			storeErr(w, err)
			return
		}
		k, _ := s.store.GetKey(r.Context(), id)
		aborted := 0
		if kill {
			aborted = s.inflight.kill(id, "")
		}
		s.alerter.Send(budget.Event{Kind: "kill_switch", Scope: store.ScopeKey, ScopeID: id, ScopeName: k.Name,
			Text: fmt.Sprintf(":octagonal_sign: OpenGate: key %q (%s) was %s by an operator (%d in-flight requests aborted).", k.Name, k.Prefix, map[bool]string{true: "KILLED", false: "re-enabled"}[kill], aborted)})
		writeJSON(w, 200, map[string]any{"virtual_key": k, "aborted_inflight": aborted})
	}
}

// policyInput is the PATCH body for model policies (nil = unchanged).
type policyInput struct {
	AllowedModels *[]string `json:"allowed_models"`
	DeniedModels  *[]string `json:"denied_models"`
	MaxTokens     *int      `json:"max_tokens"`
}

func (p policyInput) set() bool {
	return p.AllowedModels != nil || p.DeniedModels != nil || p.MaxTokens != nil
}

func (p policyInput) merge(cur store.ModelPolicy) store.ModelPolicy {
	if p.AllowedModels != nil {
		cur.AllowedModels = *p.AllowedModels
	}
	if p.DeniedModels != nil {
		cur.DeniedModels = *p.DeniedModels
	}
	if p.MaxTokens != nil {
		cur.MaxTokens = *p.MaxTokens
	}
	return cur
}

func (s *Server) adminPatchKey(w http.ResponseWriter, r *http.Request) {
	var in policyInput
	if err := decode(r, &in); err != nil {
		bad(w, err)
		return
	}
	id := r.PathValue("id")
	k, err := s.store.GetKey(r.Context(), id)
	if err != nil {
		storeErr(w, err)
		return
	}
	if err := s.store.SetKeyPolicy(r.Context(), id, in.merge(k.ModelPolicy)); err != nil {
		bad(w, err)
		return
	}
	k, _ = s.store.GetKey(r.Context(), id)
	writeJSON(w, 200, k)
}

func (s *Server) adminRevokeKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.store.RevokeKey(r.Context(), id); err != nil {
		storeErr(w, err)
		return
	}
	s.inflight.kill(id, "")
	writeJSON(w, 200, map[string]string{"status": "revoked"})
}

func (s *Server) adminLimits(w http.ResponseWriter, r *http.Request) {
	ls, err := s.store.ListLimits(r.Context())
	if err != nil {
		storeErr(w, err)
		return
	}
	keys, _ := s.store.ListKeys(r.Context(), "")
	projects, _ := s.store.ListProjects(r.Context())
	kn, pn := map[string]string{}, map[string]string{}
	for _, k := range keys {
		kn[k.ID] = k.Name + " (" + k.Prefix + "…)"
	}
	for _, p := range projects {
		pn[p.ID] = p.Name
	}
	type row struct {
		budget.LimitStatus
		Label string `json:"label"`
	}
	out := []row{}
	for _, l := range ls {
		label := l.ScopeID
		switch l.Scope {
		case store.ScopeKey:
			label = kn[l.ScopeID]
		case store.ScopeProject:
			label = pn[l.ScopeID]
		case store.ScopeAgent:
			proj, agent, _ := strings.Cut(l.ScopeID, "/")
			label = agent + " @ " + pn[proj]
		}
		out = append(out, row{LimitStatus: s.enforcer.Status(r.Context(), l), Label: label})
	}
	writeJSON(w, 200, map[string]any{"data": out})
}

func (s *Server) adminPutLimit(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Scope   string `json:"scope"`
		ScopeID string `json:"scope_id"`
		limitInput
	}
	if err := decode(r, &in); err != nil {
		bad(w, err)
		return
	}
	l := store.Limit{Scope: in.Scope, ScopeID: in.ScopeID, DailyUSD: in.DailyUSD, MonthlyUSD: in.MonthlyUSD, TotalUSD: in.TotalUSD, RPM: in.RPM}
	if err := s.store.UpsertLimit(r.Context(), l); err != nil {
		bad(w, err)
		return
	}
	if err := s.enforcer.ReloadLimits(r.Context()); err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, 200, s.enforcer.Status(r.Context(), l))
}

func (s *Server) adminDeleteLimit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if err := s.store.DeleteLimit(r.Context(), q.Get("scope"), q.Get("scope_id")); err != nil {
		storeErr(w, err)
		return
	}
	_ = s.enforcer.ReloadLimits(r.Context())
	writeJSON(w, 200, map[string]string{"status": "deleted"})
}

func (s *Server) adminProviders(w http.ResponseWriter, r *http.Request) {
	type row struct {
		Name      string `json:"name"`
		Type      string `json:"type"`
		BaseURL   string `json:"base_url"`
		KeySource string `json:"key_source"`
		Timeout   string `json:"timeout"`
	}
	out := []row{}
	for name, p := range s.cfg.Providers {
		out = append(out, row{Name: name, Type: p.Type, BaseURL: p.BaseURL, KeySource: s.keySource(r.Context(), name), Timeout: p.Timeout.String()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, 200, map[string]any{"data": out, "routes": s.cfg.Routes,
		"retry": map[string]any{"max_attempts": s.cfg.Retry.MaxAttempts, "initial_backoff": s.cfg.Retry.InitialBackoff.String(), "max_backoff": s.cfg.Retry.MaxBackoff.String()}})
}

func (s *Server) adminProviderKeys(w http.ResponseWriter, r *http.Request) {
	ks, err := s.store.ListProviderKeys(r.Context(), r.URL.Query().Get("provider"))
	if err != nil {
		storeErr(w, err)
		return
	}
	if ks == nil {
		ks = []*store.ProviderKey{}
	}
	writeJSON(w, 200, map[string]any{"data": ks})
}

func (s *Server) adminAddProviderKey(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Provider string `json:"provider"`
		Label    string `json:"label"`
		Key      string `json:"key"`
	}
	if err := decode(r, &in); err != nil {
		bad(w, err)
		return
	}
	if _, ok := s.cfg.Providers[in.Provider]; !ok {
		bad(w, fmt.Errorf("unknown provider %q", in.Provider))
		return
	}
	pk, err := s.store.AddProviderKey(r.Context(), s.cipher, in.Provider, in.Label, in.Key)
	if err != nil {
		bad(w, err)
		return
	}
	s.invalidateKeyCache()
	writeJSON(w, http.StatusCreated, pk)
}

func (s *Server) adminDeleteProviderKey(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteProviderKey(r.Context(), r.PathValue("id")); err != nil {
		storeErr(w, err)
		return
	}
	s.invalidateKeyCache()
	writeJSON(w, 200, map[string]string{"status": "deleted"})
}

func (s *Server) adminPricing(w http.ResponseWriter, r *http.Request) {
	t := s.pricing.Table()
	out := map[string]any{}
	for _, m := range t.Models() {
		prov, model, _ := strings.Cut(m, "/")
		p, _ := t.Lookup(prov, model)
		out[m] = p
	}
	writeJSON(w, 200, map[string]any{"source": s.pricing.Source(), "models": out})
}

func (s *Server) adminReloadPricing(w http.ResponseWriter, r *http.Request) {
	if err := s.pricing.Reload(); err != nil {
		bad(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "reloaded", "source": s.pricing.Source()})
}
