// Package stripemock is an in-process stand-in for the parts of Stripe that
// OpenGate Cloud uses: customers, subscription Checkout Sessions (with a
// hosted "pay" page), Customer Portal sessions (with cancel / failed-payment
// buttons), subscription retrieval, and signed webhook delivery. It is for
// tests and demos only and never talks to real Stripe.
package stripemock

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/saief1/opengate/internal/cloud"
)

// Mock is a fake Stripe server. Create with New, then serve Handler().
type Mock struct {
	// BaseURL is where the mock is reachable (used in hosted page URLs).
	BaseURL string
	// WebhookURL and WebhookSecret configure event delivery.
	WebhookURL    string
	WebhookSecret string
	// APIKey, when set, is required as the bearer token.
	APIKey string
	// Livemode is the livemode flag on delivered events (default false).
	Livemode bool
	Client   *http.Client

	mu        sync.Mutex
	customers map[string]map[string]any
	sessions  map[string]*session
	portals   map[string]*portal
	subs      map[string]*subscription
	idem      map[string]string // idempotency key -> customer id
	sessIdem  map[string]string // idempotency key -> checkout session id
	charges   map[string]map[string]any
	disputes  map[string]map[string]any
	versions  map[string]int // Stripe-Version header values seen
	calls     []string
	events    []string // delivered event types
	seq       int
}

type session struct {
	ID, Customer, Price, SuccessURL, CancelURL, ClientRef, Subscription string
	Metadata, SubMetadata                                               map[string]string
	TrialDays                                                           int
	Completed, Expired                                                  bool
	PaymentMethodCollection                                             string
}

func (s *session) status() string {
	switch {
	case s.Completed:
		return "complete"
	case s.Expired:
		return "expired"
	}
	return "open"
}

type portal struct{ ID, Customer, ReturnURL string }

type subscription struct {
	ID, Customer, Status, Price string
	PeriodEnd, CancelAt         int64
	CancelAtPeriodEnd           bool
	Metadata                    map[string]string
}

// New returns an empty mock.
func New(baseURL, webhookURL, secret string) *Mock {
	return &Mock{BaseURL: strings.TrimRight(baseURL, "/"), WebhookURL: webhookURL, WebhookSecret: secret,
		customers: map[string]map[string]any{}, sessions: map[string]*session{}, portals: map[string]*portal{},
		subs: map[string]*subscription{}, idem: map[string]string{}, sessIdem: map[string]string{},
		charges: map[string]map[string]any{}, disputes: map[string]map[string]any{}, versions: map[string]int{},
		Client: &http.Client{Timeout: 10 * time.Second}}
}

func (m *Mock) id(prefix string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	m.seq++
	return prefix + hex.EncodeToString(b)
}

// Calls lists "METHOD /path" for every API request (tests).
func (m *Mock) Calls() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.calls...)
}

// Events lists delivered webhook event types (tests).
func (m *Mock) Events() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.events...)
}

// Handler serves the fake Stripe API and hosted pages.
func (m *Mock) Handler() http.Handler {
	mux := http.NewServeMux()
	api := func(pattern string, fn func(w http.ResponseWriter, r *http.Request)) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			if m.APIKey != "" && r.Header.Get("Authorization") != "Bearer "+m.APIKey {
				stripeErr(w, 401, "authentication_error", "Invalid API Key provided")
				return
			}
			_ = r.ParseForm()
			m.mu.Lock()
			m.calls = append(m.calls, r.Method+" "+r.URL.Path)
			m.versions[r.Header.Get("Stripe-Version")]++
			m.mu.Unlock()
			fn(w, r)
		})
	}
	api("POST /v1/customers", m.createCustomer)
	api("POST /v1/checkout/sessions", m.createSession)
	api("POST /v1/billing_portal/sessions", m.createPortal)
	api("GET /v1/subscriptions/{id}", m.getSubscription)
	api("GET /v1/subscriptions", m.listSubscriptions)
	api("DELETE /v1/subscriptions/{id}", m.deleteSubscription)
	api("GET /v1/checkout/sessions/{id}", m.getSession)
	api("POST /v1/checkout/sessions/{id}/expire", m.expireSession)
	api("GET /v1/charges/{id}", m.getObj(func() map[string]map[string]any { return m.charges }, "charge"))
	api("GET /v1/disputes/{id}", m.getObj(func() map[string]map[string]any { return m.disputes }, "dispute"))
	mux.HandleFunc("GET /checkout/{id}", m.checkoutPage)
	mux.HandleFunc("POST /checkout/{id}/pay", func(w http.ResponseWriter, r *http.Request) {
		redirect, err := m.Pay(r.PathValue("id"))
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		http.Redirect(w, r, redirect, http.StatusSeeOther)
	})
	mux.HandleFunc("GET /portal/{id}", m.portalPage)
	mux.HandleFunc("POST /portal/{id}/{action}", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		p := m.portals[r.PathValue("id")]
		m.mu.Unlock()
		if p == nil {
			http.NotFound(w, r)
			return
		}
		sub := m.subFor(p.Customer)
		var err error
		switch r.PathValue("action") {
		case "cancel":
			err = m.Cancel(sub)
		case "cancel-at-period-end":
			err = m.CancelAtPeriodEnd(sub)
		case "fail-payment":
			err = m.FailPayment(sub)
		case "recover":
			err = m.RecoverPayment(sub)
		}
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		http.Redirect(w, r, "/portal/"+p.ID, http.StatusSeeOther)
	})
	return mux
}

func stripeErr(w http.ResponseWriter, code int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"type": typ, "message": msg}})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func meta(f url.Values, prefix string) map[string]string {
	out := map[string]string{}
	for k, v := range f {
		if strings.HasPrefix(k, prefix+"[") && strings.HasSuffix(k, "]") && len(v) > 0 {
			out[strings.TrimSuffix(strings.TrimPrefix(k, prefix+"["), "]")] = v[0]
		}
	}
	return out
}

func (m *Mock) createCustomer(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if k := r.Header.Get("Idempotency-Key"); k != "" {
		if id, ok := m.idem[k]; ok {
			writeJSON(w, m.customers[id])
			return
		}
	}
	c := map[string]any{"id": m.id("cus_"), "object": "customer", "email": r.Form.Get("email"), "name": r.Form.Get("name"), "metadata": meta(r.Form, "metadata")}
	m.customers[c["id"].(string)] = c
	if k := r.Header.Get("Idempotency-Key"); k != "" {
		m.idem[k] = c["id"].(string)
	}
	writeJSON(w, c)
}

func (m *Mock) createSession(w http.ResponseWriter, r *http.Request) {
	f := r.Form
	m.mu.Lock()
	defer m.mu.Unlock()
	if f.Get("mode") != "subscription" || f.Get("line_items[0][price]") == "" || f.Get("success_url") == "" || f.Get("cancel_url") == "" {
		stripeErr(w, 400, "invalid_request_error", "mode=subscription, line_items[0][price], success_url and cancel_url are required")
		return
	}
	if _, ok := m.customers[f.Get("customer")]; !ok {
		stripeErr(w, 400, "invalid_request_error", "No such customer: '"+f.Get("customer")+"'")
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if id, ok := m.sessIdem[key]; ok && key != "" {
		writeJSON(w, m.sessions[id].json(m.BaseURL))
		return
	}
	trial, _ := strconv.Atoi(f.Get("subscription_data[trial_period_days]"))
	s := &session{ID: m.id("cs_test_"), Customer: f.Get("customer"), Price: f.Get("line_items[0][price]"), SuccessURL: f.Get("success_url"),
		CancelURL: f.Get("cancel_url"), ClientRef: f.Get("client_reference_id"), Metadata: meta(f, "metadata"),
		SubMetadata: meta(f, "subscription_data[metadata]"), TrialDays: trial, PaymentMethodCollection: f.Get("payment_method_collection")}
	m.sessions[s.ID] = s
	if key != "" {
		m.sessIdem[key] = s.ID
	}
	writeJSON(w, s.json(m.BaseURL))
}

func (s *session) json(base string) map[string]any {
	o := map[string]any{"id": s.ID, "object": "checkout.session", "mode": "subscription",
		"customer": s.Customer, "client_reference_id": s.ClientRef, "status": s.status(), "metadata": s.Metadata}
	if s.status() == "open" {
		o["url"] = base + "/checkout/" + s.ID
	}
	if s.Subscription != "" {
		o["subscription"] = s.Subscription
	}
	return o
}

func (m *Mock) getSession(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessions[r.PathValue("id")]
	if s == nil {
		stripeErr(w, 404, "invalid_request_error", "No such checkout.session")
		return
	}
	writeJSON(w, s.json(m.BaseURL))
}

func (m *Mock) expireSession(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	s := m.sessions[r.PathValue("id")]
	if s == nil || s.status() != "open" {
		m.mu.Unlock()
		stripeErr(w, 400, "invalid_request_error", "Only open Checkout Sessions can be expired")
		return
	}
	s.Expired = true
	out := s.json(m.BaseURL)
	m.mu.Unlock()
	writeJSON(w, out)
}

// SessionStatus returns open|complete|expired ("" if unknown) (tests).
func (m *Mock) SessionStatus(id string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.sessions[id]; s != nil {
		return s.status()
	}
	return ""
}

// SessionTrial reports a session's trial days and payment_method_collection (tests).
func (m *Mock) SessionTrial(id string) (int, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.sessions[id]; s != nil {
		return s.TrialDays, s.PaymentMethodCollection
	}
	return 0, ""
}

// Versions returns the Stripe-Version header values seen (tests).
func (m *Mock) Versions() map[string]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]int{}
	for k, v := range m.versions {
		out[k] = v
	}
	return out
}

func (m *Mock) listSubscriptions(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data := []any{}
	for _, s := range m.subs {
		if s.Customer == r.URL.Query().Get("customer") {
			data = append(data, s.json())
		}
	}
	writeJSON(w, map[string]any{"object": "list", "data": data})
}

func (m *Mock) deleteSubscription(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	m.mu.Lock()
	s := m.subs[id]
	if s == nil {
		m.mu.Unlock()
		stripeErr(w, 404, "invalid_request_error", "No such subscription")
		return
	}
	already := s.Status == "canceled"
	s.Status = "canceled"
	out := s.json()
	m.mu.Unlock()
	writeJSON(w, out)
	if !already {
		go func() { _ = m.deliver("customer.subscription.deleted", out) }()
	}
}

func (m *Mock) getObj(store func() map[string]map[string]any, kind string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		o := store()[r.PathValue("id")]
		m.mu.Unlock()
		if o == nil {
			stripeErr(w, 404, "invalid_request_error", "No such "+kind)
			return
		}
		writeJSON(w, o)
	}
}

// Charge creates a paid charge for the customer (tests of risk events).
func (m *Mock) Charge(customer string, amount int64) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := m.id("ch_")
	m.charges[id] = map[string]any{"id": id, "object": "charge", "customer": customer, "amount": amount, "amount_refunded": 0, "currency": "usd", "refunded": false}
	return id
}

// Dispute opens a dispute on a charge (charge.dispute.created).
func (m *Mock) Dispute(charge, reason string) (string, error) {
	m.mu.Lock()
	ch := m.charges[charge]
	if ch == nil {
		m.mu.Unlock()
		return "", fmt.Errorf("no such charge %q", charge)
	}
	id := m.id("dp_")
	d := map[string]any{"id": id, "object": "dispute", "charge": charge, "amount": ch["amount"], "currency": "usd", "reason": reason, "status": "needs_response"}
	m.disputes[id] = d
	m.mu.Unlock()
	return id, m.deliver("charge.dispute.created", d)
}

// CloseDispute closes a dispute as won or lost (charge.dispute.closed).
func (m *Mock) CloseDispute(id, status string) error {
	m.mu.Lock()
	d := m.disputes[id]
	if d == nil {
		m.mu.Unlock()
		return fmt.Errorf("no such dispute %q", id)
	}
	d["status"] = status
	out := map[string]any{}
	for k, v := range d {
		out[k] = v
	}
	m.mu.Unlock()
	return m.deliver("charge.dispute.closed", out)
}

// Refund fully refunds a charge (charge.refunded).
func (m *Mock) Refund(charge string) error {
	m.mu.Lock()
	ch := m.charges[charge]
	if ch == nil {
		m.mu.Unlock()
		return fmt.Errorf("no such charge %q", charge)
	}
	ch["amount_refunded"], ch["refunded"] = ch["amount"], true
	out := map[string]any{}
	for k, v := range ch {
		out[k] = v
	}
	m.mu.Unlock()
	return m.deliver("charge.refunded", out)
}

// EarlyFraudWarning raises a Radar early fraud warning on a charge.
func (m *Mock) EarlyFraudWarning(charge string) error {
	m.mu.Lock()
	id := m.id("issfr_")
	m.mu.Unlock()
	return m.deliver("radar.early_fraud_warning.created", map[string]any{"id": id, "object": "radar.early_fraud_warning",
		"charge": charge, "fraud_type": "made_with_stolen_card", "actionable": true})
}

func (m *Mock) createPortal(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.customers[r.Form.Get("customer")]; !ok {
		stripeErr(w, 400, "invalid_request_error", "No such customer")
		return
	}
	p := &portal{ID: m.id("bps_"), Customer: r.Form.Get("customer"), ReturnURL: r.Form.Get("return_url")}
	m.portals[p.ID] = p
	writeJSON(w, map[string]any{"id": p.ID, "object": "billing_portal.session", "url": m.BaseURL + "/portal/" + p.ID, "customer": p.Customer})
}

func (s *subscription) json() map[string]any {
	return map[string]any{"id": s.ID, "object": "subscription", "customer": s.Customer, "status": s.Status,
		"cancel_at_period_end": s.CancelAtPeriodEnd, "cancel_at": s.CancelAt, "metadata": s.Metadata,
		// current_period_end on the item, like current Stripe API versions.
		"items": map[string]any{"data": []any{map[string]any{"current_period_end": s.PeriodEnd, "price": map[string]any{"id": s.Price}}}}}
}

func (m *Mock) getSubscription(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	s := m.subs[r.PathValue("id")]
	var out map[string]any
	if s != nil {
		out = s.json()
	}
	m.mu.Unlock()
	if s == nil {
		stripeErr(w, 404, "invalid_request_error", "No such subscription")
		return
	}
	writeJSON(w, out)
}

// Pay completes a Checkout Session: creates the subscription, delivers
// checkout.session.completed + customer.subscription.created, and returns
// the success URL to redirect to.
func (m *Mock) Pay(sessionID string) (string, error) {
	m.mu.Lock()
	s := m.sessions[sessionID]
	if s == nil || s.status() != "open" {
		m.mu.Unlock()
		return "", fmt.Errorf("checkout session %q is not open", sessionID)
	}
	s.Completed = true
	status := "active"
	if s.TrialDays > 0 {
		status = "trialing"
	}
	sub := &subscription{ID: m.id("sub_"), Customer: s.Customer, Status: status, Price: s.Price,
		PeriodEnd: time.Now().AddDate(0, 1, 0).Unix(), Metadata: s.SubMetadata}
	m.subs[sub.ID] = sub
	s.Subscription = sub.ID
	cs := map[string]any{"id": s.ID, "object": "checkout.session", "mode": "subscription", "status": "complete", "payment_status": "paid",
		"customer": s.Customer, "subscription": sub.ID, "client_reference_id": s.ClientRef, "metadata": s.Metadata}
	subJSON := sub.json()
	m.mu.Unlock()
	if err := m.deliver("customer.subscription.created", subJSON); err != nil {
		return "", err
	}
	if err := m.deliver("checkout.session.completed", cs); err != nil {
		return "", err
	}
	return strings.ReplaceAll(s.SuccessURL, "{CHECKOUT_SESSION_ID}", s.ID), nil
}

func (m *Mock) subFor(customer string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, s := range m.subs {
		if s.Customer == customer && s.Status != "canceled" {
			return id
		}
	}
	return ""
}

// SubscriptionFor returns the customer's live subscription id (tests).
func (m *Mock) SubscriptionFor(customer string) string { return m.subFor(customer) }

func (m *Mock) mutate(id string, fn func(s *subscription), event string, obj func(s *subscription) map[string]any) error {
	m.mu.Lock()
	s := m.subs[id]
	if s == nil {
		m.mu.Unlock()
		return fmt.Errorf("no such subscription %q", id)
	}
	fn(s)
	o := obj(s)
	m.mu.Unlock()
	return m.deliver(event, o)
}

func (m *Mock) subObj(s *subscription) map[string]any { return s.json() }

func invoiceObj(s *subscription) map[string]any {
	// Newer API shape: the subscription id lives under parent.subscription_details.
	return map[string]any{"id": "in_" + s.ID, "object": "invoice", "customer": s.Customer,
		"parent": map[string]any{"subscription_details": map[string]any{"subscription": s.ID}}}
}

// Cancel ends the subscription immediately (customer.subscription.deleted).
func (m *Mock) Cancel(id string) error {
	return m.mutate(id, func(s *subscription) { s.Status = "canceled" }, "customer.subscription.deleted", m.subObj)
}

// CancelAtPeriodEnd schedules cancellation (customer.subscription.updated)
// the way the real Customer Portal does on API 2026-09-30: cancel_at is set
// to the period end and cancel_at_period_end stays false.
func (m *Mock) CancelAtPeriodEnd(id string) error {
	return m.mutate(id, func(s *subscription) { s.CancelAt = s.PeriodEnd }, "customer.subscription.updated", m.subObj)
}

// FailPayment marks the subscription past_due (invoice.payment_failed).
func (m *Mock) FailPayment(id string) error {
	return m.mutate(id, func(s *subscription) { s.Status = "past_due" }, "invoice.payment_failed", invoiceObj)
}

// RecoverPayment marks the subscription active again (invoice.paid).
func (m *Mock) RecoverPayment(id string) error {
	return m.mutate(id, func(s *subscription) { s.Status = "active" }, "invoice.paid", invoiceObj)
}

// SetStatus changes a subscription's status without sending an event (tests
// of out-of-order delivery).
func (m *Mock) SetStatus(id, status string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.subs[id]; s != nil {
		s.Status = status
	}
}

// Event builds a signed event payload without delivering it (tests).
func (m *Mock) Event(typ string, obj any) (body []byte, signature string) {
	m.mu.Lock()
	id := m.id("evt_")
	m.mu.Unlock()
	body, _ = json.Marshal(map[string]any{"id": id, "object": "event", "type": typ, "created": time.Now().Unix(), "livemode": m.Livemode,
		"data": map[string]any{"object": obj}})
	return body, cloud.Sign(body, m.WebhookSecret, time.Now())
}

func (m *Mock) deliver(typ string, obj any) error {
	if m.WebhookURL == "" {
		return nil
	}
	body, sig := m.Event(typ, obj)
	req, err := http.NewRequest("POST", m.WebhookURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Stripe-Signature", sig)
	resp, err := m.Client.Do(req)
	if err != nil {
		return fmt.Errorf("deliver %s: %w", typ, err)
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("deliver %s: webhook answered HTTP %d", typ, resp.StatusCode)
	}
	m.mu.Lock()
	m.events = append(m.events, typ)
	m.mu.Unlock()
	return nil
}

var pageTmpl = template.Must(template.New("p").Parse(`<!doctype html><html><head><meta charset="utf-8"><title>{{.Title}} · Stripe (mock)</title>
<style>body{font:15px/1.5 system-ui,sans-serif;background:#f6f9fc;color:#1a1f36;margin:0;display:grid;place-items:center;min-height:100vh}
.card{background:#fff;border-radius:12px;box-shadow:0 8px 30px rgba(50,50,93,.12);padding:32px 36px;width:420px}
.tag{display:inline-block;background:#ffde92;color:#5a3b00;border-radius:4px;font-size:12px;font-weight:600;padding:2px 8px;margin-bottom:12px}
h1{font-size:20px;margin:0 0 4px}.muted{color:#697386;font-size:13px}dl{display:grid;grid-template-columns:auto 1fr;gap:6px 16px;margin:20px 0}
dt{color:#697386}dd{margin:0;font-family:ui-monospace,monospace;font-size:13px}button,a.b{display:block;width:100%;box-sizing:border-box;border:0;border-radius:6px;padding:11px;font-size:15px;
font-weight:600;cursor:pointer;margin-top:10px;text-align:center;text-decoration:none}.pay{background:#635bff;color:#fff}.alt{background:#e3e8ee;color:#1a1f36}.warn{background:#fde2dd;color:#a41c4e}</style></head>
<body><div class="card"><span class="tag">TEST MODE · mock Stripe, no real charges</span><h1>{{.Title}}</h1><div class="muted">{{.Sub}}</div><dl>{{range .Rows}}<dt>{{index . 0}}</dt><dd>{{index . 1}}</dd>{{end}}</dl>
{{range .Buttons}}<form method="post" action="{{index . 0}}"><button class="{{index . 2}}">{{index . 1}}</button></form>{{end}}
{{if .Back}}<a class="b alt" href="{{.Back}}">{{.BackLabel}}</a>{{end}}</div></body></html>`))

type page struct {
	Title, Sub, Back, BackLabel string
	Rows, Buttons               [][3]string
}

func (m *Mock) checkoutPage(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	s := m.sessions[r.PathValue("id")]
	var p page
	if s != nil {
		c := m.customers[s.Customer]
		p = page{Title: "Subscribe to OpenGate Cloud", Sub: "Checkout Session " + s.ID, Back: s.CancelURL, BackLabel: "Cancel",
			Rows: [][3]string{{"Customer", fmt.Sprint(c["email"])}, {"Organization", fmt.Sprint(c["name"])}, {"Price", s.Price}, {"Card", "4242 4242 4242 4242 (test)"}}}
		if s.TrialDays > 0 {
			p.Rows = append(p.Rows, [3]string{"Trial", strconv.Itoa(s.TrialDays) + " days"})
		}
		if s.status() == "open" {
			p.Buttons = [][3]string{{"/checkout/" + s.ID + "/pay", "Subscribe", "pay"}}
		}
	}
	m.mu.Unlock()
	if s == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = pageTmpl.Execute(w, p)
}

func (m *Mock) portalPage(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	pt := m.portals[r.PathValue("id")]
	m.mu.Unlock()
	if pt == nil {
		http.NotFound(w, r)
		return
	}
	subID := m.subFor(pt.Customer)
	m.mu.Lock()
	p := page{Title: "Manage billing", Sub: "Customer portal · " + pt.Customer, Back: pt.ReturnURL, BackLabel: "Return to OpenGate"}
	if s := m.subs[subID]; s != nil {
		p.Rows = [][3]string{{"Subscription", s.ID}, {"Price", s.Price}, {"Status", s.Status},
			{"Renews", time.Unix(s.PeriodEnd, 0).UTC().Format("2006-01-02")}, {"Cancel at period end", strconv.FormatBool(s.CancelAtPeriodEnd || s.CancelAt > 0)}}
		base := "/portal/" + pt.ID + "/"
		if s.Status == "past_due" {
			p.Buttons = append(p.Buttons, [3]string{base + "recover", "Update card & pay now", "pay"})
		} else {
			p.Buttons = append(p.Buttons, [3]string{base + "fail-payment", "Simulate failed renewal", "alt"})
		}
		if !s.CancelAtPeriodEnd && s.CancelAt == 0 {
			p.Buttons = append(p.Buttons, [3]string{base + "cancel-at-period-end", "Cancel at period end", "alt"})
		}
		p.Buttons = append(p.Buttons, [3]string{base + "cancel", "Cancel immediately", "warn"})
	} else {
		p.Rows = [][3]string{{"Subscription", "none"}}
	}
	m.mu.Unlock()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = pageTmpl.Execute(w, p)
}

// AddSubscription inserts a subscription directly (tests of foreign or
// crafted subscriptions).
func (m *Mock) AddSubscription(id, customer, status, price string, metadata map[string]string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.subs[id] = &subscription{ID: id, Customer: customer, Status: status, Price: price, PeriodEnd: time.Now().AddDate(0, 1, 0).Unix(), Metadata: metadata}
}

// Deliver sends a signed event for obj to the webhook URL (tests).
func (m *Mock) Deliver(typ string, obj any) error { return m.deliver(typ, obj) }

// Subscription returns the JSON form of a subscription (tests).
func (m *Mock) Subscription(id string) map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.subs[id]; s != nil {
		return s.json()
	}
	return nil
}
