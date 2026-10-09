package server

import (
	"context"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/opengate-dev/opengate/internal/metrics"
	"github.com/opengate-dev/opengate/internal/secure"
	"github.com/opengate-dev/opengate/internal/store"
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
	}
}

func isGatewayRejection(code string) bool {
	switch code {
	case "budget_exceeded", "rate_limit_exceeded", "key_killed", "project_killed", "killed", "killed_mid_stream",
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
// (or the admin token) as a bearer token unless metrics.public is set.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.Metrics.Public {
		tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		tok = strings.TrimSpace(tok)
		want := s.cfg.MetricsToken
		if want == "" {
			want = s.cfg.AdminToken
		}
		if want == "" || !secure.ConstantTimeEqual(tok, want) {
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

// lockout tracks failed admin authentications per client IP and locks an
// IP out after MaxFailures failures inside Window.
type lockout struct {
	mu       sync.Mutex
	max      int
	window   time.Duration
	duration time.Duration
	now      func() time.Time
	entries  map[string]*lockEntry
}

type lockEntry struct {
	fails       []time.Time
	lockedUntil time.Time
}

func newLockout(max int, window, duration time.Duration) *lockout {
	return &lockout{max: max, window: window, duration: duration, now: time.Now, entries: map[string]*lockEntry{}}
}

// locked reports whether ip is locked out and for how long.
func (l *lockout) locked(ip string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entries[ip]
	if e == nil {
		return false, 0
	}
	if d := e.lockedUntil.Sub(l.now()); d > 0 {
		return true, d
	}
	return false, 0
}

// fail records a failure; it returns true when this failure triggered a lockout.
func (l *lockout) fail(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	e := l.entries[ip]
	if e == nil {
		e = &lockEntry{}
		l.entries[ip] = e
	}
	keep := e.fails[:0]
	for _, t := range e.fails {
		if now.Sub(t) < l.window {
			keep = append(keep, t)
		}
	}
	e.fails = append(keep, now)
	if len(e.fails) >= l.max {
		e.lockedUntil = now.Add(l.duration)
		e.fails = nil
		return true
	}
	// Opportunistic cleanup so the map cannot grow without bound.
	if len(l.entries) > 10000 {
		for k, v := range l.entries {
			if len(v.fails) == 0 && now.After(v.lockedUntil) {
				delete(l.entries, k)
			}
		}
	}
	return false
}

func (l *lockout) success(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e := l.entries[ip]; e != nil && !l.now().Before(e.lockedUntil) {
		delete(l.entries, ip)
	}
}

// clientIP returns the caller's IP (X-Forwarded-For only when configured).
func (s *Server) clientIP(r *http.Request) string {
	if s.cfg.TrustProxyHeaders {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			first, _, _ := strings.Cut(xff, ",")
			if ip := strings.TrimSpace(first); ip != "" {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
