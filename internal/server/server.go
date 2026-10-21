// Package server wires the OpenAI-compatible gateway, the admin API and the
// embedded dashboard into one http.Handler.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"os"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/saief1/opengate/internal/billing"
	"github.com/saief1/opengate/internal/budget"
	"github.com/saief1/opengate/internal/config"
	"github.com/saief1/opengate/internal/pricing"
	"github.com/saief1/opengate/internal/provider"
	"github.com/saief1/opengate/internal/secure"
	"github.com/saief1/opengate/internal/store"
)

func init() {
	// Not in Go's built-in MIME table, and distroless images ship no /etc/mime.types.
	_ = mime.AddExtensionType(".ico", "image/x-icon")
	_ = mime.AddExtensionType(".webmanifest", "application/manifest+json")
	_ = mime.AddExtensionType(".svg", "image/svg+xml")
}

// Version is set at build time via -ldflags.
var Version = "dev"

// Options configure a Server.
type Options struct {
	Config  *config.Config
	Store   *store.Store
	Cipher  *secure.Cipher
	Pricing *pricing.Registry
	Logger  *slog.Logger
	WebFS   fs.FS // dashboard assets; nil disables the dashboard
	Alerter *budget.Alerter
	HTTP    *provider.HTTP
	Sleep   func(ctx context.Context, d time.Duration) error // for tests
	// StripeClient overrides the HTTP client used for Stripe (tests).
	StripeClient *http.Client
}

// Server is the OpenGate HTTP application.
type Server struct {
	cfg      *config.Config
	store    *store.Store
	cipher   *secure.Cipher
	pricing  *pricing.Registry
	log      *slog.Logger
	webFS    fs.FS
	enforcer *budget.Enforcer
	alerter  *budget.Alerter
	adapters map[string]provider.Adapter
	inflight *inflight
	sleep    func(ctx context.Context, d time.Duration) error

	metrics *gatewayMetrics
	lockout *lockout

	markups      *billing.Markups
	routingRules *ttlCache[[]*store.RoutingRule]
	stripe       *billing.Stripe // nil unless billing.stripe.enabled
	cloud        *cloudState     // nil unless cloud.enabled

	trusted    []*net.IPNet  // trusted_proxies
	active     atomic.Int64  // requests in the gateway (max_inflight)
	pwSem      chan struct{} // bounds concurrent argon2id hashing
	sprayMu    sync.Mutex
	sprayCount map[string]int

	mcpRates  budget.RateStore // per agent+tool MCP rate limits
	mcpClient *http.Client

	keyMu    sync.Mutex
	keyCache map[string]string // provider name -> decrypted API key ("" = none)
}

// New builds a Server.
func New(ctx context.Context, o Options) (*Server, error) {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.HTTP == nil {
		o.HTTP = provider.NewHTTP()
	}
	if o.Alerter == nil {
		o.Alerter = budget.NewAlerter(o.Config.Alerts.WebhookURL, o.Config.Alerts.Thresholds, o.Logger)
	}
	if o.Sleep == nil {
		o.Sleep = sleepCtx
	}
	var rates budget.RateStore = budget.NewRateLimiter()
	if o.Config.RateLimit.Backend == "database" {
		rates = budget.NewDBRates(o.Store)
	}
	enf, err := budget.NewEnforcer(ctx, o.Store, o.Alerter, o.Logger, budget.Options{
		Rates: rates, SyncInterval: o.Config.Budgets.SyncInterval, Strict: o.Config.Budgets.Strict})
	if err != nil {
		return nil, err
	}
	lc := o.Config.AdminLockout
	s := &Server{cfg: o.Config, store: o.Store, cipher: o.Cipher, pricing: o.Pricing, log: o.Logger, webFS: o.WebFS,
		enforcer: enf, alerter: o.Alerter, adapters: map[string]provider.Adapter{}, inflight: newInflight(),
		sleep: o.Sleep, keyCache: map[string]string{}, metrics: newGatewayMetrics(),
		lockout: newLockout(lc.MaxFailures, lc.Window, lc.Lockout)}
	if t := o.Pricing.Table(); t != nil && !t.HasDefault() && !o.Config.Budgets.Strict {
		s.log.Warn("pricing table has no `default:` entry and budgets.strict is off: unknown models will be charged $0")
	}
	if o.Config.TrustProxyHeaders && len(o.Config.TrustedProxies) == 0 {
		s.log.Warn("trust_proxy_headers is on without trusted_proxies: only the rightmost X-Forwarded-For hop is used; set trusted_proxies to your proxy CIDRs")
	}
	s.mcpRates = rates
	s.mcpClient = &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone(),
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	s.sprayCount = map[string]int{}
	s.pwSem = make(chan struct{}, 2*runtime.NumCPU())
	for _, c := range o.Config.TrustedProxies {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			return nil, fmt.Errorf("trusted_proxies: %w", err)
		}
		s.trusted = append(s.trusted, n)
	}
	s.markups = billing.NewMarkups(o.Store, 10*time.Second)
	s.routingRules = &ttlCache[[]*store.RoutingRule]{ttl: 5 * time.Second, load: func(ctx context.Context) ([]*store.RoutingRule, error) {
		return o.Store.ListRoutingRules(ctx, "")
	}}
	if o.Config.Cloud.Enabled {
		if err := o.Config.ValidateStripe(); err != nil {
			return nil, err
		}
		s.cloud = newCloudState(o.Config, o.StripeClient)
	}
	if o.Config.Billing.Stripe.Enabled {
		if err := o.Config.ValidateStripe(); err != nil {
			return nil, err
		}
		s.stripe = billing.NewStripe(o.Config.Billing.Stripe, o.Config.StripeAPIKey, o.Store, o.StripeClient, o.Logger)
	}
	for _, t := range []string{config.TypeOpenAI, config.TypeAnthropic, config.TypeGemini, config.TypeOpenRouter} {
		a, _ := provider.ForType(t, o.HTTP)
		s.adapters[t] = a
	}
	if _, err := o.Store.EnsureProject(ctx, "default"); err != nil {
		return nil, err
	}
	return s, nil
}

// Enforcer exposes the budget enforcer (used by tests and the CLI).
func (s *Server) Enforcer() *budget.Enforcer { return s.enforcer }

// Run executes background maintenance until ctx is cancelled.
func (s *Server) Run(ctx context.Context) {
	if s.stripe != nil {
		go s.stripe.Run(ctx)
	}
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	// Kills issued on another replica (shared Postgres) reach this
	// replica's in-flight streams within one poll interval.
	kt := time.NewTicker(2 * time.Second)
	defer kt.Stop()
	ht := time.NewTicker(time.Hour)
	defer ht.Stop()
	if s.cloud != nil {
		s.pruneCloud(ctx)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-kt.C:
			s.SweepKills(ctx)
		case <-ht.C:
			if s.cloud != nil {
				s.pruneCloud(ctx)
			}
		case <-t.C:
			if err := s.store.PruneRateBuckets(ctx, time.Now().Add(-10*time.Minute)); err != nil {
				s.log.Warn("prune rate buckets", "err", err)
			}
			if err := s.store.PruneSessions(ctx, time.Now()); err != nil {
				s.log.Warn("prune sessions", "err", err)
			}
			if _, err := s.store.PurgeCache(ctx, ""); err != nil {
				s.log.Warn("purge response cache", "err", err)
			}
		}
	}
}

// limitInflight enforces the optional instance-wide max_inflight cap.
func (s *Server) limitInflight(h http.HandlerFunc) http.HandlerFunc {
	if s.cfg.MaxInflight <= 0 {
		return h
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if n := s.active.Add(1); n > int64(s.cfg.MaxInflight) {
			s.active.Add(-1)
			w.Header().Set("Retry-After", "1")
			writeError(w, http.StatusServiceUnavailable, "server_error", "gateway_overloaded", "OpenGate is at its max_inflight limit; retry shortly.")
			return
		}
		defer s.active.Add(-1)
		h(w, r)
	}
}

// Handler returns the root HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", s.limitInflight(s.handleChat))
	mux.HandleFunc("POST /v1/embeddings", s.limitInflight(s.handleEmbeddings))
	mux.HandleFunc("POST /v1/responses", s.limitInflight(s.handleResponses))
	mux.HandleFunc("GET /v1/models", s.handleModels)
	// Liveness: the process is up. Readiness: dependencies answer.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": Version})
	})
	mux.HandleFunc("GET /livez", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", s.handleReady)
	if !s.cfg.Metrics.Disabled {
		mux.HandleFunc("GET /metrics", s.handleMetrics)
	}
	if !s.cfg.MCP.Disabled {
		mux.HandleFunc("POST /mcp/{server}", s.limitInflight(s.handleMCP))
		mux.HandleFunc("GET /mcp/{server}", s.handleMCP)
		mux.HandleFunc("DELETE /mcp/{server}", s.handleMCP)
		mux.HandleFunc("POST /mcp/{server}/{rest...}", s.limitInflight(s.handleMCP))
	}
	s.registerAdmin(mux)
	if s.webFS != nil && !s.cfg.DisableDashboard {
		files := http.FileServerFS(s.webFS)
		mux.Handle("GET /ui/", http.StripPrefix("/ui/", uiHeaders(files)))
		// Browsers request /favicon.ico at the site root regardless of <link> tags.
		mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "public, max-age=86400")
			http.ServeFileFS(w, r, s.webFS, "favicon.ico")
		})
		mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/ui/", http.StatusFound)
		})
	}
	return s.recoverer(s.accessLog(commonHeaders(s.cors(mux))))
}

func commonHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

func uiHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Cache-Control", "no-cache")
		if r.TLS != nil {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

// Flush keeps SSE streaming working through the recorder.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (s *Server) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		p := r.URL.Path
		if strings.HasPrefix(p, "/ui/") || p == "/healthz" || p == "/livez" || p == "/readyz" || p == "/metrics" {
			return
		}
		s.log.Info("http", "method", r.Method, "path", p, "status", rec.status, "dur_ms", time.Since(start).Milliseconds(),
			"request_id", rec.Header().Get("X-OpenGate-Request-Id"), "remote_ip", s.clientIP(r))
	})
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				s.log.Error("panic", "err", v, "path", r.URL.Path, "stack", string(debug.Stack()))
				writeError(w, http.StatusInternalServerError, "server_error", "internal_error", "internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// ---------- helpers ----------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// apiError is an OpenAI-format error body.
type apiError struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    string  `json:"code"`
}

func errorBody(typ, code, msg string) map[string]apiError {
	return map[string]apiError{"error": {Message: msg, Type: typ, Code: code}}
}

func writeError(w http.ResponseWriter, status int, typ, code, msg string) {
	writeJSON(w, status, errorBody(typ, code, msg))
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// apiKeyFor returns the API key for a provider: newest stored (encrypted)
// key first, then the provider's api_key_env variable.
func (s *Server) apiKeyFor(ctx context.Context, org, name string) (string, error) {
	ck := org + "\x00" + name
	s.keyMu.Lock()
	if k, ok := s.keyCache[ck]; ok {
		s.keyMu.Unlock()
		return k, nil
	}
	s.keyMu.Unlock()
	keys, err := s.store.ListProviderKeys(ctx, org, name)
	if err != nil {
		return "", err
	}
	var key string
	if len(keys) > 0 {
		pt, err := s.cipher.Decrypt(keys[0].Ciphertext, store.ProviderKeyAAD(keys[0].ID, keys[0].Provider))
		if err != nil {
			return "", fmt.Errorf("decrypt provider key %s: %w", keys[0].ID, err)
		}
		key = string(pt)
	} else if env := s.cfg.Providers[name].APIKeyEnv; env != "" && s.envKeysFor(org) {
		key = os.Getenv(env)
	}
	s.keyMu.Lock()
	s.keyCache[ck] = key
	s.keyMu.Unlock()
	return key, nil
}

// envKeysFor reports whether an org may use provider keys from the environment.
func (s *Server) envKeysFor(org string) bool {
	return org == store.DefaultOrgID || s.cfg.ShareEnvProviderKeys
}

func (s *Server) invalidateKeyCache() {
	s.keyMu.Lock()
	s.keyCache = map[string]string{}
	s.keyMu.Unlock()
}

// keySource reports where a provider's key comes from (for the dashboard).
func (s *Server) keySource(ctx context.Context, org, name string) string {
	keys, _ := s.store.ListProviderKeys(ctx, org, name)
	if len(keys) > 0 {
		return "stored"
	}
	if env := s.cfg.Providers[name].APIKeyEnv; env != "" && os.Getenv(env) != "" && s.envKeysFor(org) {
		return "env:" + env
	}
	return "none"
}

// errKilled is the cancel cause used by the kill switch.
var errKilled = errors.New("killed by operator")

type flight struct {
	keyID, projectID string
	cancel           context.CancelCauseFunc
}

type inflight struct {
	mu sync.Mutex
	m  map[string]flight
}

func newInflight() *inflight { return &inflight{m: map[string]flight{}} }

func (f *inflight) add(id string, fl flight) {
	f.mu.Lock()
	f.m[id] = fl
	f.mu.Unlock()
}

func (f *inflight) remove(id string) {
	f.mu.Lock()
	delete(f.m, id)
	f.mu.Unlock()
}

// snapshot returns the distinct key and project IDs currently in flight.
func (f *inflight) snapshot() (keys, projects []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ks, ps := map[string]bool{}, map[string]bool{}
	for _, fl := range f.m {
		if !ks[fl.keyID] {
			ks[fl.keyID] = true
			keys = append(keys, fl.keyID)
		}
		if !ps[fl.projectID] {
			ps[fl.projectID] = true
			projects = append(projects, fl.projectID)
		}
	}
	return
}

// SweepKills re-reads the kill/revoke flags of everything in flight from the
// database and aborts requests whose key or project was killed elsewhere
// (another replica, or directly in the DB). It returns the number aborted.
func (s *Server) SweepKills(ctx context.Context) int {
	keys, projects := s.inflight.snapshot()
	if len(keys) == 0 {
		return 0
	}
	deadKeys, deadProjects, err := s.store.KilledAmong(ctx, keys, projects)
	if err != nil {
		s.log.Warn("kill sweep", "err", err)
		return 0
	}
	n := 0
	for _, k := range deadKeys {
		n += s.inflight.kill(k, "")
	}
	for _, p := range deadProjects {
		n += s.inflight.kill("", p)
	}
	if n > 0 {
		s.log.Info("kill sweep aborted in-flight requests", "count", n)
	}
	return n
}

// kill cancels in-flight requests matching keyID or projectID.
func (f *inflight) kill(keyID, projectID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, fl := range f.m {
		if (keyID != "" && fl.keyID == keyID) || (projectID != "" && fl.projectID == projectID) {
			fl.cancel(errKilled)
			n++
		}
	}
	return n
}
