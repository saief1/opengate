package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// OrgBilling is an organization's cloud plan and Stripe subscription state.
// Orgs without a row are on the default plan.
type OrgBilling struct {
	OrgID                string    `json:"org_id"`
	Plan                 string    `json:"plan"`
	Status               string    `json:"status"` // active | trialing | past_due | unpaid | canceled | incomplete | ...
	StripeCustomerID     string    `json:"stripe_customer_id,omitempty"`
	StripeSubscriptionID string    `json:"stripe_subscription_id,omitempty"`
	CurrentPeriodEnd     time.Time `json:"current_period_end"`
	CancelAtPeriodEnd    bool      `json:"cancel_at_period_end"`
	PastDueSince         time.Time `json:"past_due_since"`
	// CheckoutSessionID/CheckoutPlan remember the org's open Checkout Session
	// so a second "Upgrade" click reuses (or expires) it instead of opening a
	// parallel one that could create a duplicate subscription.
	CheckoutSessionID string `json:"-"`
	CheckoutPlan      string `json:"-"`
	// RiskStatus is set by dispute / fraud events: dispute_open (flag only),
	// dispute_lost or fraud_warning (paid plan suspended until the operator
	// clears it). RiskDetail names the Stripe object.
	RiskStatus string    `json:"risk_status,omitempty"`
	RiskDetail string    `json:"risk_detail,omitempty"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// Risk statuses.
const (
	RiskDisputeOpen  = "dispute_open"
	RiskDisputeLost  = "dispute_lost"
	RiskFraudWarning = "fraud_warning"
)

// Suspended reports whether a risk flag suspends the paid plan.
func (b *OrgBilling) Suspended() bool {
	return b.RiskStatus == RiskDisputeLost || b.RiskStatus == RiskFraudWarning
}

const billingCols = `org_id,plan,status,stripe_customer_id,stripe_subscription_id,current_period_end,cancel_at_period_end,past_due_since,checkout_session_id,checkout_plan,risk_status,risk_detail,updated_at`

func scanBilling(sc interface{ Scan(...any) error }) (*OrgBilling, error) {
	var b OrgBilling
	var cpe, pds, upd int64
	var cancel int
	if err := sc.Scan(&b.OrgID, &b.Plan, &b.Status, &b.StripeCustomerID, &b.StripeSubscriptionID, &cpe, &cancel, &pds,
		&b.CheckoutSessionID, &b.CheckoutPlan, &b.RiskStatus, &b.RiskDetail, &upd); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	b.CurrentPeriodEnd, b.PastDueSince, b.UpdatedAt = fromMS0(cpe), fromMS0(pds), fromMS(upd)
	b.CancelAtPeriodEnd = cancel != 0
	return &b, nil
}

// GetOrgBilling returns the org's billing row, or a default-plan row (not
// persisted) when the org has none yet.
func (s *Store) GetOrgBilling(ctx context.Context, orgID, defaultPlan string) (*OrgBilling, error) {
	b, err := scanBilling(s.queryRow(ctx, `SELECT `+billingCols+` FROM org_billing WHERE org_id=?`, orgID))
	if errors.Is(err, ErrNotFound) {
		return &OrgBilling{OrgID: orgID, Plan: defaultPlan, Status: "active"}, nil
	}
	return b, err
}

// OrgBillingByCustomer finds the org linked to a Stripe customer.
func (s *Store) OrgBillingByCustomer(ctx context.Context, customerID string) (*OrgBilling, error) {
	if customerID == "" {
		return nil, ErrNotFound
	}
	return scanBilling(s.queryRow(ctx, `SELECT `+billingCols+` FROM org_billing WHERE stripe_customer_id=?`, customerID))
}

// PutOrgBilling upserts the full billing row.
func (s *Store) PutOrgBilling(ctx context.Context, b *OrgBilling) error {
	b.UpdatedAt = time.Now().UTC()
	cancel := 0
	if b.CancelAtPeriodEnd {
		cancel = 1
	}
	_, err := s.exec(ctx, `INSERT INTO org_billing(`+billingCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(org_id) DO UPDATE SET plan=excluded.plan, status=excluded.status, stripe_customer_id=excluded.stripe_customer_id,
 stripe_subscription_id=excluded.stripe_subscription_id, current_period_end=excluded.current_period_end,
 cancel_at_period_end=excluded.cancel_at_period_end, past_due_since=excluded.past_due_since,
 checkout_session_id=excluded.checkout_session_id, checkout_plan=excluded.checkout_plan,
 risk_status=excluded.risk_status, risk_detail=excluded.risk_detail, updated_at=excluded.updated_at`,
		b.OrgID, b.Plan, b.Status, b.StripeCustomerID, b.StripeSubscriptionID, ms0(b.CurrentPeriodEnd), cancel, ms0(b.PastDueSince),
		b.CheckoutSessionID, b.CheckoutPlan, b.RiskStatus, b.RiskDetail, ms(b.UpdatedAt))
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "unique") {
		return ErrConflict
	}
	return err
}

// ms0 maps the zero time to 0 instead of a large negative number.
func ms0(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return ms(t)
}

func fromMS0(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return fromMS(v)
}

// UsagePeriod is the calendar month (UTC) usage counters are kept for.
func UsagePeriod(t time.Time) string { return t.UTC().Format("2006-01") }

// IncrOrgUsage atomically counts one request for the org in the period
// and returns the new total.
func (s *Store) IncrOrgUsage(ctx context.Context, orgID, period string) (int64, error) {
	var n int64
	err := s.queryRow(ctx, `INSERT INTO org_usage(org_id,period,requests) VALUES(?,?,1)
ON CONFLICT(org_id,period) DO UPDATE SET requests=org_usage.requests+1 RETURNING requests`, orgID, period).Scan(&n)
	return n, err
}

// OrgUsage returns the org's request count in the period.
func (s *Store) OrgUsage(ctx context.Context, orgID, period string) (int64, error) {
	var n int64
	err := s.queryRow(ctx, `SELECT requests FROM org_usage WHERE org_id=? AND period=?`, orgID, period).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return n, err
}

// Event claim results.
const (
	EventClaimed = iota // this delivery should process the event
	EventDone           // already processed: acknowledge as a duplicate
	EventBusy           // another delivery is processing it right now
)

// ClaimStripeEvent atomically claims a webhook event for processing. The
// unique insert means two concurrent deliveries of the same event can't both
// process it; a claim left behind by a crashed process is taken over after
// staleAfter.
func (s *Store) ClaimStripeEvent(ctx context.Context, id, typ string, staleAfter time.Duration) (int, error) {
	now := time.Now()
	res, err := s.exec(ctx, `INSERT INTO stripe_events(id,type,received_at,state) VALUES(?,?,?,'processing') ON CONFLICT DO NOTHING`, id, typ, ms(now))
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return EventClaimed, nil
	}
	res, err = s.exec(ctx, `UPDATE stripe_events SET received_at=? WHERE id=? AND state='processing' AND received_at<?`, ms(now), id, ms(now.Add(-staleAfter)))
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return EventClaimed, nil
	}
	var state string
	if err := s.queryRow(ctx, `SELECT state FROM stripe_events WHERE id=?`, id).Scan(&state); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return EventBusy, nil // released between our statements; let Stripe retry
		}
		return 0, err
	}
	if state == "done" {
		return EventDone, nil
	}
	return EventBusy, nil
}

// FinishStripeEvent marks a claimed event processed.
func (s *Store) FinishStripeEvent(ctx context.Context, id string) error {
	_, err := s.exec(ctx, `UPDATE stripe_events SET state='done' WHERE id=?`, id)
	return err
}

// ReleaseStripeEvent drops a claim after a processing failure so Stripe's
// retry can process the event again.
func (s *Store) ReleaseStripeEvent(ctx context.Context, id string) error {
	_, err := s.exec(ctx, `DELETE FROM stripe_events WHERE id=? AND state='processing'`, id)
	return err
}

// StripeEventSeen reports whether a webhook event was already processed.
func (s *Store) StripeEventSeen(ctx context.Context, id string) (bool, error) {
	var x string
	err := s.queryRow(ctx, `SELECT id FROM stripe_events WHERE id=? AND state='done'`, id).Scan(&x)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// AcquireLease takes or renews the named single-writer lease for holder.
// It succeeds when the lease is free, expired, or already held by holder,
// so only one process (across replicas sharing the database) runs the job.
func (s *Store) AcquireLease(ctx context.Context, name, holder string, ttl time.Duration) (bool, error) {
	now := time.Now()
	res, err := s.exec(ctx, `INSERT INTO job_leases(name,holder,expires_at) VALUES(?,?,?)
ON CONFLICT(name) DO UPDATE SET holder=excluded.holder, expires_at=excluded.expires_at
WHERE job_leases.expires_at < ? OR job_leases.holder = excluded.holder`, name, holder, ms(now.Add(ttl)), ms(now))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// ReleaseLease gives the lease up if holder still owns it.
func (s *Store) ReleaseLease(ctx context.Context, name, holder string) error {
	_, err := s.exec(ctx, `DELETE FROM job_leases WHERE name=? AND holder=?`, name, holder)
	return err
}

// PruneStripeEvents forgets processed event ids older than before (Stripe
// stops retrying after 3 days).
func (s *Store) PruneStripeEvents(ctx context.Context, before time.Time) error {
	_, err := s.exec(ctx, `DELETE FROM stripe_events WHERE received_at < ?`, ms(before))
	return err
}

// CountProjects returns the number of projects in an org.
func (s *Store) CountProjects(ctx context.Context, orgID string) (int, error) {
	var n int
	err := s.queryRow(ctx, `SELECT COUNT(*) FROM projects WHERE org_id=?`, orgID).Scan(&n)
	return n, err
}

// CountSeats returns members plus pending (unexpired, unaccepted) invites.
func (s *Store) CountSeats(ctx context.Context, orgID string, now time.Time) (members, pending int, err error) {
	if err = s.queryRow(ctx, `SELECT COUNT(*) FROM memberships WHERE org_id=?`, orgID).Scan(&members); err != nil {
		return
	}
	err = s.queryRow(ctx, `SELECT COUNT(*) FROM invites WHERE org_id=? AND accepted_at=0 AND expires_at > ?`, orgID, ms(now)).Scan(&pending)
	return
}

// CountOwnedOrgs returns how many orgs the user owns.
func (s *Store) CountOwnedOrgs(ctx context.Context, userID string) (int, error) {
	var n int
	err := s.queryRow(ctx, `SELECT COUNT(*) FROM memberships WHERE user_id=? AND role=?`, userID, RoleOwner).Scan(&n)
	return n, err
}

// PruneOrgLogs deletes the org's request logs older than before.
func (s *Store) PruneOrgLogs(ctx context.Context, orgID string, before time.Time) (int64, error) {
	res, err := s.exec(ctx, `DELETE FROM request_logs WHERE ts < ? AND project_id IN (SELECT id FROM projects WHERE org_id=?)`, ms(before), orgID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
