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
