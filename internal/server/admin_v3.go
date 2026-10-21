package server

import (
	"encoding/csv"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/saief1/opengate/internal/store"
)

// Team-plan features. On a self-hosted install they are unlocked unless
// `plan: free` (OPENGATE_PLAN=free) is configured; in cloud mode the org's
// subscription decides.
const (
	FeatureAuditLog        = "audit_log"
	FeatureRoles           = "roles"
	FeatureCustomerBilling = "customer_billing"
)

var teamFeatures = []string{FeatureAuditLog, FeatureRoles, FeatureCustomerBilling}

// comingSoon is shown in the dashboard and returned by /admin/api/plan.
var comingSoon = []string{"sso", "2fa", "opentelemetry_export", "approval_workflows"}

// planName is the effective plan for the request's org.
func (s *Server) planName(r *http.Request) string {
	if s.cloud != nil {
		p, err := s.orgPlan(r.Context(), orgID(r))
		if err != nil {
			return "free"
		}
		return p.ID
	}
	if s.cfg.Plan == "" {
		return "self-hosted"
	}
	return s.cfg.Plan
}

// planAllows reports whether the org's plan includes a Team feature.
func (s *Server) planAllows(r *http.Request, feature string) bool {
	if p := principalOf(r); s.cloud != nil && p != nil && p.Kind == "token" {
		return true // the cloud operator (admin token) is never plan-limited
	}
	switch s.planName(r) {
	case "free":
		return false
	}
	return true
}

func planRequired(w http.ResponseWriter, feature string) {
	writeError(w, http.StatusPaymentRequired, "permission_error", "plan_required",
		fmt.Sprintf("OpenGate: %s is part of the Team plan. Upgrade in Plan & billing (self-hosted: unset OPENGATE_PLAN or set it to team).", strings.ReplaceAll(feature, "_", " ")))
}

// gated wraps a handler so it answers 402 unless the plan has the feature.
func (s *Server) gated(feature string, fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.planAllows(r, feature) {
			planRequired(w, feature)
			return
		}
		fn(w, r)
	}
}

func (s *Server) registerAdminV3(h func(string, http.HandlerFunc)) {
	h("GET /admin/api/plan", s.adminPlan)

	h("GET /admin/api/audit", s.gated(FeatureAuditLog, s.adminAudit))
	h("GET /admin/api/audit/export.csv", s.gated(FeatureAuditLog, s.adminAuditCSV))

	h("GET /admin/api/mcp/servers", s.adminMCPServers)
	h("POST /admin/api/mcp/servers", s.adminCreateMCPServer)
	h("PATCH /admin/api/mcp/servers/{id}", s.adminPatchMCPServer)
	h("DELETE /admin/api/mcp/servers/{id}", s.adminDeleteMCPServer)
	h("GET /admin/api/mcp/servers/{id}/credentials", s.adminMCPCredentials)
	h("PUT /admin/api/mcp/servers/{id}/credentials", s.adminPutMCPCredential)
	h("DELETE /admin/api/mcp/servers/{id}/credentials/{cid}", s.adminDeleteMCPCredential)
	h("GET /admin/api/mcp/policies", s.adminMCPPolicies)
	h("POST /admin/api/mcp/policies", s.adminAddMCPPolicy)
	h("DELETE /admin/api/mcp/policies/{id}", s.adminDeleteMCPPolicy)
	h("GET /admin/api/mcp/calls", s.adminMCPCalls)
	h("POST /admin/api/mcp/check", s.adminMCPCheck)
}

func (s *Server) adminPlan(w http.ResponseWriter, r *http.Request) {
	feats := map[string]bool{}
	for _, f := range teamFeatures {
		feats[f] = s.planAllows(r, f)
	}
	p := principalOf(r)
	writeJSON(w, 200, map[string]any{"plan": s.planName(r), "features": feats, "coming_soon": comingSoon,
		"role": p.Role, "cloud": s.cloud != nil})
}

// ---------- audit ----------

func (s *Server) auditFilter(r *http.Request) (store.AuditFilter, error) {
	q := r.URL.Query()
	f := store.AuditFilter{OrgID: orgID(r), Actor: q.Get("actor"), Action: q.Get("action"), Target: q.Get("target"),
		Limit: atoiDefault(q.Get("limit"), 100), Offset: atoiDefault(q.Get("offset"), 0)}
	parse := func(v string) (time.Time, error) {
		if v == "" {
			return time.Time{}, nil
		}
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			return t, nil
		}
		return time.Parse("2006-01-02", v)
	}
	var err error
	if f.Since, err = parse(q.Get("since")); err != nil {
		return f, errors.New("since must be RFC3339 or YYYY-MM-DD")
	}
	if f.Until, err = parse(q.Get("until")); err != nil {
		return f, errors.New("until must be RFC3339 or YYYY-MM-DD")
	}
	if len(q.Get("until")) == 10 {
		f.Until = f.Until.AddDate(0, 0, 1) // inclusive day
	}
	return f, nil
}

func (s *Server) adminAudit(w http.ResponseWriter, r *http.Request) {
	f, err := s.auditFilter(r)
	if err != nil {
		bad(w, err)
		return
	}
	rows, total, err := s.store.ListAuditFiltered(r.Context(), f)
	if err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"data": rows, "total": total})
}

func (s *Server) adminAuditCSV(w http.ResponseWriter, r *http.Request) {
	f, err := s.auditFilter(r)
	if err != nil {
		bad(w, err)
		return
	}
	f.Limit, f.Offset = 5000, 0
	rows, _, err := s.store.ListAuditFiltered(r.Context(), f)
	if err != nil {
		storeErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="opengate-audit.csv"`)
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"id", "ts", "actor_type", "actor_id", "actor_email", "action", "target", "status", "ip", "before", "after", "detail"})
	for _, a := range rows {
		_ = cw.Write([]string{a.ID, a.TS.UTC().Format(time.RFC3339), a.ActorType, a.ActorID, csvSafe(a.ActorEmail), csvSafe(a.Action), csvSafe(a.Target),
			strconv.Itoa(a.Status), a.IP, csvSafe(string(a.Before)), csvSafe(string(a.After)), csvSafe(string(a.Detail))})
	}
	cw.Flush()
}

// csvSafe defuses spreadsheet formula injection.
func csvSafe(v string) string {
	if v != "" && strings.ContainsAny(v[:1], "=+-@\t\r") {
		return "'" + v
	}
	return v
}

// ---------- MCP servers ----------

type mcpServerInput struct {
	Name        *string `json:"name"`
	URL         *string `json:"url"`
	Transport   *string `json:"transport"`
	AuthHeader  *string `json:"auth_header"`
	Secret      *string `json:"secret"` // write-only; "" clears
	DefaultRPM  *int    `json:"default_rpm"`
	Enabled     *bool   `json:"enabled"`
	ClearSecret bool    `json:"clear_secret"`
}

func secretHint(v string) string {
	if len(v) < 8 {
		return "…set"
	}
	return "…" + v[len(v)-4:]
}

// mcpPublic is what the API returns for a server (never the secret).
func mcpPublic(m *store.MCPServer) map[string]any {
	return map[string]any{"id": m.ID, "name": m.Name, "url": m.URL, "transport": m.Transport, "auth_header": m.AuthHeader,
		"has_secret": m.SecretCT != "", "secret_hint": m.SecretHint, "default_rpm": m.DefaultRPM, "enabled": m.Enabled,
		"gateway_path": "/mcp/" + m.Name, "created_at": m.CreatedAt, "updated_at": m.UpdatedAt}
}

func (s *Server) adminMCPServers(w http.ResponseWriter, r *http.Request) {
	ms, err := s.store.ListMCPServers(r.Context(), orgID(r))
	if err != nil {
		storeErr(w, err)
		return
	}
	out := make([]map[string]any, 0, len(ms))
	for _, m := range ms {
		out = append(out, mcpPublic(m))
	}
	writeJSON(w, 200, map[string]any{"data": out})
}

func (s *Server) applyMCPInput(m *store.MCPServer, in mcpServerInput) error {
	if in.URL != nil {
		m.URL = *in.URL
	}
	if in.Transport != nil {
		m.Transport = *in.Transport
	}
	if in.AuthHeader != nil {
		m.AuthHeader = *in.AuthHeader
	}
	if in.DefaultRPM != nil {
		m.DefaultRPM = *in.DefaultRPM
	}
	if in.Enabled != nil {
		m.Enabled = *in.Enabled
	}
	if err := store.ValidateMCPServer(m, s.cfg.AllowInsecureUpstream); err != nil {
		return err
	}
	if in.ClearSecret || (in.Secret != nil && *in.Secret == "") {
		m.SecretCT, m.SecretHint = "", ""
	} else if in.Secret != nil {
		ct, err := s.cipher.Encrypt([]byte(*in.Secret), store.MCPSecretAAD(m.ID))
		if err != nil {
			return err
		}
		m.SecretCT, m.SecretHint = ct, secretHint(*in.Secret)
	}
	return nil
}

func (s *Server) adminCreateMCPServer(w http.ResponseWriter, r *http.Request) {
	var in mcpServerInput
	if err := decode(r, &in); err != nil {
		bad(w, err)
		return
	}
	m := &store.MCPServer{ID: store.NewMCPServerID(), OrgID: orgID(r), Enabled: true}
	if in.Name != nil {
		m.Name = *in.Name
	}
	if err := s.applyMCPInput(m, in); err != nil {
		bad(w, err)
		return
	}
	if err := s.store.CreateMCPServer(r.Context(), m); err != nil {
		if strings.Contains(err.Error(), "already exists") {
			bad(w, err)
			return
		}
		storeErr(w, err)
		return
	}
	s.auditChange(r, "mcp.server.create", m.ID, nil, mcpAuditView(m))
	writeJSON(w, http.StatusCreated, mcpPublic(m))
}

// mcpAuditView is a server's audit snapshot (secret presence only).
func mcpAuditView(m *store.MCPServer) map[string]any {
	v := mcpPublic(m)
	delete(v, "created_at")
	delete(v, "updated_at")
	return v
}

func (s *Server) adminPatchMCPServer(w http.ResponseWriter, r *http.Request) {
	var in mcpServerInput
	if err := decode(r, &in); err != nil {
		bad(w, err)
		return
	}
	if in.Name != nil {
		bad(w, errors.New("name cannot be changed (it is part of the gateway URL); create a new server instead"))
		return
	}
	m, err := s.store.GetMCPServer(r.Context(), orgID(r), r.PathValue("id"))
	if err != nil {
		storeErr(w, err)
		return
	}
	before := mcpAuditView(m)
	if err := s.applyMCPInput(m, in); err != nil {
		bad(w, err)
		return
	}
	if err := s.store.UpdateMCPServer(r.Context(), m); err != nil {
		storeErr(w, err)
		return
	}
	after := mcpAuditView(m)
	if in.Secret != nil && *in.Secret != "" {
		after["secret_rotated"] = true
	}
	s.auditChange(r, "mcp.server.update", m.ID, before, after)
	writeJSON(w, 200, mcpPublic(m))
}

func (s *Server) adminDeleteMCPServer(w http.ResponseWriter, r *http.Request) {
	m, err := s.store.GetMCPServer(r.Context(), orgID(r), r.PathValue("id"))
	if err != nil {
		storeErr(w, err)
		return
	}
	if err := s.store.DeleteMCPServer(r.Context(), orgID(r), m.ID); err != nil {
		storeErr(w, err)
		return
	}
	s.auditChange(r, "mcp.server.delete", m.ID, mcpAuditView(m), nil)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) mcpServerOr404(w http.ResponseWriter, r *http.Request, id string) (*store.MCPServer, bool) {
	m, err := s.store.GetMCPServer(r.Context(), orgID(r), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "invalid_request_error", "not_found", "MCP server not found")
		return nil, false
	}
	return m, true
}

func (s *Server) adminMCPCredentials(w http.ResponseWriter, r *http.Request) {
	m, ok := s.mcpServerOr404(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	cs, err := s.store.ListMCPCredentials(r.Context(), m.ID)
	if err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"data": cs})
}

func (s *Server) adminPutMCPCredential(w http.ResponseWriter, r *http.Request) {
	m, ok := s.mcpServerOr404(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	var in struct {
		Agent  string `json:"agent"`
		Header string `json:"header"`
		Secret string `json:"secret"`
	}
	if err := decode(r, &in); err != nil {
		bad(w, err)
		return
	}
	if in.Secret == "" {
		bad(w, errors.New("secret is required"))
		return
	}
	c := &store.MCPCredential{ID: store.NewMCPCredentialID(), ServerID: m.ID, Agent: strings.TrimSpace(in.Agent), Header: in.Header, SecretHint: secretHint(in.Secret)}
	ct, err := s.cipher.Encrypt([]byte(in.Secret), store.MCPSecretAAD(c.ID))
	if err != nil {
		storeErr(w, err)
		return
	}
	c.SecretCT = ct
	if err := s.store.PutMCPCredential(r.Context(), c); err != nil {
		bad(w, err)
		return
	}
	s.auditChange(r, "mcp.credential.set", m.Name+"/"+c.Agent, nil, map[string]string{"agent": c.Agent, "header": c.Header, "secret_hint": c.SecretHint})
	writeJSON(w, 200, c)
}

func (s *Server) adminDeleteMCPCredential(w http.ResponseWriter, r *http.Request) {
	m, ok := s.mcpServerOr404(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	if err := s.store.DeleteMCPCredential(r.Context(), m.ID, r.PathValue("cid")); err != nil {
		storeErr(w, err)
		return
	}
	s.auditChange(r, "mcp.credential.delete", m.Name+"/"+r.PathValue("cid"), map[string]string{"credential_id": r.PathValue("cid")}, nil)
	w.WriteHeader(http.StatusNoContent)
}

// ---------- MCP policies ----------

func (s *Server) adminMCPPolicies(w http.ResponseWriter, r *http.Request) {
	ps, err := s.store.ListMCPPolicies(r.Context(), orgID(r), r.URL.Query().Get("server_id"))
	if err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"data": ps, "default_deny": s.cfg.MCP.DefaultDeny})
}

func (s *Server) adminAddMCPPolicy(w http.ResponseWriter, r *http.Request) {
	var in store.MCPPolicy
	if err := decode(r, &in); err != nil {
		bad(w, err)
		return
	}
	if _, ok := s.mcpServerOr404(w, r, in.ServerID); !ok {
		return
	}
	in.OrgID = orgID(r)
	if err := s.store.AddMCPPolicy(r.Context(), &in); err != nil {
		bad(w, err)
		return
	}
	s.auditChange(r, "mcp.policy.add", in.ID, nil, in)
	writeJSON(w, http.StatusCreated, in)
}

func (s *Server) adminDeleteMCPPolicy(w http.ResponseWriter, r *http.Request) {
	p, err := s.store.GetMCPPolicy(r.Context(), orgID(r), r.PathValue("id"))
	if err != nil {
		storeErr(w, err)
		return
	}
	if err := s.store.DeleteMCPPolicy(r.Context(), orgID(r), p.ID); err != nil {
		storeErr(w, err)
		return
	}
	s.auditChange(r, "mcp.policy.delete", p.ID, p, nil)
	w.WriteHeader(http.StatusNoContent)
}

// adminMCPCheck is a dry run: would agent X be allowed to call tool Y?
func (s *Server) adminMCPCheck(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ServerID string `json:"server_id"`
		Agent    string `json:"agent"`
		Tool     string `json:"tool"`
	}
	if err := decode(r, &in); err != nil {
		bad(w, err)
		return
	}
	m, ok := s.mcpServerOr404(w, r, in.ServerID)
	if !ok {
		return
	}
	ps, err := s.store.ListMCPPolicies(r.Context(), orgID(r), m.ID)
	if err != nil {
		storeErr(w, err)
		return
	}
	d := decideTool(ps, in.Agent, in.Tool, s.cfg.MCP.DefaultDeny, m.DefaultRPM)
	writeJSON(w, 200, map[string]any{"allowed": d.Allowed, "rpm": d.RPM, "rule": d.Rule})
}

func (s *Server) adminMCPCalls(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	calls, total, err := s.store.ListMCPCalls(r.Context(), store.MCPCallFilter{OrgID: orgID(r), ServerID: q.Get("server_id"), Agent: q.Get("agent"),
		Tool: q.Get("tool"), ErrorsOnly: q.Get("errors") == "1", Limit: atoiDefault(q.Get("limit"), 100), Offset: atoiDefault(q.Get("offset"), 0)})
	if err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"data": calls, "total": total})
}
