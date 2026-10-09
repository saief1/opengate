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
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/opengate-dev/opengate/internal/billing"
	"github.com/opengate-dev/opengate/internal/budget"
	"github.com/opengate-dev/opengate/internal/config"
	"github.com/opengate-dev/opengate/internal/pricing"
	"github.com/opengate-dev/opengate/internal/provider"
	"github.com/opengate-dev/opengate/internal/secure"
	"github.com/opengate-dev/opengate/internal/store"
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
	s.markups = billing.NewMarkups(o.Store, 10*time.Second)
	s.routingRules = &ttlCache[[]*store.RoutingRule]{ttl: 5 * time.Second, load: func(ctx context.Context) ([]*store.RoutingRule, error) {
		return o.Store.ListRoutingRules(ctx, "")
	}}
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
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.store.PruneRateBuckets(ctx, time.Now().Add(-10*time.Minute)); err != nil {
				s.log.Warn("prune rate buckets", "err", err)
			}
			if _, err := s.store.PurgeCache(ctx, ""); err != nil {
				s.log.Warn("purge response cache", "err", err)
			}
		}
	}
}

// Handler returns the root HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", s.handleChat)
	mux.HandleFunc("POST /v1/embeddings", s.handleEmbeddings)
	mux.HandleFunc("POST /v1/responses", s.handleResponses)
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
	return s.recoverer(s.accessLog(commonHeaders(mux)))
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
				s.log.Error("panic", "err", v, "path", r.URL.Path)
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
func (s *Server) apiKeyFor(ctx context.Context, name string) (string, error) {
	s.keyMu.Lock()
	if k, ok := s.keyCache[name]; ok {
		s.keyMu.Unlock()
		return k, nil
	}
	s.keyMu.Unlock()
	keys, err := s.store.ListProviderKeys(ctx, name)
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
	} else if env := s.cfg.Providers[name].APIKeyEnv; env != "" {
		key = os.Getenv(env)
	}
	s.keyMu.Lock()
	s.keyCache[name] = key
	s.keyMu.Unlock()
	return key, nil
}

func (s *Server) invalidateKeyCache() {
	s.keyMu.Lock()
	s.keyCache = map[string]string{}
	s.keyMu.Unlock()
}

// keySource reports where a provider's key comes from (for the dashboard).
func (s *Server) keySource(ctx context.Context, name string) string {
	keys, _ := s.store.ListProviderKeys(ctx, name)
	if len(keys) > 0 {
		return "stored"
	}
	if env := s.cfg.Providers[name].APIKeyEnv; env != "" && os.Getenv(env) != "" {
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
