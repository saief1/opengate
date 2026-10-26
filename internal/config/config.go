// Package config loads OpenGate configuration from YAML plus environment variables.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
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
	TrustProxyHeaders bool `yaml:"trust_proxy_headers"`
	// TrustedProxies (CIDRs) restricts X-Forwarded-* to these peers; the
	// client IP is the rightmost X-Forwarded-For hop not in this list.
	TrustedProxies []string `yaml:"trusted_proxies"`
	// AllowInsecureUpstream permits plain-http provider / Stripe / webhook
	// URLs to non-loopback hosts (keys would travel in cleartext).
	AllowInsecureUpstream bool `yaml:"allow_insecure_upstream"`
	// MaxInflight caps concurrent gateway requests instance-wide (0 = off).
	MaxInflight  int                       `yaml:"max_inflight"`
	Providers    map[string]ProviderConfig `yaml:"providers"`
	Routes       map[string][]string       `yaml:"routes"`
	Retry        RetryConfig               `yaml:"retry"`
	Alerts       AlertsConfig              `yaml:"alerts"`
	Budgets      BudgetsConfig             `yaml:"budgets"`
	RateLimit    RateLimitConfig           `yaml:"rate_limit"`
	Metrics      MetricsConfig             `yaml:"metrics"`
	Log          LogConfig                 `yaml:"log"`
	AdminLockout LockoutConfig             `yaml:"admin_lockout"`
	SmartRouting SmartRoutingConfig        `yaml:"smart_routing"`
	Guardrails   GuardrailsConfig          `yaml:"guardrails"`
	Billing      BillingConfig             `yaml:"billing"`
	Cache        CacheConfig               `yaml:"cache"`
	Auth         AuthConfig                `yaml:"auth"`
	Cloud        CloudConfig               `yaml:"cloud"`
	// ShareEnvProviderKeys lets every organization use provider keys from
	// environment variables. Default false: env keys serve only the default
	// org; other orgs must bring their own (stored encrypted).
	ShareEnvProviderKeys bool `yaml:"share_env_provider_keys"`
	// Plan gates the Team features (audit log viewer, roles, customer
	// billing pass-through) on a self-hosted install: "" (default) or
	// "team"/"enterprise" = unlocked; "free" = locked. In cloud mode each
	// org's subscription decides instead. Env: OPENGATE_PLAN.
	Plan string `yaml:"plan"`
	// CORSOrigins lists browser origins allowed to call the gateway (/v1/*,
	// /mcp/*) cross-origin. Empty (default) = no CORS headers at all. The
	// admin API never sends CORS headers. Env: OPENGATE_CORS_ORIGINS (comma
	// separated). "*" allows any origin (no credentials).
	CORSOrigins []string  `yaml:"cors_origins"`
	MCP         MCPConfig `yaml:"mcp"`

	// Secrets are only ever read from the environment, never from YAML.
	AdminToken   string `yaml:"-"`
	MasterKey    string `yaml:"-"`
	MetricsToken string `yaml:"-"` // OPENGATE_METRICS_TOKEN; falls back to the admin token
	StripeAPIKey string `yaml:"-"` // STRIPE_API_KEY (billing.stripe meter sync, or cloud subscriptions)
	// StripeWebhookSecret (STRIPE_WEBHOOK_SECRET, "whsec_...") verifies
	// Stripe webhook signatures in cloud mode.
	StripeWebhookSecret string `yaml:"-"`
	// StripeAllowLive (OPENGATE_STRIPE_ALLOW_LIVE=true) permits live-mode
	// Stripe keys. Off by default so a misplaced sk_live_ key can't charge
	// real cards from a test deployment.
	StripeAllowLive bool `yaml:"-"`
	// Production (OPENGATE_ENV=production or OPENGATE_PRODUCTION=true) refuses
	// demo flags and requires a real database backup path.
	Production bool `yaml:"-"`
}

// MCPConfig configures the MCP (Model Context Protocol) tool gateway.
type MCPConfig struct {
	// Disabled turns off /mcp/{server} entirely.
	Disabled bool `yaml:"disabled"`
	// Timeout bounds one upstream MCP request (default 60s; SSE streams are
	// bounded by StreamTimeout instead).
	Timeout       time.Duration `yaml:"timeout"`
	StreamTimeout time.Duration `yaml:"stream_timeout"` // default 10m
	// ArgsLogMaxBytes caps logged (redacted) tool arguments (default 4096).
	ArgsLogMaxBytes int `yaml:"args_log_max_bytes"`
	// DefaultDeny makes tools without an explicit allow rule forbidden
	// (default false: allowed unless denied).
	DefaultDeny bool `yaml:"default_deny"`
}

// CloudConfig turns an install into a hosted, multi-tenant "OpenGate Cloud":
// self-serve sign-up, per-org plans with quotas, and Stripe subscriptions
// (Checkout, Customer Portal, webhooks).
type CloudConfig struct {
	Enabled bool `yaml:"enabled"`
	// BillingDisabled (OPENGATE_BILLING_DISABLED=true) runs cloud mode
	// without Stripe: no keys are required, checkout/portal/webhook routes
	// answer 503 billing_not_enabled, and every org stays on DefaultPlan.
	BillingDisabled bool `yaml:"billing_disabled"`
	// PublicURL is the externally visible base URL (https://cloud.example.com);
	// Stripe redirects back to it after Checkout and the portal.
	PublicURL string `yaml:"public_url"`
	// DefaultPlan is assigned to new orgs and to orgs whose subscription
	// ended (default "free").
	DefaultPlan string `yaml:"default_plan"`
	// MaxOrgsPerUser caps how many orgs one user may own (default 3).
	MaxOrgsPerUser int `yaml:"max_orgs_per_user"`
	// GracePeriod keeps a past_due subscription on its paid plan while
	// Stripe retries the payment (default 7 days).
	GracePeriod time.Duration `yaml:"grace_period"`
	// SalesURL is linked from "contact sales" plans (mailto: or https:).
	SalesURL string `yaml:"sales_url"`
	// Plans; defaults to DefaultCloudPlans() when empty.
	Plans  []CloudPlan       `yaml:"plans"`
	Stripe CloudStripeConfig `yaml:"stripe"`
}

// CloudPlan is one sellable plan. Limits of 0 mean unlimited.
type CloudPlan struct {
	ID               string   `yaml:"id" json:"id"`
	Name             string   `yaml:"name" json:"name"`
	PriceUSD         float64  `yaml:"price_usd" json:"price_usd"` // display only; Stripe's price is authoritative
	Interval         string   `yaml:"interval" json:"interval"`   // display: "month"
	StripePriceID    string   `yaml:"stripe_price_id" json:"-"`
	ContactSales     bool     `yaml:"contact_sales" json:"contact_sales"` // not self-serve; set by the operator
	MaxRequestsMonth int64    `yaml:"max_requests_month" json:"max_requests_month"`
	MaxMembers       int      `yaml:"max_members" json:"max_members"`
	MaxProjects      int      `yaml:"max_projects" json:"max_projects"`
	LogRetentionDays int      `yaml:"log_retention_days" json:"log_retention_days"`
	Features         []string `yaml:"features" json:"features"` // marketing bullets for the dashboard
}

// SelfServe reports whether the plan can be bought through Stripe Checkout.
func (p CloudPlan) SelfServe() bool { return p.StripePriceID != "" && !p.ContactSales }

// StripeAPIVersion is the Stripe API version OpenGate is built and tested
// against. Every request pins it with the Stripe-Version header; webhook
// endpoints should be created with the same version.
const StripeAPIVersion = "2026-09-30.endive"

// CloudStripeConfig configures Stripe subscriptions for cloud mode.
type CloudStripeConfig struct {
	BaseURL string `yaml:"base_url"` // default https://api.stripe.com
	// APIVersion overrides the pinned Stripe-Version (default StripeAPIVersion).
	APIVersion string `yaml:"api_version"`
	// PortalConfiguration is the bpc_... Customer Portal configuration to
	// use (default: the account's default configuration).
	PortalConfiguration string `yaml:"portal_configuration"`
	TrialDays           int    `yaml:"trial_days"`
	// TrialWithoutCard lets a trial start without a payment method
	// (payment_method_collection=if_required); a trial that ends with no
	// card on file is canceled and the org returns to the default plan.
	TrialWithoutCard    bool `yaml:"trial_without_card"`
	AllowPromotionCodes bool `yaml:"allow_promotion_codes"`
	AutomaticTax        bool `yaml:"automatic_tax"` // requires Stripe Tax to be set up
}

// DefaultCloudPlans is the draft plan catalog (prices are placeholders until
// the Stripe prices exist; set stripe_price_id per plan).
func DefaultCloudPlans() []CloudPlan {
	return []CloudPlan{
		{ID: "free", Name: "Free", Interval: "month", MaxRequestsMonth: 10_000, MaxMembers: 3, MaxProjects: 2, LogRetentionDays: 7,
			Features: []string{"10k requests / month", "3 team members", "2 projects", "7-day request logs"}},
		{ID: "team", Name: "Team", PriceUSD: 49, Interval: "month", MaxRequestsMonth: 1_000_000, MaxMembers: 25, LogRetentionDays: 30,
			Features: []string{"1M requests / month", "25 team members", "Unlimited projects", "30-day request logs", "Email support"}},
		{ID: "enterprise", Name: "Enterprise", Interval: "month", ContactSales: true, LogRetentionDays: 365,
			Features: []string{"Unlimited requests", "SSO & custom contracts", "1-year request logs", "Priority support"}},
	}
}

// Plan returns the plan with the given id.
func (c *CloudConfig) Plan(id string) (CloudPlan, bool) {
	for _, p := range c.Plans {
		if p.ID == id {
			return p, true
		}
	}
	return CloudPlan{}, false
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
	// Window is the fixed reporting bucket (default 1h). Usage is reported
	// per closed, epoch-aligned window with an identifier derived from the
	// window, so retries and concurrent replicas can't double-report.
	Window time.Duration `yaml:"window"`
	// APIVersion pins the Stripe-Version header (default StripeAPIVersion).
	APIVersion string `yaml:"api_version"`
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
	// ShareAcrossKeys lets keys of the same project share entries (default
	// false: the virtual key is part of the cache key).
	ShareAcrossKeys bool `yaml:"share_across_keys"`
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
	// OrgDailyUSD / OrgMonthlyUSD are optional org-wide spend caps applied
	// to every organization that has not set its own cap through the admin
	// API (0 = none). Over-cap requests get 429 budget_exceeded.
	OrgDailyUSD   float64 `yaml:"org_daily_usd"`
	OrgMonthlyUSD float64 `yaml:"org_monthly_usd"`
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
	set(&c.StripeWebhookSecret, "STRIPE_WEBHOOK_SECRET")
	set(&c.Cloud.PublicURL, "OPENGATE_PUBLIC_URL")
	set(&c.Plan, "OPENGATE_PLAN")
	if v := os.Getenv("OPENGATE_BILLING_DISABLED"); v == "1" || strings.EqualFold(v, "true") {
		c.Cloud.BillingDisabled = true
	}
	if v := os.Getenv("OPENGATE_STRIPE_ALLOW_LIVE"); v == "1" || strings.EqualFold(v, "true") {
		c.StripeAllowLive = true
	}
	if v := os.Getenv("OPENGATE_ENV"); strings.EqualFold(v, "production") || strings.EqualFold(v, "prod") {
		c.Production = true
	}
	if v := os.Getenv("OPENGATE_PRODUCTION"); v == "1" || strings.EqualFold(v, "true") {
		c.Production = true
	}
	if v := os.Getenv("OPENGATE_CORS_ORIGINS"); v != "" {
		c.CORSOrigins = nil
		for _, o := range strings.Split(v, ",") {
			if o = strings.TrimSpace(o); o != "" {
				c.CORSOrigins = append(c.CORSOrigins, o)
			}
		}
	}
	if v := os.Getenv("OPENGATE_ALLOW_INSECURE_UPSTREAM"); v == "1" || strings.EqualFold(v, "true") {
		c.AllowInsecureUpstream = true
	}
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
	if c.MCP.Timeout <= 0 {
		c.MCP.Timeout = 60 * time.Second
	}
	if c.MCP.StreamTimeout <= 0 {
		c.MCP.StreamTimeout = 10 * time.Minute
	}
	if c.MCP.ArgsLogMaxBytes <= 0 {
		c.MCP.ArgsLogMaxBytes = 4096
	}
	switch strings.ToLower(c.Plan) {
	case "", "unlocked", "free", "team", "enterprise":
		c.Plan = strings.ToLower(c.Plan)
	default:
		return fmt.Errorf("plan must be empty, free, team or enterprise (got %q)", c.Plan)
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
	if c.Budgets.OrgDailyUSD < 0 || c.Budgets.OrgMonthlyUSD < 0 {
		return fmt.Errorf("budgets.org_daily_usd and budgets.org_monthly_usd must not be negative")
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
		if err := c.checkURL(p.BaseURL); err != nil {
			return fmt.Errorf("provider %q: base_url: %w", name, err)
		}
		if p.Timeout <= 0 {
			p.Timeout = 120 * time.Second
		}
		c.Providers[name] = p
	}
	if c.Alerts.WebhookURL != "" {
		if err := c.checkURL(c.Alerts.WebhookURL); err != nil {
			return fmt.Errorf("alerts.webhook_url: %w", err)
		}
	}
	for _, cidr := range c.TrustedProxies {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			return fmt.Errorf("trusted_proxies: %w", err)
		}
	}
	if c.MaxInflight < 0 {
		return errors.New("max_inflight must be >= 0")
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
	if err := c.checkURL(st.BaseURL); err != nil {
		return fmt.Errorf("billing.stripe.base_url: %w", err)
	}
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
	if st.Window <= 0 {
		st.Window = time.Hour
	}
	if st.Window < time.Second || (24*time.Hour)%st.Window != 0 {
		return fmt.Errorf("billing.stripe.window: must be at least 1s and divide 24h evenly, got %s", st.Window)
	}
	if st.APIVersion == "" {
		st.APIVersion = StripeAPIVersion
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
	if err := c.normalizeCloud(); err != nil {
		return err
	}
	if c.Cache.MaxTTL <= 0 {
		c.Cache.MaxTTL = 24 * time.Hour
	}
	if c.Cache.MaxEntryBytes <= 0 {
		c.Cache.MaxEntryBytes = 1 << 20
	}
	return nil
}

// ValidateStripe checks that Stripe sync / cloud billing have their secrets.
func (c *Config) ValidateStripe() error {
	if (strings.HasPrefix(c.StripeAPIKey, "sk_live_") || strings.HasPrefix(c.StripeAPIKey, "rk_live_")) && !c.StripeAllowLive {
		return errors.New("STRIPE_API_KEY is a live-mode key; OpenGate only accepts test-mode keys (sk_test_/rk_test_) unless OPENGATE_STRIPE_ALLOW_LIVE=true is set")
	}
	if c.Billing.Stripe.Enabled && c.StripeAPIKey == "" {
		return errors.New("billing.stripe.enabled requires STRIPE_API_KEY in the environment")
	}
	if c.Cloud.Enabled && !c.Cloud.BillingDisabled {
		if c.StripeAPIKey == "" {
			return errors.New("cloud.enabled requires STRIPE_API_KEY in the environment")
		}
		if !strings.HasPrefix(c.StripeWebhookSecret, "whsec_") {
			return errors.New("cloud.enabled requires STRIPE_WEBHOOK_SECRET (whsec_...) in the environment")
		}
	}
	return nil
}

func (c *Config) normalizeCloud() error {
	cc := &c.Cloud
	if len(cc.Plans) == 0 {
		cc.Plans = DefaultCloudPlans()
	}
	if cc.DefaultPlan == "" {
		cc.DefaultPlan = "free"
	}
	if cc.MaxOrgsPerUser <= 0 {
		cc.MaxOrgsPerUser = 3
	}
	if cc.GracePeriod <= 0 {
		cc.GracePeriod = 7 * 24 * time.Hour
	}
	if cc.Stripe.BaseURL == "" {
		cc.Stripe.BaseURL = "https://api.stripe.com"
	}
	if cc.Stripe.APIVersion == "" {
		cc.Stripe.APIVersion = StripeAPIVersion
	}
	if p := cc.Stripe.PortalConfiguration; p != "" && !strings.HasPrefix(p, "bpc_") {
		return fmt.Errorf("cloud.stripe.portal_configuration: want bpc_..., got %q", p)
	}
	cc.Stripe.BaseURL = strings.TrimRight(cc.Stripe.BaseURL, "/")
	if !cc.Enabled {
		return nil
	}
	seen, prices := map[string]bool{}, map[string]string{}
	for _, p := range cc.Plans {
		if p.ID == "" || p.Name == "" {
			return errors.New("cloud.plans: every plan needs an id and a name")
		}
		if seen[p.ID] {
			return fmt.Errorf("cloud.plans: duplicate plan id %q", p.ID)
		}
		seen[p.ID] = true
		if p.StripePriceID != "" {
			if other, dup := prices[p.StripePriceID]; dup {
				return fmt.Errorf("cloud.plans: %q and %q share stripe_price_id %q", other, p.ID, p.StripePriceID)
			}
			prices[p.StripePriceID] = p.ID
		}
		if p.MaxRequestsMonth < 0 || p.MaxMembers < 0 || p.MaxProjects < 0 || p.LogRetentionDays < 0 {
			return fmt.Errorf("cloud.plans: %q has a negative limit", p.ID)
		}
	}
	def, ok := cc.Plan(cc.DefaultPlan)
	if !ok {
		return fmt.Errorf("cloud.default_plan %q is not in cloud.plans", cc.DefaultPlan)
	}
	if def.StripePriceID != "" {
		return fmt.Errorf("cloud.default_plan %q must be free (no stripe_price_id)", cc.DefaultPlan)
	}
	if cc.PublicURL == "" {
		return errors.New("cloud.public_url (or OPENGATE_PUBLIC_URL) is required in cloud mode")
	}
	cc.PublicURL = strings.TrimRight(cc.PublicURL, "/")
	if err := c.checkURL(cc.PublicURL); err != nil {
		return fmt.Errorf("cloud.public_url: %w", err)
	}
	if err := c.checkURL(cc.Stripe.BaseURL); err != nil {
		return fmt.Errorf("cloud.stripe.base_url: %w", err)
	}
	if cc.SalesURL != "" && !strings.HasPrefix(cc.SalesURL, "mailto:") && !strings.HasPrefix(cc.SalesURL, "https://") {
		return errors.New("cloud.sales_url must be a mailto: or https:// URL")
	}
	// Tenant isolation: cloud tenants must never spend the operator's
	// provider keys or report usage into the operator's Stripe account.
	if c.ShareEnvProviderKeys {
		return errors.New("cloud.enabled cannot be combined with share_env_provider_keys (tenants would spend the operator's provider keys)")
	}
	if c.Billing.Stripe.Enabled {
		return errors.New("cloud.enabled cannot be combined with billing.stripe (tenant usage would be metered into the operator's Stripe account)")
	}
	c.Auth.AllowSignup = true
	if strings.HasPrefix(cc.PublicURL, "https://") {
		c.Auth.SecureCookies = true
	}
	return nil
}

// checkURL requires https, except for loopback hosts (local mocks, sidecars)
// or when allow_insecure_upstream is set.
func (c *Config) checkURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("%q is not an absolute URL", raw)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		h := u.Hostname()
		if ip := net.ParseIP(h); h == "localhost" || (ip != nil && ip.IsLoopback()) || c.AllowInsecureUpstream {
			return nil
		}
		return fmt.Errorf("%q uses plain http to a non-loopback host; use https or set allow_insecure_upstream: true", raw)
	}
	return fmt.Errorf("%q: unsupported scheme %q", raw, u.Scheme)
}

// TruthyEnv reports common on-values (1, true, yes, on).
func TruthyEnv(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// RefuseDemoFlags fails closed in production when demo-only switches are set.
// OPENGATE_DEMO_MOCK_LISTEN, OPENGATE_DEMO_TRAFFIC, and OPENGATE_PUBLIC_PLAYGROUND
// must not activate on a production config.
func (c *Config) RefuseDemoFlags() error {
	if c == nil || !c.Production {
		return nil
	}
	var bad []string
	if os.Getenv("OPENGATE_DEMO_MOCK_LISTEN") != "" {
		bad = append(bad, "OPENGATE_DEMO_MOCK_LISTEN")
	}
	if TruthyEnv(os.Getenv("OPENGATE_DEMO_TRAFFIC")) {
		bad = append(bad, "OPENGATE_DEMO_TRAFFIC")
	}
	if TruthyEnv(os.Getenv("OPENGATE_PUBLIC_PLAYGROUND")) {
		bad = append(bad, "OPENGATE_PUBLIC_PLAYGROUND")
	}
	if len(bad) > 0 {
		return fmt.Errorf("production mode refuses demo flags: %s", strings.Join(bad, ", "))
	}
	return nil
}

// CheckPlaygroundFlag requires the in-process mock whenever the public
// playground is explicitly enabled. The route stays unregistered otherwise.
func CheckPlaygroundFlag() error {
	if !TruthyEnv(os.Getenv("OPENGATE_PUBLIC_PLAYGROUND")) {
		return nil
	}
	if os.Getenv("OPENGATE_DEMO_MOCK_LISTEN") == "" {
		return errors.New("OPENGATE_PUBLIC_PLAYGROUND requires OPENGATE_DEMO_MOCK_LISTEN; the playground is mock-only")
	}
	return nil
}

// ValidateProductionStorage requires Postgres, or SQLite with an off-site
// Litestream bucket, before a production process serves traffic.
func (c *Config) ValidateProductionStorage() error {
	if c == nil || !c.Production {
		return nil
	}
	dsn := strings.ToLower(strings.TrimSpace(c.Database))
	if strings.Contains(dsn, "demo.db") {
		return errors.New("production database must be a fresh database, not a demo.db file")
	}
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		return nil
	}
	if os.Getenv("LITESTREAM_BUCKET") != "" || os.Getenv("OPENGATE_SQLITE_NO_REPLICA") == "1" {
		return nil
	}
	return errors.New("production requires Postgres (OPENGATE_DATABASE=postgres://...) or SQLite with LITESTREAM_BUCKET for off-site backups (OPENGATE_SQLITE_NO_REPLICA=1 acknowledges the risk)")
}
