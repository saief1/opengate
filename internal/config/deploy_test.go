package config

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The shipped deploy templates must stay valid as the config evolves.
func TestDeployTemplates(t *testing.T) {
	t.Setenv("OPENGATE_PUBLIC_URL", "https://cloud.example.com")
	t.Setenv("STRIPE_PRICE_TEAM", "price_123")
	t.Setenv("STRIPE_API_KEY", "sk_test_x")
	t.Setenv("STRIPE_WEBHOOK_SECRET", "whsec_x")
	for _, f := range []string{"../../deploy/cloud.yaml", "../../examples/cloud-demo.yaml"} {
		c, err := Load(f)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if !c.Cloud.Enabled || c.ValidateStripe() != nil {
			t.Fatalf("%s: cloud not enabled/valid", f)
		}
		team, ok := c.Cloud.Plan("team")
		if !ok || !team.SelfServe() {
			t.Fatalf("%s: team plan not purchasable: %+v", f, team)
		}
	}
	if c, _ := Load("../../deploy/cloud.yaml"); c.Cloud.PublicURL != "https://cloud.example.com" || !c.Budgets.Strict || len(c.TrustedProxies) == 0 {
		t.Fatalf("cloud.yaml settings: %+v", c.Cloud)
	}
	for _, f := range []string{"../../deploy/k8s/opengate.yaml", "../../deploy/render.yaml", "../../deploy/docker-compose.cloud.yml"} {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		dec, docs := yaml.NewDecoder(bytes.NewReader(data)), 0
		for {
			var v any
			if err := dec.Decode(&v); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				t.Fatalf("%s: %v", f, err)
			}
			docs++
		}
		if docs == 0 {
			t.Fatalf("%s: empty", f)
		}
	}
}

func TestFlyProdConfig(t *testing.T) {
	b, err := os.ReadFile("../../deploy/fly-prod/fly.toml")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{
		"app = \"opengate-prod\"",
		"min_machines_running = 1",
		"auto_stop_machines = \"off\"",
		"path = \"/readyz\"",
		"path = \"/livez\"",
		"OPENGATE_ENV = \"production\"",
		"OPENGATE_PUBLIC_URL = \"https://opengatehq.com\"",
		"dockerfile = \"../../Dockerfile.prod\"",
		"local_path = \"deploy/cloud.yaml\"",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("fly-prod missing %s", want)
		}
	}
	// Names may appear in comments that forbid them. Assignments must not.
	for _, bad := range []string{
		"OPENGATE_DEMO_MOCK_LISTEN =",
		"OPENGATE_DEMO_TRAFFIC =",
		"OPENGATE_PUBLIC_PLAYGROUND =",
		"127.0.0.1:9090",
		"source = \"opengate_data\"",
	} {
		if strings.Contains(s, bad) {
			t.Errorf("fly-prod must not set %s", bad)
		}
	}
	entry, err := os.ReadFile("../../deploy/fly-prod/entrypoint.sh")
	if err != nil {
		t.Fatal(err)
	}
	es := string(entry)
	for _, want := range []string{"postgres://", "litestream restore -if-replica-exists -if-db-not-exists", "litestream replicate", "LITESTREAM_BUCKET"} {
		if !strings.Contains(es, want) {
			t.Errorf("entrypoint missing %s", want)
		}
	}
	ls, err := os.ReadFile("../../deploy/fly-prod/litestream.yml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(ls), "${LITESTREAM_REPLICA_URL}") || !strings.Contains(string(ls), "${OPENGATE_DATABASE}") {
		t.Fatalf("litestream.yml: %s", ls)
	}
}
