package billing

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opengate-dev/opengate/internal/config"
	"github.com/opengate-dev/opengate/internal/store"
	"github.com/opengate-dev/opengate/internal/storetest"
)

func TestResolveMarkup(t *testing.T) {
	rules := []*store.MarkupRule{
		{ID: "a", MarkupPct: 20},                                     // global default
		{ID: "b", ProjectID: "p1", MarkupPct: 30},                    // project
		{ID: "c", Model: "anthropic/*", MarkupPct: 40},               // model
		{ID: "d", ProjectID: "p1", CustomerID: "acme", MarkupPct: 5}, // negotiated deal
	}
	cases := []struct {
		proj, cust, model string
		want              float64
	}{
		{"p2", "x", "openai/gpt-4o", 20},
		{"p1", "x", "openai/gpt-4o", 30},
		{"p1", "x", "anthropic/claude", 40},
		{"p1", "acme", "anthropic/claude", 5},
	}
	for _, c := range cases {
		if got, _ := ResolveMarkup(rules, "", c.proj, c.cust, c.model); got != c.want {
			t.Errorf("%+v: got %v", c, got)
		}
	}
	if got, _ := ResolveMarkup(nil, "", "p", "c", "m"); got != 0 {
		t.Fatal("no rules = 0")
	}
	if b := Billed(1.0, 25); b != 1.25 {
		t.Fatal(b)
	}
}

func TestCSVEscapesFormulas(t *testing.T) {
	var buf bytes.Buffer
	err := WriteCSV(&buf, "2026-10", []*store.CustomerUsage{{ProjectID: "p", CustomerID: "=HYPERLINK(\"x\")", Requests: 2, CostUSD: 1, BilledUSD: 1.5}},
		map[string]string{"p": "app"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `'=HYPERLINK`) || !strings.Contains(buf.String(), "1.500000") {
		t.Fatal(buf.String())
	}
}

type fakeStripe struct {
	mu     sync.Mutex
	events []url.Values
	fail   bool
	lose   int // accept the next N events but answer 500 (lost response)
	calls  int
}

func (f *fakeStripe) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if r.URL.Path != "/v1/billing/meter_events" || r.Header.Get("Authorization") != "Bearer sk_test_fake" ||
		r.Header.Get("Stripe-Version") != config.StripeAPIVersion || r.Header.Get("Idempotency-Key") == "" {
		http.Error(w, `{"error":{"message":"bad auth or path"}}`, 401)
		return
	}
	if f.fail {
		http.Error(w, `{"error":{"message":"temporarily unavailable"}}`, 503)
		return
	}
	b, _ := io.ReadAll(r.Body)
	v, _ := url.ParseQuery(string(b))
	for _, e := range f.events {
		if e.Get("identifier") == v.Get("identifier") {
			w.WriteHeader(400)
			io.WriteString(w, `{"error":{"message":"An event with identifier `+v.Get("identifier")+` already exists."}}`)
			return
		}
	}
	f.events = append(f.events, v)
	if f.lose > 0 {
		f.lose--
		http.Error(w, `{"error":{"message":"connection reset"}}`, 502)
		return
	}
	io.WriteString(w, `{"object":"billing.meter_event"}`)
}

func TestStripeSyncAgainstMockServer(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(storetest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	p, _ := st.CreateProject(ctx, "app", false)
	if err := st.UpsertCustomer(ctx, &store.Customer{ProjectID: p.ID, ID: "acme", Name: "Acme", StripeCustomerID: "cus_123"}); err != nil {
		t.Fatal(err)
	}
	// Unmapped customer: never reported.
	_ = st.UpsertCustomer(ctx, &store.Customer{ProjectID: p.ID, ID: "nomap"})

	start := time.Now()
	ins := func(cust string, at time.Time, billed float64, status int) {
		l := &store.RequestLog{ID: "req_" + cust + at.Format("150405.000000"), TS: at, ProjectID: p.ID, KeyID: "k", CustomerID: cust,
			Status: status, CostUSD: billed / 1.5, BilledUSD: billed, PromptTokens: 10, CompletionTokens: 5}
		if err := st.InsertLog(ctx, l); err != nil {
			t.Fatal(err)
		}
	}
	ins("acme", start.Add(1*time.Second), 0.123, 200)
	ins("acme", start.Add(2*time.Second), 0.004, 200)
	ins("acme", start.Add(3*time.Second), 9.99, 429) // rejected: not billed
	ins("nomap", start.Add(1*time.Second), 5, 200)

	fs := &fakeStripe{}
	srv := httptest.NewServer(fs)
	defer srv.Close()
	cfg := config.StripeConfig{Enabled: true, BaseURL: srv.URL, EventName: "opengate_usage", Unit: "cents", Interval: time.Hour, Lag: time.Minute, Window: time.Minute}
	s := NewStripe(cfg, "sk_test_fake", st, srv.Client(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	now := start.Add(time.Hour)
	s.now = func() time.Time { return now }

	fs.fail = true
	if r := s.Sync(ctx); len(r.Errors) != 1 || r.Events != 0 {
		t.Fatalf("failing stripe: %+v", r)
	}
	fs.fail = false
	if r := s.Sync(ctx); r.Events != 1 || len(r.Errors) != 0 {
		t.Fatalf("sync: %+v", r)
	}
	if len(fs.events) != 1 {
		t.Fatalf("events %d", len(fs.events))
	}
	e := fs.events[0]
	// 0.127 USD = 12.7 cents -> 12 now, 0.7 carried.
	if e.Get("event_name") != "opengate_usage" || e.Get("payload[stripe_customer_id]") != "cus_123" || e.Get("payload[value]") != "12" || !strings.HasPrefix(e.Get("identifier"), "og_") {
		t.Fatalf("event %v", e)
	}
	// Nothing new: no event, cursor still moves.
	now = now.Add(time.Hour)
	if r := s.Sync(ctx); r.Events != 0 {
		t.Fatalf("idle sync sent %+v", r)
	}
	// New usage 0.0035 USD -> 0.35c + 0.7c carried = 1.05c -> 1.
	ins("acme", now.Add(10*time.Minute), 0.0035, 200)
	now = now.Add(time.Hour)
	if r := s.Sync(ctx); r.Events != 1 || fs.events[1].Get("payload[value]") != "1" {
		t.Fatalf("carry: %+v %v", r, fs.events)
	}
	states, _ := st.StripeSyncStates(ctx)
	if len(states) != 1 || states[0].Remainder < 0.04 || states[0].Remainder > 0.06 || states[0].LastError != "" {
		t.Fatalf("state %+v", states[0])
	}
}

// setupMeter returns a store with one mapped customer whose cursor sits on a
// window boundary, and a function to insert billed usage.
func setupMeter(t *testing.T) (*store.Store, string, time.Time, func(at time.Time, usd float64)) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(storetest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	p, _ := st.CreateProject(ctx, "app", false)
	if err := st.UpsertCustomer(ctx, &store.Customer{ProjectID: p.ID, ID: "acme", StripeCustomerID: "cus_123"}); err != nil {
		t.Fatal(err)
	}
	states, _ := st.StripeSyncStates(ctx)
	base := states[0].SyncedTo
	n := 0
	return st, p.ID, base, func(at time.Time, usd float64) {
		n++
		l := &store.RequestLog{ID: fmt.Sprintf("req_%d", n), TS: at, ProjectID: p.ID, KeyID: "k", CustomerID: "acme", Status: 200, CostUSD: usd, BilledUSD: usd}
		if err := st.InsertLog(ctx, l); err != nil {
			t.Fatal(err)
		}
	}
}

func sumValues(evs []url.Values) (total int64, ids map[string]int) {
	ids = map[string]int{}
	for _, e := range evs {
		v, _ := strconv.ParseInt(e.Get("payload[value]"), 10, 64)
		total += v
		ids[e.Get("identifier")]++
	}
	return
}

// B1: a lost response must not double-report. The retry happens later (the
// sync cutoff moved), yet it must resend the same window with the same
// identifier, which Stripe de-duplicates.
func TestStripeSyncLostResponseNoDoubleBilling(t *testing.T) {
	ctx := context.Background()
	st, _, base, ins := setupMeter(t)
	ins(base.Add(10*time.Second), 0.50) // window 1
	ins(base.Add(3*time.Minute), 0.25)  // a later window

	fs := &fakeStripe{lose: 1}
	srv := httptest.NewServer(fs)
	defer srv.Close()
	cfg := config.StripeConfig{Enabled: true, BaseURL: srv.URL, EventName: "u", Unit: "cents", Lag: time.Second, Window: time.Minute}
	s := NewStripe(cfg, "sk_test_fake", st, srv.Client(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	now := base.Add(2 * time.Minute)
	s.now = func() time.Time { return now }
	if r := s.Sync(ctx); len(r.Errors) != 1 {
		t.Fatalf("lost response should surface as an error: %+v", r)
	}
	// Much later: the cutoff now covers several more windows.
	now = base.Add(30 * time.Minute)
	if r := s.Sync(ctx); len(r.Errors) != 0 {
		t.Fatalf("retry: %+v", r)
	}
	total, ids := sumValues(fs.events)
	if total != 75 {
		t.Fatalf("Stripe recorded %d cents, want 75 (events %v)", total, fs.events)
	}
	for id, n := range ids {
		if n != 1 {
			t.Fatalf("identifier %s recorded %d times", id, n)
		}
	}
	if fs.calls != 3 { // first (lost), resend of the same window (dedup 400), second window
		t.Fatalf("calls %d", fs.calls)
	}
}

// B1: replicas syncing concurrently (separate processes sharing the
// database) report each window once.
func TestStripeSyncConcurrentReplicas(t *testing.T) {
	ctx := context.Background()
	st, _, base, ins := setupMeter(t)
	want := int64(0)
	for i := 0; i < 40; i++ {
		ins(base.Add(time.Duration(i)*45*time.Second+time.Second), 0.10)
		want += 10
	}
	fs := &fakeStripe{}
	srv := httptest.NewServer(fs)
	defer srv.Close()
	cfg := config.StripeConfig{Enabled: true, BaseURL: srv.URL, EventName: "u", Unit: "cents", Lag: time.Second, Window: time.Minute}
	now := base.Add(time.Hour)
	var wg sync.WaitGroup
	for r := 0; r < 4; r++ {
		s := NewStripe(cfg, "sk_test_fake", st, srv.Client(), slog.New(slog.NewTextHandler(io.Discard, nil)))
		skew := time.Duration(r) * 7 * time.Second // replicas' clocks/cutoffs differ
		s.now = func() time.Time { return now.Add(skew) }
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 5; i++ {
				s.Sync(ctx)
			}
		}()
	}
	wg.Wait()
	// Whatever is left (a replica may have skipped while another held the lease).
	s := NewStripe(cfg, "sk_test_fake", st, srv.Client(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.now = func() time.Time { return now }
	s.Sync(ctx)
	total, ids := sumValues(fs.events)
	if total != want {
		t.Fatalf("reported %d cents, want %d", total, want)
	}
	for id, n := range ids {
		if n != 1 {
			t.Fatalf("identifier %s sent %d times", id, n)
		}
	}
}

func TestWindowAfter(t *testing.T) {
	w := time.Hour
	b := time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC)
	if got := windowAfter(b, w); !got.Equal(b.Add(time.Hour)) {
		t.Fatalf("boundary: %v", got)
	}
	if got := windowAfter(b.Add(17*time.Minute), w); !got.Equal(b.Add(time.Hour)) {
		t.Fatalf("inside: %v", got)
	}
	// Deterministic: the same cursor always yields the same window and id.
	c := b.Add(17 * time.Minute)
	if eventID("p", "c", c, windowAfter(c, w)) != eventID("p", "c", c, windowAfter(c, w)) {
		t.Fatal("unstable identifier")
	}
}
