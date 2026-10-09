package cloud

import (
	"time"

	"github.com/opengate-dev/opengate/internal/config"
	"github.com/opengate-dev/opengate/internal/store"
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

// ApplySubscription folds a fetched subscription into the billing row.
// It returns false when the subscription must be ignored: an unknown price,
// or a stale/foreign subscription that would clobber the org's current one.
func ApplySubscription(cc *config.CloudConfig, b *store.OrgBilling, sub *Subscription, now time.Time) bool {
	plan, ok := PlanForPrice(cc, sub.PriceID())
	if !ok {
		return false
	}
	if b.StripeCustomerID != "" && string(sub.Customer) != b.StripeCustomerID {
		return false // belongs to another customer
	}
	live := paidStatuses[sub.Status] || sub.Status == "past_due" || sub.Status == "incomplete"
	if b.StripeSubscriptionID != "" && b.StripeSubscriptionID != sub.ID && !live {
		return false // an old, ended subscription must not override the current one
	}
	b.StripeCustomerID = string(sub.Customer)
	b.Status = sub.Status
	b.CurrentPeriodEnd = sub.PeriodEnd()
	b.CancelAtPeriodEnd = sub.CancelAtPeriodEnd
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
	return true
}
