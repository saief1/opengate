// Package guard detects and redacts PII and secrets and flags likely
// prompt-injection attempts. It is deliberately heuristic, dependency-free
// and fast (RE2 regexes plus checksum validation to cut false positives);
// it is a safety net, not a DLP product.
package guard

import (
	"math/big"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// Categories.
const (
	Email      = "email"
	Phone      = "phone"
	CreditCard = "credit_card"
	SSN        = "ssn"
	SIN        = "sin"
	IBAN       = "iban"
	Secret     = "secret"
	Injection  = "prompt_injection"
)

// AllCategories lists every PII/secret category in detection order.
var AllCategories = []string{Secret, CreditCard, IBAN, SSN, SIN, Email, Phone}

// Modes.
const (
	ModeOff    = "off"
	ModeLog    = "log"
	ModeRedact = "redact"
	ModeBlock  = "block"
)

type detector struct {
	cat   string
	re    *regexp.Regexp
	group int               // submatch to replace (0 = whole match)
	valid func(string) bool // optional validation of the matched text
}

var detectors = []detector{
	// Secrets first so that e.g. digits inside keys are not mistaken for cards.
	{cat: Secret, re: regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z0-9 ]*PRIVATE KEY-----`)},
	{cat: Secret, re: regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_\-]{20,}`)},
	{cat: Secret, re: regexp.MustCompile(`\bsk-(?:proj-|svcacct-|admin-)?[A-Za-z0-9_\-]{20,}`)},
	{cat: Secret, re: regexp.MustCompile(`\b(?:sk|rk|pk)_(?:live|test)_[A-Za-z0-9]{16,}`)},
	{cat: Secret, re: regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{cat: Secret, re: regexp.MustCompile(`\b(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{36}\b|\bgithub_pat_[A-Za-z0-9_]{22,}`)},
	{cat: Secret, re: regexp.MustCompile(`\bglpat-[A-Za-z0-9_\-]{20}\b`)},
	{cat: Secret, re: regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9\-]{10,}`)},
	{cat: Secret, re: regexp.MustCompile(`\bAIza[0-9A-Za-z_\-]{35}`)},
	{cat: Secret, re: regexp.MustCompile(`\bog-[A-Za-z0-9_\-]{20,}`)},
	{cat: Secret, re: regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{10,}\.eyJ[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}`)},
	{cat: Secret, re: regexp.MustCompile(`(?i)\b(?:password|passwd|pwd|secret|api[_-]?key|access[_-]?token|auth[_-]?token|client[_-]?secret)\b["']?\s*[:=]\s*["']?([^\s"',;]{8,})`), group: 1},
	{cat: CreditCard, re: regexp.MustCompile(`\b(?:\d[ \-]?){12,18}\d\b`), valid: validCard},
	{cat: IBAN, re: regexp.MustCompile(`\b[A-Z]{2}\d{2}(?: ?[A-Z0-9]{4}){2,7}(?: ?[A-Z0-9]{1,4})?\b`), valid: validIBAN},
	{cat: SSN, re: regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`), valid: validSSN},
	{cat: SIN, re: regexp.MustCompile(`\b\d{3}[ \-]\d{3}[ \-]\d{3}\b`), valid: validSIN},
	{cat: Email, re: regexp.MustCompile(`\b[A-Za-z0-9._%+\-]+@[A-Za-z0-9\-]+(?:\.[A-Za-z0-9\-]+)*\.[A-Za-z]{2,}\b`)},
	{cat: Phone, re: regexp.MustCompile(`(?:\+\d{1,3}[ .\-]?)?(?:\(\d{2,4}\)[ .\-]?|\d{2,4}[ .\-])\d{3,4}[ .\-]\d{3,4}\b|\+\d{10,14}\b`), valid: validPhone},
}

func digits(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Luhn reports whether a digit string passes the Luhn checksum.
func Luhn(d string) bool {
	if len(d) < 2 {
		return false
	}
	sum, dbl := 0, false
	for i := len(d) - 1; i >= 0; i-- {
		n := int(d[i] - '0')
		if n < 0 || n > 9 {
			return false
		}
		if dbl {
			if n *= 2; n > 9 {
				n -= 9
			}
		}
		sum, dbl = sum+n, !dbl
	}
	return sum%10 == 0
}

func validCard(s string) bool {
	d := digits(s)
	if len(d) < 13 || len(d) > 19 || strings.Count(d, string(d[0])) == len(d) {
		return false
	}
	// Known issuer prefixes (Visa, Mastercard, Amex, Discover, JCB, Diners, UnionPay).
	switch {
	case d[0] == '4', d[0] == '5' && d[1] >= '1' && d[1] <= '5', d[0] == '2' && d[1] >= '2' && d[1] <= '7',
		strings.HasPrefix(d, "34"), strings.HasPrefix(d, "37"), strings.HasPrefix(d, "6"),
		strings.HasPrefix(d, "35"), strings.HasPrefix(d, "30"), strings.HasPrefix(d, "36"), strings.HasPrefix(d, "38"):
	default:
		return false
	}
	return Luhn(d)
}

// ValidIBAN checks length and the ISO 13616 mod-97 checksum.
func validIBAN(s string) bool {
	s = strings.ReplaceAll(s, " ", "")
	if len(s) < 15 || len(s) > 34 {
		return false
	}
	r := s[4:] + s[:4]
	var b strings.Builder
	for _, c := range r {
		switch {
		case c >= '0' && c <= '9':
			b.WriteRune(c)
		case c >= 'A' && c <= 'Z':
			b.WriteString(big.NewInt(int64(c - 'A' + 10)).String())
		default:
			return false
		}
	}
	n, ok := new(big.Int).SetString(b.String(), 10)
	return ok && new(big.Int).Mod(n, big.NewInt(97)).Int64() == 1
}

func validSSN(s string) bool {
	d := digits(s)
	area, group, serial := d[:3], d[3:5], d[5:]
	return area != "000" && area != "666" && area[0] != '9' && group != "00" && serial != "0000"
}

func validSIN(s string) bool {
	d := digits(s)
	return Luhn(d)
}

func validPhone(s string) bool {
	n := len(digits(s))
	return n >= 10 && n <= 15
}

// Finding counts matches per category.
type Finding map[string]int

// Add merges another finding.
func (f Finding) Add(o Finding) {
	for k, v := range o {
		f[k] += v
	}
}

// Categories returns the found categories sorted.
func (f Finding) Categories() []string {
	out := make([]string, 0, len(f))
	for k, v := range f {
		if v > 0 {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// Scanner detects (and optionally redacts) a set of categories.
type Scanner struct {
	cats map[string]bool
}

// NewScanner enables the given categories (empty = all).
func NewScanner(categories []string) *Scanner {
	s := &Scanner{cats: map[string]bool{}}
	if len(categories) == 0 {
		categories = AllCategories
	}
	for _, c := range categories {
		s.cats[strings.TrimSpace(c)] = true
	}
	return s
}

// ValidCategory reports whether c is a known PII/secret category.
func ValidCategory(c string) bool {
	for _, k := range AllCategories {
		if k == c {
			return true
		}
	}
	return false
}

// Placeholder is the replacement text for a category.
func Placeholder(cat string) string { return "[REDACTED_" + strings.ToUpper(cat) + "]" }

// Scan returns text with findings replaced by placeholders (when redact is
// true) and what was found. Matched values are never returned.
func (s *Scanner) Scan(text string, redact bool) (string, Finding) {
	found := Finding{}
	if text == "" {
		return text, found
	}
	orig := text // detectors always run on the progressively masked text so
	// one value is never counted twice (e.g. a card number as a phone)
	for _, d := range detectors {
		if !s.cats[d.cat] {
			continue
		}
		idx := d.re.FindAllStringSubmatchIndex(text, -1)
		if len(idx) == 0 {
			continue
		}
		var b strings.Builder
		last := 0
		for _, m := range idx {
			st, en := m[2*d.group], m[2*d.group+1]
			if st < 0 || (d.valid != nil && !d.valid(text[st:en])) || !boundaryOK(text, st, en) {
				continue
			}
			found[d.cat]++
			b.WriteString(text[last:st])
			b.WriteString(Placeholder(d.cat))
			last = en
		}
		if last > 0 {
			b.WriteString(text[last:])
			text = b.String()
		}
	}
	if !redact {
		return orig, found
	}
	return text, found
}

// boundaryOK rejects digit-ish matches glued to letters/digits (e.g. inside
// UUIDs or longer numbers), which RE2's \b alone does not prevent for "-".
func boundaryOK(text string, st, en int) bool {
	if st > 0 {
		r := rune(text[st-1])
		if unicode.IsDigit(r) {
			return false
		}
	}
	if en < len(text) {
		r := rune(text[en])
		if unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

// ---------- prompt injection ----------

type injRule struct {
	re     *regexp.Regexp
	weight int
	name   string
}

var injRules = []injRule{
	{regexp.MustCompile(`(?i)\b(ignore|disregard|forget|override|bypass)\b[^.\n]{0,40}\b(previous|prior|above|earlier|all|any|your|system|developer)\b[^.\n]{0,30}\b(instructions?|prompts?|rules|directions|guidelines|messages?)\b`), 3, "ignore_instructions"},
	{regexp.MustCompile(`(?i)\b(reveal|show|print|repeat|output|leak|display|tell me)\b[^.\n]{0,40}\b(system prompt|hidden (instructions|prompt)|initial (instructions|prompt)|developer (message|prompt)|your instructions)\b`), 3, "prompt_exfiltration"},
	{regexp.MustCompile(`(?i)\byou are now\b[^.\n]{0,40}\b(DAN|jailbroken|unrestricted|unfiltered|uncensored|evil)\b`), 3, "persona_override"},
	{regexp.MustCompile(`(?i)\b(developer|god|jailbreak|DAN|sudo) mode\b`), 2, "mode_switch"},
	{regexp.MustCompile(`(?i)\bdo anything now\b`), 2, "dan"},
	{regexp.MustCompile(`(?i)\bpretend\b[^.\n]{0,30}\bno (rules|restrictions|guidelines|filters|limits)\b`), 2, "no_rules"},
	{regexp.MustCompile(`(?i)\b(act|respond|behave) as\b[^.\n]{0,40}\bwithout (any )?(restrictions|limits|filters|censorship)\b`), 2, "no_rules"},
	{regexp.MustCompile(`(?i)<\|?/?(system|im_start|im_end)\|?>|\[/?INST\]|<<SYS>>`), 2, "token_smuggling"},
	{regexp.MustCompile(`(?i)\b(new|updated|real) (system )?instructions?\s*:`), 1, "new_instructions"},
	{regexp.MustCompile(`(?i)\bthe (above|previous) (was|is) (a test|fake|not real)\b`), 1, "context_reset"},
}

// InjectionThreshold is the score at which text is flagged.
const InjectionThreshold = 3

// InjectionScore scores text for prompt-injection phrasing and returns the
// matched heuristic names.
func InjectionScore(text string) (int, []string) {
	score := 0
	var hits []string
	for _, r := range injRules {
		if r.re.MatchString(text) {
			score += r.weight
			hits = append(hits, r.name)
		}
	}
	return score, hits
}
