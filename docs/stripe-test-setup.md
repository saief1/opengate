# Stripe test-mode setup and live test run (sandbox)

Account: **Opengate AI sandbox** (`acct_1UOmINK91DirmFuM`, Canada), **test mode only**. Created 2026-10-09 (ET) with
the test secret key from the environment (`OPENGATE_STRIPE_SECRET_KEY`, passed to tools via env and never written to disk).
Every create call used an idempotency key; every object carries `metadata[app]=opengate`. All IDs below are
non-secret test-mode identifiers.

## Configuration objects

| Object | ID | Details | Idempotency key |
| --- | --- | --- | --- |
| Product | `prod_VPbz89dZWNKhkG` | "OpenGate Cloud Team", tax code `txcd_10103001` (SaaS, business use), `metadata[plan]=team` | `opengate-setup-product-team-v1` |
| Price | `price_1UOmlPK91DirmFuMAJvbYeg4` | USD 49.00 / month, `lookup_key=opengate_team_monthly`, `tax_behavior=exclusive` | `opengate-setup-price-team-monthly-v1` |
| Portal configuration | `bpc_1UOmlYK91DirmFuMbxb3vDLb` | payment-method update ✔, invoice history ✔, cancel **at period end** ✔ (no proration, with reasons), customer update (email, address, tax ID) ✔, **plan switching off**, pause off. Became the account default. | `opengate-setup-portal-v1` |

Reproduce on a fresh account (example; key from env only):

```sh
K="$OPENGATE_STRIPE_SECRET_KEY"; V=2026-09-30.endive
curl -s https://api.stripe.com/v1/products -u "$K:" -H "Stripe-Version: $V" -H "Idempotency-Key: opengate-setup-product-team-v1" \
  -d name="OpenGate Cloud Team" -d tax_code=txcd_10103001 -d "metadata[app]=opengate" -d "metadata[plan]=team"
curl -s https://api.stripe.com/v1/prices -u "$K:" -H "Stripe-Version: $V" -H "Idempotency-Key: opengate-setup-price-team-monthly-v1" \
  -d product=prod_... -d currency=usd -d unit_amount=4900 -d "recurring[interval]=month" \
  -d lookup_key=opengate_team_monthly -d tax_behavior=exclusive -d "metadata[app]=opengate"
curl -s https://api.stripe.com/v1/billing_portal/configurations -u "$K:" -H "Stripe-Version: $V" -H "Idempotency-Key: opengate-setup-portal-v1" \
  -d "features[payment_method_update][enabled]=true" -d "features[invoice_history][enabled]=true" \
  -d "features[subscription_cancel][enabled]=true" -d "features[subscription_cancel][mode]=at_period_end" \
  -d "features[subscription_cancel][proration_behavior]=none" -d "features[subscription_update][enabled]=false" \
  -d "features[customer_update][enabled]=true" -d "features[customer_update][allowed_updates][]=email" \
  -d "features[customer_update][allowed_updates][]=address" -d "features[customer_update][allowed_updates][]=tax_id" \
  -d "metadata[app]=opengate"
```

No webhook endpoint was created in the account: the live run used `stripe listen` (CLI-provided signing secret, via env).

## Live end-to-end run (2026-10-09, 19:00-19:16 ET)

Setup: `opengate serve -config examples/cloud-stripe-test.yaml` on **PostgreSQL 16** (`opengate_e2e`), real
`https://api.stripe.com` with the sandbox test key, `stripe listen --latest --forward-to
http://127.0.0.1:8080/cloud/stripe/webhook` (14 event types), mock AI provider, headless Chrome (playwright-core)
completing the Stripe-hosted pages. 27 events were forwarded: 26 got `200`, and 1 failed to connect because the gateway
was being restarted (a test-harness restart, not an app error).

| # | Scenario | Result |
| --- | --- | --- |
| 1 | Sign up `demo@example.com` → Checkout → card 4242 on hosted Checkout | ✅ `invoice.paid`, `customer.subscription.created`, `checkout.session.completed` → org **Team / active**; gateway request 200; second click before paying **reused** the same session; Checkout while subscribed → `409 subscription_exists` |
| 2 | Portal session → "Cancel subscription" (reason: too expensive) | ✅ after a fix: Stripe (2026-09-30) sets `cancel_at` and leaves `cancel_at_period_end=false`. The first build missed it; it now treats `cancel_at` as scheduled cancellation (unit + mock tests added). Org shows "cancels Nov 9, 2026", still Team |
| 3 | Declined card `4000 0000 0000 0341` on hosted Checkout | ✅ Checkout shows "Your credit card was declined"; no subscription; org stays **Free**; session stays open |
| 3b | Same org, API path: attach `pm_card_chargeCustomerFail`, create subscription (`allow_incomplete`) | ✅ `incomplete` + `invoice.payment_failed`; effective plan **Free**. Clicking Upgrade again **canceled the incomplete subscription** (→ `incomplete_expired`) and **reused** the open session; paying it with 4242 → **Team / active** (found and fixed in this run: incomplete subscriptions used to block Checkout and expire the open session) |
| 4 | Dispute card `4000 0000 0000 0259` | ✅ `charge.dispute.created` → `risk_status=dispute_open`, plan kept, audit + alert. Closed via `POST /v1/disputes/{id}/close` (lost) → `charge.dispute.closed` → **suspended, effective Free**, dashboard banner |
| 5 | Partial refund ($1.00) of org 1's invoice charge | ✅ `charge.refunded` → audit `billing.charge.refunded` (`amount_refunded=100`) + alert webhook received |
| 6 | Early-fraud-warning card `4000 0000 0000 5423` | ✅ `radar.early_fraud_warning.created` (arrived before `invoice.paid`) → `fraud_warning`, **suspended**, alert received; operator `POST /admin/api/billing/risk/clear` (admin token) → Team again |
| 7 | 14-day trial, no card (`trial_days: 14`, `trial_without_card: true`) | ✅ hosted Checkout shows "14 days free" with no card fields; `trialing`, `trial_settings.end_behavior.missing_payment_method=cancel`; org **Team / trialing** until Oct 23 |
| — | API version | ✅ every OpenGate request sent `Stripe-Version: 2026-09-30.endive`; no API errors |

Not exercised live (covered by the mock suite): `past_due` → grace period → recovery (needs a test clock; OpenGate
creates customers without one), the duplicate-subscription auto-cancel, concurrent duplicate webhook deliveries,
livemode mismatch.

### Test objects left in the sandbox (harmless, test mode)

| Org (e2e DB) | Customer | Subscription(s) | Other |
| --- | --- | --- | --- |
| Acme AI (e2e), `demo@example.com` | `cus_VPc0Odm8DtcOYd` | `sub_1UOmnNK91DirmFuMPTFRad8X` active, cancels Nov 9 | charge `ch_3UOmnMK91DirmFuM1HbXFZ8t`, refund `re_3UOmnMK91DirmFuM1OI0zLc5` ($1) |
| Declined Card Co (e2e) | `cus_VPc7bFLYC3oncp` | `sub_1UOmwIK91DirmFuMLSh78PPv` incomplete_expired; `sub_1UOmwfK91DirmFuMamvyKeML` active | PM `pm_1UOmwHK91DirmFuM0P6ZqvkP` (chargeCustomerFail) |
| Dispute Co (e2e) | `cus_VPcBuV3h80AN8S` | `sub_1UOmxCK91DirmFuMVJUDD8Mx` active | dispute `du_1UOmxAK91DirmFuMedx3TPwO` (lost) on `ch_3UOmx9K91DirmFuM1DR69X6c` |
| EFW Co (e2e) | `cus_VPcD92PPPwXynF` | `sub_1UOmyhK91DirmFuM1qdy7iht` active | early fraud warning `issfr_1UOmyhK91DirmFuMlr1Puhra` on `ch_3UOmyfK91DirmFuM1AGhKNTU` |
| Trial Co (e2e) | `cus_VPcDwJJbg9GQFi` | `sub_1UOmznK91DirmFuMuryhXvO9` trialing (cancels at trial end without a card) | — |

Plus several Checkout Sessions (complete/open) for these customers. To clean up later: cancel the four active
subscriptions in the Dashboard (test mode), or leave them; nothing in test mode moves money.

Screenshots: [`docs/screenshots/cloud-live-test/`](screenshots/cloud-live-test/): hosted Checkout (`1-team-stripe-checkout.png`),
OpenGate billing after upgrade (`1-team-opengate-billing.png`), portal before/after cancel (`2-`, `3-stripe-portal-canceled.png`),
billing with cancellation scheduled (`3-opengate-billing-cancel-scheduled.png`), declined card (`4-*`), retry after decline (`5-*`),
dispute (`6-*`, incl. `6-dispute-lost-opengate-billing.png`), early fraud warning (`7-*`), and no-card trial (`8-`, `9-*`).
