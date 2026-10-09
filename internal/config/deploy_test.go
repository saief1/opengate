package config

import (
	"bytes"
	"errors"
	"io"
	"os"
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
