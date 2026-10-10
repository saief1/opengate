// Package billing attributes gateway usage to the end customers of an AI
// app: markup rules, monthly CSV export and Stripe usage-based billing sync
// via Billing Meter Events (POST /v1/billing/meter_events).
package billing

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
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

	"github.com/saief1/opengate/internal/config"
	"github.com/saief1/opengate/internal/store"
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
//
// Correctness rules (no double billing):
//   - Usage is reported per fixed, epoch-aligned window (cfg.Window). The
//     window that follows a cursor is a pure function of the cursor, and the
//     meter event identifier is a hash of (project, customer, from, to), so a
//     retry after a lost response, or another replica, sends the *same*
//     identifier and Stripe de-duplicates it.
//   - The cursor only advances by compare-and-swap from the value the window
//     was computed from.
//   - A database lease makes one process (across replicas) the writer.
type Stripe struct {
	cfg    config.StripeConfig
	apiKey string
	st     *store.Store
	client *http.Client
	log    *slog.Logger
	now    func() time.Time
	holder string     // lease holder id of this process
	mu     sync.Mutex // one sync at a time per process
}

// syncLease is the job_leases row guarding the meter sync.
const syncLease = "stripe_meter_sync"

// maxWindowsPerRun bounds catch-up work per customer per run.
const maxWindowsPerRun = 2000

// NewStripe creates a syncer. client may be nil.
func NewStripe(cfg config.StripeConfig, apiKey string, st *store.Store, client *http.Client, log *slog.Logger) *Stripe {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	if cfg.Window <= 0 {
		cfg.Window = time.Hour
	}
	if cfg.APIVersion == "" {
		cfg.APIVersion = config.StripeAPIVersion
	}
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return &Stripe{cfg: cfg, apiKey: apiKey, st: st, client: client, log: log, now: time.Now, holder: "sync_" + hex.EncodeToString(b)}
}

// SyncResult summarizes one run.
type SyncResult struct {
	Customers int      `json:"customers"`
	Events    int      `json:"events"`
	Skipped   bool     `json:"skipped,omitempty"` // another replica holds the lease
	Errors    []string `json:"errors,omitempty"`
}

// Sync reports usage for every mapped customer, one closed window at a time,
// up to now-lag. Fractional cents are carried forward.
func (s *Stripe) Sync(ctx context.Context) SyncResult { return s.sync(ctx, nil) }

// SyncProjects syncs only customers of the given projects (one org's).
func (s *Stripe) SyncProjects(ctx context.Context, projectIDs []string) SyncResult {
	allow := map[string]bool{}
	for _, id := range projectIDs {
		allow[id] = true
	}
	return s.sync(ctx, allow)
}

// windowAfter returns the end of the fixed window that contains from
// (windows are [k*w, (k+1)*w) in Unix milliseconds; a cursor on a boundary
// starts the next window).
func windowAfter(from time.Time, w time.Duration) time.Time {
	wm := w.Milliseconds()
	f := from.UnixMilli()
	end := (f/wm + 1) * wm
	if f < 0 && f%wm != 0 {
		end -= wm
	}
	return time.UnixMilli(end).UTC()
}

func (s *Stripe) sync(ctx context.Context, only map[string]bool) SyncResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	var res SyncResult
	ttl := 10 * time.Minute
	if ok, err := s.st.AcquireLease(ctx, syncLease, s.holder, ttl); err != nil {
		res.Errors = append(res.Errors, "lease: "+err.Error())
		return res
	} else if !ok {
		res.Skipped = true
		return res
	}
	defer func() { _ = s.st.ReleaseLease(context.WithoutCancel(ctx), syncLease, s.holder) }()
	all, err := s.st.StripeSyncStates(ctx)
	if err != nil {
		res.Errors = append(res.Errors, err.Error())
		return res
	}
	cutoff := s.now().Add(-s.cfg.Lag)
	renew := time.Now().Add(ttl / 2)
	for _, st := range all {
		if only != nil && !only[st.ProjectID] {
			continue
		}
		if windowAfter(st.SyncedTo, s.cfg.Window).After(cutoff) {
			continue // the current window hasn't closed yet
		}
		res.Customers++
		from, rem := st.SyncedTo, st.Remainder
		for i := 0; i < maxWindowsPerRun; i++ {
			end := windowAfter(from, s.cfg.Window)
			if end.After(cutoff) {
				break
			}
			if time.Now().After(renew) { // long catch-up: keep the lease
				if ok, err := s.st.AcquireLease(ctx, syncLease, s.holder, ttl); err != nil || !ok {
					res.Errors = append(res.Errors, "lease lost")
					return res
				}
				renew = time.Now().Add(ttl / 2)
			}
			billed, tokens, err := s.st.UsageWindow(ctx, st.ProjectID, st.CustomerID, from, end)
			if err != nil {
				res.Errors = append(res.Errors, err.Error())
				break
			}
			units := float64(tokens)
			if s.cfg.Unit == "cents" {
				units = billed*100 + rem
			}
			value := int64(math.Floor(units + 1e-9))
			nrem := units - float64(value)
			if nrem < 0 || s.cfg.Unit != "cents" {
				nrem = 0
			}
			if value > 0 {
				id := eventID(st.ProjectID, st.CustomerID, from, end)
				// The event is timestamped inside its window (Stripe buckets
				// usage by timestamp and rejects future timestamps).
				if err := s.send(ctx, st.StripeCustomerID, value, id, end.Add(-time.Second)); err != nil {
					msg := fmt.Sprintf("%s/%s: %v", st.ProjectID, st.CustomerID, err)
					res.Errors = append(res.Errors, msg)
					_ = s.st.FailStripeSync(ctx, st.ProjectID, st.CustomerID, err.Error())
					s.log.Warn("stripe meter event failed", "project_id", st.ProjectID, "customer_id", st.CustomerID, "window_end", end, "err", err)
					break // retry this exact window (same identifier) next run
				}
				res.Events++
			}
			if err := s.st.AdvanceStripeSync(ctx, st.ProjectID, st.CustomerID, from, end, nrem, value); err != nil {
				if !errors.Is(err, store.ErrNotFound) {
					res.Errors = append(res.Errors, err.Error())
				}
				break // cursor moved under us: another writer; stop for this customer
			}
			from, rem = end, nrem
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
	req.Header.Set("Stripe-Version", s.cfg.APIVersion)
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
	msg := strings.ToLower(e.Error.Message)
	if resp.StatusCode == http.StatusBadRequest && (e.Error.Code == "resource_already_exists" ||
		(strings.Contains(msg, "identifier") && (strings.Contains(msg, "already") || strings.Contains(msg, "duplicate")))) {
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
