// Package pricing turns token usage into USD cost using an editable YAML table.
package pricing

import (
	_ "embed"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync/atomic"

	"gopkg.in/yaml.v3"
)

//go:embed default_pricing.yaml
var DefaultYAML []byte

// Price is USD per 1M tokens.
//
// CacheRead is the rate for prompt tokens served from the provider's prompt
// cache (OpenAI "cached input", Anthropic "cache hits", Gemini "context
// caching"); CacheWrite is the rate for tokens written to the cache
// (Anthropic 5-minute cache writes, OpenAI cache writes on newer models).
// Both default to Input when omitted. LongContext optionally switches to a
// second rate card when the prompt exceeds Threshold tokens.
type Price struct {
	Input       float64    `yaml:"input" json:"input"`
	Output      float64    `yaml:"output" json:"output"`
	CacheRead   *float64   `yaml:"cache_read,omitempty" json:"cache_read,omitempty"`
	CacheWrite  *float64   `yaml:"cache_write,omitempty" json:"cache_write,omitempty"`
	LongContext *LongPrice `yaml:"long_context,omitempty" json:"long_context,omitempty"`
}

// LongPrice is the alternative rate card for prompts above Threshold tokens.
type LongPrice struct {
	Threshold  int      `yaml:"threshold" json:"threshold"`
	Input      float64  `yaml:"input" json:"input"`
	Output     float64  `yaml:"output" json:"output"`
	CacheRead  *float64 `yaml:"cache_read,omitempty" json:"cache_read,omitempty"`
	CacheWrite *float64 `yaml:"cache_write,omitempty" json:"cache_write,omitempty"`
}

// Tokens is the usage of one request. Prompt is the total number of input
// tokens INCLUDING CacheRead and CacheWrite tokens (OpenAI convention).
type Tokens struct {
	Prompt     int
	Completion int
	CacheRead  int
	CacheWrite int
}

// rates resolves the effective per-1M rates for a request size.
func (p Price) rates(promptTokens int) (in, out, cr, cw float64) {
	in, out = p.Input, p.Output
	crp, cwp := p.CacheRead, p.CacheWrite
	if lc := p.LongContext; lc != nil && lc.Threshold > 0 && promptTokens > lc.Threshold {
		in, out, crp, cwp = lc.Input, lc.Output, lc.CacheRead, lc.CacheWrite
	}
	cr, cw = in, in
	if crp != nil {
		cr = *crp
	}
	if cwp != nil {
		cw = *cwp
	}
	return
}

type file struct {
	Default *Price           `yaml:"default"`
	Models  map[string]Price `yaml:"models"`
}

type prefixEntry struct {
	prefix string
	price  Price
}

// Table is an immutable, parsed price table.
type Table struct {
	exact    map[string]Price
	prefixes []prefixEntry // sorted longest first
	def      *Price
}

// HasDefault reports whether the table has a fallback price for unknown
// models (without one, unknown models cost $0 unless budgets.strict).
func (t *Table) HasDefault() bool { return t != nil && t.def != nil }

// Parse parses a YAML price table.
func Parse(data []byte) (*Table, error) {
	var f file
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, err
	}
	t := &Table{exact: map[string]Price{}, def: f.Default}
	for k, p := range f.Models {
		neg := p.Input < 0 || p.Output < 0 || (p.CacheRead != nil && *p.CacheRead < 0) || (p.CacheWrite != nil && *p.CacheWrite < 0)
		if lc := p.LongContext; lc != nil {
			neg = neg || lc.Input < 0 || lc.Output < 0 || lc.Threshold < 0
		}
		if neg {
			return nil, fmt.Errorf("price for %q must not be negative", k)
		}
		if strings.HasSuffix(k, "*") {
			t.prefixes = append(t.prefixes, prefixEntry{strings.TrimSuffix(k, "*"), p})
		} else {
			t.exact[k] = p
		}
	}
	sort.Slice(t.prefixes, func(i, j int) bool { return len(t.prefixes[i].prefix) > len(t.prefixes[j].prefix) })
	return t, nil
}

// Lookup returns the price for provider/model. known is false when the
// default rate was used (or no price at all is configured).
func (t *Table) Lookup(provider, model string) (p Price, known bool) {
	key := provider + "/" + model
	if p, ok := t.exact[key]; ok {
		return p, true
	}
	for _, e := range t.prefixes {
		if strings.HasPrefix(key, e.prefix) {
			return e.price, true
		}
	}
	if t.def != nil {
		return *t.def, false
	}
	return Price{}, false
}

// Cost computes the USD cost of a request.
func (t *Table) Cost(provider, model string, promptTokens, completionTokens int) (float64, bool) {
	p, known := t.Lookup(provider, model)
	return Calc(p, promptTokens, completionTokens), known
}

// Calc applies a price to token counts (no prompt caching).
func Calc(p Price, promptTokens, completionTokens int) float64 {
	return CalcTokens(p, Tokens{Prompt: promptTokens, Completion: completionTokens})
}

// CalcTokens applies a price to full usage, charging cache reads and writes
// at their own rates.
func CalcTokens(p Price, t Tokens) float64 {
	in, out, cr, cw := p.rates(t.Prompt)
	uncached := t.Prompt - t.CacheRead - t.CacheWrite
	if uncached < 0 {
		uncached = 0
	}
	return (float64(uncached)*in + float64(t.CacheRead)*cr + float64(t.CacheWrite)*cw + float64(t.Completion)*out) / 1_000_000
}

// CostTokens computes the USD cost of a request with cache-aware usage.
func (t *Table) CostTokens(provider, model string, u Tokens) (float64, bool) {
	p, known := t.Lookup(provider, model)
	return CalcTokens(p, u), known
}

// Models lists all exact model keys (used by /v1/models).
func (t *Table) Models() []string {
	out := make([]string, 0, len(t.exact))
	for k := range t.exact {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Registry holds the current table and supports hot reloads.
type Registry struct {
	path string
	cur  atomic.Pointer[Table]
}

// NewRegistry loads the table from path, or the embedded default if path is "".
func NewRegistry(path string) (*Registry, error) {
	r := &Registry{path: path}
	if err := r.Reload(); err != nil {
		return nil, err
	}
	return r, nil
}

// Reload re-reads the price file.
func (r *Registry) Reload() error {
	data := DefaultYAML
	if r.path != "" {
		b, err := os.ReadFile(r.path)
		if err != nil {
			return fmt.Errorf("read pricing file: %w", err)
		}
		data = b
	}
	t, err := Parse(data)
	if err != nil {
		return fmt.Errorf("parse pricing: %w", err)
	}
	r.cur.Store(t)
	return nil
}

// Table returns the current table.
func (r *Registry) Table() *Table { return r.cur.Load() }

// Source describes where prices come from.
func (r *Registry) Source() string {
	if r.path == "" {
		return "embedded default"
	}
	return r.path
}
