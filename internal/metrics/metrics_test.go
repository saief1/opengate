package metrics

import (
	"strings"
	"testing"
)

func TestExposition(t *testing.T) {
	r := New()
	c := r.NewCounter("og_requests_total", "Requests.", "provider", "status")
	h := r.NewHistogram("og_latency_seconds", "Latency.", []float64{0.1, 1}, "provider")
	r.NewGaugeFunc("og_up", "Up.", func() float64 { return 1 })
	c.Inc("openai", "200")
	c.Add(2, "openai", "200")
	c.Inc(`we"ird`, "500")
	h.Observe(0.05, "openai")
	h.Observe(0.5, "openai")
	h.Observe(5, "openai")
	var b strings.Builder
	r.WriteText(&b)
	out := b.String()
	for _, want := range []string{
		"# TYPE og_requests_total counter",
		`og_requests_total{provider="openai",status="200"} 3`,
		`og_requests_total{provider="we\"ird",status="500"} 1`,
		`og_latency_seconds_bucket{provider="openai",le="0.1"} 1`,
		`og_latency_seconds_bucket{provider="openai",le="1"} 2`,
		`og_latency_seconds_bucket{provider="openai",le="+Inf"} 3`,
		`og_latency_seconds_count{provider="openai"} 3`,
		"og_up 1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
}
