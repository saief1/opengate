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

	"github.com/opengate-dev/opengate/internal/cloud"
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
	Client *http.Client

	mu        sync.Mutex
	customers map[string]map[string]any
	sessions  map[string]*session
	portals   map[string]*portal
	subs      map[string]*subscription
	idem      map[string]string // idempotency key -> customer id
	calls     []string
	events    []string // delivered event types
	seq       int
}

type session struct {
	ID, Customer, Price, SuccessURL, CancelURL, ClientRef, Subscription string
	Metadata, SubMetadata                                               map[string]string
	TrialDays                                                           int
	Completed                                                           bool
}

type portal struct{ ID, Customer, ReturnURL string }

type subscription struct {
	ID, Customer, Status, Price string
	PeriodEnd                   int64
	CancelAtPeriodEnd           bool
	Metadata                    map[string]string
}

// New returns an empty mock.
func New(baseURL, webhookURL, secret string) *Mock {
	return &Mock{BaseURL: strings.TrimRight(baseURL, "/"), WebhookURL: webhookURL, WebhookSecret: secret,
		customers: map[string]map[string]any{}, sessions: map[string]*session{}, portals: map[string]*portal{},
		subs: map[string]*subscription{}, idem: map[string]string{}, Client: &http.Client{Timeout: 10 * time.Second}}
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
			m.mu.Unlock()
			fn(w, r)
		})
	}
	api("POST /v1/customers", m.createCustomer)
	api("POST /v1/checkout/sessions", m.createSession)
	api("POST /v1/billing_portal/sessions", m.createPortal)
	api("GET /v1/subscriptions/{id}", m.getSubscription)
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
	trial, _ := strconv.Atoi(f.Get("subscription_data[trial_period_days]"))
	s := &session{ID: m.id("cs_test_"), Customer: f.Get("customer"), Price: f.Get("line_items[0][price]"), SuccessURL: f.Get("success_url"),
		CancelURL: f.Get("cancel_url"), ClientRef: f.Get("client_reference_id"), Metadata: meta(f, "metadata"),
		SubMetadata: meta(f, "subscription_data[metadata]"), TrialDays: trial}
	m.sessions[s.ID] = s
	writeJSON(w, map[string]any{"id": s.ID, "object": "checkout.session", "url": m.BaseURL + "/checkout/" + s.ID, "mode": "subscription",
		"customer": s.Customer, "client_reference_id": s.ClientRef, "status": "open"})
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
		"cancel_at_period_end": s.CancelAtPeriodEnd, "metadata": s.Metadata,
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
	if s == nil || s.Completed {
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

// CancelAtPeriodEnd schedules cancellation (customer.subscription.updated).
func (m *Mock) CancelAtPeriodEnd(id string) error {
	return m.mutate(id, func(s *subscription) { s.CancelAtPeriodEnd = true }, "customer.subscription.updated", m.subObj)
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
	body, _ = json.Marshal(map[string]any{"id": id, "object": "event", "type": typ, "created": time.Now().Unix(), "livemode": false,
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
		if !s.Completed {
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
			{"Renews", time.Unix(s.PeriodEnd, 0).UTC().Format("2006-01-02")}, {"Cancel at period end", strconv.FormatBool(s.CancelAtPeriodEnd)}}
		base := "/portal/" + pt.ID + "/"
		if s.Status == "past_due" {
			p.Buttons = append(p.Buttons, [3]string{base + "recover", "Update card & pay now", "pay"})
		} else {
			p.Buttons = append(p.Buttons, [3]string{base + "fail-payment", "Simulate failed renewal", "alt"})
		}
		if !s.CancelAtPeriodEnd {
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
