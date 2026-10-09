// Package config loads OpenGate configuration from YAML plus environment variables.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Provider types understood by OpenGate.
const (
	TypeOpenAI     = "openai"     // OpenAI and any OpenAI-compatible API
	TypeAnthropic  = "anthropic"  // Anthropic Messages API
	TypeGemini     = "gemini"     // Google Gemini generateContent API
	TypeOpenRouter = "openrouter" // OpenAI-compatible with OpenRouter headers
)

// Config is the full gateway configuration.
type Config struct {
	Listen           string `yaml:"listen"`
	Database         string `yaml:"database"`
	PricingFile      string `yaml:"pricing_file"`
	MaxRequestBytes  int64  `yaml:"max_request_bytes"`
	BodyLogMaxBytes  int    `yaml:"body_log_max_bytes"`
	DefaultMaxTokens int    `yaml:"default_max_tokens"`
	DisableDashboard bool   `yaml:"disable_dashboard"`
	// TrustProxyHeaders uses X-Forwarded-For as the client IP (admin
	// lockout, audit log). Only enable behind a proxy you control.
	TrustProxyHeaders bool                      `yaml:"trust_proxy_headers"`
	Providers         map[string]ProviderConfig `yaml:"providers"`
	Routes            map[string][]string       `yaml:"routes"`
	Retry             RetryConfig               `yaml:"retry"`
	Alerts            AlertsConfig              `yaml:"alerts"`
	Budgets           BudgetsConfig             `yaml:"budgets"`
	RateLimit         RateLimitConfig           `yaml:"rate_limit"`
	Metrics           MetricsConfig             `yaml:"metrics"`
	Log               LogConfig                 `yaml:"log"`
	AdminLockout      LockoutConfig             `yaml:"admin_lockout"`
	SmartRouting      SmartRoutingConfig        `yaml:"smart_routing"`
	Guardrails        GuardrailsConfig          `yaml:"guardrails"`
	Billing           BillingConfig             `yaml:"billing"`
	Cache             CacheConfig               `yaml:"cache"`
	Auth              AuthConfig                `yaml:"auth"`
	// ShareEnvProviderKeys lets every organization use provider keys from
	// environment variables. Default false: env keys serve only the default
	// org; other orgs must bring their own (stored encrypted).
	ShareEnvProviderKeys bool `yaml:"share_env_provider_keys"`

	// Secrets are only ever read from the environment, never from YAML.
	AdminToken   string `yaml:"-"`
	MasterKey    string `yaml:"-"`
	MetricsToken string `yaml:"-"` // OPENGATE_METRICS_TOKEN; falls back to the admin token
	StripeAPIKey string `yaml:"-"` // STRIPE_API_KEY (only used when billing.stripe.enabled)
}

// SmartRoutingConfig configures the "auto" alias. Routing rules themselves
// live in the database (admin API / dashboard).
type SmartRoutingConfig struct {
	Auto AutoConfig `yaml:"auto"`
}

// AutoConfig: requests for Alias are classified into a quality tier and sent
// to the cheapest model of at least that tier.
type AutoConfig struct {
	Enabled bool   `yaml:"enabled"`
	Alias   string `yaml:"alias"` // default "auto"
	// Baseline is what savings are measured against (default: the most
	// expensive tier-3 model).
	Baseline string      `yaml:"baseline"`
	Models   []AutoModel `yaml:"models"`
}

// AutoModel is one candidate model with its quality tier (1..3).
type AutoModel struct {
	Model string `yaml:"model"`
	Tier  int    `yaml:"tier"`
}

// GuardrailsConfig holds server-wide guardrail defaults; projects can override.
type GuardrailsConfig struct {
	PII        string   `yaml:"pii"`        // off (default) | log | redact | block
	Categories []string `yaml:"categories"` // empty = all
	Responses  bool     `yaml:"responses"`  // also scan non-streaming responses
	Injection  string   `yaml:"injection"`  // off (default) | log | block
}

// BillingConfig controls end-customer attribution and Stripe usage sync.
type BillingConfig struct {
	// CustomerHeader carries the end-customer ID (default X-OpenGate-Customer).
	CustomerHeader string `yaml:"customer_header"`
	// IgnoreUserField stops using the OpenAI "user" body field as the
	// customer ID when the header is absent.
	IgnoreUserField bool         `yaml:"ignore_user_field"`
	Stripe          StripeConfig `yaml:"stripe"`
}

// StripeConfig reports per-customer usage as Stripe Billing meter events.
type StripeConfig struct {
	Enabled   bool          `yaml:"enabled"`
	BaseURL   string        `yaml:"base_url"`   // default https://api.stripe.com
	EventName string        `yaml:"event_name"` // meter event_name (default opengate_usage)
	Unit      string        `yaml:"unit"`       // cents (billed USD x100, default) | tokens
	Interval  time.Duration `yaml:"interval"`   // default 1h
	// Lag excludes the most recent window so in-flight requests are
	// logged before their window is reported (default 15m).
	Lag time.Duration `yaml:"lag"`
}

// AuthConfig controls dashboard user accounts.
type AuthConfig struct {
	SessionTTL time.Duration `yaml:"session_ttl"` // idle timeout, default 12h
	// SessionMaxAge caps a session's total lifetime (default 7 days).
	SessionMaxAge time.Duration `yaml:"session_max_age"`
	// AllowSignup lets anyone create an account plus a new organization
	// (hosted/cloud mode). Default false: users join via invites.
	AllowSignup bool `yaml:"allow_signup"`
	// SecureCookies forces the Secure cookie flag (it is always set when the
	// request arrived over TLS or, with trust_proxy_headers, X-Forwarded-Proto=https).
	SecureCookies bool `yaml:"secure_cookies"`
	// InviteTTL is how long invite links stay valid (default 7 days).
	InviteTTL time.Duration `yaml:"invite_ttl"`
}

// CacheConfig tunes the opt-in exact-match response cache.
type CacheConfig struct {
	Disabled      bool          `yaml:"disabled"`
	MaxTTL        time.Duration `yaml:"max_ttl"`         // cap for per-request TTLs (default 24h)
	MaxEntryBytes int           `yaml:"max_entry_bytes"` // default 1 MiB
	// ShareAcrossCustomers lets identical requests from different end
	// customers share entries (default false: the customer ID is part of the key).
	ShareAcrossCustomers bool `yaml:"share_across_customers"`
}

// BudgetsConfig tunes spend enforcement.
type BudgetsConfig struct {
	// Strict refuses requests whose worst-case (max_tokens) cost would
	// exceed the remaining budget. Default false: admit while
	// committed+reserved spend is under the limit.
	Strict bool `yaml:"strict"`
	// SyncInterval re-reads spend from the database so replicas sharing
	// PostgreSQL converge (default 5s for PostgreSQL, 0 for SQLite).
	SyncInterval time.Duration `yaml:"sync_interval"`
}

// RateLimitConfig selects the rate limit backend.
type RateLimitConfig struct {
	// Backend is "database" (default; persisted, shared across replicas
	// when using PostgreSQL) or "memory" (fastest, per process).
	Backend string `yaml:"backend"`
}

// MetricsConfig controls the Prometheus endpoint.
type MetricsConfig struct {
	// Disabled turns GET /metrics off.
	Disabled bool `yaml:"disabled"`
	// Public serves /metrics without a bearer token (only do this when the
	// port is not reachable from untrusted networks).
	Public bool `yaml:"public"`
}

// LogConfig controls structured logging.
type LogConfig struct {
	Level  string `yaml:"level"`  // debug|info|warn|error (default info)
	Format string `yaml:"format"` // json (default) | text
}

// LockoutConfig throttles failed admin logins per client IP.
type LockoutConfig struct {
	MaxFailures int           `yaml:"max_failures"` // default 10
	Window      time.Duration `yaml:"window"`       // default 5m
	Lockout     time.Duration `yaml:"lockout"`      // default 15m
}

// ProviderConfig describes one upstream provider.
type ProviderConfig struct {
	Type      string            `yaml:"type"`
	BaseURL   string            `yaml:"base_url"`
	APIKeyEnv string            `yaml:"api_key_env"`
	Headers   map[string]string `yaml:"headers"`
	Timeout   time.Duration     `yaml:"timeout"`
}

// RetryConfig controls per-target retries before failing over.
type RetryConfig struct {
	MaxAttempts    int           `yaml:"max_attempts"`
	InitialBackoff time.Duration `yaml:"initial_backoff"`
	MaxBackoff     time.Duration `yaml:"max_backoff"`
}

// AlertsConfig controls budget threshold webhooks.
type AlertsConfig struct {
	WebhookURL string    `yaml:"webhook_url"`
	Thresholds []float64 `yaml:"thresholds"`
}

// DefaultProviders are always available; YAML entries with the same name
// override individual fields.
func DefaultProviders() map[string]ProviderConfig {
	return map[string]ProviderConfig{
		"openai":     {Type: TypeOpenAI, BaseURL: "https://api.openai.com/v1", APIKeyEnv: "OPENAI_API_KEY"},
		"anthropic":  {Type: TypeAnthropic, BaseURL: "https://api.anthropic.com", APIKeyEnv: "ANTHROPIC_API_KEY"},
		"gemini":     {Type: TypeGemini, BaseURL: "https://generativelanguage.googleapis.com", APIKeyEnv: "GEMINI_API_KEY"},
		"openrouter": {Type: TypeOpenRouter, BaseURL: "https://openrouter.ai/api/v1", APIKeyEnv: "OPENROUTER_API_KEY"},
	}
}

// Default returns a configuration with secure, sensible defaults.
func Default() *Config {
	return &Config{
		Listen:           "127.0.0.1:8080",
		Database:         "opengate.db",
		MaxRequestBytes:  10 << 20,
		BodyLogMaxBytes:  64 << 10,
		DefaultMaxTokens: 4096,
		Providers:        DefaultProviders(),
		Routes:           map[string][]string{},
		Retry:            RetryConfig{MaxAttempts: 2, InitialBackoff: 250 * time.Millisecond, MaxBackoff: 4 * time.Second},
		Alerts:           AlertsConfig{Thresholds: []float64{0.8, 1.0}},
		RateLimit:        RateLimitConfig{Backend: "database"},
		Log:              LogConfig{Level: "info", Format: "json"},
		AdminLockout:     LockoutConfig{MaxFailures: 10, Window: 5 * time.Minute, Lockout: 15 * time.Minute},
	}
}

var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// Load reads the YAML file at path (optional) and applies environment overrides.
func Load(path string) (*Config, error) {
	cfg := Default()
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config: %w", err)
		}
		if err := cfg.parseYAML(data); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
	}
	cfg.applyEnv()
	if err := cfg.Normalize(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) parseYAML(data []byte) error {
	// Expand ${VAR} references (only that form, so literal '$' stays intact).
	data = envRef.ReplaceAllFunc(data, func(m []byte) []byte {
		return []byte(os.Getenv(string(envRef.FindSubmatch(m)[1])))
	})
	defaults := c.Providers
	c.Providers = nil
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(c); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	userProviders := c.Providers
	c.Providers = defaults
	for name, p := range userProviders {
		base, ok := c.Providers[name]
		if !ok {
			base = ProviderConfig{}
		}
		if p.Type != "" {
			base.Type = p.Type
		}
		if p.BaseURL != "" {
			base.BaseURL = p.BaseURL
		}
		if p.APIKeyEnv != "" {
			base.APIKeyEnv = p.APIKeyEnv
		}
		if p.Headers != nil {
			base.Headers = p.Headers
		}
		if p.Timeout != 0 {
			base.Timeout = p.Timeout
		}
		c.Providers[name] = base
	}
	return nil
}

func (c *Config) applyEnv() {
	set := func(dst *string, key string) {
		if v, ok := os.LookupEnv(key); ok && v != "" {
			*dst = v
		}
	}
	set(&c.Listen, "OPENGATE_LISTEN")
	set(&c.Database, "OPENGATE_DATABASE")
	set(&c.PricingFile, "OPENGATE_PRICING_FILE")
	set(&c.Alerts.WebhookURL, "OPENGATE_ALERT_WEBHOOK_URL")
	set(&c.AdminToken, "OPENGATE_ADMIN_TOKEN")
	set(&c.MasterKey, "OPENGATE_MASTER_KEY")
	set(&c.MetricsToken, "OPENGATE_METRICS_TOKEN")
	set(&c.Log.Level, "OPENGATE_LOG_LEVEL")
	set(&c.Log.Format, "OPENGATE_LOG_FORMAT")
	set(&c.RateLimit.Backend, "OPENGATE_RATE_LIMIT_BACKEND")
	set(&c.StripeAPIKey, "STRIPE_API_KEY")
	// OPENGATE_PROVIDER_<NAME>_BASE_URL overrides a provider base URL.
	for name, p := range c.Providers {
		key := "OPENGATE_PROVIDER_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_")) + "_BASE_URL"
		if v := os.Getenv(key); v != "" {
			p.BaseURL = v
			c.Providers[name] = p
		}
	}
}

// Normalize fills defaults and validates non-secret settings.
func (c *Config) Normalize() error {
	if c.Providers == nil {
		c.Providers = DefaultProviders()
	}
	if c.Routes == nil {
		c.Routes = map[string][]string{}
	}
	if c.MaxRequestBytes <= 0 {
		c.MaxRequestBytes = 10 << 20
	}
	if c.BodyLogMaxBytes <= 0 {
		c.BodyLogMaxBytes = 64 << 10
	}
	if c.DefaultMaxTokens <= 0 {
		c.DefaultMaxTokens = 4096
	}
	if c.Retry.MaxAttempts <= 0 {
		c.Retry.MaxAttempts = 1
	}
	if c.Retry.InitialBackoff <= 0 {
		c.Retry.InitialBackoff = 250 * time.Millisecond
	}
	if c.Retry.MaxBackoff < c.Retry.InitialBackoff {
		c.Retry.MaxBackoff = c.Retry.InitialBackoff
	}
	if len(c.Alerts.Thresholds) == 0 {
		c.Alerts.Thresholds = []float64{0.8, 1.0}
	}
	sort.Float64s(c.Alerts.Thresholds)
	switch c.RateLimit.Backend {
	case "":
		c.RateLimit.Backend = "database"
	case "database", "memory":
	default:
		return fmt.Errorf("rate_limit.backend: want database|memory, got %q", c.RateLimit.Backend)
	}
	if c.Budgets.SyncInterval == 0 && (strings.HasPrefix(c.Database, "postgres://") || strings.HasPrefix(c.Database, "postgresql://")) {
		c.Budgets.SyncInterval = 5 * time.Second
	}
	if c.Budgets.SyncInterval < 0 {
		c.Budgets.SyncInterval = 0
	}
	switch strings.ToLower(c.Log.Level) {
	case "", "info", "debug", "warn", "error":
	default:
		return fmt.Errorf("log.level: want debug|info|warn|error, got %q", c.Log.Level)
	}
	switch c.Log.Format {
	case "":
		c.Log.Format = "json"
	case "json", "text":
	default:
		return fmt.Errorf("log.format: want json|text, got %q", c.Log.Format)
	}
	if c.AdminLockout.MaxFailures <= 0 {
		c.AdminLockout.MaxFailures = 10
	}
	if c.AdminLockout.Window <= 0 {
		c.AdminLockout.Window = 5 * time.Minute
	}
	if c.AdminLockout.Lockout <= 0 {
		c.AdminLockout.Lockout = 15 * time.Minute
	}
	if err := c.normalizePhase2(); err != nil {
		return err
	}
	for name, p := range c.Providers {
		switch p.Type {
		case TypeOpenAI, TypeAnthropic, TypeGemini, TypeOpenRouter:
		default:
			return fmt.Errorf("provider %q: unknown type %q (want openai|anthropic|gemini|openrouter)", name, p.Type)
		}
		if p.BaseURL == "" {
			return fmt.Errorf("provider %q: base_url is required", name)
		}
		p.BaseURL = strings.TrimRight(p.BaseURL, "/")
		if p.Timeout <= 0 {
			p.Timeout = 120 * time.Second
		}
		c.Providers[name] = p
	}
	for alias, targets := range c.Routes {
		if len(targets) == 0 {
			return fmt.Errorf("route %q has no targets", alias)
		}
		for _, t := range targets {
			prov, model, ok := strings.Cut(t, "/")
			if !ok || model == "" {
				return fmt.Errorf("route %q: target %q must look like provider/model", alias, t)
			}
			if _, ok := c.Providers[prov]; !ok {
				return fmt.Errorf("route %q: target %q references unknown provider %q", alias, t, prov)
			}
		}
	}
	return nil
}

// ValidateSecrets enforces the secure-by-default startup requirements.
func (c *Config) ValidateSecrets() error {
	if len(c.AdminToken) < 16 {
		return errors.New("OPENGATE_ADMIN_TOKEN must be set to a random value of at least 16 characters")
	}
	if c.MasterKey == "" {
		return errors.New("OPENGATE_MASTER_KEY must be set (generate one with `opengate keygen`)")
	}
	return nil
}

func (c *Config) normalizePhase2() error {
	a := &c.SmartRouting.Auto
	if a.Alias == "" {
		a.Alias = "auto"
	}
	if a.Enabled && len(a.Models) == 0 {
		return errors.New("smart_routing.auto.models must list at least one model")
	}
	for _, m := range a.Models {
		if m.Tier < 1 || m.Tier > 3 {
			return fmt.Errorf("smart_routing.auto: model %q tier must be 1..3", m.Model)
		}
		if _, _, ok := strings.Cut(m.Model, "/"); !ok {
			return fmt.Errorf("smart_routing.auto: model %q must look like provider/model", m.Model)
		}
	}
	if _, clash := c.Routes[a.Alias]; clash && a.Enabled {
		return fmt.Errorf("smart_routing.auto.alias %q clashes with a route", a.Alias)
	}
	g := &c.Guardrails
	switch g.PII {
	case "":
		g.PII = "off"
	case "off", "log", "redact", "block":
	default:
		return fmt.Errorf("guardrails.pii: want off|log|redact|block, got %q", g.PII)
	}
	switch g.Injection {
	case "":
		g.Injection = "off"
	case "off", "log", "block":
	default:
		return fmt.Errorf("guardrails.injection: want off|log|block, got %q", g.Injection)
	}
	if c.Billing.CustomerHeader == "" {
		c.Billing.CustomerHeader = "X-OpenGate-Customer"
	}
	st := &c.Billing.Stripe
	if st.BaseURL == "" {
		st.BaseURL = "https://api.stripe.com"
	}
	st.BaseURL = strings.TrimRight(st.BaseURL, "/")
	if st.EventName == "" {
		st.EventName = "opengate_usage"
	}
	switch st.Unit {
	case "":
		st.Unit = "cents"
	case "cents", "tokens":
	default:
		return fmt.Errorf("billing.stripe.unit: want cents|tokens, got %q", st.Unit)
	}
	if st.Interval <= 0 {
		st.Interval = time.Hour
	}
	if st.Lag <= 0 {
		st.Lag = 15 * time.Minute
	}
	if c.Auth.SessionTTL <= 0 {
		c.Auth.SessionTTL = 12 * time.Hour
	}
	if c.Auth.SessionMaxAge <= 0 {
		c.Auth.SessionMaxAge = 7 * 24 * time.Hour
	}
	if c.Auth.InviteTTL <= 0 {
		c.Auth.InviteTTL = 7 * 24 * time.Hour
	}
	if c.Cache.MaxTTL <= 0 {
		c.Cache.MaxTTL = 24 * time.Hour
	}
	if c.Cache.MaxEntryBytes <= 0 {
		c.Cache.MaxEntryBytes = 1 << 20
	}
	return nil
}

// ValidateStripe checks that Stripe sync has a key when enabled.
func (c *Config) ValidateStripe() error {
	if c.Billing.Stripe.Enabled && c.StripeAPIKey == "" {
		return errors.New("billing.stripe.enabled requires STRIPE_API_KEY in the environment")
	}
	return nil
}
