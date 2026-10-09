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
type Price struct {
	Input  float64 `yaml:"input" json:"input"`
	Output float64 `yaml:"output" json:"output"`
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

// Parse parses a YAML price table.
func Parse(data []byte) (*Table, error) {
	var f file
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, err
	}
	t := &Table{exact: map[string]Price{}, def: f.Default}
	for k, p := range f.Models {
		if p.Input < 0 || p.Output < 0 {
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

// Calc applies a price to token counts.
func Calc(p Price, promptTokens, completionTokens int) float64 {
	return (float64(promptTokens)*p.Input + float64(completionTokens)*p.Output) / 1_000_000
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
