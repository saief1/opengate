package pricing

import (
	"math"
	"os"
	"path/filepath"
	"strings"
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

func TestCacheAndLongContextPricing(t *testing.T) {
	tb, err := Parse([]byte(`
models:
  anthropic/claude-sonnet-4-5*: {input: 3, output: 15, cache_read: 0.30, cache_write: 3.75}
  anthropic/claude-haiku-5-5*:
    input: 0.10
    output: 0.50
    cache_read: 0.01
    cache_write: 0.125
    long_context: {threshold: 100000, input: 0.50, output: 2.50, cache_read: 0.05, cache_write: 0.625}
  openai/gpt-4o: {input: 2.5, output: 10, cache_read: 1.25}
`))
	if err != nil {
		t.Fatal(err)
	}
	// Anthropic: 1000 uncached + 9000 cache reads + 2000 cache writes (prompt total 12000), 500 out.
	got, _ := tb.CostTokens("anthropic", "claude-sonnet-4-5-20250929", Tokens{Prompt: 12000, Completion: 500, CacheRead: 9000, CacheWrite: 2000})
	want := (1000*3 + 9000*0.30 + 2000*3.75 + 500*15) / 1e6
	if !approx(got, want) {
		t.Fatalf("anthropic cache cost %v want %v", got, want)
	}
	// OpenAI: cached tokens are part of prompt_tokens; no write charge configured => input rate.
	got, _ = tb.CostTokens("openai", "gpt-4o", Tokens{Prompt: 2000, Completion: 0, CacheRead: 1024})
	if want := (976*2.5 + 1024*1.25) / 1e6; !approx(got, want) {
		t.Fatalf("openai cached %v want %v", got, want)
	}
	// Long-context switch above threshold.
	small, _ := tb.CostTokens("anthropic", "claude-haiku-5-5", Tokens{Prompt: 100000, Completion: 1000})
	big, _ := tb.CostTokens("anthropic", "claude-haiku-5-5", Tokens{Prompt: 100001, Completion: 1000})
	if !approx(small, (100000*0.10+1000*0.50)/1e6) || !approx(big, (100001*0.50+1000*2.50)/1e6) {
		t.Fatalf("long context: %v %v", small, big)
	}
	// Malformed: cache tokens larger than prompt never go negative.
	if c, _ := tb.CostTokens("openai", "gpt-4o", Tokens{Prompt: 10, CacheRead: 50}); c < 0 {
		t.Fatal("negative cost")
	}
}

func TestBundledTableParsesAndCoversMajorModels(t *testing.T) {
	tb, err := Parse(DefaultYAML)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{"openai/gpt-5", "openai/gpt-4o-mini", "openai/text-embedding-3-small", "anthropic/claude-sonnet-4-5-20250929",
		"anthropic/claude-haiku-4-5", "anthropic/claude-opus-4-1-20250805", "gemini/gemini-2.5-flash", "gemini/gemini-3.1-pro-preview"} {
		prov, model, _ := strings.Cut(m, "/")
		if _, known := tb.Lookup(prov, model); !known {
			t.Errorf("bundled table has no price for %s", m)
		}
	}
	p, _ := tb.Lookup("anthropic", "claude-sonnet-4-5-20250929")
	if p.CacheRead == nil || *p.CacheRead != 0.30 || p.CacheWrite == nil || *p.CacheWrite != 3.75 {
		t.Fatalf("sonnet 4.5 cache prices: %+v", p)
	}
	if p, _ := tb.Lookup("anthropic", "claude-opus-4-1-20250805"); p.Input != 15 {
		t.Fatalf("opus 4.1 must keep legacy pricing, got %+v", p)
	}
	if p, _ := tb.Lookup("anthropic", "claude-opus-4-5-20251101"); p.Input != 5 {
		t.Fatalf("opus 4.5 is $5 input, got %+v", p)
	}
}
