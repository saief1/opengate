package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/saief1/opengate/internal/budget"
	"github.com/saief1/opengate/internal/pricing"
	"github.com/saief1/opengate/internal/provider"
	"github.com/saief1/opengate/internal/secure"
	"github.com/saief1/opengate/internal/store"
)

// Endpoints served by the gateway (stored in request_logs.endpoint).
const (
	EndpointChat       = "chat"
	EndpointEmbeddings = "embeddings"
	EndpointResponses  = "responses"
)

type targetSpec struct {
	Provider string
	Model    string
}

func (t targetSpec) String() string { return t.Provider + "/" + t.Model }

func parseTarget(s string) (targetSpec, bool) {
	prov, m, ok := strings.Cut(s, "/")
	return targetSpec{Provider: prov, Model: m}, ok && m != "" && prov != ""
}

// resolve maps a requested model (alias or provider/model) to an ordered
// list of targets.
func (s *Server) resolve(model string) ([]targetSpec, error) {
	list := s.cfg.Routes[model]
	if len(list) == 0 {
		list = []string{model}
	}
	out := make([]targetSpec, 0, len(list))
	for _, t := range list {
		ts, ok := parseTarget(t)
		if !ok {
			return nil, fmt.Errorf("model %q must be \"provider/model\" (e.g. openai/gpt-4o) or a configured alias", model)
		}
		if _, ok := s.cfg.Providers[ts.Provider]; !ok {
			return nil, fmt.Errorf("unknown provider %q in model %q", ts.Provider, t)
		}
		out = append(out, ts)
	}
	return out, nil
}

// authenticate validates the virtual key.
func (s *Server) authenticate(r *http.Request) (*store.VirtualKey, *store.Project, int, string, string) {
	auth := r.Header.Get("Authorization")
	tok, ok := strings.CutPrefix(auth, "Bearer ")
	tok = strings.TrimSpace(tok)
	if !ok || tok == "" {
		return nil, nil, http.StatusUnauthorized, "missing_api_key", "Missing OpenGate virtual key. Send 'Authorization: Bearer og-...'."
	}
	if !secure.LooksLikeVirtualKey(tok) {
		return nil, nil, http.StatusUnauthorized, "invalid_api_key", "Invalid OpenGate virtual key."
	}
	k, err := s.store.KeyByHash(r.Context(), secure.HashKey(tok))
	if err != nil || k.Revoked {
		return nil, nil, http.StatusUnauthorized, "invalid_api_key", "Invalid OpenGate virtual key."
	}
	p, err := s.store.GetProject(r.Context(), k.ProjectID)
	if err != nil {
		return nil, nil, http.StatusUnauthorized, "invalid_api_key", "Invalid OpenGate virtual key."
	}
	return k, p, 0, "", ""
}

// reqState carries a request through the pipeline.
type reqState struct {
	log    *store.RequestLog
	key    *store.VirtualKey
	proj   *store.Project
	scopes []budget.Scope
	start  time.Time
	res    *budget.Reservation
	body   []byte
	raw    map[string]json.RawMessage
	r      *http.Request

	baseline string        // model savings are measured against ("" = none)
	cacheKey string        // response cache key when caching applies
	cacheTTL time.Duration // 0 = do not cache
}

func (s *Server) truncate(b []byte) string {
	if len(b) > s.cfg.BodyLogMaxBytes {
		return string(b[:s.cfg.BodyLogMaxBytes]) + "…[truncated]"
	}
	return string(b)
}

// finish persists the log, converts the budget reservation into actual
// spend and updates metrics.
func (s *Server) finish(rs *reqState) {
	rs.log.LatencyMS = time.Since(rs.start).Milliseconds()
	// Use a fresh context: the client may already be gone, but the spend must be recorded.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s.bill(ctx, rs)
	if err := s.store.InsertLog(ctx, rs.log); err != nil {
		s.log.Error("insert request log", "err", err, "request_id", rs.log.ID)
	}
	rs.res.Commit(ctx, rs.log.CostUSD)
	s.observe(rs.log)
	s.publishLog(ctx, rs.log)
}

func (s *Server) reject(w http.ResponseWriter, rs *reqState, status int, typ, code, msg string, retryAfter time.Duration) {
	if retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter.Seconds())))
	}
	if rs != nil {
		w.Header().Set("X-OpenGate-Request-Id", rs.log.ID)
	}
	writeError(w, status, typ, code, msg)
	if rs != nil {
		rs.log.Status, rs.log.ErrorCode, rs.log.ErrorMessage = status, code, msg
		s.finish(rs)
	}
}

// begin authenticates, reads and parses the JSON body and applies the kill
// switches. It returns nil if the request was already answered.
func (s *Server) begin(w http.ResponseWriter, r *http.Request, endpoint string) *reqState {
	start := time.Now()
	key, proj, st, code, msg := s.authenticate(r)
	if key == nil {
		s.metrics.rejected.Inc(code)
		writeError(w, st, "invalid_request_error", code, msg)
		return nil
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.cfg.MaxRequestBytes))
	rs := &reqState{key: key, proj: proj, start: start, r: r, log: &store.RequestLog{
		ID: secure.RandomID("req_"), TS: start.UTC(), ProjectID: proj.ID, KeyID: key.ID, Agent: key.Agent, UserLabel: key.UserLabel,
		Endpoint: endpoint,
	}}
	if err != nil {
		s.reject(w, rs, http.StatusRequestEntityTooLarge, "invalid_request_error", "request_too_large", "Request body too large or unreadable.", 0)
		return nil
	}
	rs.body = body
	if err := json.Unmarshal(body, &rs.raw); err != nil || rs.raw == nil {
		s.reject(w, rs, http.StatusBadRequest, "invalid_request_error", "invalid_json", "Request body must be a JSON object.", 0)
		return nil
	}
	var model string
	_ = json.Unmarshal(rs.raw["model"], &model)
	var stream bool
	_ = json.Unmarshal(rs.raw["stream"], &stream)
	rs.log.Model, rs.log.Stream = model, stream
	if key.Killed {
		s.reject(w, rs, http.StatusForbidden, "permission_error", "key_killed", "OpenGate: this key has been disabled by the kill switch.", 0)
		return nil
	}
	if proj.Killed {
		s.reject(w, rs, http.StatusForbidden, "permission_error", "project_killed", fmt.Sprintf("OpenGate: project %q has been disabled by the kill switch.", proj.Name), 0)
		return nil
	}
	if model == "" {
		s.reject(w, rs, http.StatusBadRequest, "invalid_request_error", "invalid_request", "'model' is required.", 0)
		return nil
	}
	if !s.cloudGate(w, rs) {
		return nil
	}
	cust, err := s.customerID(r, key, proj, rs.raw)
	if err != nil {
		s.reject(w, rs, http.StatusForbidden, "permission_error", "customer_mismatch", "OpenGate: "+err.Error()+".", 0)
		return nil
	}
	rs.log.CustomerID = cust
	if hit, err := s.store.KillHit(r.Context(), proj.OrgID, agentScopeID(key), cust); err != nil {
		s.log.Error("kill switch check failed", "err", err, "request_id", rs.log.ID)
		s.reject(w, rs, http.StatusInternalServerError, "server_error", "internal_error", "kill switch check failed", 0)
		return nil
	} else if hit != "" {
		msg := map[string]string{
			store.KillGlobal:   "OpenGate: every request in this organization is stopped by the global kill switch.",
			store.KillAgent:    fmt.Sprintf("OpenGate: agent %q has been disabled by the kill switch.", key.Agent),
			store.KillCustomer: fmt.Sprintf("OpenGate: customer %q has been disabled by the kill switch.", cust),
		}[hit]
		code := map[string]string{store.KillGlobal: "org_killed", store.KillAgent: "agent_killed", store.KillCustomer: "customer_killed"}[hit]
		s.reject(w, rs, http.StatusForbidden, "permission_error", code, msg, 0)
		return nil
	}
	if !s.applyRequestGuard(w, rs) {
		return nil
	}
	// Bodies are logged only after the guardrail ran, so redacted values
	// are never persisted and blocked requests store no body at all.
	if proj.LogBodies {
		rs.log.RequestBody = s.truncate(rs.body)
	}
	return rs
}

// ---------- model policy ----------

func matchModel(pattern, name string) bool {
	if p, ok := strings.CutSuffix(pattern, "*"); ok {
		return strings.HasPrefix(name, p)
	}
	return pattern == name
}

func matchAny(patterns []string, names ...string) bool {
	for _, p := range patterns {
		for _, n := range names {
			if matchModel(p, n) {
				return true
			}
		}
	}
	return false
}

// policyAllows applies one allow/deny policy to a target. Allowing an alias
// allows its targets; an explicit deny on either name always wins.
func policyAllows(p store.ModelPolicy, requested string, t targetSpec) bool {
	if matchAny(p.DeniedModels, requested, t.String()) {
		return false
	}
	return len(p.AllowedModels) == 0 || matchAny(p.AllowedModels, requested, t.String())
}

// filterTargets drops targets forbidden by the key or project policy.
func (s *Server) filterTargets(w http.ResponseWriter, rs *reqState, requested string, targets []targetSpec) ([]targetSpec, bool) {
	var out []targetSpec
	for _, t := range targets {
		if policyAllows(rs.key.ModelPolicy, requested, t) && policyAllows(rs.proj.ModelPolicy, requested, t) {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		s.reject(w, rs, http.StatusForbidden, "permission_error", "model_not_allowed",
			fmt.Sprintf("OpenGate: model %q is not allowed for this key/project.", requested), 0)
		return nil, false
	}
	return out, true
}

// tokenCap returns the effective max_tokens cap (0 = none).
func tokenCap(rs *reqState) int {
	c := rs.key.MaxTokens
	if p := rs.proj.MaxTokens; p > 0 && (c == 0 || p < c) {
		c = p
	}
	return c
}

// ---------- budgets ----------

// estimateCost is the worst case over all candidate targets.
func (s *Server) estimateCost(targets []targetSpec, promptTokens, outputTokens int) float64 {
	tb := s.pricing.Table()
	max := 0.0
	for _, t := range targets {
		p, _ := tb.Lookup(t.Provider, t.Model)
		if c := pricing.Calc(p, promptTokens, outputTokens); c > max {
			max = c
		}
	}
	return max
}

// reserve checks budgets and rate limits and reserves the estimated cost.
func (s *Server) reserve(w http.ResponseWriter, rs *reqState, estimate float64) bool {
	rs.scopes = budget.ScopesFor(rs.key, rs.proj)
	res, denial, err := s.enforcer.Reserve(rs.r.Context(), rs.scopes, estimate)
	if err != nil {
		s.log.Error("budget check failed", "err", err, "request_id", rs.log.ID)
		s.reject(w, rs, http.StatusInternalServerError, "server_error", "internal_error", "budget check failed", 0)
		return false
	}
	if denial != nil {
		s.reject(w, rs, denial.Status, denial.Type, denial.Code, denial.Message, denial.RetryAfter)
		return false
	}
	rs.res = res
	return true
}

// ---------- upstream attempts ----------

func (s *Server) backoff(attempt int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		return retryAfter
	}
	d := s.cfg.Retry.InitialBackoff << (attempt - 1)
	if d > s.cfg.Retry.MaxBackoff || d <= 0 {
		d = s.cfg.Retry.MaxBackoff
	}
	jitter := 0.8 + 0.4*rand.Float64()
	return time.Duration(float64(d) * jitter)
}

// target builds the concrete upstream for a spec (decrypting its key).
func (s *Server) target(ctx context.Context, org string, t targetSpec) (provider.Target, error) {
	pc := s.cfg.Providers[t.Provider]
	apiKey, err := s.apiKeyFor(ctx, org, t.Provider)
	if err != nil {
		return provider.Target{}, err
	}
	return provider.Target{Provider: t.Provider, Model: t.Model, Config: pc, APIKey: apiKey}, nil
}

// attemptLoop tries targets in order with per-target retries. do returns an
// error for a failed attempt; the loop decides whether to retry, fail over or stop.
func (s *Server) attemptLoop(ctx context.Context, rs *reqState, targets []targetSpec, do func(t provider.Target) error) error {
	var lastErr error
	for _, t := range targets {
		tgt, err := s.target(ctx, rs.proj.OrgID, t)
		if err != nil {
			lastErr = err
			rs.log.Hops = append(rs.log.Hops, store.Hop{Provider: t.Provider, Model: t.Model, StartedAt: time.Now().UTC(), Error: "skipped: no usable provider credentials"})
			continue
		}
		rs.log.Provider, rs.log.ProviderModel = t.Provider, t.Model
		var ue *provider.UpstreamError
		for attempt := 0; attempt < s.cfg.Retry.MaxAttempts; attempt++ {
			if attempt > 0 {
				wait := s.backoff(attempt, ue.RetryAfter)
				if wait > s.cfg.Retry.MaxBackoff {
					break // provider asked us to wait too long: fail over instead
				}
				if err := s.sleep(ctx, wait); err != nil {
					return err
				}
			}
			rs.log.Attempts++
			hopStart := time.Now()
			err := do(tgt)
			rs.log.Hops = append(rs.log.Hops, hopFor(t, hopStart, err, ctx.Err() != nil))
			if err == nil {
				s.metrics.attempts.Inc(t.Provider, "ok")
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			lastErr = err
			if !errors.As(err, &ue) {
				s.metrics.attempts.Inc(t.Provider, "error")
				return err
			}
			s.metrics.attempts.Inc(t.Provider, "upstream_"+strconv.Itoa(ue.Status))
			s.log.Warn("upstream attempt failed", "request_id", rs.log.ID, "provider", t.Provider, "model", t.Model, "attempt", attempt+1, "err", ue.Error())
			if !ue.Retryable() {
				break
			}
		}
		if ue != nil && !ue.Failover() {
			return lastErr
		}
	}
	if lastErr == nil {
		lastErr = errors.New("no targets available")
	}
	return lastErr
}

// hopFor describes one finished upstream attempt for the request log.
// Non-upstream errors may carry internal detail, so only a generic message
// is stored for them.
func hopFor(t targetSpec, start time.Time, err error, cancelled bool) store.Hop {
	h := store.Hop{Provider: t.Provider, Model: t.Model, StartedAt: start.UTC(), LatencyMS: time.Since(start).Milliseconds()}
	var ue *provider.UpstreamError
	switch {
	case err == nil:
		h.Status = http.StatusOK
	case cancelled:
		h.Status, h.Error = 499, "aborted (client gone or kill switch)"
	case errors.As(err, &ue):
		h.Status, h.Error = ue.Status, ue.Error()
	default:
		h.Error = "gateway error (see server log)"
	}
	return h
}

// writeUpstreamFailure maps a final error onto the response and log.
func (s *Server) writeUpstreamFailure(ctx context.Context, w http.ResponseWriter, rs *reqState, err error) {
	if cause := context.Cause(ctx); errors.Is(cause, errKilled) {
		s.reject(w, rs, http.StatusForbidden, "permission_error", "killed", "OpenGate: request aborted by the kill switch.", 0)
		return
	}
	if ctx.Err() != nil {
		rs.log.Status, rs.log.ErrorCode, rs.log.ErrorMessage = 499, "client_closed", "client closed request"
		s.finish(rs)
		return
	}
	var ue *provider.UpstreamError
	if errors.As(err, &ue) {
		status := ue.Status
		if status == 0 || status >= 500 {
			status = http.StatusBadGateway
		}
		typ := "upstream_error"
		if status == http.StatusBadRequest {
			typ = "invalid_request_error"
		}
		s.reject(w, rs, status, typ, "upstream_error", fmt.Sprintf("All upstream attempts failed (%d). Last error: %s", rs.log.Attempts, ue.Error()), 0)
		return
	}
	// Non-upstream failures (e.g. provider-key decryption) may carry internal
	// detail: log it, return a generic message with the request ID.
	s.log.Error("gateway request failed", "request_id", rs.log.ID, "err", err)
	s.reject(w, rs, http.StatusBadGateway, "upstream_error", "upstream_error",
		"OpenGate could not complete the request (request "+rs.log.ID+"); see the gateway log.", 0)
}

// withInflight registers the request for the kill switch.
func (s *Server) withInflight(rs *reqState) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(rs.r.Context())
	s.inflight.add(rs.log.ID, flight{keyID: rs.key.ID, projectID: rs.proj.ID, orgID: rs.proj.OrgID, agentID: agentScopeID(rs.key), customerID: rs.log.CustomerID, cancel: cancel})
	s.metrics.inflight.Add(1)
	// Close the check-to-register window: a kill that landed after begin()
	// read the flags but before registration is caught here.
	if k, err := s.store.GetKey(rs.r.Context(), rs.key.ID); err == nil && k.Killed {
		cancel(errKilled)
	} else if p, err := s.store.GetProject(rs.r.Context(), rs.proj.ID); err == nil && p.Killed {
		cancel(errKilled)
	}
	return ctx, func() {
		s.inflight.remove(rs.log.ID)
		s.metrics.inflight.Add(-1)
		cancel(nil)
	}
}

// agentScopeID is project_id/agent for keys with an agent label, else "".
func agentScopeID(k *store.VirtualKey) string {
	if k.Agent == "" {
		return ""
	}
	return k.ProjectID + "/" + k.Agent
}

func estimateTokens(chars int) int { return (chars + 3) / 4 }

func promptChars(req *provider.ChatRequest) int {
	n := 0
	for _, m := range req.Messages {
		n += len(m.Content) + len(m.Role)
		for _, tc := range m.ToolCalls {
			n += len(tc.Function.Arguments) + len(tc.Function.Name)
		}
	}
	for _, t := range req.Tools {
		n += len(t.Function.Parameters) + len(t.Function.Description)
	}
	return n
}

// price finalizes token counts and cost for the log. promptEstimate is used
// when the provider reported no usage.
func (s *Server) price(rs *reqState, u provider.Usage, reported bool, promptEstimate, outChars int) {
	if !reported {
		u.PromptTokens = promptEstimate
		u.CompletionTokens = estimateTokens(outChars)
		rs.log.UsageEstimated = true
	} else if u.CompletionTokens == 0 && outChars > 0 {
		u.CompletionTokens = estimateTokens(outChars)
		rs.log.UsageEstimated = true
	}
	rs.log.PromptTokens, rs.log.CompletionTokens = u.PromptTokens, u.CompletionTokens
	rs.log.CachedTokens, rs.log.CacheWriteTokens = u.Cached(), u.CacheWrite()
	rs.log.CostUSD, _ = s.pricing.Table().CostTokens(rs.log.Provider, rs.log.ProviderModel, pricing.Tokens{
		Prompt: u.PromptTokens, Completion: u.CompletionTokens, CacheRead: u.Cached(), CacheWrite: u.CacheWrite()})
	if rs.log.Route != "" && rs.baseline != "" {
		rs.log.BaselineCostUSD = s.baselineCost(rs, rs.baseline)
	}
}

func (s *Server) gatewayHeaders(w http.ResponseWriter, rs *reqState) {
	h := w.Header()
	h.Set("X-OpenGate-Request-Id", rs.log.ID)
	h.Set("X-OpenGate-Provider", rs.log.Provider)
	h.Set("X-OpenGate-Model", rs.log.Provider+"/"+rs.log.ProviderModel)
	h.Set("X-OpenGate-Attempts", strconv.Itoa(rs.log.Attempts))
}

func sseHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
}
