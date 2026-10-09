package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadMergesDefaultsAndEnv(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.yaml")
	t.Setenv("MOCK_URL", "http://127.0.0.1:9999")
	t.Setenv("OPENGATE_LISTEN", "0.0.0.0:9000")
	os.WriteFile(p, []byte(`
database: /tmp/x.db
providers:
  openai:
    base_url: ${MOCK_URL}/v1/
    timeout: 5s
  local:
    type: openai
    base_url: http://localhost:11434/v1
routes:
  smart: [anthropic/claude-sonnet-4-5, openai/gpt-4o]
retry:
  max_attempts: 3
`), 0o600)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != "0.0.0.0:9000" || c.Database != "/tmp/x.db" {
		t.Fatalf("listen/db: %+v", c)
	}
	if got := c.Providers["openai"]; got.BaseURL != "http://127.0.0.1:9999/v1" || got.Type != TypeOpenAI || got.Timeout != 5*time.Second || got.APIKeyEnv != "OPENAI_API_KEY" {
		t.Fatalf("openai merge: %+v", got)
	}
	if c.Providers["anthropic"].Type != TypeAnthropic || c.Providers["local"].Timeout != 120*time.Second {
		t.Fatal("defaults not kept")
	}
	if c.Retry.MaxAttempts != 3 || c.Retry.InitialBackoff != 250*time.Millisecond {
		t.Fatalf("retry: %+v", c.Retry)
	}
}

func TestLoadRejectsBadConfig(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"unknown field": "listne: x\n",
		"bad route":     "routes:\n  a: [nosuchprovider/x]\n",
		"bad type":      "providers:\n  x:\n    type: foo\n    base_url: http://a\n",
	} {
		p := filepath.Join(dir, "c.yaml")
		os.WriteFile(p, []byte(body), 0o600)
		if _, err := Load(p); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestValidateSecrets(t *testing.T) {
	c := Default()
	if c.ValidateSecrets() == nil {
		t.Fatal("empty secrets must fail")
	}
	c.AdminToken = "0123456789abcdef"
	c.MasterKey = "x"
	if err := c.ValidateSecrets(); err != nil {
		t.Fatal(err)
	}
}

func TestExampleConfigsLoad(t *testing.T) {
	for _, f := range []string{"../../examples/opengate.example.yaml", "../../examples/demo.yaml"} {
		c, err := Load(f)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if c.Guardrails.PII != "off" || c.Billing.Stripe.Unit != "cents" || c.SmartRouting.Auto.Alias != "auto" {
			t.Fatalf("%s: %+v %+v", f, c.Guardrails, c.Billing.Stripe)
		}
	}
}

func TestUpstreamURLMustBeHTTPS(t *testing.T) {
	for _, c := range []struct {
		url      string
		insecure bool
		ok       bool
	}{
		{"https://api.example.com/v1", false, true},
		{"http://127.0.0.1:9090/v1", false, true},
		{"http://localhost:9090", false, true},
		{"http://[::1]:9090", false, true},
		{"http://mock:9090/v1", false, false},
		{"http://mock:9090/v1", true, true},
		{"ftp://x", true, false},
		{"not a url", false, false},
	} {
		cfg := &Config{AllowInsecureUpstream: c.insecure}
		if err := cfg.checkURL(c.url); (err == nil) != c.ok {
			t.Errorf("%s (insecure=%v): err=%v", c.url, c.insecure, err)
		}
	}
}

func TestCloudConfigValidation(t *testing.T) {
	base := func() *Config {
		c := Default()
		c.Cloud.Enabled = true
		c.Cloud.PublicURL = "https://cloud.example.com/"
		return c
	}
	c := base()
	if err := c.Normalize(); err != nil {
		t.Fatal(err)
	}
	if !c.Auth.AllowSignup || !c.Auth.SecureCookies || c.Cloud.PublicURL != "https://cloud.example.com" || len(c.Cloud.Plans) != 3 {
		t.Fatalf("cloud defaults: %+v", c.Cloud)
	}
	if err := c.ValidateStripe(); err == nil {
		t.Fatal("missing Stripe secrets accepted")
	}
	c.StripeAPIKey, c.StripeWebhookSecret = "sk_test_x", "not-a-whsec"
	if err := c.ValidateStripe(); err == nil {
		t.Fatal("bad webhook secret accepted")
	}
	c.StripeWebhookSecret = "whsec_x"
	if err := c.ValidateStripe(); err != nil {
		t.Fatal(err)
	}
	bad := map[string]func(*Config){
		"no public url":        func(c *Config) { c.Cloud.PublicURL = "" },
		"http public url":      func(c *Config) { c.Cloud.PublicURL = "http://cloud.example.com" },
		"shared env keys":      func(c *Config) { c.ShareEnvProviderKeys = true },
		"operator meter sync":  func(c *Config) { c.Billing.Stripe.Enabled = true },
		"unknown default plan": func(c *Config) { c.Cloud.DefaultPlan = "gold" },
		"paid default plan":    func(c *Config) { c.Cloud.Plans = []CloudPlan{{ID: "free", Name: "F", StripePriceID: "price_1"}} },
		"duplicate plan":       func(c *Config) { c.Cloud.Plans = []CloudPlan{{ID: "free", Name: "F"}, {ID: "free", Name: "G"}} },
		"shared price": func(c *Config) {
			c.Cloud.Plans = []CloudPlan{{ID: "free", Name: "F"}, {ID: "a", Name: "A", StripePriceID: "p"}, {ID: "b", Name: "B", StripePriceID: "p"}}
		},
		"negative limit":      func(c *Config) { c.Cloud.Plans = []CloudPlan{{ID: "free", Name: "F", MaxMembers: -1}} },
		"insecure stripe url": func(c *Config) { c.Cloud.Stripe.BaseURL = "http://stripe.evil.example" },
	}
	for name, mut := range bad {
		c := base()
		mut(c)
		if err := c.Normalize(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Not enabled: nothing is enforced.
	off := Default()
	off.ShareEnvProviderKeys = true
	if err := off.Normalize(); err != nil {
		t.Fatal(err)
	}
}
