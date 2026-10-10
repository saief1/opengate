package cloud

import (
	"time"

	"github.com/saief1/opengate/internal/config"
	"github.com/saief1/opengate/internal/store"
)

// paidStatuses keep the subscribed plan.
var paidStatuses = map[string]bool{"active": true, "trialing": true}

// Effective returns the plan whose limits apply right now: the subscribed
// plan while the subscription is active/trialing (or past_due within the
// grace period), otherwise the default plan. Operator-assigned plans (no
// Stripe subscription) always apply.
func Effective(cc *config.CloudConfig, b *store.OrgBilling, now time.Time) config.CloudPlan {
	def, _ := cc.Plan(cc.DefaultPlan)
	p, ok := cc.Plan(b.Plan)
	if !ok {
		return def
	}
	if b.Suspended() {
		return def // lost dispute or early fraud warning: paid features paused
	}
	if b.StripeSubscriptionID == "" {
		return p // default plan, or assigned manually by the operator
	}
	switch {
	case paidStatuses[b.Status]:
		return p
	case b.Status == "past_due" && !b.PastDueSince.IsZero() && now.Sub(b.PastDueSince) < cc.GracePeriod:
		return p
	}
	return def
}

// PlanForPrice maps a Stripe price id to a plan.
func PlanForPrice(cc *config.CloudConfig, priceID string) (config.CloudPlan, bool) {
	if priceID == "" {
		return config.CloudPlan{}, false
	}
	for _, p := range cc.Plans {
		if p.StripePriceID == priceID {
			return p, true
		}
	}
	return config.CloudPlan{}, false
}

// Outcome is what ApplySubscription did with a subscription.
type Outcome int

const (
	// Applied: the billing row now reflects the subscription.
	Applied Outcome = iota
	// Ignored: unknown price, foreign customer, or a stale subscription.
	Ignored
	// Duplicate: a second live subscription for an org that already has a
	// live one (e.g. two Checkout tabs paid). It must not replace the
	// current one; the caller cancels it.
	Duplicate
)

// LiveStatus reports whether a subscription in this status can still charge.
func LiveStatus(status string) bool {
	switch status {
	case "active", "trialing", "past_due", "incomplete", "unpaid", "paused":
		return true
	}
	return false
}

// Blocking reports whether a subscription in this status means the org is
// already subscribed, so a new Checkout must not be opened. "incomplete"
// (first payment failed or awaiting authentication) is not blocking: the
// caller cancels it before opening a new Checkout.
func Blocking(status string) bool { return LiveStatus(status) && status != "incomplete" }

// currentStatus reports whether the org's recorded subscription is in force.
func currentStatus(status string) bool {
	return status == "active" || status == "trialing" || status == "past_due"
}

// ApplySubscription folds a fetched subscription into the billing row.
// Events for any subscription other than the org's current one are never
// applied while the current one is in force.
func ApplySubscription(cc *config.CloudConfig, b *store.OrgBilling, sub *Subscription, now time.Time) Outcome {
	plan, ok := PlanForPrice(cc, sub.PriceID())
	if !ok {
		return Ignored
	}
	if b.StripeCustomerID != "" && string(sub.Customer) != b.StripeCustomerID {
		return Ignored // belongs to another customer
	}
	if b.StripeSubscriptionID != "" && b.StripeSubscriptionID != sub.ID {
		live := sub.Status == "active" || sub.Status == "trialing" || sub.Status == "past_due" || sub.Status == "incomplete"
		if !live {
			return Ignored // an old, ended subscription must not override the current one
		}
		if currentStatus(b.Status) {
			return Duplicate
		}
	}
	b.StripeCustomerID = string(sub.Customer)
	b.Status = sub.Status
	b.CurrentPeriodEnd = sub.PeriodEnd()
	b.CancelAtPeriodEnd = sub.CancelScheduled()
	switch {
	case sub.Status == "past_due":
		if b.PastDueSince.IsZero() {
			b.PastDueSince = now.UTC()
		}
	default:
		b.PastDueSince = time.Time{}
	}
	switch sub.Status {
	case "canceled", "incomplete_expired":
		if b.StripeSubscriptionID == sub.ID || b.StripeSubscriptionID == "" {
			b.Plan, b.StripeSubscriptionID, b.Status = cc.DefaultPlan, "", "canceled"
			b.CancelAtPeriodEnd = false
		}
	default:
		b.Plan, b.StripeSubscriptionID = plan.ID, sub.ID
	}
	return Applied
}
