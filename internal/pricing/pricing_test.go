package pricing

import (
	"math"
	"os"
	"path/filepath"
	"testing"
)

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-12 }

func TestCostCalculation(t *testing.T) {
	tb, err := Parse([]byte(`
default: {input: 10, output: 30}
models:
  openai/gpt-4o: {input: 2.5, output: 10}
  anthropic/claude-sonnet-4*: {input: 3, output: 15}
  anthropic/claude-sonnet-4-5-special*: {input: 1, output: 1}
  openrouter/meta-llama/*: {input: 0.2, output: 0.6}
`))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		prov, model string
		in, out     int
		want        float64
		known       bool
	}{
		{"openai", "gpt-4o", 1000, 500, 0.0025 + 0.005, true},
		{"openai", "gpt-4o", 1_000_000, 1_000_000, 12.5, true},
		{"anthropic", "claude-sonnet-4-5-20250929", 2000, 1000, 0.006 + 0.015, true},
		{"anthropic", "claude-sonnet-4-5-special-x", 1_000_000, 0, 1, true}, // longest prefix wins
		{"openrouter", "meta-llama/llama-3.3-70b", 1_000_000, 1_000_000, 0.8, true},
		{"openai", "brand-new-model", 1000, 1000, 0.01 + 0.03, false}, // default rate
		{"openai", "gpt-4o", 0, 0, 0, true},
	}
	for _, c := range cases {
		got, known := tb.Cost(c.prov, c.model, c.in, c.out)
		if !approx(got, c.want) || known != c.known {
			t.Errorf("%s/%s: got %v known=%v, want %v known=%v", c.prov, c.model, got, known, c.want, c.known)
		}
	}
}

func TestNoDefaultIsFree(t *testing.T) {
	tb, _ := Parse([]byte("models: {}"))
	if c, known := tb.Cost("x", "y", 100, 100); c != 0 || known {
		t.Fatal("expected zero, unknown")
	}
}

func TestRejectNegative(t *testing.T) {
	if _, err := Parse([]byte("models:\n  a/b: {input: -1, output: 1}\n")); err == nil {
		t.Fatal("negative price must be rejected")
	}
}

func TestEmbeddedDefaultParsesAndReload(t *testing.T) {
	r, err := NewRegistry("")
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := r.Table().Lookup("openai", "gpt-4o-mini"); !ok || p.Input != 0.15 {
		t.Fatalf("embedded table lookup: %+v %v", p, ok)
	}
	path := filepath.Join(t.TempDir(), "p.yaml")
	os.WriteFile(path, []byte("models:\n  mock/m: {input: 1, output: 2}\n"), 0o600)
	r2, err := NewRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(path, []byte("models:\n  mock/m: {input: 5, output: 2}\n"), 0o600)
	if err := r2.Reload(); err != nil {
		t.Fatal(err)
	}
	if p, _ := r2.Table().Lookup("mock", "m"); p.Input != 5 {
		t.Fatal("reload did not apply")
	}
}
