package server

import (
	"context"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/saief1/opengate/internal/metrics"
	"github.com/saief1/opengate/internal/secure"
	"github.com/saief1/opengate/internal/store"
)

// gatewayMetrics are the Prometheus series exported on /metrics.
type gatewayMetrics struct {
	reg        *metrics.Registry
	requests   *metrics.Counter
	latency    *metrics.Histogram
	tokens     *metrics.Counter
	cost       *metrics.Counter
	attempts   *metrics.Counter
	rejected   *metrics.Counter
	inflight   *metrics.Gauge
	adminFails *metrics.Counter
	guard      *metrics.Counter
	cacheHits  *metrics.Counter
	saved      *metrics.Counter
}

func newGatewayMetrics() *gatewayMetrics {
	r := metrics.New()
	m := &gatewayMetrics{reg: r,
		requests: r.NewCounter("opengate_requests_total", "Gateway requests by endpoint, provider, model and HTTP status.", "endpoint", "provider", "model", "status"),
		latency: r.NewHistogram("opengate_request_duration_seconds", "End-to-end request latency.",
			[]float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120}, "endpoint", "provider"),
		tokens:     r.NewCounter("opengate_tokens_total", "Tokens by provider, model and type (prompt|completion|cached|cache_write).", "provider", "model", "type"),
		cost:       r.NewCounter("opengate_cost_usd_total", "Upstream cost in USD by project, provider and model.", "project", "provider", "model"),
		attempts:   r.NewCounter("opengate_upstream_attempts_total", "Upstream attempts by provider and outcome.", "provider", "outcome"),
		rejected:   r.NewCounter("opengate_rejections_total", "Requests rejected by OpenGate by error code.", "code"),
		inflight:   r.NewGauge("opengate_inflight_requests", "Requests currently being proxied."),
		adminFails: r.NewCounter("opengate_admin_auth_failures_total", "Failed admin authentications."),
		guard:      r.NewCounter("opengate_guardrail_findings_total", "Guardrail findings by detector and action.", "detector", "action"),
		cacheHits:  r.NewCounter("opengate_cache_hits_total", "Responses served from the OpenGate response cache."),
		saved:      r.NewCounter("opengate_saved_usd_total", "Estimated USD saved vs. the requested/baseline model, by reason (routing|cache).", "reason"),
	}
	r.NewGaugeFunc("opengate_build_info", "Always 1; see the version label on /healthz.", func() float64 { return 1 })
	return m
}

func (s *Server) observe(l *store.RequestLog) {
	m := s.metrics
	status := itoa(l.Status)
	m.requests.Inc(l.Endpoint, l.Provider, l.ProviderModel, status)
	if isGatewayRejection(l.ErrorCode) {
		m.rejected.Inc(l.ErrorCode)
	}
	if l.Provider != "" {
		m.latency.Observe(float64(l.LatencyMS)/1000, l.Endpoint, l.Provider)
		m.tokens.Add(float64(l.PromptTokens), l.Provider, l.ProviderModel, "prompt")
		m.tokens.Add(float64(l.CompletionTokens), l.Provider, l.ProviderModel, "completion")
		if l.CachedTokens > 0 {
			m.tokens.Add(float64(l.CachedTokens), l.Provider, l.ProviderModel, "cached")
		}
		if l.CacheWriteTokens > 0 {
			m.tokens.Add(float64(l.CacheWriteTokens), l.Provider, l.ProviderModel, "cache_write")
		}
		m.cost.Add(l.CostUSD, l.ProjectID, l.Provider, l.ProviderModel)
	}
	if l.CacheHit {
		m.cacheHits.Inc()
		m.saved.Add(l.BaselineCostUSD, "cache")
	} else if l.Route != "" && l.BaselineCostUSD > l.CostUSD {
		m.saved.Add(l.BaselineCostUSD-l.CostUSD, "routing")
	}
	if l.Guard != "" {
		for _, c := range strings.Split(l.Guard, ",") {
			m.guard.Inc(c, l.GuardAction)
		}
	}
}

func isGatewayRejection(code string) bool {
	switch code {
	case "budget_exceeded", "rate_limit_exceeded", "key_killed", "project_killed", "org_killed", "agent_killed", "customer_killed", "killed", "killed_mid_stream",
		"model_not_allowed", "max_tokens_exceeded", "guardrail_blocked", "prompt_injection":
		return true
	}
	return false
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	n := len(b)
	for i > 0 {
		n--
		b[n] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		n--
		b[n] = '-'
	}
	return string(b[n:])
}

// handleMetrics serves Prometheus metrics. It requires OPENGATE_METRICS_TOKEN
// as a bearer token unless metrics.public is set. (It deliberately does not
// fall back to the admin token, which would end up in scrape configs.)
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.Metrics.Public {
		ip := s.clientIP(r)
		if locked, d := s.lockout.locked("metrics|" + ip); locked {
			w.Header().Set("Retry-After", itoa(int(d.Seconds())+1))
			writeError(w, http.StatusTooManyRequests, "rate_limit_error", "locked_out", "too many failed metrics authentications")
			return
		}
		tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		tok = strings.TrimSpace(tok)
		want := s.cfg.MetricsToken
		if want == "" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="opengate-metrics"`)
			writeError(w, http.StatusUnauthorized, "invalid_request_error", "metrics_token_unset", "set OPENGATE_METRICS_TOKEN (or metrics.public: true) to enable scraping")
			return
		}
		if !secure.ConstantTimeEqual(tok, want) {
			s.lockout.fail("metrics|" + ip)
			w.Header().Set("WWW-Authenticate", `Bearer realm="opengate-metrics"`)
			writeError(w, http.StatusUnauthorized, "invalid_request_error", "invalid_metrics_token", "metrics token required")
			return
		}
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_ = s.metrics.reg.WriteText(w)
}

// handleReady reports readiness: the database must answer.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	checks := map[string]string{"database": "ok"}
	status := http.StatusOK
	if err := s.store.Ping(ctx); err != nil {
		checks["database"] = "error"
		status = http.StatusServiceUnavailable
	}
	if s.pricing.Table() == nil {
		checks["pricing"] = "error"
		status = http.StatusServiceUnavailable
	} else {
		checks["pricing"] = "ok"
	}
	state := "ready"
	if status != http.StatusOK {
		state = "not_ready"
	}
	writeJSON(w, status, map[string]any{"status": state, "checks": checks, "version": Version})
}

// ---------- admin login throttling ----------

// lockout tracks failed authentications per key (an IP, or "account|IP") and
// locks the key after max failures inside window. Entries expire once both
// their failures and lock are older than the window, and the table is capped
// so rotating source addresses cannot exhaust memory.
type lockout struct {
	mu       sync.Mutex
	max      int
	window   time.Duration
	duration time.Duration
	cap      int
	now      func() time.Time
	entries  map[string]*lockEntry
}

type lockEntry struct {
	fails       []time.Time
	lockedUntil time.Time
	last        time.Time
}

const lockoutCap = 50000

func newLockout(max int, window, duration time.Duration) *lockout {
	return &lockout{max: max, window: window, duration: duration, cap: lockoutCap, now: time.Now, entries: map[string]*lockEntry{}}
}

// locked reports whether key is locked out and for how long.
func (l *lockout) locked(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entries[key]
	if e == nil {
		return false, 0
	}
	if d := e.lockedUntil.Sub(l.now()); d > 0 {
		return true, d
	}
	return false, 0
}

// fail records a failure; it returns true when this failure triggered a lockout.
func (l *lockout) fail(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	e := l.entries[key]
	if e == nil {
		if len(l.entries) >= l.cap {
			l.evict(now)
		}
		e = &lockEntry{}
		l.entries[key] = e
	}
	keep := e.fails[:0]
	for _, t := range e.fails {
		if now.Sub(t) < l.window {
			keep = append(keep, t)
		}
	}
	e.fails, e.last = append(keep, now), now
	if len(e.fails) >= l.max {
		e.lockedUntil = now.Add(l.duration)
		e.fails = nil
		return true
	}
	return false
}

// evict drops expired entries; if the table is still full it drops the
// least recently failed unlocked entries (locked ones are kept).
func (l *lockout) evict(now time.Time) {
	for k, e := range l.entries {
		if now.After(e.lockedUntil) && now.Sub(e.last) >= l.window {
			delete(l.entries, k)
		}
	}
	for len(l.entries) >= l.cap {
		var oldK string
		var oldT time.Time
		for k, e := range l.entries {
			if now.Before(e.lockedUntil) {
				continue
			}
			if oldK == "" || e.last.Before(oldT) {
				oldK, oldT = k, e.last
			}
		}
		if oldK == "" { // everything locked: drop the oldest lock
			for k, e := range l.entries {
				if oldK == "" || e.last.Before(oldT) {
					oldK, oldT = k, e.last
				}
			}
		}
		delete(l.entries, oldK)
	}
}

func (l *lockout) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}

func (l *lockout) success(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e := l.entries[key]; e != nil && !l.now().Before(e.lockedUntil) {
		delete(l.entries, key)
	}
}

// clientIP returns the caller's IP. X-Forwarded-For is used only with
// trust_proxy_headers, and then walked from the right: the first address
// that is not a configured trusted proxy is the client (the leftmost
// entries are client-controlled and never trusted). Without
// trusted_proxies, only the single rightmost entry (added by the one proxy
// in front) is used.
func (s *Server) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if !s.cfg.TrustProxyHeaders || !s.fromTrustedProxy(host) {
		return host
	}
	var hops []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		for _, h := range strings.Split(v, ",") {
			if h = strings.TrimSpace(h); h != "" {
				hops = append(hops, h)
			}
		}
	}
	for i := len(hops) - 1; i >= 0; i-- {
		ip := net.ParseIP(hops[i])
		if ip == nil {
			break
		}
		if len(s.trusted) == 0 || !s.isTrusted(ip) {
			return ip.String()
		}
	}
	return host
}

func (s *Server) isTrusted(ip net.IP) bool {
	for _, n := range s.trusted {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// fromTrustedProxy: with trusted_proxies configured, X-Forwarded-For is only
// honored when the TCP peer is one of them.
func (s *Server) fromTrustedProxy(peer string) bool {
	if len(s.trusted) == 0 {
		return true
	}
	ip := net.ParseIP(peer)
	return ip != nil && s.isTrusted(ip)
}

// cors answers browser CORS for the gateway surfaces (/v1/*, /mcp/*) when
// cors_origins is configured. The admin API and dashboard never get CORS
// headers, so a foreign page can't drive them with a user's session.
func (s *Server) cors(next http.Handler) http.Handler {
	if len(s.cfg.CORSOrigins) == 0 {
		return next
	}
	anyOrigin := false
	allowed := map[string]bool{}
	for _, o := range s.cfg.CORSOrigins {
		if o == "*" {
			anyOrigin = true
		}
		allowed[strings.TrimRight(o, "/")] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		p := r.URL.Path
		if origin == "" || !(strings.HasPrefix(p, "/v1/") || strings.HasPrefix(p, "/mcp/")) || !(anyOrigin || allowed[origin]) {
			next.ServeHTTP(w, r)
			return
		}
		h := w.Header()
		if anyOrigin {
			h.Set("Access-Control-Allow-Origin", "*")
		} else {
			h.Set("Access-Control-Allow-Origin", origin)
			h.Add("Vary", "Origin")
		}
		h.Set("Access-Control-Expose-Headers", "X-OpenGate-Request-Id, Mcp-Session-Id, Retry-After")
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			h.Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
			h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Accept, Mcp-Session-Id, Mcp-Protocol-Version, X-OpenGate-Customer, Last-Event-Id")
			h.Set("Access-Control-Max-Age", "600")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
