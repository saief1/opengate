package billing

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/saief1/opengate/internal/config"
	"github.com/saief1/opengate/internal/store"
	"github.com/saief1/opengate/internal/storetest"
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
}

func (f *fakeStripe) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path != "/v1/billing/meter_events" || r.Header.Get("Authorization") != "Bearer sk_test_fake" {
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
	cfg := config.StripeConfig{Enabled: true, BaseURL: srv.URL, EventName: "opengate_usage", Unit: "cents", Interval: time.Hour, Lag: time.Minute}
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
