// Package metrics is a tiny, dependency-free Prometheus text-format
// exporter (counters, gauges and histograms with labels). OpenGate only
// needs a handful of series, so pulling in client_golang (and its
// transitive dependencies) is not worth the supply-chain cost.
package metrics

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Registry holds metric families in registration order.
type Registry struct {
	mu       sync.Mutex
	families []*family
}

type kind string

const (
	counterKind   kind = "counter"
	gaugeKind     kind = "gauge"
	histogramKind kind = "histogram"
)

type family struct {
	name, help string
	kind       kind
	labels     []string
	buckets    []float64
	mu         sync.Mutex
	series     map[string]*series
	gaugeFunc  func() float64
}

type series struct {
	labelValues []string
	value       float64
	counts      []uint64 // histogram buckets (non-cumulative)
	sum         float64
	count       uint64
}

// New returns an empty registry.
func New() *Registry { return &Registry{} }

func (r *Registry) add(f *family) *family {
	f.series = map[string]*series{}
	r.mu.Lock()
	r.families = append(r.families, f)
	r.mu.Unlock()
	return f
}

// Counter is a monotonically increasing labelled value.
type Counter struct{ f *family }

// Gauge is a labelled value that can go up and down.
type Gauge struct{ f *family }

// Histogram observes value distributions.
type Histogram struct{ f *family }

// NewCounter registers a counter.
func (r *Registry) NewCounter(name, help string, labels ...string) *Counter {
	return &Counter{r.add(&family{name: name, help: help, kind: counterKind, labels: labels})}
}

// NewGauge registers a gauge.
func (r *Registry) NewGauge(name, help string, labels ...string) *Gauge {
	return &Gauge{r.add(&family{name: name, help: help, kind: gaugeKind, labels: labels})}
}

// NewGaugeFunc registers an unlabelled gauge computed at scrape time.
func (r *Registry) NewGaugeFunc(name, help string, fn func() float64) {
	r.add(&family{name: name, help: help, kind: gaugeKind, gaugeFunc: fn})
}

// NewHistogram registers a histogram with upper bucket bounds.
func (r *Registry) NewHistogram(name, help string, buckets []float64, labels ...string) *Histogram {
	b := append([]float64(nil), buckets...)
	sort.Float64s(b)
	return &Histogram{r.add(&family{name: name, help: help, kind: histogramKind, labels: labels, buckets: b})}
}

func (f *family) get(values []string) *series {
	if len(values) != len(f.labels) {
		panic(fmt.Sprintf("metrics: %s wants %d labels, got %d", f.name, len(f.labels), len(values)))
	}
	k := strings.Join(values, "\xff")
	s, ok := f.series[k]
	if !ok {
		s = &series{labelValues: append([]string(nil), values...)}
		if f.kind == histogramKind {
			s.counts = make([]uint64, len(f.buckets))
		}
		f.series[k] = s
	}
	return s
}

// Add increases the counter.
func (c *Counter) Add(v float64, labels ...string) {
	if v < 0 || math.IsNaN(v) {
		return
	}
	c.f.mu.Lock()
	c.f.get(labels).value += v
	c.f.mu.Unlock()
}

// Inc adds one.
func (c *Counter) Inc(labels ...string) { c.Add(1, labels...) }

// Value returns the current value (tests).
func (c *Counter) Value(labels ...string) float64 {
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	return c.f.get(labels).value
}

// Add changes the gauge.
func (g *Gauge) Add(v float64, labels ...string) {
	g.f.mu.Lock()
	g.f.get(labels).value += v
	g.f.mu.Unlock()
}

// Set sets the gauge.
func (g *Gauge) Set(v float64, labels ...string) {
	g.f.mu.Lock()
	g.f.get(labels).value = v
	g.f.mu.Unlock()
}

// Observe records one value.
func (h *Histogram) Observe(v float64, labels ...string) {
	h.f.mu.Lock()
	s := h.f.get(labels)
	for i, b := range h.f.buckets {
		if v <= b {
			s.counts[i]++
			break
		}
	}
	s.sum += v
	s.count++
	h.f.mu.Unlock()
}

func escape(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, "\n", `\n`)
	return strings.ReplaceAll(v, `"`, `\"`)
}

func labelStr(names, values []string, extra ...string) string {
	var parts []string
	for i, n := range names {
		parts = append(parts, n+`="`+escape(values[i])+`"`)
	}
	for i := 0; i+1 < len(extra); i += 2 {
		parts = append(parts, extra[i]+`="`+extra[i+1]+`"`)
	}
	if len(parts) == 0 {
		return ""
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func num(v float64) string {
	if math.IsInf(v, 1) {
		return "+Inf"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// WriteText writes the Prometheus text exposition format (version 0.0.4).
func (r *Registry) WriteText(w io.Writer) error {
	r.mu.Lock()
	fams := append([]*family(nil), r.families...)
	r.mu.Unlock()
	var b strings.Builder
	for _, f := range fams {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", f.name, f.help, f.name, f.kind)
		if f.gaugeFunc != nil {
			fmt.Fprintf(&b, "%s %s\n", f.name, num(f.gaugeFunc()))
			continue
		}
		f.mu.Lock()
		keys := make([]string, 0, len(f.series))
		for k := range f.series {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			s := f.series[k]
			if f.kind != histogramKind {
				fmt.Fprintf(&b, "%s%s %s\n", f.name, labelStr(f.labels, s.labelValues), num(s.value))
				continue
			}
			var cum uint64
			for i, ub := range f.buckets {
				cum += s.counts[i]
				fmt.Fprintf(&b, "%s_bucket%s %d\n", f.name, labelStr(f.labels, s.labelValues, "le", num(ub)), cum)
			}
			fmt.Fprintf(&b, "%s_bucket%s %d\n", f.name, labelStr(f.labels, s.labelValues, "le", "+Inf"), s.count)
			fmt.Fprintf(&b, "%s_sum%s %s\n", f.name, labelStr(f.labels, s.labelValues), num(s.sum))
			fmt.Fprintf(&b, "%s_count%s %d\n", f.name, labelStr(f.labels, s.labelValues), s.count)
		}
		f.mu.Unlock()
	}
	_, err := io.WriteString(w, b.String())
	return err
}
