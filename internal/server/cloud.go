package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/opengate-dev/opengate/internal/budget"
	"github.com/opengate-dev/opengate/internal/cloud"
	"github.com/opengate-dev/opengate/internal/config"
	"github.com/opengate-dev/opengate/internal/store"
)

// cloudState is present only when cloud.enabled.
type cloudState struct {
	stripe *cloud.Stripe
	mu     sync.Mutex
	plans  map[string]cachedPlan // org -> effective plan
}

type cachedPlan struct {
	plan config.CloudPlan
	exp  time.Time
}

func newCloudState(cfg *config.Config, client *http.Client) *cloudState {
	return &cloudState{stripe: &cloud.Stripe{BaseURL: cfg.Cloud.Stripe.BaseURL, APIKey: cfg.StripeAPIKey, APIVersion: cfg.Cloud.Stripe.APIVersion, Client: client},
		plans: map[string]cachedPlan{}}
}

// orgPlan returns the org's effective plan (cached for a few seconds; a
// webhook on this replica invalidates immediately).
func (s *Server) orgPlan(ctx context.Context, org string) (config.CloudPlan, error) {
	if org == store.DefaultOrgID {
		// The operator's own org is never plan-limited.
		return config.CloudPlan{ID: "operator", Name: "Operator"}, nil
	}
	c := s.cloud
	now := time.Now()
	c.mu.Lock()
	if e, ok := c.plans[org]; ok && now.Before(e.exp) {
		c.mu.Unlock()
		return e.plan, nil
	}
	c.mu.Unlock()
	b, err := s.store.GetOrgBilling(ctx, org, s.cfg.Cloud.DefaultPlan)
	if err != nil {
		return config.CloudPlan{}, err
	}
	p := cloud.Effective(&s.cfg.Cloud, b, now)
	c.mu.Lock()
	if len(c.plans) > 100_000 {
		c.plans = map[string]cachedPlan{}
	}
	c.plans[org] = cachedPlan{plan: p, exp: now.Add(5 * time.Second)}
	c.mu.Unlock()
	return p, nil
}

func (s *Server) forgetPlan(org string) {
	s.cloud.mu.Lock()
	delete(s.cloud.plans, org)
	s.cloud.mu.Unlock()
}

// cloudGate counts a gateway request against the org's monthly plan quota.
func (s *Server) cloudGate(w http.ResponseWriter, rs *reqState) bool {
	if s.cloud == nil {
		return true
	}
	ctx := rs.r.Context()
	plan, err := s.orgPlan(ctx, rs.proj.OrgID)
	if err == nil {
		var n int64
		if n, err = s.store.IncrOrgUsage(ctx, rs.proj.OrgID, store.UsagePeriod(time.Now())); err == nil {
			if plan.MaxRequestsMonth > 0 && n > plan.MaxRequestsMonth {
				s.reject(w, rs, http.StatusTooManyRequests, "insufficient_quota", "plan_quota_exceeded",
					fmt.Sprintf("OpenGate Cloud: your organization used its %d included requests for this month on the %s plan. Upgrade in the dashboard (Billing) to continue.", plan.MaxRequestsMonth, plan.Name), 0)
				return false
			}
			return true
		}
	}
	s.log.Error("cloud plan check failed", "err", err, "request_id", rs.log.ID)
	s.reject(w, rs, http.StatusInternalServerError, "server_error", "internal_error", "plan check failed", 0)
	return false
}

// planLimit answers 402 when adding one more of something would exceed the
// org's plan; it returns false if the request was answered.
func (s *Server) planLimit(w http.ResponseWriter, r *http.Request, org, what string) bool {
	if s.cloud == nil {
		return true
	}
	plan, err := s.orgPlan(r.Context(), org)
	if err != nil {
		storeErr(w, err)
		return false
	}
	var used, max int
	switch what {
	case "projects":
		max = plan.MaxProjects
		if max > 0 {
			used, err = s.store.CountProjects(r.Context(), org)
		}
	case "members":
		max = plan.MaxMembers
		if max > 0 {
			var m, p int
			m, p, err = s.store.CountSeats(r.Context(), org, time.Now())
			used = m + p
		}
	}
	if err != nil {
		storeErr(w, err)
		return false
	}
	if max > 0 && used >= max {
		writeError(w, http.StatusPaymentRequired, "invalid_request_error", "plan_limit_reached",
			fmt.Sprintf("The %s plan includes %d %s. Upgrade under Billing to add more.", plan.Name, max, what))
		return false
	}
	return true
}

// seatAvailable is checked when an invite turns into a membership (pending
// invites may outnumber seats after a downgrade).
func (s *Server) seatAvailable(w http.ResponseWriter, r *http.Request, org string) bool {
	if s.cloud == nil {
		return true
	}
	plan, err := s.orgPlan(r.Context(), org)
	if err != nil {
		storeErr(w, err)
		return false
	}
	if plan.MaxMembers <= 0 {
		return true
	}
	members, _, err := s.store.CountSeats(r.Context(), org, time.Now())
	if err != nil {
		storeErr(w, err)
		return false
	}
	if members >= plan.MaxMembers {
		writeError(w, http.StatusPaymentRequired, "invalid_request_error", "plan_limit_reached",
			"This organization has no free seats on its plan; ask an owner to upgrade.")
		return false
	}
	return true
}

// orgCreationAllowed caps the orgs a user may own in cloud mode.
func (s *Server) orgCreationAllowed(w http.ResponseWriter, r *http.Request, userID string) bool {
	if s.cloud == nil || userID == "" {
		return true
	}
	n, err := s.store.CountOwnedOrgs(r.Context(), userID)
	if err != nil {
		storeErr(w, err)
		return false
	}
	if n >= s.cfg.Cloud.MaxOrgsPerUser {
		writeError(w, http.StatusForbidden, "permission_error", "org_limit_reached",
			fmt.Sprintf("You already own %d organizations, the maximum per account.", n))
		return false
	}
	return true
}

// ---------- admin API ----------

func (s *Server) registerCloud(mux *http.ServeMux, h func(string, http.HandlerFunc)) {
	if s.cloud == nil {
		return
	}
	h("GET /admin/api/billing", s.cloudBilling)
	h("POST /admin/api/billing/checkout", s.cloudCheckout)
	h("POST /admin/api/billing/portal", s.cloudPortal)
	h("PUT /admin/api/billing/plan", s.cloudSetPlan)
	h("POST /admin/api/billing/risk/clear", s.cloudClearRisk)
	mux.HandleFunc("POST /cloud/stripe/webhook", s.cloudWebhook)
}

func (s *Server) cloudBilling(w http.ResponseWriter, r *http.Request) {
	org := orgID(r)
	b, err := s.store.GetOrgBilling(r.Context(), org, s.cfg.Cloud.DefaultPlan)
	if err != nil {
		storeErr(w, err)
		return
	}
	now := time.Now()
	s.forgetPlan(org) // show fresh state right after a redirect from Stripe
	eff, err := s.orgPlan(r.Context(), org)
	if err != nil {
		storeErr(w, err)
		return
	}
	period := store.UsagePeriod(now)
	reqs, _ := s.store.OrgUsage(r.Context(), org, period)
	projects, _ := s.store.CountProjects(r.Context(), org)
	members, pending, _ := s.store.CountSeats(r.Context(), org, now)
	type planOut struct {
		config.CloudPlan
		SelfServe bool `json:"self_serve"`
	}
	plans := make([]planOut, 0, len(s.cfg.Cloud.Plans))
	for _, p := range s.cfg.Cloud.Plans {
		plans = append(plans, planOut{p, p.SelfServe()})
	}
	p := principalOf(r)
	writeJSON(w, 200, map[string]any{
		"plan": b.Plan, "effective_plan": eff, "status": b.Status, "current_period_end": b.CurrentPeriodEnd,
		"cancel_at_period_end": b.CancelAtPeriodEnd, "past_due_since": b.PastDueSince,
		"has_subscription": b.StripeSubscriptionID != "", "has_customer": b.StripeCustomerID != "",
		"risk_status": b.RiskStatus, "suspended": b.Suspended(),
		"usage": map[string]any{"period": period, "requests": reqs, "projects": projects, "members": members, "pending_invites": pending},
		"plans": plans, "sales_url": s.cfg.Cloud.SalesURL,
		"can_manage": p.Kind == "token" || p.Role == store.RoleOwner,
	})
}

// requireOwner: subscriptions and payment methods are owner-only.
func requireOwner(w http.ResponseWriter, r *http.Request) bool {
	if p := principalOf(r); p.Kind == "token" || p.Role == store.RoleOwner {
		return true
	}
	writeError(w, http.StatusForbidden, "permission_error", "insufficient_role", "only organization owners can manage billing")
	return false
}

func (s *Server) cloudCheckout(w http.ResponseWriter, r *http.Request) {
	if !requireOwner(w, r) {
		return
	}
	var in struct {
		Plan string `json:"plan"`
	}
	if err := decode(r, &in); err != nil {
		bad(w, err)
		return
	}
	plan, ok := s.cfg.Cloud.Plan(in.Plan)
	if !ok || !plan.SelfServe() {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "plan_not_purchasable", "This plan can't be bought online; contact sales.")
		return
	}
	org := orgID(r)
	b, err := s.store.GetOrgBilling(r.Context(), org, s.cfg.Cloud.DefaultPlan)
	if err != nil {
		storeErr(w, err)
		return
	}
	if b.StripeSubscriptionID != "" && cloud.LiveStatus(b.Status) {
		writeError(w, http.StatusConflict, "invalid_request_error", "subscription_exists", "This organization already has a subscription; change or cancel it in the billing portal.")
		return
	}
	if b.StripeCustomerID == "" {
		o, err := s.store.GetOrg(r.Context(), org)
		if err != nil {
			storeErr(w, err)
			return
		}
		email := ""
		if p := principalOf(r); p.User != nil {
			email = p.User.Email
		}
		c, err := s.cloud.stripe.CreateCustomer(r.Context(), org, o.Name, email)
		if err != nil {
			s.stripeFail(w, "create customer", err)
			return
		}
		b.StripeCustomerID = c.ID
		if err := s.store.PutOrgBilling(r.Context(), b); err != nil {
			storeErr(w, err)
			return
		}
	}
	// A1: Stripe is the source of truth. If the customer already has a live
	// subscription (a missed webhook, another tab), adopt it instead of
	// opening a second Checkout. Any past subscription also means the trial
	// was used.
	subs, err := s.cloud.stripe.ListSubscriptions(r.Context(), b.StripeCustomerID)
	if err != nil {
		s.stripeFail(w, "list subscriptions", err)
		return
	}
	for _, sub := range subs {
		if cloud.LiveStatus(sub.Status) {
			if err := s.applySubscription(r.Context(), "checkout", sub, org); err != nil {
				s.log.Warn("adopt existing subscription", "org_id", org, "err", err)
			}
			writeError(w, http.StatusConflict, "invalid_request_error", "subscription_exists", "This organization already has a subscription; change or cancel it in the billing portal.")
			return
		}
	}
	// Reuse the org's open Checkout Session for the same plan; expire one
	// for another plan, so at most one payable session exists per org.
	if b.CheckoutSessionID != "" {
		cs, err := s.cloud.stripe.GetCheckoutSession(r.Context(), b.CheckoutSessionID)
		switch {
		case err != nil:
			var ae *cloud.APIError
			if !errors.As(err, &ae) || ae.Status != http.StatusNotFound {
				s.stripeFail(w, "get checkout session", err)
				return
			}
		case cs.Status == "open" && b.CheckoutPlan == plan.ID && cs.URL != "":
			writeJSON(w, 200, map[string]any{"url": cs.URL, "id": cs.ID, "reused": true})
			return
		case cs.Status == "open":
			if err := s.cloud.stripe.ExpireCheckoutSession(r.Context(), cs.ID); err != nil {
				s.stripeFail(w, "expire checkout session", err)
				return
			}
		case cs.Status == "complete":
			writeError(w, http.StatusConflict, "invalid_request_error", "checkout_completed", "Payment received; your plan will update in a moment.")
			return
		}
	}
	trial := s.cfg.Cloud.Stripe.TrialDays
	if len(subs) > 0 {
		trial = 0 // one trial per organization
	}
	base := s.cfg.Cloud.PublicURL + "/ui/"
	// The idempotency key is derived from the state this request saw, so a
	// double submit or a retry after a lost response returns the same session.
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\x00%d", org, plan.ID, b.StripeCustomerID, b.CheckoutSessionID, trial)
	for _, sub := range subs {
		fmt.Fprintf(h, "\x00%s:%s", sub.ID, sub.Status)
	}
	params := cloud.CheckoutParams{
		OrgID: org, CustomerID: b.StripeCustomerID, PriceID: plan.StripePriceID,
		SuccessURL: base + "#billing?checkout=success&session_id={CHECKOUT_SESSION_ID}", CancelURL: base + "#billing?checkout=cancel",
		IdempotencyKey: "og-checkout-" + hex.EncodeToString(h.Sum(nil)[:12]),
		TrialDays:      trial, TrialWithoutCard: s.cfg.Cloud.Stripe.TrialWithoutCard,
		AllowPromotionCodes: s.cfg.Cloud.Stripe.AllowPromotionCodes, AutomaticTax: s.cfg.Cloud.Stripe.AutomaticTax,
	}
	sess, err := s.cloud.stripe.CreateCheckoutSession(r.Context(), params)
	if err == nil && sess.Status != "" && sess.Status != "open" {
		// The key replayed a session that has since expired: open a fresh one.
		params.IdempotencyKey += "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
		sess, err = s.cloud.stripe.CreateCheckoutSession(r.Context(), params)
	}
	if err != nil {
		s.stripeFail(w, "create checkout session", err)
		return
	}
	b.CheckoutSessionID, b.CheckoutPlan = sess.ID, plan.ID
	if err := s.store.PutOrgBilling(r.Context(), b); err != nil {
		storeErr(w, err)
		return
	}
	s.audit(r, "billing.checkout", org, map[string]string{"plan": plan.ID, "session": sess.ID})
	writeJSON(w, 200, map[string]any{"url": sess.URL, "id": sess.ID})
}

func (s *Server) cloudPortal(w http.ResponseWriter, r *http.Request) {
	if !requireOwner(w, r) {
		return
	}
	org := orgID(r)
	b, err := s.store.GetOrgBilling(r.Context(), org, s.cfg.Cloud.DefaultPlan)
	if err != nil {
		storeErr(w, err)
		return
	}
	if b.StripeCustomerID == "" {
		writeError(w, http.StatusConflict, "invalid_request_error", "no_billing_account", "Subscribe to a plan first.")
		return
	}
	sess, err := s.cloud.stripe.CreatePortalSession(r.Context(), b.StripeCustomerID, s.cfg.Cloud.PublicURL+"/ui/#billing", s.cfg.Cloud.Stripe.PortalConfiguration)
	if err != nil {
		s.stripeFail(w, "create portal session", err)
		return
	}
	s.audit(r, "billing.portal", org, nil)
	writeJSON(w, 200, map[string]string{"url": sess.URL})
}

// cloudSetPlan lets the operator (admin token) assign a plan by hand, e.g.
// Enterprise contracts invoiced outside Checkout, or comped accounts.
func (s *Server) cloudSetPlan(w http.ResponseWriter, r *http.Request) {
	if principalOf(r).Kind != "token" {
		writeError(w, http.StatusForbidden, "permission_error", "operator_only", "only the operator (admin token) can assign plans")
		return
	}
	var in struct {
		Plan string `json:"plan"`
	}
	if err := decode(r, &in); err != nil {
		bad(w, err)
		return
	}
	if _, ok := s.cfg.Cloud.Plan(in.Plan); !ok {
		bad(w, errors.New("unknown plan"))
		return
	}
	org := orgID(r)
	b, err := s.store.GetOrgBilling(r.Context(), org, s.cfg.Cloud.DefaultPlan)
	if err != nil {
		storeErr(w, err)
		return
	}
	if b.StripeSubscriptionID != "" && b.Status != "canceled" {
		writeError(w, http.StatusConflict, "invalid_request_error", "subscription_exists", "This org has a live Stripe subscription; change it in Stripe instead.")
		return
	}
	b.Plan, b.Status, b.StripeSubscriptionID = in.Plan, "active", ""
	if err := s.store.PutOrgBilling(r.Context(), b); err != nil {
		storeErr(w, err)
		return
	}
	s.forgetPlan(org)
	s.audit(r, "billing.plan.set", org, map[string]string{"plan": in.Plan})
	writeJSON(w, 200, b)
}

// cloudClearRisk lets the operator lift a dispute/fraud suspension after
// reviewing it in the Stripe Dashboard.
func (s *Server) cloudClearRisk(w http.ResponseWriter, r *http.Request) {
	if principalOf(r).Kind != "token" {
		writeError(w, http.StatusForbidden, "permission_error", "operator_only", "only the operator (admin token) can clear risk flags")
		return
	}
	org := orgID(r)
	b, err := s.store.GetOrgBilling(r.Context(), org, s.cfg.Cloud.DefaultPlan)
	if err != nil {
		storeErr(w, err)
		return
	}
	prev := b.RiskStatus
	b.RiskStatus, b.RiskDetail = "", ""
	if err := s.store.PutOrgBilling(r.Context(), b); err != nil {
		storeErr(w, err)
		return
	}
	s.forgetPlan(org)
	s.audit(r, "billing.risk.clear", org, map[string]string{"previous": prev})
	writeJSON(w, 200, b)
}

func (s *Server) stripeFail(w http.ResponseWriter, what string, err error) {
	s.log.Error("stripe "+what+" failed", "err", err)
	writeError(w, http.StatusBadGateway, "api_error", "billing_unavailable", "The billing provider is unavailable; please try again.")
}

// ---------- webhooks ----------

// cloudWebhook receives Stripe events. Events are verified, de-duplicated and
// treated as notifications: the subscription is re-fetched from Stripe, so
// out-of-order or replayed deliveries can't install stale state.
func (s *Server) cloudWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "invalid_request", "payload too large")
		return
	}
	if err := cloud.VerifySignature(body, r.Header.Get("Stripe-Signature"), s.cfg.StripeWebhookSecret, 5*time.Minute, time.Now()); err != nil {
		s.log.Warn("stripe webhook rejected", "err", err, "ip", s.clientIP(r))
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_signature", "invalid signature")
		return
	}
	var ev cloud.Event
	if err := json.Unmarshal(body, &ev); err != nil || ev.ID == "" || ev.Type == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_request", "malformed event")
		return
	}
	// A10: a test-mode event must never change state in a live deployment
	// (and vice versa), whatever endpoint it was sent to.
	if ev.Livemode != s.cloud.stripe.Livemode() {
		s.log.Warn("stripe webhook: livemode mismatch", "event", ev.ID, "event_livemode", ev.Livemode)
		writeError(w, http.StatusBadRequest, "invalid_request_error", "livemode_mismatch", "event livemode does not match this deployment's Stripe key")
		return
	}
	// A12: atomic claim (unique insert) so concurrent deliveries of one event
	// can't both process it.
	ctx := r.Context()
	claim, err := s.store.ClaimStripeEvent(ctx, ev.ID, ev.Type, 5*time.Minute)
	if err != nil {
		storeErr(w, err)
		return
	}
	switch claim {
	case store.EventDone:
		writeJSON(w, 200, map[string]any{"received": true, "duplicate": true})
		return
	case store.EventBusy:
		// Another delivery is processing it; a 409 makes Stripe retry later.
		writeError(w, http.StatusConflict, "invalid_request_error", "event_in_progress", "event is being processed")
		return
	}
	if err := s.handleStripeEvent(ctx, &ev); err != nil {
		_ = s.store.ReleaseStripeEvent(context.WithoutCancel(ctx), ev.ID)
		// 5xx makes Stripe retry with backoff for up to 3 days.
		s.log.Error("stripe webhook processing failed", "event", ev.ID, "type", ev.Type, "err", err)
		writeError(w, http.StatusInternalServerError, "server_error", "internal_error", "processing failed")
		return
	}
	if err := s.store.FinishStripeEvent(ctx, ev.ID); err != nil {
		s.log.Warn("finish stripe event", "err", err)
	}
	writeJSON(w, 200, map[string]any{"received": true})
}

func (s *Server) handleStripeEvent(ctx context.Context, ev *cloud.Event) error {
	switch {
	case ev.Type == "checkout.session.completed" || ev.Type == "checkout.session.expired":
		var cs cloud.Session
		if err := json.Unmarshal(ev.Data.Object, &cs); err != nil {
			return err
		}
		if cs.Mode != "subscription" || cs.ClientReferenceID == "" {
			return nil
		}
		// Forget the session first, so applying the subscription doesn't try
		// to expire the session that just completed.
		if err := s.clearCheckout(ctx, cs.ClientReferenceID, cs.ID); err != nil {
			return err
		}
		if cs.Subscription == "" {
			return nil
		}
		return s.syncSubscription(ctx, ev, string(cs.Subscription), cs.ClientReferenceID)
	case strings.HasPrefix(ev.Type, "customer.subscription."):
		var sub cloud.Subscription
		if err := json.Unmarshal(ev.Data.Object, &sub); err != nil {
			return err
		}
		return s.syncSubscription(ctx, ev, sub.ID, "")
	case ev.Type == "invoice.paid" || ev.Type == "invoice.payment_failed" || ev.Type == "invoice.payment_action_required":
		sub, _ := cloud.InvoiceSubscription(ev.Data.Object)
		if sub == "" {
			return nil
		}
		return s.syncSubscription(ctx, ev, sub, "")
	case strings.HasPrefix(ev.Type, "charge.dispute."):
		return s.handleDispute(ctx, ev)
	case ev.Type == "charge.refunded":
		return s.handleRefund(ctx, ev)
	case strings.HasPrefix(ev.Type, "radar.early_fraud_warning."):
		return s.handleFraudWarning(ctx, ev)
	}
	return nil // not an event we act on
}

// clearCheckout forgets the org's open session once it completed/expired.
func (s *Server) clearCheckout(ctx context.Context, org, sessionID string) error {
	b, err := s.store.GetOrgBilling(ctx, org, s.cfg.Cloud.DefaultPlan)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	if b.CheckoutSessionID != sessionID {
		return nil
	}
	b.CheckoutSessionID, b.CheckoutPlan = "", ""
	if _, err := s.store.GetOrg(ctx, org); err != nil {
		return nil // unknown org: nothing to clear
	}
	return s.store.PutOrgBilling(ctx, b)
}

// syncSubscription fetches the subscription and applies it to its org.
func (s *Server) syncSubscription(ctx context.Context, ev *cloud.Event, subID, org string) error {
	sub, err := s.cloud.stripe.GetSubscription(ctx, subID)
	if err != nil {
		var ae *cloud.APIError
		if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
			s.log.Warn("stripe webhook: subscription not found", "event", ev.ID, "subscription", subID)
			return nil
		}
		return err
	}
	return s.applySubscription(ctx, ev.ID+" "+ev.Type, sub, org)
}

// applySubscription applies a fetched subscription to its org. The org
// comes from the Checkout session's client_reference_id, the subscription
// metadata (both set by OpenGate), or the linked customer.
func (s *Server) applySubscription(ctx context.Context, source string, sub *cloud.Subscription, org string) error {
	if org == "" {
		org = sub.Metadata["org_id"]
	}
	var b *store.OrgBilling
	var err error
	if org != "" {
		if _, err := s.store.GetOrg(ctx, org); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				s.log.Warn("stripe: unknown org", "source", source, "org_id", org)
				return nil
			}
			return err
		}
		if b, err = s.store.GetOrgBilling(ctx, org, s.cfg.Cloud.DefaultPlan); err != nil {
			return err
		}
	} else if b, err = s.store.OrgBillingByCustomer(ctx, string(sub.Customer)); errors.Is(err, store.ErrNotFound) {
		s.log.Warn("stripe: no org for customer", "source", source, "customer", sub.Customer)
		return nil
	} else if err != nil {
		return err
	}
	before := *b
	switch cloud.ApplySubscription(&s.cfg.Cloud, b, sub, time.Now()) {
	case cloud.Ignored:
		s.log.Warn("stripe: subscription ignored", "source", source, "org_id", b.OrgID, "subscription", sub.ID,
			"customer", sub.Customer, "price", sub.PriceID(), "status", sub.Status)
		return nil
	case cloud.Duplicate:
		return s.cancelDuplicate(ctx, source, b, sub)
	}
	if cloud.LiveStatus(b.Status) && b.CheckoutSessionID != "" {
		// The org is subscribed now: any other open Checkout could only create
		// a duplicate, so close it.
		if err := s.cloud.stripe.ExpireCheckoutSession(ctx, b.CheckoutSessionID); err != nil {
			s.log.Debug("expire stale checkout session", "session", b.CheckoutSessionID, "err", err)
		}
		b.CheckoutSessionID, b.CheckoutPlan = "", ""
	}
	if err := s.store.PutOrgBilling(ctx, b); err != nil {
		if errors.Is(err, store.ErrConflict) {
			s.log.Warn("stripe: customer already linked to another org", "source", source, "org_id", b.OrgID, "customer", sub.Customer)
			return nil
		}
		return err
	}
	s.forgetPlan(b.OrgID)
	if before.Plan != b.Plan || before.Status != b.Status || before.CancelAtPeriodEnd != b.CancelAtPeriodEnd {
		s.billingAudit(ctx, b.OrgID, "billing.subscription", sub.ID, map[string]any{"plan": b.Plan, "status": b.Status, "from_plan": before.Plan,
			"from_status": before.Status, "cancel_at_period_end": b.CancelAtPeriodEnd, "source": source})
	}
	s.log.Info("subscription synced", "org_id", b.OrgID, "plan", b.Plan, "status", b.Status, "source", source)
	return nil
}

// cancelDuplicate cancels a second live subscription for an org that
// already has one, so the customer isn't billed twice.
func (s *Server) cancelDuplicate(ctx context.Context, source string, b *store.OrgBilling, sub *cloud.Subscription) error {
	if _, err := s.cloud.stripe.CancelSubscription(ctx, sub.ID, "og-cancel-dup-"+sub.ID); err != nil {
		var ae *cloud.APIError
		if !errors.As(err, &ae) || ae.Status != http.StatusNotFound {
			return err // retry via Stripe's webhook retries
		}
	}
	s.billingAudit(ctx, b.OrgID, "billing.subscription.duplicate_canceled", sub.ID, map[string]any{"current": b.StripeSubscriptionID, "source": source})
	s.alert("billing_duplicate_subscription", b.OrgID, fmt.Sprintf("OpenGate Cloud: canceled duplicate subscription %s for org %s (current: %s). If its first invoice was paid, refund it in the Stripe Dashboard.",
		sub.ID, b.OrgID, b.StripeSubscriptionID))
	return nil
}

func (s *Server) billingAudit(ctx context.Context, org, action, target string, detail map[string]any) {
	d, _ := json.Marshal(detail)
	if err := s.store.InsertAudit(ctx, &store.AuditEntry{OrgID: org, ActorType: "stripe", Action: action, Target: target, Status: 200, Detail: d}); err != nil {
		s.log.Warn("audit billing change", "err", err)
	}
}

func (s *Server) alert(kind, org, text string) {
	if s.alerter != nil {
		s.alerter.Send(budget.Event{Kind: kind, Scope: "org", ScopeID: org, Text: text})
	}
}

// ---------- risk events (A3) ----------

// orgForCharge resolves a charge to its org via the charge's customer.
func (s *Server) orgForCharge(ctx context.Context, chargeID string) (*store.OrgBilling, *cloud.Charge, error) {
	if chargeID == "" {
		return nil, nil, nil
	}
	ch, err := s.cloud.stripe.GetCharge(ctx, chargeID)
	if err != nil {
		var ae *cloud.APIError
		if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	b, err := s.store.OrgBillingByCustomer(ctx, string(ch.Customer))
	if errors.Is(err, store.ErrNotFound) {
		s.log.Warn("stripe risk event: no org for charge", "charge", chargeID, "customer", ch.Customer)
		return nil, ch, nil
	}
	return b, ch, err
}

func riskRank(r string) int {
	switch r {
	case store.RiskFraudWarning:
		return 3
	case store.RiskDisputeLost:
		return 2
	case store.RiskDisputeOpen:
		return 1
	}
	return 0
}

// setRisk raises (never lowers) the org's risk flag.
func (s *Server) setRisk(ctx context.Context, b *store.OrgBilling, risk, detail string) error {
	if riskRank(risk) <= riskRank(b.RiskStatus) {
		return nil
	}
	b.RiskStatus, b.RiskDetail = risk, detail
	if err := s.store.PutOrgBilling(ctx, b); err != nil {
		return err
	}
	s.forgetPlan(b.OrgID)
	return nil
}

func money(amount int64, cur string) string {
	return fmt.Sprintf("%.2f %s", float64(amount)/100, strings.ToUpper(cur))
}

func (s *Server) handleDispute(ctx context.Context, ev *cloud.Event) error {
	var d cloud.Dispute
	if err := json.Unmarshal(ev.Data.Object, &d); err != nil {
		return err
	}
	if fresh, err := s.cloud.stripe.GetDispute(ctx, d.ID); err == nil {
		d = *fresh
	} else {
		var ae *cloud.APIError
		if !errors.As(err, &ae) || ae.Status != http.StatusNotFound {
			return err
		}
	}
	b, _, err := s.orgForCharge(ctx, string(d.Charge))
	if err != nil || b == nil {
		return err
	}
	s.billingAudit(ctx, b.OrgID, "billing."+strings.TrimPrefix(ev.Type, "charge."), d.ID, map[string]any{
		"status": d.Status, "reason": d.Reason, "amount": d.Amount, "currency": d.Currency, "charge": string(d.Charge), "event": ev.ID})
	switch d.Status {
	case "lost":
		if err := s.setRisk(ctx, b, store.RiskDisputeLost, d.ID); err != nil {
			return err
		}
		s.alert("billing_dispute_lost", b.OrgID, fmt.Sprintf("OpenGate Cloud: dispute %s (%s) for org %s was lost; paid plan suspended until cleared.", d.ID, money(d.Amount, d.Currency), b.OrgID))
	case "won", "warning_closed", "prevented":
		if (b.RiskStatus == store.RiskDisputeOpen || b.RiskStatus == store.RiskDisputeLost) && b.RiskDetail == d.ID {
			b.RiskStatus, b.RiskDetail = "", ""
			if err := s.store.PutOrgBilling(ctx, b); err != nil {
				return err
			}
			s.forgetPlan(b.OrgID)
		}
		if ev.Type == "charge.dispute.closed" {
			s.alert("billing_dispute_closed", b.OrgID, fmt.Sprintf("OpenGate Cloud: dispute %s for org %s closed (%s).", d.ID, b.OrgID, d.Status))
		}
	default: // open: needs_response, under_review, warning_*
		if err := s.setRisk(ctx, b, store.RiskDisputeOpen, d.ID); err != nil {
			return err
		}
		if ev.Type == "charge.dispute.created" {
			s.alert("billing_dispute_created", b.OrgID, fmt.Sprintf("OpenGate Cloud: new dispute %s (%s, reason %s) for org %s. Respond in the Stripe Dashboard.", d.ID, money(d.Amount, d.Currency), d.Reason, b.OrgID))
		}
	}
	return nil
}

func (s *Server) handleRefund(ctx context.Context, ev *cloud.Event) error {
	var ch cloud.Charge
	if err := json.Unmarshal(ev.Data.Object, &ch); err != nil {
		return err
	}
	b, fresh, err := s.orgForCharge(ctx, ch.ID)
	if err != nil || b == nil {
		return err
	}
	s.billingAudit(ctx, b.OrgID, "billing.charge.refunded", fresh.ID, map[string]any{"amount": fresh.Amount, "amount_refunded": fresh.AmountRefunded,
		"currency": fresh.Currency, "full": fresh.Refunded, "event": ev.ID})
	s.alert("billing_refund", b.OrgID, fmt.Sprintf("OpenGate Cloud: charge %s for org %s refunded (%s of %s).", fresh.ID, b.OrgID, money(fresh.AmountRefunded, fresh.Currency), money(fresh.Amount, fresh.Currency)))
	return nil
}

func (s *Server) handleFraudWarning(ctx context.Context, ev *cloud.Event) error {
	var w cloud.EarlyFraudWarning
	if err := json.Unmarshal(ev.Data.Object, &w); err != nil {
		return err
	}
	b, _, err := s.orgForCharge(ctx, string(w.Charge))
	if err != nil || b == nil {
		return err
	}
	s.billingAudit(ctx, b.OrgID, "billing."+strings.TrimPrefix(ev.Type, "radar."), w.ID, map[string]any{"fraud_type": w.FraudType, "actionable": w.Actionable,
		"charge": string(w.Charge), "event": ev.ID})
	if ev.Type != "radar.early_fraud_warning.created" {
		return nil
	}
	if err := s.setRisk(ctx, b, store.RiskFraudWarning, w.ID); err != nil {
		return err
	}
	s.alert("billing_fraud_warning", b.OrgID, fmt.Sprintf("OpenGate Cloud: early fraud warning %s (%s) on charge %s for org %s; paid plan suspended. Review, refund and cancel in Stripe, then clear the flag.",
		w.ID, w.FraudType, w.Charge, b.OrgID))
	return nil
}

// pruneCloud enforces per-plan log retention and forgets old webhook ids.
func (s *Server) pruneCloud(ctx context.Context) {
	orgs, err := s.store.ListOrgs(ctx, "")
	if err != nil {
		s.log.Warn("cloud retention: list orgs", "err", err)
		return
	}
	now := time.Now()
	for _, o := range orgs {
		p, err := s.orgPlan(ctx, o.ID)
		if err != nil || p.LogRetentionDays <= 0 {
			continue
		}
		if n, err := s.store.PruneOrgLogs(ctx, o.ID, now.AddDate(0, 0, -p.LogRetentionDays)); err != nil {
			s.log.Warn("cloud retention", "org_id", o.ID, "err", err)
		} else if n > 0 {
			s.log.Info("cloud retention pruned logs", "org_id", o.ID, "deleted", n)
		}
	}
	if err := s.store.PruneStripeEvents(ctx, now.AddDate(0, 0, -30)); err != nil {
		s.log.Warn("prune stripe events", "err", err)
	}
}
