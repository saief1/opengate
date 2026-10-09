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
	Listen           string                    `yaml:"listen"`
	Database         string                    `yaml:"database"`
	PricingFile      string                    `yaml:"pricing_file"`
	MaxRequestBytes  int64                     `yaml:"max_request_bytes"`
	BodyLogMaxBytes  int                       `yaml:"body_log_max_bytes"`
	DefaultMaxTokens int                       `yaml:"default_max_tokens"`
	DisableDashboard bool                      `yaml:"disable_dashboard"`
	Providers        map[string]ProviderConfig `yaml:"providers"`
	Routes           map[string][]string       `yaml:"routes"`
	Retry            RetryConfig               `yaml:"retry"`
	Alerts           AlertsConfig              `yaml:"alerts"`

	// Secrets are only ever read from the environment, never from YAML.
	AdminToken string `yaml:"-"`
	MasterKey  string `yaml:"-"`
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
