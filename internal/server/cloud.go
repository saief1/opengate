package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

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
	return &cloudState{stripe: &cloud.Stripe{BaseURL: cfg.Cloud.Stripe.BaseURL, APIKey: cfg.StripeAPIKey, Client: client},
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
	if b.StripeSubscriptionID != "" && b.Status != "canceled" && b.Status != "incomplete_expired" {
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
	base := s.cfg.Cloud.PublicURL + "/ui/"
	sess, err := s.cloud.stripe.CreateCheckoutSession(r.Context(), cloud.CheckoutParams{
		OrgID: org, CustomerID: b.StripeCustomerID, PriceID: plan.StripePriceID,
		SuccessURL: base + "#billing?checkout=success&session_id={CHECKOUT_SESSION_ID}", CancelURL: base + "#billing?checkout=cancel",
		TrialDays: s.cfg.Cloud.Stripe.TrialDays, AllowPromotionCodes: s.cfg.Cloud.Stripe.AllowPromotionCodes, AutomaticTax: s.cfg.Cloud.Stripe.AutomaticTax,
	})
	if err != nil {
		s.stripeFail(w, "create checkout session", err)
		return
	}
	s.audit(r, "billing.checkout", org, map[string]string{"plan": plan.ID, "session": sess.ID})
	writeJSON(w, 200, map[string]string{"url": sess.URL, "id": sess.ID})
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
	sess, err := s.cloud.stripe.CreatePortalSession(r.Context(), b.StripeCustomerID, s.cfg.Cloud.PublicURL+"/ui/#billing")
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
	ctx := r.Context()
	if seen, err := s.store.StripeEventSeen(ctx, ev.ID); err != nil {
		storeErr(w, err)
		return
	} else if seen {
		writeJSON(w, 200, map[string]any{"received": true, "duplicate": true})
		return
	}
	if err := s.handleStripeEvent(ctx, &ev); err != nil {
		// 5xx makes Stripe retry with backoff for up to 3 days.
		s.log.Error("stripe webhook processing failed", "event", ev.ID, "type", ev.Type, "err", err)
		writeError(w, http.StatusInternalServerError, "server_error", "internal_error", "processing failed")
		return
	}
	if err := s.store.MarkStripeEvent(ctx, ev.ID, ev.Type); err != nil {
		s.log.Warn("mark stripe event", "err", err)
	}
	writeJSON(w, 200, map[string]any{"received": true})
}

func (s *Server) handleStripeEvent(ctx context.Context, ev *cloud.Event) error {
	switch {
	case ev.Type == "checkout.session.completed":
		var cs cloud.Session
		if err := json.Unmarshal(ev.Data.Object, &cs); err != nil {
			return err
		}
		if cs.Mode != "subscription" || cs.ClientReferenceID == "" || cs.Subscription == "" {
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
	}
	return nil // not an event we act on
}

// syncSubscription fetches the subscription and applies it to its org.
// The org comes from the Checkout session's client_reference_id, the
// subscription metadata (both set by OpenGate), or the linked customer.
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
	if org == "" {
		org = sub.Metadata["org_id"]
	}
	var b *store.OrgBilling
	if org != "" {
		if _, err := s.store.GetOrg(ctx, org); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				s.log.Warn("stripe webhook: unknown org", "event", ev.ID, "org_id", org)
				return nil
			}
			return err
		}
		if b, err = s.store.GetOrgBilling(ctx, org, s.cfg.Cloud.DefaultPlan); err != nil {
			return err
		}
	} else if b, err = s.store.OrgBillingByCustomer(ctx, string(sub.Customer)); errors.Is(err, store.ErrNotFound) {
		s.log.Warn("stripe webhook: no org for customer", "event", ev.ID, "customer", sub.Customer)
		return nil
	} else if err != nil {
		return err
	}
	before := *b
	if !cloud.ApplySubscription(&s.cfg.Cloud, b, sub, time.Now()) {
		s.log.Warn("stripe webhook: subscription ignored", "event", ev.ID, "org_id", b.OrgID, "subscription", sub.ID,
			"customer", sub.Customer, "price", sub.PriceID(), "status", sub.Status)
		return nil
	}
	if err := s.store.PutOrgBilling(ctx, b); err != nil {
		if errors.Is(err, store.ErrConflict) {
			s.log.Warn("stripe webhook: customer already linked to another org", "event", ev.ID, "org_id", b.OrgID, "customer", sub.Customer)
			return nil
		}
		return err
	}
	s.forgetPlan(b.OrgID)
	if before.Plan != b.Plan || before.Status != b.Status || before.CancelAtPeriodEnd != b.CancelAtPeriodEnd {
		detail, _ := json.Marshal(map[string]any{"plan": b.Plan, "status": b.Status, "from_plan": before.Plan, "from_status": before.Status,
			"cancel_at_period_end": b.CancelAtPeriodEnd, "event": ev.ID, "type": ev.Type})
		if err := s.store.InsertAudit(ctx, &store.AuditEntry{OrgID: b.OrgID, ActorType: "stripe", Action: "billing.subscription",
			Target: sub.ID, Status: 200, Detail: detail}); err != nil {
			s.log.Warn("audit billing change", "err", err)
		}
	}
	s.log.Info("subscription synced", "org_id", b.OrgID, "plan", b.Plan, "status", b.Status, "event", ev.ID)
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
