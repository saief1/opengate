// Package cloud implements OpenGate Cloud billing: plan resolution and
// Stripe subscriptions (Checkout, Customer Portal, signed webhooks).
// It talks to Stripe's REST API with the standard library only.
package cloud

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Stripe is a minimal Stripe API client.
type Stripe struct {
	BaseURL string
	APIKey  string
	// APIVersion is sent as Stripe-Version on every request so responses
	// don't change shape when the account's default version moves.
	APIVersion string
	Client     *http.Client
}

// Livemode reports whether the API key is a live-mode key. Anything that
// isn't sk_live_/rk_live_ (test keys, mock keys) is treated as test mode.
func (s *Stripe) Livemode() bool {
	return strings.HasPrefix(s.APIKey, "sk_live_") || strings.HasPrefix(s.APIKey, "rk_live_")
}

// APIError is a non-2xx Stripe answer.
type APIError struct {
	Status  int
	Type    string
	Code    string
	Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("stripe HTTP %d: %s", e.Status, e.Message) }

func (s *Stripe) do(ctx context.Context, method, path string, form url.Values, idemKey string, out any) error {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, s.BaseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.APIKey)
	if s.APIVersion != "" {
		req.Header.Set("Stripe-Version", s.APIVersion)
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	c := s.Client
	if c == nil {
		c = &http.Client{Timeout: 20 * time.Second}
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error struct {
				Type    string `json:"type"`
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		return &APIError{Status: resp.StatusCode, Type: e.Error.Type, Code: e.Error.Code, Message: e.Error.Message}
	}
	return json.Unmarshal(data, out)
}

// ID is a Stripe reference that may arrive as a string or an expanded object.
type ID string

func (i *ID) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '{' {
		var o struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(b, &o); err != nil {
			return err
		}
		*i = ID(o.ID)
		return nil
	}
	var s *string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	if s != nil {
		*i = ID(*s)
	}
	return nil
}

// Customer is a Stripe customer.
type Customer struct {
	ID string `json:"id"`
}

// CreateCustomer creates a customer tagged with the org id. The idempotency
// key makes double clicks create a single customer.
func (s *Stripe) CreateCustomer(ctx context.Context, orgID, orgName, email string) (*Customer, error) {
	f := url.Values{}
	f.Set("email", email)
	f.Set("name", orgName)
	f.Set("metadata[org_id]", orgID)
	var c Customer
	return &c, s.do(ctx, "POST", "/v1/customers", f, "opengate-customer-"+orgID, &c)
}

// CheckoutParams describes a subscription Checkout Session.
type CheckoutParams struct {
	OrgID, CustomerID, PriceID, SuccessURL, CancelURL string
	// IdempotencyKey makes a retried or double-submitted create return the
	// same session instead of opening a second one.
	IdempotencyKey                    string
	TrialDays                         int
	TrialWithoutCard                  bool
	AllowPromotionCodes, AutomaticTax bool
}

// Session is a Checkout or portal session (only what we use).
type Session struct {
	ID                string            `json:"id"`
	URL               string            `json:"url"`
	Customer          ID                `json:"customer"`
	Subscription      ID                `json:"subscription"`
	ClientReferenceID string            `json:"client_reference_id"`
	Mode              string            `json:"mode"`
	Status            string            `json:"status"` // open | complete | expired
	PaymentStatus     string            `json:"payment_status"`
	Metadata          map[string]string `json:"metadata"`
}

// CreateCheckoutSession starts a hosted Stripe Checkout for a subscription.
func (s *Stripe) CreateCheckoutSession(ctx context.Context, p CheckoutParams) (*Session, error) {
	f := url.Values{}
	f.Set("mode", "subscription")
	f.Set("customer", p.CustomerID)
	f.Set("client_reference_id", p.OrgID)
	f.Set("line_items[0][price]", p.PriceID)
	f.Set("line_items[0][quantity]", "1")
	f.Set("success_url", p.SuccessURL)
	f.Set("cancel_url", p.CancelURL)
	f.Set("metadata[org_id]", p.OrgID)
	f.Set("subscription_data[metadata][org_id]", p.OrgID)
	if p.TrialDays > 0 {
		f.Set("subscription_data[trial_period_days]", strconv.Itoa(p.TrialDays))
		if p.TrialWithoutCard {
			f.Set("payment_method_collection", "if_required")
			f.Set("subscription_data[trial_settings][end_behavior][missing_payment_method]", "cancel")
		}
	}
	if p.AllowPromotionCodes {
		f.Set("allow_promotion_codes", "true")
	}
	if p.AutomaticTax {
		f.Set("automatic_tax[enabled]", "true")
		f.Set("customer_update[address]", "auto")
		f.Set("customer_update[name]", "auto")
		f.Set("tax_id_collection[enabled]", "true")
	}
	var out Session
	return &out, s.do(ctx, "POST", "/v1/checkout/sessions", f, p.IdempotencyKey, &out)
}

func validID(id, prefix string) error {
	if !strings.HasPrefix(id, prefix) || strings.ContainsAny(id, "/?#%") {
		return fmt.Errorf("invalid Stripe id %q", id)
	}
	return nil
}

// GetCheckoutSession retrieves a Checkout Session.
func (s *Stripe) GetCheckoutSession(ctx context.Context, id string) (*Session, error) {
	if err := validID(id, "cs_"); err != nil {
		return nil, err
	}
	var out Session
	return &out, s.do(ctx, "GET", "/v1/checkout/sessions/"+id, nil, "", &out)
}

// ExpireCheckoutSession expires an open Checkout Session so it can no longer
// be paid.
func (s *Stripe) ExpireCheckoutSession(ctx context.Context, id string) error {
	if err := validID(id, "cs_"); err != nil {
		return err
	}
	var out Session
	return s.do(ctx, "POST", "/v1/checkout/sessions/"+id+"/expire", url.Values{}, "", &out)
}

// ListSubscriptions returns the customer's subscriptions in any status
// (most recent first, up to 20).
func (s *Stripe) ListSubscriptions(ctx context.Context, customerID string) ([]*Subscription, error) {
	if err := validID(customerID, "cus_"); err != nil {
		return nil, err
	}
	q := url.Values{"customer": {customerID}, "status": {"all"}, "limit": {"20"}}
	var out struct {
		Data []*Subscription `json:"data"`
	}
	return out.Data, s.do(ctx, "GET", "/v1/subscriptions?"+q.Encode(), nil, "", &out)
}

// CancelSubscription cancels a subscription immediately.
func (s *Stripe) CancelSubscription(ctx context.Context, id, idemKey string) (*Subscription, error) {
	if err := validID(id, "sub_"); err != nil {
		return nil, err
	}
	var out Subscription
	return &out, s.do(ctx, "DELETE", "/v1/subscriptions/"+id, nil, idemKey, &out)
}

// Charge is the subset of a charge OpenGate needs for risk events.
type Charge struct {
	ID             string `json:"id"`
	Customer       ID     `json:"customer"`
	Amount         int64  `json:"amount"`
	AmountRefunded int64  `json:"amount_refunded"`
	Currency       string `json:"currency"`
	Refunded       bool   `json:"refunded"`
}

// GetCharge retrieves a charge.
func (s *Stripe) GetCharge(ctx context.Context, id string) (*Charge, error) {
	if err := validID(id, "ch_"); err != nil && validID(id, "py_") != nil {
		return nil, err
	}
	var out Charge
	return &out, s.do(ctx, "GET", "/v1/charges/"+id, nil, "", &out)
}

// Dispute is the subset of a dispute OpenGate needs.
type Dispute struct {
	ID       string `json:"id"`
	Charge   ID     `json:"charge"`
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
	Reason   string `json:"reason"`
	Status   string `json:"status"` // warning_needs_response | needs_response | under_review | won | lost | ...
}

// GetDispute retrieves a dispute (the source of truth for its status).
func (s *Stripe) GetDispute(ctx context.Context, id string) (*Dispute, error) {
	if err := validID(id, "dp_"); err != nil && validID(id, "du_") != nil {
		return nil, err
	}
	var out Dispute
	return &out, s.do(ctx, "GET", "/v1/disputes/"+id, nil, "", &out)
}

// EarlyFraudWarning is a Radar early fraud warning.
type EarlyFraudWarning struct {
	ID         string `json:"id"`
	Charge     ID     `json:"charge"`
	FraudType  string `json:"fraud_type"`
	Actionable bool   `json:"actionable"`
}

// CreatePortalSession opens the Stripe Customer Portal for a customer.
// configuration (bpc_...) is optional.
func (s *Stripe) CreatePortalSession(ctx context.Context, customerID, returnURL, configuration string) (*Session, error) {
	f := url.Values{}
	f.Set("customer", customerID)
	f.Set("return_url", returnURL)
	if configuration != "" {
		f.Set("configuration", configuration)
	}
	var out Session
	return &out, s.do(ctx, "POST", "/v1/billing_portal/sessions", f, "", &out)
}

// Subscription is the subset of a Stripe subscription OpenGate needs.
// Newer API versions moved current_period_end onto the subscription items;
// PeriodEnd handles both.
type Subscription struct {
	ID                string            `json:"id"`
	Customer          ID                `json:"customer"`
	Status            string            `json:"status"`
	CancelAtPeriodEnd bool              `json:"cancel_at_period_end"`
	CurrentPeriodEnd  int64             `json:"current_period_end"`
	Metadata          map[string]string `json:"metadata"`
	Items             struct {
		Data []struct {
			CurrentPeriodEnd int64 `json:"current_period_end"`
			Price            struct {
				ID string `json:"id"`
			} `json:"price"`
		} `json:"data"`
	} `json:"items"`
}

// PriceID returns the first item's price.
func (s *Subscription) PriceID() string {
	if len(s.Items.Data) == 0 {
		return ""
	}
	return s.Items.Data[0].Price.ID
}

// PeriodEnd returns the end of the current billing period.
func (s *Subscription) PeriodEnd() time.Time {
	v := s.CurrentPeriodEnd
	if v == 0 && len(s.Items.Data) > 0 {
		v = s.Items.Data[0].CurrentPeriodEnd
	}
	if v == 0 {
		return time.Time{}
	}
	return time.Unix(v, 0).UTC()
}

// GetSubscription fetches the current state of a subscription (webhooks are
// treated as notifications; this is the source of truth).
func (s *Stripe) GetSubscription(ctx context.Context, id string) (*Subscription, error) {
	if id == "" || strings.ContainsAny(id, "/?#") {
		return nil, errors.New("invalid subscription id")
	}
	var out Subscription
	return &out, s.do(ctx, "GET", "/v1/subscriptions/"+url.PathEscape(id), nil, "", &out)
}

// ---------- webhooks ----------

// ErrBadSignature means the Stripe-Signature header did not verify.
var ErrBadSignature = errors.New("invalid Stripe webhook signature")

// VerifySignature checks a Stripe-Signature header ("t=...,v1=...[,v1=...]")
// against the raw payload: HMAC-SHA256(secret, t + "." + payload), compared in
// constant time, with a timestamp tolerance against replays.
func VerifySignature(payload []byte, header, secret string, tolerance time.Duration, now time.Time) error {
	var ts int64
	var sigs [][]byte
	for _, part := range strings.Split(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch k {
		case "t":
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return ErrBadSignature
			}
			ts = n
		case "v1":
			if b, err := hex.DecodeString(v); err == nil {
				sigs = append(sigs, b)
			}
		}
	}
	if ts == 0 || len(sigs) == 0 || secret == "" {
		return ErrBadSignature
	}
	if d := now.Sub(time.Unix(ts, 0)); d > tolerance || d < -tolerance {
		return fmt.Errorf("%w: timestamp outside tolerance", ErrBadSignature)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%d.", ts)
	mac.Write(payload)
	want := mac.Sum(nil)
	for _, s := range sigs {
		if hmac.Equal(s, want) {
			return nil
		}
	}
	return ErrBadSignature
}

// Sign produces a Stripe-Signature header value (used by the mock and tests).
func Sign(payload []byte, secret string, t time.Time) string {
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%d.", t.Unix())
	mac.Write(payload)
	return fmt.Sprintf("t=%d,v1=%s", t.Unix(), hex.EncodeToString(mac.Sum(nil)))
}

// Event is a Stripe webhook event envelope.
type Event struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Livemode bool   `json:"livemode"`
	Data     struct {
		Object json.RawMessage `json:"object"`
	} `json:"data"`
}

// InvoiceSubscription extracts the subscription id from an invoice object
// (top-level "subscription" on older API versions, parent.subscription_details on newer).
func InvoiceSubscription(obj json.RawMessage) (sub, customer string) {
	var inv struct {
		Subscription ID `json:"subscription"`
		Customer     ID `json:"customer"`
		Parent       struct {
			SubscriptionDetails struct {
				Subscription ID `json:"subscription"`
			} `json:"subscription_details"`
		} `json:"parent"`
	}
	_ = json.Unmarshal(obj, &inv)
	sub = string(inv.Subscription)
	if sub == "" {
		sub = string(inv.Parent.SubscriptionDetails.Subscription)
	}
	return sub, string(inv.Customer)
}
