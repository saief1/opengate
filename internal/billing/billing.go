// Package billing attributes gateway usage to the end customers of an AI
// app: markup rules, monthly CSV export and Stripe usage-based billing sync
// via Billing Meter Events (POST /v1/billing/meter_events).
package billing

import (
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/opengate-dev/opengate/internal/config"
	"github.com/opengate-dev/opengate/internal/store"
)

// ---------- markup ----------

func matchModel(p, name string) bool {
	if pre, ok := strings.CutSuffix(p, "*"); ok {
		return strings.HasPrefix(name, pre)
	}
	return p == name
}

// ResolveMarkup returns the markup percentage for a request: the most
// specific matching rule wins (customer > model > project); ties go to the
// oldest rule. No match = 0%.
func ResolveMarkup(rules []*store.MarkupRule, orgID, projectID, customerID, model string) (float64, *store.MarkupRule) {
	best, bestScore := (*store.MarkupRule)(nil), -1
	for _, r := range rules {
		if r.OrgID != "" && orgID != "" && r.OrgID != orgID {
			continue
		}
		score := 0
		if r.ProjectID != "" {
			if r.ProjectID != projectID {
				continue
			}
			score++
		}
		if r.Model != "" {
			if !matchModel(r.Model, model) {
				continue
			}
			score += 2
		}
		if r.CustomerID != "" {
			if r.CustomerID != customerID {
				continue
			}
			score += 4
		}
		if score > bestScore {
			best, bestScore = r, score
		}
	}
	if best == nil {
		return 0, nil
	}
	return best.MarkupPct, best
}

// Billed applies a markup to a cost.
func Billed(cost, markupPct float64) float64 { return cost * (1 + markupPct/100) }

// Markups caches markup rules briefly so the hot path does not hit the DB.
type Markups struct {
	st  *store.Store
	ttl time.Duration
	mu  sync.Mutex
	at  time.Time
	rs  []*store.MarkupRule
}

// NewMarkups creates a cache with the given TTL.
func NewMarkups(st *store.Store, ttl time.Duration) *Markups { return &Markups{st: st, ttl: ttl} }

// Invalidate forces a reload on next use.
func (m *Markups) Invalidate() {
	m.mu.Lock()
	m.at = time.Time{}
	m.mu.Unlock()
}

// Pct resolves the markup for a request.
func (m *Markups) Pct(ctx context.Context, orgID, projectID, customerID, model string) float64 {
	m.mu.Lock()
	if time.Since(m.at) > m.ttl {
		if rs, err := m.st.ListMarkupRules(ctx, ""); err == nil {
			m.rs, m.at = rs, time.Now()
		}
	}
	rs := m.rs
	m.mu.Unlock()
	p, _ := ResolveMarkup(rs, orgID, projectID, customerID, model)
	return p
}

// ---------- CSV export ----------

// MonthRange parses "YYYY-MM" (UTC) into [start, end).
func MonthRange(month string) (time.Time, time.Time, error) {
	t, err := time.Parse("2006-01", month)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("month must look like 2026-10")
	}
	return t, t.AddDate(0, 1, 0), nil
}

// WriteCSV writes per-customer usage rows.
func WriteCSV(w io.Writer, month string, usage []*store.CustomerUsage, projects map[string]string, meta map[string]*store.Customer) error {
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"month", "project_id", "project", "customer_id", "customer_name", "stripe_customer_id", "requests", "prompt_tokens", "completion_tokens", "cost_usd", "billed_usd"})
	for _, u := range usage {
		m := meta[u.ProjectID+"\x00"+u.CustomerID]
		name, stripeID := "", ""
		if m != nil {
			name, stripeID = m.Name, m.StripeCustomerID
		}
		if err := cw.Write([]string{month, u.ProjectID, safeCell(projects[u.ProjectID]), safeCell(u.CustomerID), safeCell(name), stripeID,
			strconv.Itoa(u.Requests), strconv.FormatInt(u.PromptTokens, 10), strconv.FormatInt(u.CompletionTokens, 10),
			strconv.FormatFloat(u.CostUSD, 'f', 6, 64), strconv.FormatFloat(u.BilledUSD, 'f', 6, 64)}); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// safeCell neutralizes spreadsheet formula injection in user-controlled cells.
func safeCell(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

// ---------- Stripe ----------

// Stripe syncs usage to Stripe Billing meter events.
type Stripe struct {
	cfg    config.StripeConfig
	apiKey string
	st     *store.Store
	client *http.Client
	log    *slog.Logger
	now    func() time.Time
	mu     sync.Mutex // one sync at a time per process
}

// NewStripe creates a syncer. client may be nil.
func NewStripe(cfg config.StripeConfig, apiKey string, st *store.Store, client *http.Client, log *slog.Logger) *Stripe {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &Stripe{cfg: cfg, apiKey: apiKey, st: st, client: client, log: log, now: time.Now}
}

// SyncResult summarizes one run.
type SyncResult struct {
	Customers int      `json:"customers"`
	Events    int      `json:"events"`
	Errors    []string `json:"errors,omitempty"`
}

// Sync reports usage for every mapped customer from its cursor up to now-lag.
// Each event carries a deterministic identifier, so a retried window is
// de-duplicated by Stripe. Fractional cents are carried forward.
func (s *Stripe) Sync(ctx context.Context) SyncResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	var res SyncResult
	states, err := s.st.StripeSyncStates(ctx)
	if err != nil {
		res.Errors = append(res.Errors, err.Error())
		return res
	}
	end := s.now().Add(-s.cfg.Lag).Truncate(time.Millisecond)
	for _, st := range states {
		if !end.After(st.SyncedTo) {
			continue
		}
		res.Customers++
		billed, tokens, err := s.st.UsageWindow(ctx, st.ProjectID, st.CustomerID, st.SyncedTo, end)
		if err != nil {
			res.Errors = append(res.Errors, err.Error())
			continue
		}
		units := float64(tokens)
		if s.cfg.Unit == "cents" {
			units = billed*100 + st.Remainder
		}
		value := int64(math.Floor(units + 1e-9))
		rem := units - float64(value)
		if rem < 0 {
			rem = 0
		}
		if value > 0 {
			id := eventID(st.ProjectID, st.CustomerID, st.SyncedTo, end)
			if err := s.send(ctx, st.StripeCustomerID, value, id, end); err != nil {
				msg := fmt.Sprintf("%s/%s: %v", st.ProjectID, st.CustomerID, err)
				res.Errors = append(res.Errors, msg)
				_ = s.st.FailStripeSync(ctx, st.ProjectID, st.CustomerID, err.Error())
				s.log.Warn("stripe meter event failed", "project_id", st.ProjectID, "customer_id", st.CustomerID, "err", err)
				continue
			}
			res.Events++
		}
		if err := s.st.AdvanceStripeSync(ctx, st.ProjectID, st.CustomerID, end, rem, value); err != nil {
			res.Errors = append(res.Errors, err.Error())
		}
	}
	return res
}

func eventID(project, customer string, from, to time.Time) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d\x00%d", project, customer, from.UnixMilli(), to.UnixMilli())))
	return "og_" + hex.EncodeToString(h[:16])
}

func (s *Stripe) send(ctx context.Context, stripeCustomer string, value int64, identifier string, ts time.Time) error {
	form := url.Values{}
	form.Set("event_name", s.cfg.EventName)
	form.Set("payload[stripe_customer_id]", stripeCustomer)
	form.Set("payload[value]", strconv.FormatInt(value, 10))
	form.Set("identifier", identifier)
	form.Set("timestamp", strconv.FormatInt(ts.Unix(), 10))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.BaseURL+"/v1/billing/meter_events", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.apiKey)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Idempotency-Key", identifier)
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode/100 == 2 {
		return nil
	}
	var e struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &e)
	// A duplicate identifier means Stripe already has this window.
	if resp.StatusCode == http.StatusBadRequest && strings.Contains(strings.ToLower(e.Error.Message), "identifier") && strings.Contains(strings.ToLower(e.Error.Message), "already") {
		return nil
	}
	return fmt.Errorf("stripe HTTP %d: %s", resp.StatusCode, e.Error.Message)
}

// Run syncs every interval until ctx is done.
func (s *Stripe) Run(ctx context.Context) {
	t := time.NewTicker(s.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r := s.Sync(ctx)
			if r.Events > 0 || len(r.Errors) > 0 {
				s.log.Info("stripe usage sync", "customers", r.Customers, "events", r.Events, "errors", len(r.Errors))
			}
		}
	}
}
