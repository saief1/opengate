package routing

import (
	"net/http"
	"strings"
	"testing"

	"github.com/saief1/opengate/internal/store"
)

func TestEvaluateRules(t *testing.T) {
	rules := []*store.RoutingRule{
		{Name: "long-to-flash", Priority: 10, Enabled: true, Match: store.RouteMatch{MinPromptTokens: 4000}, Target: "gemini/gemini-2.5-flash"},
		{Name: "batch-tag", Priority: 20, Enabled: true, Match: store.RouteMatch{Tags: []string{"batch"}}, Target: "openai/gpt-4.1-mini", Fallbacks: []string{"anthropic/claude-haiku-4-5"}},
		{Name: "ceiling", Priority: 30, Enabled: true, Match: store.RouteMatch{Models: []string{"openai/*"}, EstCostAboveUSD: 0.05}, Target: "openai/gpt-4.1-mini"},
		{Name: "disabled", Priority: 1, Enabled: false, Target: "x/y"},
		{Name: "other-project", Priority: 1, Enabled: true, ProjectID: "prj_other", Target: "x/y"},
		{Name: "project-wins", Priority: 20, Enabled: true, ProjectID: "prj_1", Match: store.RouteMatch{Tags: []string{"batch"}, Agents: []string{"planner"}}, Target: "openai/gpt-4.1-nano"},
	}
	cost := func(m string) float64 {
		if strings.Contains(m, "gpt-4.1-mini") {
			return 0.01
		}
		return 0.10
	}
	base := Request{Model: "openai/gpt-4.1", ProjectID: "prj_1", Headers: http.Header{}, EstCost: cost}

	r := base
	r.PromptTokens = 5000
	if d := Evaluate(rules, r); d == nil || d.Rule.Name != "long-to-flash" {
		t.Fatalf("long prompt: %+v", d)
	}
	r = base
	r.Headers = http.Header{"X-Opengate-Tags": {"Batch, nightly"}}
	if d := Evaluate(rules, r); d == nil || d.Rule.Name != "batch-tag" || d.Fallbacks[0] != "anthropic/claude-haiku-4-5" {
		t.Fatalf("tag: %+v", d)
	}
	r.Agent = "planner"
	if d := Evaluate(rules, r); d == nil || d.Rule.Name != "project-wins" {
		t.Fatalf("project-specific should win ties: %+v", d)
	}
	r = base
	if d := Evaluate(rules, r); d == nil || d.Rule.Name != "ceiling" {
		t.Fatalf("cost ceiling: %+v", d)
	}
	r.Model = "anthropic/claude-sonnet-4-5"
	if d := Evaluate(rules, r); d != nil {
		t.Fatalf("no rule should match: %+v", d)
	}
}

func TestClassifyAndPick(t *testing.T) {
	simple := Classify(Signals{Text: "Translate 'good morning' to French.", Messages: 1, PromptToks: 10})
	if simple.Tier != 1 {
		t.Fatalf("simple: %+v", simple)
	}
	mid := Classify(Signals{Text: "Write a Go func that parses a CSV line:\n```go\nfunc parse(s string) {}\n```", Messages: 1, PromptToks: 40})
	if mid.Tier != 2 {
		t.Fatalf("mid: %+v", mid)
	}
	hard := Classify(Signals{Text: "Analyze the trade-offs and prove the algorithm's complexity step by step.", PromptToks: 3000, HasTools: true})
	if hard.Tier != 3 {
		t.Fatalf("hard: %+v", hard)
	}
	hinted := Classify(Signals{Text: "hi", MinTierHint: 3})
	if hinted.Tier != 3 {
		t.Fatalf("hint: %+v", hinted)
	}

	models := []TierModel{{"openai/gpt-4.1", 3}, {"openai/gpt-4.1-nano", 1}, {"openai/gpt-4.1-mini", 2}, {"anthropic/claude-sonnet-4-5", 3}}
	price := map[string]float64{"openai/gpt-4.1": 2, "openai/gpt-4.1-nano": 0.1, "openai/gpt-4.1-mini": 0.4, "anthropic/claude-sonnet-4-5": 3}
	p := func(m string) float64 { return price[m] }
	if got := Pick(models, 1, p); strings.Join(got, ",") != "openai/gpt-4.1-nano,openai/gpt-4.1-mini,openai/gpt-4.1,anthropic/claude-sonnet-4-5" {
		t.Fatalf("tier1: %v", got)
	}
	if got := Pick(models, 3, p); strings.Join(got, ",") != "openai/gpt-4.1,anthropic/claude-sonnet-4-5" {
		t.Fatalf("tier3: %v", got)
	}
	if got := Pick(models[1:3], 3, p); strings.Join(got, ",") != "openai/gpt-4.1-mini" {
		t.Fatalf("fallback to best tier: %v", got)
	}
}
