// Package routing implements rules-based model routing and the "auto"
// alias, which classifies request difficulty with a cheap local heuristic
// and picks the cheapest configured model that meets the required quality
// tier. It is pure logic with no I/O so it is easy to test and reason about.
package routing

import (
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/opengate-dev/opengate/internal/store"
)

// Request describes a chat request for routing decisions.
type Request struct {
	Model        string // requested model or alias
	PromptTokens int    // estimated
	Agent        string
	ProjectID    string
	// OrgID is the requesting project's organization. Rules of other orgs
	// never match (they must not shadow this org's rules).
	OrgID   string
	Headers http.Header
	// EstCost returns the estimated cost of this request on a model name
	// (alias or provider/model); used for cost-ceiling rules.
	EstCost func(model string) float64
}

// Tags returns the X-OpenGate-Tags header as a set.
func (r Request) Tags() map[string]bool {
	out := map[string]bool{}
	for _, v := range r.Headers.Values("X-OpenGate-Tags") {
		for _, t := range strings.Split(v, ",") {
			if t = strings.ToLower(strings.TrimSpace(t)); t != "" {
				out[t] = true
			}
		}
	}
	return out
}

// Decision is the outcome of rule evaluation.
type Decision struct {
	Rule      *store.RoutingRule
	Target    string   // replacement model/alias ("auto" allowed)
	Fallbacks []string // extra targets appended after Target's list
}

func matchPattern(p, name string) bool {
	if pre, ok := strings.CutSuffix(p, "*"); ok {
		return strings.HasPrefix(name, pre)
	}
	return p == name
}

// Matches reports whether a rule's conditions hold for r.
func Matches(rule *store.RoutingRule, r Request) bool {
	if !rule.Enabled || rule.OrgID != r.OrgID || (rule.ProjectID != "" && rule.ProjectID != r.ProjectID) {
		return false
	}
	m := rule.Match
	if len(m.Models) > 0 {
		ok := false
		for _, p := range m.Models {
			if matchPattern(p, r.Model) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	if m.MinPromptTokens > 0 && r.PromptTokens < m.MinPromptTokens {
		return false
	}
	if m.MaxPromptTokens > 0 && r.PromptTokens > m.MaxPromptTokens {
		return false
	}
	if len(m.Agents) > 0 {
		ok := false
		for _, a := range m.Agents {
			if a == r.Agent {
				ok = true
			}
		}
		if !ok {
			return false
		}
	}
	if len(m.Tags) > 0 {
		tags := r.Tags()
		for _, t := range m.Tags {
			if !tags[strings.ToLower(t)] {
				return false
			}
		}
	}
	for k, v := range m.Headers {
		if r.Headers.Get(k) != v {
			return false
		}
	}
	if m.EstCostAboveUSD > 0 {
		if r.EstCost == nil || r.EstCost(r.Model) <= m.EstCostAboveUSD {
			return false
		}
	}
	return true
}

// Evaluate returns the first matching rule. Project-specific rules win over
// org-wide ones at equal priority. Rules are never chained: the target of a
// rule is not routed again (except through "auto").
func Evaluate(rules []*store.RoutingRule, r Request) *Decision {
	sorted := append([]*store.RoutingRule(nil), rules...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Priority != sorted[j].Priority {
			return sorted[i].Priority < sorted[j].Priority
		}
		return sorted[i].ProjectID != "" && sorted[j].ProjectID == ""
	})
	for _, rule := range sorted {
		if Matches(rule, r) {
			if rule.Target == r.Model && len(rule.Fallbacks) == 0 {
				continue // no-op rule
			}
			return &Decision{Rule: rule, Target: rule.Target, Fallbacks: rule.Fallbacks}
		}
	}
	return nil
}

// ---------- auto ----------

// Tier is a quality tier: 1 = simple, 2 = standard, 3 = hard.
type Tier int

// TierModel is one candidate model of the auto alias.
type TierModel struct {
	Model string // provider/model
	Tier  Tier
}

// Classification explains an auto decision.
type Classification struct {
	Tier    Tier     `json:"tier"`
	Score   int      `json:"score"`
	Signals []string `json:"signals"`
}

var (
	reCode     = regexp.MustCompile("```|\\bfunc |\\bdef |\\bclass |\\bimport |#include|=>|\\bSELECT\\b.+\\bFROM\\b|\\{\\s*\"")
	reHard     = regexp.MustCompile(`(?i)\b(prove|proof|theorem|derive|derivation|architect\w*|refactor\w*|debug\w*|optimi[sz]\w*|analy[sz]e|analysis|step[- ]by[- ]step|reason\w*|legal|contract|diagnos\w*|trade-?offs?|algorithm\w*|complexity|multi-step|plan|strategy|research|evaluate|critique|security review|threat model)\b`)
	reSimple   = regexp.MustCompile(`(?i)\b(translate|summari[sz]e|tl;?dr|classify|categori[sz]e|extract|yes or no|spell|grammar|rephrase|rewrite|sentiment|label|tag|keywords?|title for|one word)\b`)
	reMathLike = regexp.MustCompile(`[∑∫√≤≥≠]|\\frac|\\sum|\\int|\b\d+\s*[\^*/]\s*\d+`)
)

// Signals summarizes the request text for classification.
type Signals struct {
	Text        string // concatenated message text (user + system)
	Messages    int
	PromptToks  int
	HasTools    bool
	JSONSchema  bool
	MinTierHint Tier // from X-OpenGate-Quality: low|medium|high
}

// Classify scores difficulty. The heuristic is intentionally simple and
// explainable; the signals are returned so routing decisions are auditable.
func Classify(s Signals) Classification {
	score := 0
	var sig []string
	add := func(n int, why string) { score += n; sig = append(sig, fmt.Sprintf("%s(%+d)", why, n)) }
	switch {
	case s.PromptToks > 8000:
		add(2, "long_prompt")
	case s.PromptToks > 2000:
		add(1, "medium_prompt")
	}
	if reCode.MatchString(s.Text) {
		add(1, "code")
	}
	if reMathLike.MatchString(s.Text) {
		add(1, "math")
	}
	if n := len(reHard.FindAllStringIndex(s.Text, 3)); n > 0 {
		if n > 2 {
			n = 2
		}
		add(n, "hard_keywords")
	}
	if s.HasTools {
		add(1, "tools")
	}
	if s.JSONSchema {
		add(1, "json_schema")
	}
	if s.Messages > 8 {
		add(1, "long_conversation")
	}
	if reSimple.MatchString(s.Text) && score <= 1 {
		add(-1, "simple_task")
	}
	t := Tier(1)
	switch {
	case score >= 3:
		t = 3
	case score >= 1:
		t = 2
	}
	if s.MinTierHint > t {
		t = s.MinTierHint
		sig = append(sig, fmt.Sprintf("quality_hint(tier>=%d)", s.MinTierHint))
	}
	return Classification{Tier: t, Score: score, Signals: sig}
}

// QualityHint parses X-OpenGate-Quality.
func QualityHint(h http.Header) Tier {
	switch strings.ToLower(strings.TrimSpace(h.Get("X-OpenGate-Quality"))) {
	case "high", "3":
		return 3
	case "medium", "standard", "2":
		return 2
	case "low", "1":
		return 1
	}
	return 0
}

// Pick returns the candidates whose tier is at least t, cheapest first
// (by the given blended price). If none qualifies, the highest-tier models
// are returned so the request still has somewhere to go.
func Pick(models []TierModel, t Tier, price func(model string) float64) []string {
	var ok []TierModel
	best := Tier(0)
	for _, m := range models {
		if m.Tier >= t {
			ok = append(ok, m)
		}
		if m.Tier > best {
			best = m.Tier
		}
	}
	if len(ok) == 0 {
		for _, m := range models {
			if m.Tier == best {
				ok = append(ok, m)
			}
		}
	}
	sort.SliceStable(ok, func(i, j int) bool {
		pi, pj := price(ok[i].Model), price(ok[j].Model)
		if pi != pj {
			return pi < pj
		}
		return ok[i].Tier < ok[j].Tier
	})
	out := make([]string, len(ok))
	for i, m := range ok {
		out[i] = m.Model
	}
	return out
}
