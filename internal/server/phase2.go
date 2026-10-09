package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/opengate-dev/opengate/internal/billing"
	"github.com/opengate-dev/opengate/internal/budget"
	"github.com/opengate-dev/opengate/internal/guard"
	"github.com/opengate-dev/opengate/internal/pricing"
	"github.com/opengate-dev/opengate/internal/provider"
	"github.com/opengate-dev/opengate/internal/routing"
	"github.com/opengate-dev/opengate/internal/store"
)

// ---------- small TTL cache for DB-backed settings on the hot path ----------

type ttlCache[T any] struct {
	mu   sync.Mutex
	at   time.Time
	ttl  time.Duration
	val  T
	load func(ctx context.Context) (T, error)
}

func (c *ttlCache[T]) get(ctx context.Context) T {
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.at) > c.ttl {
		if v, err := c.load(ctx); err == nil {
			c.val, c.at = v, time.Now()
		}
	}
	return c.val
}

func (c *ttlCache[T]) invalidate() {
	c.mu.Lock()
	c.at = time.Time{}
	c.mu.Unlock()
}

// ---------- end customers ----------

func validCustomerID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, r := range id {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// customerID resolves the end customer. A key bound to a customer always
// uses it (and rejects a conflicting assertion). Otherwise the header / the
// OpenAI "user" field is honored only when the project trusts it, because
// any key holder can set them.
func (s *Server) customerID(r *http.Request, key *store.VirtualKey, proj *store.Project, raw map[string]json.RawMessage) (string, error) {
	asserted := strings.TrimSpace(r.Header.Get(s.cfg.Billing.CustomerHeader))
	fromHeader := asserted != ""
	if asserted == "" && !s.cfg.Billing.IgnoreUserField {
		_ = json.Unmarshal(raw["user"], &asserted)
		asserted = strings.TrimSpace(asserted)
	}
	if key.CustomerID != "" {
		if fromHeader && asserted != key.CustomerID {
			return "", fmt.Errorf("this key is bound to a different customer")
		}
		return key.CustomerID, nil
	}
	if !proj.TrustCustomerHeader || !validCustomerID(asserted) {
		return "", nil
	}
	return asserted, nil
}

// bill applies markup rules to the final cost.
func (s *Server) bill(ctx context.Context, rs *reqState) {
	l := rs.log
	if l.CostUSD <= 0 {
		return
	}
	pct := s.markups.Pct(ctx, rs.proj.OrgID, l.ProjectID, l.CustomerID, l.Provider+"/"+l.ProviderModel)
	l.BilledUSD = billing.Billed(l.CostUSD, pct)
}

// ---------- guardrails ----------

func (s *Server) guardSettings(p *store.Project) store.GuardSettings {
	if p.Guardrails != nil {
		g := *p.Guardrails
		if g.PII == "" {
			g.PII = guard.ModeOff
		}
		if g.Injection == "" {
			g.Injection = guard.ModeOff
		}
		return g
	}
	d := s.cfg.Guardrails
	return store.GuardSettings{PII: d.PII, Categories: d.Categories, Responses: d.Responses, Injection: d.Injection}
}

func guardFields(endpoint string) []string {
	switch endpoint {
	case EndpointChat:
		return []string{"messages"}
	case EndpointEmbeddings:
		return []string{"input"}
	case EndpointResponses:
		return []string{"input", "instructions"}
	}
	return nil
}

func addGuard(l *store.RequestLog, cats []string, action string) {
	if len(cats) == 0 {
		return
	}
	set := map[string]bool{}
	for _, c := range strings.Split(l.Guard, ",") {
		if c != "" {
			set[c] = true
		}
	}
	for _, c := range cats {
		set[c] = true
	}
	all := make([]string, 0, len(set))
	for _, c := range append(guard.AllCategories, guard.Injection) {
		if set[c] {
			all = append(all, c)
		}
	}
	l.Guard = strings.Join(all, ",")
	rank := map[string]int{"": 0, "logged": 1, "redacted": 2, "blocked": 3}
	if rank[action] > rank[l.GuardAction] {
		l.GuardAction = action
	}
}

// applyRequestGuard scans the request; it returns false if it was rejected.
func (s *Server) applyRequestGuard(w http.ResponseWriter, rs *reqState) bool {
	g := s.guardSettings(rs.proj)
	if g.PII == guard.ModeOff && g.Injection == guard.ModeOff {
		return true
	}
	fields := guardFields(rs.log.Endpoint)
	if g.Injection != guard.ModeOff {
		best, injHits := 0, []string(nil)
		_, _ = guard.Walk(cloneRaw(rs.raw, fields), fields, func(role, text string) string {
			if role == "system" || role == "developer" {
				return text // the app's own instructions are trusted
			}
			if sc, hits := guard.InjectionScore(text); sc > best {
				best, injHits = sc, hits
			}
			return text
		})
		if best >= guard.InjectionThreshold {
			if g.Injection == guard.ModeBlock {
				addGuard(rs.log, []string{guard.Injection}, "blocked")
				s.reject(w, rs, http.StatusBadRequest, "invalid_request_error", "prompt_injection",
					"OpenGate: request blocked by the prompt-injection guardrail ("+strings.Join(injHits, ", ")+").", 0)
				return false
			}
			addGuard(rs.log, []string{guard.Injection}, "logged")
		}
	}
	if g.PII == guard.ModeOff {
		return true
	}
	sc := guard.NewScanner(g.Categories)
	redact := g.PII == guard.ModeRedact
	found := guard.Finding{}
	changed, err := guard.Walk(rs.raw, fields, func(role, text string) string {
		out, f := sc.Scan(text, redact)
		found.Add(f)
		return out
	})
	if err != nil {
		return true // malformed content is rejected later by the handler
	}
	cats := found.Categories()
	if len(cats) == 0 {
		return true
	}
	switch g.PII {
	case guard.ModeBlock:
		addGuard(rs.log, cats, "blocked")
		s.reject(w, rs, http.StatusBadRequest, "invalid_request_error", "guardrail_blocked",
			"OpenGate: request blocked by the data guardrail (found: "+strings.Join(cats, ", ")+").", 0)
		return false
	case guard.ModeRedact:
		addGuard(rs.log, cats, "redacted")
		if changed {
			if b, err := json.Marshal(rs.raw); err == nil {
				rs.body = b
			}
		}
	default:
		addGuard(rs.log, cats, "logged")
	}
	return true
}

func cloneRaw(raw map[string]json.RawMessage, fields []string) map[string]json.RawMessage {
	c := make(map[string]json.RawMessage, len(fields))
	for _, f := range fields {
		if v, ok := raw[f]; ok {
			c[f] = append(json.RawMessage(nil), v...)
		}
	}
	return c
}

// guardResponse scans a non-streaming JSON response. It returns the
// (possibly redacted) body and whether the response must be withheld.
func (s *Server) guardResponse(rs *reqState, body []byte) ([]byte, bool) {
	g := s.guardSettings(rs.proj)
	if !g.Responses || g.PII == guard.ModeOff {
		return body, false
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(body, &raw) != nil {
		return body, false
	}
	sc := guard.NewScanner(g.Categories)
	redact := g.PII == guard.ModeRedact
	found := guard.Finding{}
	changed, _ := guard.Walk(raw, []string{"choices", "output"}, func(role, text string) string {
		out, f := sc.Scan(text, redact)
		found.Add(f)
		return out
	})
	cats := found.Categories()
	if len(cats) == 0 {
		return body, false
	}
	switch g.PII {
	case guard.ModeBlock:
		addGuard(rs.log, cats, "blocked")
		return nil, true
	case guard.ModeRedact:
		addGuard(rs.log, cats, "redacted")
		if changed {
			if b, err := json.Marshal(raw); err == nil {
				return b, false
			}
		}
	default:
		addGuard(rs.log, cats, "logged")
	}
	return body, false
}

// guardStreamText inspects streamed output after the fact (log only: it has
// already been delivered).
func (s *Server) guardStreamText(rs *reqState, text string) {
	g := s.guardSettings(rs.proj)
	if !g.Responses || g.PII == guard.ModeOff || text == "" {
		return
	}
	_, f := guard.NewScanner(g.Categories).Scan(text, false)
	addGuard(rs.log, f.Categories(), "logged")
}

// ---------- smart routing ----------

// routePlan is the routed target list for a chat request.
type routePlan struct {
	targets  []targetSpec
	route    string // "" = not routed
	baseline string // model savings are measured against
	class    *routing.Classification
}

func (s *Server) autoEnabled(model string) bool {
	a := s.cfg.SmartRouting.Auto
	return a.Enabled && model == a.Alias
}

// blendedPrice is a rough per-request price used to rank models (3:1
// input:output token mix).
func (s *Server) blendedPrice(model string) float64 {
	ts, ok := parseTarget(model)
	if !ok {
		return 0
	}
	p, _ := s.pricing.Table().Lookup(ts.Provider, ts.Model)
	return 3*p.Input + p.Output
}

func (s *Server) autoBaseline() string {
	a := s.cfg.SmartRouting.Auto
	if a.Baseline != "" {
		return a.Baseline
	}
	best, bp := "", -1.0
	for _, m := range a.Models {
		if p := s.blendedPrice(m.Model); m.Tier == 3 && p > bp {
			best, bp = m.Model, p
		}
	}
	return best
}

func chatSignals(req *provider.ChatRequest, raw map[string]json.RawMessage, promptTokens int, h http.Header) routing.Signals {
	var b strings.Builder
	for _, m := range req.Messages {
		if m.Role == "user" || m.Role == "system" || m.Role == "developer" {
			b.WriteString(m.Text())
			b.WriteByte('\n')
		}
	}
	var rf struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(raw["response_format"], &rf)
	return routing.Signals{Text: b.String(), Messages: len(req.Messages), PromptToks: promptTokens, HasTools: len(req.Tools) > 0,
		JSONSchema: rf.Type == "json_schema", MinTierHint: routing.QualityHint(h)}
}

func (s *Server) expand(model string, req *provider.ChatRequest, rs *reqState, promptTokens int, plan *routePlan) ([]targetSpec, error) {
	if s.autoEnabled(model) {
		c := routing.Classify(chatSignals(req, rs.raw, promptTokens, rs.r.Header))
		plan.class = &c
		models := make([]routing.TierModel, 0, len(s.cfg.SmartRouting.Auto.Models))
		for _, m := range s.cfg.SmartRouting.Auto.Models {
			models = append(models, routing.TierModel{Model: m.Model, Tier: routing.Tier(m.Tier)})
		}
		var out []targetSpec
		for _, m := range routing.Pick(models, c.Tier, s.blendedPrice) {
			ts, err := s.resolve(m)
			if err != nil {
				return nil, err
			}
			out = append(out, ts...)
		}
		return out, nil
	}
	return s.resolve(model)
}

// planChat applies routing rules and the auto alias.
func (s *Server) planChat(rs *reqState, req *provider.ChatRequest, promptTokens, outTokens int) (*routePlan, error) {
	requested := req.Model
	plan := &routePlan{baseline: requested}
	rules := s.routingRules.get(rs.r.Context())
	target, fallbacks := requested, []string(nil)
	if len(rules) > 0 {
		d := routing.Evaluate(rules, routing.Request{Model: requested, PromptTokens: promptTokens, Agent: rs.key.Agent, ProjectID: rs.proj.ID,
			Headers: rs.r.Header, EstCost: func(m string) float64 {
				ts, err := s.expand(m, req, rs, promptTokens, &routePlan{})
				if err != nil || len(ts) == 0 {
					return 0
				}
				return s.estimateCost(ts[:1], promptTokens, outTokens)
			}})
		if d != nil && d.Rule.OrgID == rs.proj.OrgID {
			target, fallbacks = d.Target, d.Fallbacks
			plan.route = "rule:" + d.Rule.Name
		}
	}
	targets, err := s.expand(target, req, rs, promptTokens, plan)
	if err != nil {
		return nil, err
	}
	if plan.class != nil {
		if plan.route != "" {
			plan.route += ">"
		}
		plan.route += "auto:t" + strconv.Itoa(int(plan.class.Tier))
	}
	seen := map[targetSpec]bool{}
	for _, t := range targets {
		seen[t] = true
	}
	for _, f := range fallbacks {
		ts, err := s.expand(f, req, rs, promptTokens, &routePlan{})
		if err != nil {
			return nil, fmt.Errorf("routing rule fallback: %w", err)
		}
		for _, t := range ts {
			if !seen[t] {
				seen[t] = true
				targets = append(targets, t)
			}
		}
	}
	if s.autoEnabled(requested) {
		plan.baseline = s.autoBaseline()
	}
	plan.targets = targets
	return plan, nil
}

// baselineCost prices the actual usage at the baseline model.
func (s *Server) baselineCost(rs *reqState, baseline string) float64 {
	ts, err := s.resolve(baseline)
	if err != nil || len(ts) == 0 {
		return 0
	}
	l := rs.log
	c, _ := s.pricing.Table().CostTokens(ts[0].Provider, ts[0].Model, pricing.Tokens{
		Prompt: l.PromptTokens, Completion: l.CompletionTokens, CacheRead: l.CachedTokens, CacheWrite: l.CacheWriteTokens})
	return c
}

// ---------- response cache ----------

// cacheTTL returns the TTL for this request (0 = do not cache).
func (s *Server) cacheTTL(rs *reqState) time.Duration {
	if s.cfg.Cache.Disabled || rs.log.Stream {
		return 0
	}
	h := strings.ToLower(strings.TrimSpace(rs.r.Header.Get("X-OpenGate-Cache")))
	if h == "off" || h == "no-store" || h == "bypass" || rs.proj.CacheTTLSeconds <= 0 {
		return 0 // caching must be enabled for the project; clients cannot opt in alone
	}
	ttl := time.Duration(rs.proj.CacheTTLSeconds) * time.Second
	if v := rs.r.Header.Get("X-OpenGate-Cache-TTL"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			ttl = time.Duration(n) * time.Second
		}
	}
	if ttl > s.cfg.Cache.MaxTTL {
		ttl = s.cfg.Cache.MaxTTL
	}
	return ttl
}

func (s *Server) cacheKey(rs *reqState) string {
	canon, _ := json.Marshal(rs.raw) // map keys are sorted: field order does not matter
	h := sha256.New()
	fmt.Fprintf(h, "v1\x00%s\x00%s\x00", rs.proj.ID, rs.log.Endpoint)
	if !s.cfg.Cache.ShareAcrossCustomers {
		fmt.Fprintf(h, "c:%s\x00", rs.log.CustomerID)
	}
	if !s.cfg.Cache.ShareAcrossKeys {
		fmt.Fprintf(h, "k:%s\x00", rs.key.ID)
	}
	h.Write(canon)
	return hex.EncodeToString(h.Sum(nil))
}

// serveCached answers from the cache if possible.
func (s *Server) serveCached(w http.ResponseWriter, rs *reqState, key string) bool {
	e, err := s.store.CacheGet(rs.r.Context(), key, time.Now())
	if err != nil {
		return false
	}
	// Hits are free but still count against rate limits and require the
	// key to be within budget.
	res, denial, err := s.enforcer.Reserve(rs.r.Context(), budget.ScopesFor(rs.key, rs.proj), 0)
	if err != nil || denial != nil {
		if denial != nil {
			s.reject(w, rs, denial.Status, denial.Type, denial.Code, denial.Message, denial.RetryAfter)
			return true
		}
		return false
	}
	rs.res = res
	l := rs.log
	l.Provider, l.ProviderModel, l.CacheHit, l.Status = e.Provider, e.ProviderModel, true, http.StatusOK
	l.CostUSD, l.BaselineCostUSD = 0, e.CostUSD
	if rs.proj.LogBodies {
		l.ResponseBody = s.truncate(e.Body)
	}
	s.gatewayHeaders(w, rs)
	w.Header().Set("X-OpenGate-Cache", "HIT")
	w.Header().Set("X-OpenGate-Cost-USD", "0.00000000")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(e.Body)
	s.finish(rs)
	return true
}

func (s *Server) storeCached(rs *reqState, key string, ttl time.Duration, body []byte) {
	if len(body) > s.cfg.Cache.MaxEntryBytes {
		return
	}
	l := rs.log
	err := s.store.CachePut(context.Background(), &store.CacheEntry{Key: key, ProjectID: l.ProjectID, Endpoint: l.Endpoint,
		Provider: l.Provider, ProviderModel: l.ProviderModel, Body: body, PromptTokens: l.PromptTokens,
		CompletionTokens: l.CompletionTokens, CostUSD: l.CostUSD, ExpiresAt: time.Now().Add(ttl)})
	if err != nil {
		s.log.Warn("cache put", "err", err, "request_id", l.ID)
	}
}
