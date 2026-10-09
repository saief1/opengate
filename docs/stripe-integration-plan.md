# OpenGate Cloud: Stripe integration plan (Payments, Billing, Connect, Invoicing)

Status: plan written 2026-10-09 on `v2-dev`. **Decisions approved by Saief on 2026-10-09** (below). Phase 0/1 fixes
B1, A1, A2, A3, A10 and A12 are **implemented** and were run end to end against the Stripe sandbox `acct_1UOmINK91DirmFuM`
(test mode); see [stripe-test-setup.md](stripe-test-setup.md). Everything else is still a plan.
Inputs: the Stripe implementation planner (guide `iguide_61VYAHSe1RVP0yYBR41GpvdUG1rIZ`, test account
`acct_1UCreIGpvdUG1rIZ`, accepted path: hosted Checkout subscriptions + Customer Portal on Accounts v2),
Stripe's best-practices and Connect guidance, and Stripe docs searches. Items marked **(verify)** need a
test-mode check or a word with Stripe before we build on them.

## Decisions (approved by Saief, 2026-10-09)

| # | Decision | Effect on this plan |
| --- | --- | --- |
| D1 | **Keep v1 Customers for launch**; migrate to Accounts v2 **before Connect** (Phase 5). | §1.2 / A4: Phase 1-4 use `customer=cus_...`. The Accounts v2 migration (one `v2/core/account` per org with `customer` + `merchant` configurations, `customer_account` everywhere) becomes the first task of Phase 5. Keep `stripe_customer_id` as the migration key. |
| D2 | **Overage via Billing Meters** (not Metronome). | §5: meter `opengate_overage_cents` + metered price as a second subscription item; hourly fixed-window reporting with the B1 deterministic identifiers. |
| D3 | **Credit ledger inside OpenGate.** | §4: option 3. Checkout `mode=payment` funds an `org_credits` ledger; Stripe credit grants and Metronome are out. |
| D4 | **Connect earns via the plan fee only** (no application fee) initially. | §6: no `application_fee_*` and no Platform Pricing Tool at launch. Founders pay their OpenGate plan; Stripe fees are theirs (`fees_collector=stripe`). Revisit after launch. |
| D5 | **Team trial: 14 days, no card required.** | Implemented: `cloud.stripe.trial_days: 14`, `trial_without_card: true`, which sends `subscription_data[trial_period_days]=14`, `payment_method_collection=if_required`, `subscription_data[trial_settings][end_behavior][missing_payment_method]=cancel`. Only an org's first subscription gets a trial. When the trial ends without a card, the subscription is canceled and the org returns to Free. `customer.subscription.trial_will_end` is forwarded (a reminder email is a later task). |
| D6 | **Enterprise payments (Canadian entity): cards, Canadian PAD (`acss_debit`), or manually recorded wires** (`paid_out_of_band`). | §3: no US ACH or virtual bank accounts, and no US entity for now. Turn on PAD in Payment methods settings. Record wires with `POST /v1/invoices/{id}/pay?paid_out_of_band=true` and a reconciliation note. Use a per-plan grace period of ~30 days for net 30. |

These answer open decisions 1-5 and 7 in §11. Still open: 6 (onboarding for founders with an existing Stripe account)
and 8 (Managed Payments vs. Stripe Tax).

Business context: OpenGate Inc. is Canada-based, sells to businesses on the web, globally, in USD.

| Need | Stripe product(s) | Phase |
| --- | --- | --- |
| Free / Team $49/mo / Enterprise (custom) plans with quotas | Billing (Subscriptions) + hosted Checkout + Customer Portal | 1 (mostly built) |
| Enterprise annual contracts, net 30, bank payment | Quotes + Invoicing (`send_invoice`, `days_until_due=30`) | 2 |
| Prepaid AI credit top-ups (small markup) | Checkout `mode=payment` + a credit ledger (Billing credit grants, or Metronome) | 3 |
| Usage overage on token spend | Metronome with Stripe Subscriptions (Stripe's default), or Billing Meters | 4 |
| Founders bill *their* end customers through their own Stripe | Connect (Accounts v2, SaaS/direct charges) + Billing Meters on the connected account | 5 |
| Tax | Stripe Tax (after registrations exist) | cross-cutting |

---

## 1. Platform foundations (do these first; they affect every product)

1. **Pin the API version.** Send `Stripe-Version: 2026-09-30.endive` (the current version) on every request, and
   create webhook endpoints at that same version. Upgrade on purpose: change the version, run the test suite
   against test mode, then move the endpoint. Today OpenGate sends no version header, so it gets whatever the
   account default is.
2. **One Stripe identity per org: Accounts v2.** Stripe's guidance for platforms is to model each business as
   one `v2/core/account` with *configurations*. These are `customer` (it pays OpenGate) and, later,
   `merchant` (it takes payments as a Connect account). Stripe says not to create a separate v1 Customer to
   bill a connected account. A founder org is both an OpenGate customer and a future connected account, so
   create orgs as Accounts v2 with `configuration.customer` from day one. Then pass `customer_account=acct_...`
   (not `customer=cus_...`) to Checkout, Subscriptions, Portal and Invoices. We have no live customers, so the
   switch is cheap now and expensive later. **(verify)** That every flow below (Checkout subscription,
   Portal, Quotes, Invoicing with bank transfer, credit grants, meter events) takes `customer_account` on
   2026-09-30. Where one doesn't, keep v1 Customers for Phase 1 and plan the migration.
3. **Keys: restricted keys only, one per job.** Use `rk_` keys, not `sk_`, with separate test and live keys.
   Keep them in the host's secret store (Fly secrets, Render secret files, k8s Secret from a vault), not in
   committed env files. Set an IP access policy on each live key.
4. **Webhooks are the source of truth.** Verify signatures, dedupe on `event.id`, return 2xx fast, and re-fetch
   the object before acting on it. Map events to orgs through Stripe's object graph (customer/account ID →
   org), with metadata only as a fallback.
5. **Idempotency keys** on every create call, derived from our own intent ID (for example
   `checkout:{org}:{plan}:{nonce stored server-side}`), so a retried request can't create a second object.
6. **Stripe Tax.** Canada GST/HST registration first. Add US/EU/UK registrations as thresholds are crossed;
   use Tax threshold monitoring until then. Don't turn on `automatic_tax` until a registration is active:
   Stripe collects nothing, and shows no error, without one. For B2B, collect tax IDs
   (`tax_id_collection[enabled]=true`) whether or not automatic tax is on.

## 2. Billing: SaaS subscriptions (Phase 1)

**Choice:** Billing Subscriptions + **Stripe-hosted Checkout** (redirect is fine; the planner leaf is
`redirect_ok → no managed payments`) + **Customer Portal** for self-service. Trials are optional.

Objects:
- One Product per plan: `OpenGate Cloud Team`, later `OpenGate Cloud Enterprise`. Don't put two plans on one
  Product. Extra Prices on a Product are for billing variants of the same plan.
- Prices: Team `$49.00 USD / month` (`lookup_key=team_monthly`). Optional `team_annual`. Enterprise gets
  per-contract prices or a quote (see §3). Free has no Stripe objects.
- Checkout Session: `mode=subscription`, `customer_account` (or `customer`), `client_reference_id=org_id`,
  `line_items[0][price]`, `subscription_data[metadata][org_id]`,
  `subscription_data[billing_mode][type]=flexible` **(verify the default on 2026-09-30)**,
  `tax_id_collection`, `automatic_tax` (only once registered), `allow_promotion_codes`,
  `integration_identifier=opengate-cloud-team-<8 letters>`, and an idempotency key. **Never pass
  `payment_method_types`**: payment methods come from the Dashboard.
- One open checkout per org: store the open Session ID and expire it (`POST /v1/checkout/sessions/{id}/expire`)
  before creating a new one. Also refuse a new one while a live subscription exists (already done).
- Portal configuration, created once and passed as `configuration=bpc_...`: invoice history, payment-method
  update, tax-ID/address update, cancel at period end (with a reason), and plan switching limited to the Team
  monthly/annual prices with `proration_behavior=create_prorations`. Customers can't buy Enterprise in the
  portal.

Entitlements (OpenGate side, already the design): plan = f(price ID, status). `active`/`trialing` gives the
paid plan. `past_due` gets a grace period, then drops to Free. `canceled`, `unpaid`, `incomplete_expired` and
`paused` give Free. Quotas are enforced locally; Stripe is never called on the request path.

Webhooks (Phase 1):
`checkout.session.completed`, `customer.subscription.created|updated|deleted|paused|resumed|trial_will_end`,
`invoice.paid`, `invoice.payment_failed`, `invoice.payment_action_required`, `invoice.upcoming` (optional
notice), and the risk events `charge.dispute.created`, `charge.dispute.closed`, `charge.refunded`,
`radar.early_fraud_warning.created`. With Accounts v2 add `v2.core.account[configuration.customer].updated`
and `v2.core.account[identity].updated`, or `customer.updated` on v1 if we keep local copies. Dunning: turn
on Smart Retries and the failed-payment emails in Billing → Revenue recovery. OpenGate's grace period should
be at least the retry schedule.

## 3. Invoicing: Enterprise annual contracts (Phase 2)

**Choice:** sales-led. **Quote → Subscription** (`collection_method=send_invoice`, `days_until_due=30`,
yearly price), paid on the **Hosted Invoice Page**, reconciled automatically by Stripe. Invoice branding and
metadata (PO number, contract ID) go on the invoice. Ad-hoc items (setup, extra commit) are one-off invoices
through the Invoicing API.

Flow: sales creates a customer account with `metadata[org_id]` → Quote (Dashboard or API) with the Enterprise
yearly price, `collection_method=send_invoice`, `invoice_settings[days_until_due]=30` → customer accepts →
the Subscription and first invoice are created → `invoice.finalized` / `invoice.sent` → customer pays → `invoice.paid`.

**Payment methods. This is the biggest open question for a Canadian entity:**
- Stripe's Invoicing payment-method table lists **ACH Direct Debit** as *business location: United States*
  and **Bank transfer** (customer balance / virtual account numbers) as *EU (SEPA), Indonesia, Japan, Mexico,
  UK, US*. **Canada is not listed for either.** A Canada-based Stripe account probably can't offer US ACH
  debit or Stripe virtual-account bank transfers on invoices.
- Available to a Canadian account: cards (incl. corporate cards), **Canadian pre-authorized debit
  (`acss_debit`, CAD or USD, Canadian bank accounts only)**, and Link. Off-Stripe wires can be recorded with
  `POST /v1/invoices/{id}/pay` and `paid_out_of_band=true`, but then nothing is reconciled automatically.
- Options: (a) cards + PAD + manual wires for now; (b) a US entity (for example a Delaware C-corp) with its
  own Stripe account for US customers, which unlocks ACH Direct Debit and US bank transfers with automatic
  reconciliation; (c) ask Stripe whether cross-border bank transfers are now offered to CA accounts
  **(verify)**. Turn on whatever qualifies in Settings → Payment methods; don't hard-code types.

Webhooks: `quote.accepted`, `invoice.finalized`, `invoice.sent`, `invoice.paid`, `invoice.overdue`,
`invoice.payment_failed`, `invoice.marked_uncollectible`, `invoice.voided`. If customer-balance bank
transfers become available, add `cash_balance.funds_available` and `customer_cash_balance_transaction.created`.
Enterprise entitlement should follow the subscription (`active` while invoices are open but not overdue), with
a **per-plan grace period** of about 30 days, because net-30 plus wire delays will trip a 7-day grace.

## 4. Payments: prepaid AI credit top-ups (Phase 3)

**Choice:** hosted **Checkout `mode=payment`** with preset packs (for example $25 / $100 / $500 Products with
one Price each), plus `invoice_creation[enabled]=true` so B2B buyers get a receipt-invoice. Credits go into a
**ledger**, and gateway spend draws it down.

Ledger options (decision):
1. **Billing credit grants** (`POST /v1/billing/credit_grants`, `category=paid`, `applicability_config` scoped
   to the usage price). Stripe applies credits to *metered* invoice line items. This only makes sense once
   overage is billed through Stripe (Billing Meters). Stripe's skill sends new prepaid-credit / burndown
   designs to **Metronome** instead.
2. **Metronome** prepaid commits/credits. One system for credits, overage, and enterprise commits.
3. **OpenGate-native ledger** (the recommended start, fits "minimal deps"): the Checkout payment funds an
   `org_credits` ledger row in OpenGate, and the gateway decrements it in real time using the existing
   reservation/budget engine. Stripe is only the payment rail. Real-time spend control stays local, which is
   OpenGate's core feature.

Fulfillment (any option): grant on `checkout.session.completed` **only if `payment_status=paid`**, else wait
for `checkout.session.async_payment_succeeded`; handle `checkout.session.async_payment_failed`. Do this
idempotently, keyed on the session ID. Get the pack from `line_items` (re-fetched), not from client input.
Refunds/disputes (`charge.refunded`, `charge.dispute.created`) claw back unspent credit and flag the org.
Pricing: the "small markup" is in the pack price (for example $100 buys $95 of provider cost at list price).
Card fees (~2.9% + 30¢; more for international and FX) can wipe out a thin markup, so set a minimum pack
size. Credits expire after 12 months; set the expiry in the terms.

## 5. Usage-based overage on token spend (Phase 4)

**Stripe's default recommendation:** **Metronome with Stripe Subscriptions**. The $49 Team subscription,
Checkout and Portal stay as they are, and Metronome bills only the usage, pushing a separate usage invoice to
the same Stripe customer. Use it if the pricing gets complex (tiers, commits, credits, enterprise ramps).
**Leaner alternative:** **Billing Meters**. A `opengate_overage_cents` meter (aggregation `sum`,
`customer_mapping` on the customer/account ID), a metered Price on its own Product, added as a second item on
the Team subscription, reported by an hourly job that sends only the overage above the plan allowance. This is
enough for "pay-as-you-go above quota" and reuses the meter-sync code we already have.
Rules either way: report in arrears, aggregated per hour (not per request), with deterministic identifiers
(see review B1). Meter events must be within the past ~35 days and no more than ~5 minutes in the future
**(verify exact limits)**. Use `billing.meter.error_report_triggered` and the v2 meter-event stream if volume
grows. Align the OpenGate quota period with the subscription's billing period, not the calendar month.

## 6. Connect: founders bill their own end customers (Phase 5)

**Choice: SaaS platform, direct charges.** The founder is merchant of record, owns the customer relationship
and their Stripe Dashboard, and gets paid out by Stripe. OpenGate supplies the metering and takes an
optional application fee.

Connected account configuration (Accounts v2, `POST /v2/core/accounts`):
- `configuration.merchant` with `capabilities.card_payments.requested=true` (plus any other local payment
  methods needed). For the same org, `configuration.customer` already exists from §1.
- `dashboard: "full"`, `defaults.responsibilities.fees_collector: "stripe"`, `losses_collector: "stripe"`.
  Stripe-managed risk, so OpenGate isn't liable for founders' negative balances.
- Never use legacy `type: standard|express|custom`.
- Onboarding: **embedded onboarding** (`account_onboarding`) or Account Links, never API onboarding. Embed the
  `notification_banner`, `account_management`, `payments` and `payouts` components in the OpenGate dashboard.
- Founders who already have a Stripe account need to *connect the existing account*. Plan an OAuth-style
  connect flow with a per-request `state` (CSRF), and confirm how Accounts v2 handles existing accounts
  **(verify; this decides the onboarding UX)**.
- Gate on `configuration.merchant.capabilities.card_payments.status == "active"` (not `charges_enabled`), and
  re-check it on `v2.core.account[configuration.merchant].capability_status_updated` /
  `v2.core.account[requirements].updated`.

Charge flow (all calls with `Stripe-Account: acct_founder`):
- The founder's Products/Prices/Meter and their end customers live **on the connected account**.
- OpenGate's existing per-end-customer meter sync (Phase 2 code) sends meter events **to the founder's account**
  with `Stripe-Account`. Billing Meters, not Metronome, is the right tool here, because Metronome doesn't
  support Connect.
- Subscriptions/invoices on the connected account carry `application_fee_percent` (subscriptions) or
  `application_fee_amount` (one-off PaymentIntents/Checkout) when OpenGate takes a fee. Alternatively, set
  fees centrally in the Dashboard **Platform Pricing Tool**. Pick one, not both.
- Cross-border: direct charges let each founder accept cards from anywhere their own Stripe account supports.
  Founders must be in Stripe-supported countries. **(verify)** The planner's tree flags cross-border cases
  for SaaS direct charges.

Fees: (a) **SaaS fee**, where founders pay the OpenGate Team/Enterprise plan they're already on (the
simplest, and it's what §2 already does), and/or (b) an **application fee** (for example 0.5-1% of
end-customer billings). Founders pay Stripe's processing fees themselves (`fees_collector=stripe`), so OpenGate's
fee is pure margin. Watch the Connect margin report.

Connect webhooks: a **separate Connect endpoint** ("events on connected accounts") for `invoice.paid`,
`invoice.payment_failed`, `customer.subscription.*`, `charge.dispute.created`, `billing.meter.error_report_triggered`,
plus v2 account events through an **event destination** (thin events) for
`v2.core.account[configuration.merchant].capability_status_updated`, `v2.core.account[requirements].updated`,
`v2.core.account.updated`. Thin events carry only IDs: fetch the object, then act.

## 7. Webhook endpoints (summary)

| Endpoint | Events from | Format | Events |
| --- | --- | --- | --- |
| `https://<cloud>/cloud/stripe/webhook` | platform account | snapshot | §2 + §3 + §4 events, risk events |
| `https://<cloud>/cloud/stripe/connect-webhook` | connected accounts | snapshot | §6 list, `event.account` gives the founder |
| `https://<cloud>/cloud/stripe/v2-events` | event destination | thin (v2) | `v2.core.account*` |

Each endpoint gets its own signing secret, and rotation is supported (accept the old and new secret during
rollover). Optionally add an IP allowlist of Stripe's webhook IPs. Turn on failure alert emails.

## 8. Test-mode setup steps (to do later, in `acct_1UCreIGpvdUG1rIZ` only, never livemode)

1. Settings: business name/branding (logo, emerald `#079A6A`) for Checkout, invoices and Portal. Statement
   descriptor `OPENGATE`. Public support URL/email.
2. Products/Prices: `OpenGate Cloud Team` → `$49/mo` (`team_monthly`) [+ `team_annual`];
   `OpenGate Cloud Enterprise` → yearly placeholder price (`enterprise_annual`); credit packs
   `OpenGate Credits $25/$100/$500` (one-time). If Billing Meters for overage: meter `opengate_overage_cents` +
   Product `OpenGate overage` with a metered price of $0.01/unit.
3. Customer Portal configuration as in §2 (via API so it's reproducible). Store the `bpc_` ID in config.
4. Payment methods: card, Link, Canadian PAD. Leave dynamic payment methods on.
5. Billing: Smart Retries, failed-payment and upcoming-renewal emails, invoice branding, net-30 default terms
   for invoices.
6. Tax: add a test registration for CA, then enable `automatic_tax` in the test config.
7. Webhook endpoints from §7 at API version `2026-09-30.endive`. Local dev: `stripe listen --forward-to
   localhost:8080/cloud/stripe/webhook`, or keep the built-in `opengate mock-stripe`.
8. Restricted keys (test first, mirror in live):
   - `opengate-cloud-billing`: Customers or Accounts v2 customer config **write**; Checkout Sessions **write**;
     Customer portal **write**; Subscriptions **write** (OpenGate cancels duplicate/incomplete subscriptions); Charges + Disputes **read** (risk events);
     Invoices **read**; Prices/Products **read**; Credit notes/Refunds **none** (handled in the Dashboard).
   - `opengate-cloud-usage` (overage/credits): Billing meter events **write**; Billing meters **read**;
     Credit grants **write** (if used).
   - `opengate-connect` (Phase 5): Accounts v2 **write**, Account Sessions/Links **write**, and **Connect
     permissions** on meter events write / subscriptions read for connected accounts.
   - The Dashboard/sales team uses its own logins, not keys.
9. Connect (Phase 5): platform profile (SaaS, Stripe-managed risk), branding for onboarding, Platform Pricing
   Tool if used, and a test connected account through embedded onboarding.

## 9. Security checklist

- RAKs only, least privilege, per-service, per-environment. Vault-stored, IP-restricted, rotated when staff
  leave. A pre-commit hook blocks `sk_`/`rk_`/`whsec_` strings. Never log keys or full Stripe error bodies
  that could echo request data.
- Webhooks: signature + 5-minute tolerance (done), dedupe (done), re-fetch (done). Reject events whose
  `livemode` doesn't match the configured key mode. A separate secret per endpoint, with rotation.
- Authorization: only org owners can open Checkout/Portal/onboarding (done). A connected-account ID is bound
  to exactly one org and is never accepted from the client.
- CSP: the dashboard only redirects to Stripe-hosted pages. If Stripe.js / Connect embedded components are
  added, extend CSP with `https://*.stripe.com` in `script-src`, `frame-src` and `connect-src`.
- 2FA (passkeys) and SSO on the Stripe Dashboard. Separate test/live access.

## 10. Phased rollout

| Phase | Scope | Exit criteria |
| --- | --- | --- |
| 0 | Foundations: API version pin, RAKs, Accounts v2 decision, webhook hardening (review A1-A6) | Mock + test-mode suite green |
| 1 | Team self-serve: Checkout/Portal/webhooks (built) + fixes from the review | Test-mode end to end: subscribe, upgrade, fail, recover, cancel, dispute |
| 2 | Enterprise: Quotes + `send_invoice` net 30, per-plan grace, operator link org↔customer | A test quote is accepted, the invoice is paid, entitlement tracks it |
| 3 | Credits: Checkout payment packs + ledger + async/refund handling | Idempotent grant, clawback on refund |
| 4 | Overage: Metronome or Billing Meters on the Team subscription | Usage invoice matches OpenGate logs |
| 5 | Connect: Accounts v2 merchant config, embedded onboarding, meter sync on connected accounts, app fee | A test founder bills a test end customer, the fee lands on the platform |
| Go-live | Live keys/endpoints, Tax registrations, Stripe go-live checklist | — |

## 11. Open decisions for Saief

1. **Entity and bank rails for Enterprise:** stay Canada-only (cards + PAD + manual wires), or open a US
   entity/Stripe account for ACH debit and US bank transfers?
2. **Accounts v2 now** (recommended, before any live customers) vs. ship Phase 1 on v1 Customers and migrate.
3. **Overage engine:** Metronome (Stripe's default; another vendor/contract) vs. Billing Meters (simpler, reuses code).
4. **Credits ledger:** OpenGate-native (recommended) vs. Stripe credit grants vs. Metronome; pack sizes, markup, expiry.
5. **Connect monetization:** SaaS fee only, application fee (what %?), or both. Platform Pricing Tool vs. per-charge fees.
6. **Existing-account onboarding** for founders who already use Stripe (OAuth connect vs. new account).
7. **Trials** for Team (length; card required?), and **grace periods** per plan (Team 7d, Enterprise ~30d?).
8. **Managed Payments** (Stripe as merchant of record for tax) vs. Stripe Tax with our own registrations. MP
   eligibility for a Canadian Connect platform is unconfirmed.

---

## 12. Review of the existing integration against this plan

Scope: Cloud billing on `v2-dev` (`internal/cloud/stripe.go`, `internal/cloud/plans.go`, `internal/server/cloud.go`,
`internal/stripemock`) and the Phase 2 customer meter sync (`internal/billing/billing.go`). What's already right:
hosted Checkout + Portal, no `payment_method_types`, HMAC verification with a 5-minute tolerance and
constant-time compare, event dedupe, re-fetching the subscription instead of trusting the payload, the
customer↔org binding guard, owner-only billing actions, local quota enforcement, a mock Stripe in CI, and
a config rule that stops cloud mode from metering tenant usage into the operator's account.

Severity: **High** = can charge customers wrongly or break entitlement in production; **Medium** = a best-practice
gap that should be fixed before go-live; **Low** = hardening/hygiene; **Info** = needed for later phases.

### A. Cloud subscriptions

| # | Sev | Finding | Fix |
| --- | --- | --- | --- |
| A1 | **High** ✅ fixed | **Double subscription.** The 409 guard only checks the *stored* subscription. An owner who opens Checkout twice (two tabs, or back-and-retry) gets two live Sessions and can pay both. `ApplySubscription` accepts the newer one and the older one keeps billing, which charges the customer twice. Checkout Session create has no idempotency key. | Store the open Session ID per org and expire it before creating another. Add an idempotency key per checkout attempt. In the webhook, if a second live subscription appears for a customer that already has one, alert and cancel/refund it. Test it with the mock. |
| A2 | Medium ✅ fixed | **No API version pin.** No `Stripe-Version` header, so behaviour follows the account default and changes on upgrade (the code already works around `current_period_end` moving). | Pin `2026-09-30.endive` in `do()`, and use the same version on the webhook endpoint. Add a config override. |
| A3 | Medium ✅ fixed | **Risk events not handled:** `charge.dispute.created/closed`, `charge.refunded`, `radar.early_fraud_warning.created`. Stripe treats these as required for a production subscription integration. | Handle them: audit log, alert the operator, optionally suspend the org on a lost dispute. Add them to the endpoint subscription list in `docs/cloud.md`. |
| A4 | Medium (deferred, D1) | **v1 Customers instead of Accounts v2.** Founder orgs will also become Connect accounts (Phase 5). Stripe guidance is one Account with `customer` + `merchant` configurations, not a separate Customer. | Decide now (open decision 2). If adopted, create an `Account` with `configuration.customer` and pass `customer_account` everywhere. Keep `stripe_customer_id` as a migration column. |
| A5 | Medium | **Enterprise can't be linked to Stripe.** `PlanForPrice` only knows configured self-serve prices, and Enterprise has no `stripe_price_id`. Quote/invoice subscriptions created by sales are ignored (or mapped only by customer). The operator path is a manual `billing/plan` override with no Stripe link. A single global `grace_period` (default 7d) would downgrade net-30 customers whose wire is late. | Allow several price IDs per plan (incl. `contact_sales` plans). Add an operator API to bind `cus_/acct_` → org. Add a per-plan `grace_period`. Treat `send_invoice` subscriptions with open, not-overdue invoices as in good standing. |
| A6 | Medium | **Quota period ≠ billing period.** `org_usage` is per calendar month UTC; Stripe bills from the subscription anchor. That's fine for flat quotas, wrong for overage (Phase 4). | Key usage by `current_period_start` of the live subscription (fall back to the calendar month for Free). |
| A7 | Low | `tax_id_collection` is only sent when `automatic_tax` is on. B2B buyers want tax IDs on invoices either way. `automatic_tax` can be switched on with no registration (it silently collects nothing). | Always send `tax_id_collection[enabled]=true`. Add a docs/config warning, or a startup check that a registration exists via the Tax Registrations API (read). |
| A8 | Low | No `subscription_data[billing_mode][type]=flexible` and no `integration_identifier`. | Add both (after confirming the defaults on the pinned version). |
| A9 | Low ✅ fixed | Portal sessions use the Dashboard default configuration. | Create a portal configuration via API (§2) and pass `configuration`. Restrict switching to Team prices. |
| A10 | Low ✅ fixed | `event.livemode` is parsed but not checked. | Reject events whose `livemode` differs from the key mode (`sk_live`/`rk_live` vs. `_test`). |
| A11 | Low | Single webhook secret, so it can't rotate without downtime. | Accept a comma-separated `STRIPE_WEBHOOK_SECRET` list (Stripe signs with all active secrets during rollover). |
| A12 | Low ✅ fixed | Dedupe is check-then-mark (not atomic), so two concurrent duplicate deliveries can both process. Harmless today (the handlers are idempotent re-fetches), but it would double-grant credits in Phase 3. | Insert the event ID first (`INSERT … ON CONFLICT DO NOTHING`, and process only if a row was inserted). Delete it on failure so Stripe's retry reprocesses. |
| A13 | Low | One `STRIPE_API_KEY` env var feeds both cloud billing and meter sync. The docs say "secret key". | Recommend `rk_` keys with the §8 permissions, and separate env vars per job (`STRIPE_BILLING_KEY`, `STRIPE_USAGE_KEY`). |
| A14 | Low | No retry/backoff on 429/5xx or network errors for Stripe API calls. | Retry with jitter only for requests with idempotency keys. Surface a clear error otherwise. |
| A15 | Low | The customer idempotency key `opengate-customer-<org>` is reused forever. Within 24h, different params (for example a different owner email) return an idempotency error. | Include a per-attempt nonce, and rely on the stored `stripe_customer_id` to avoid duplicates. |
| A16 | Low | Downgrading via the portal leaves over-limit projects/seats in place. | Mark the org over-limit: block new projects/invites until it's back within limits (already blocked by `planLimit`). Show a dashboard banner. |
| A17 | Info | No `customer.updated` / account-updated handling. Fine, because OpenGate stores no customer PII copy. Keep it that way. | — |

### B. Phase 2 customer meter sync (`internal/billing`)

| # | Sev | Finding | Fix |
| --- | --- | --- | --- |
| B1 | **High** ✅ fixed | **A timeout can double-bill end customers.** The meter event `identifier` (and Idempotency-Key) is a hash of the window `[synced_to, end]`, and `end = now - lag` moves on every run. If Stripe accepts an event but the response is lost (timeout/5xx), the cursor doesn't advance. The next run sends a *different* identifier for an overlapping, larger window, so Stripe counts the usage twice. Two instances running the sync (multi-instance Postgres) hit the same problem because each computes its own `end`. | Persist a pending window (`from`, `end`, `identifier`) before sending, and resend exactly that window until it's acknowledged; only then start the next one. Or use fixed buckets (for example hourly `[h, h+1)`, identifier = hash(project, customer, h)) so every retry and every instance produces the same identifier. Guard the job with a DB lease for multi-instance. |
| B2 | Medium | **Stale windows fail forever.** Stripe rejects meter events older than ~35 days. A customer whose sync has been failing that long never catches up and gets no alert. | Alert on `last_error` age. Cap the window at the oldest acceptable timestamp and report or record the gap. Handle `billing.meter.error_report_triggered`. |
| B3 | Medium | **Only works for the operator's own Stripe account.** No `Stripe-Account` header and one global key. Phase 5 needs per-org connected accounts (and the cloud-mode guard currently forbids meter sync). | Add `stripe_account` per org/project. Send `Stripe-Account: acct_…` with the platform key. Relax the cloud guard only for orgs with a verified connected account. Map the founder's end customers to `cus_` IDs *on their account*. |
| B4 | Low ◐ partly | Duplicate detection matches English error text (`"identifier" … "already"`). | Match on the error `code`. Don't treat other 400s as success. |
| B5 | Low ◐ partly | No API version pin or retry/backoff, and errors don't separate retryable (429/5xx) from permanent (400 unknown customer / meter inactive). | Same client hardening as A2/A14. Mark permanent failures so they stop retrying and alert. |
| B6 | Low | `customer_mapping` payload key is hard-coded to `stripe_customer_id`. | Make it configurable to match the meter definition (and `customer_account` mapping under Accounts v2). |
| B7 | Info | Sync is one event per customer per interval (good: aggregated, well under rate limits). The first sync waits a full interval after boot. | Optionally sync once on start. |

### C. Docs, mock and tests

| # | Sev | Finding | Fix |
| --- | --- | --- | --- |
| C1 | Low | `docs/cloud.md` setup lists only lifecycle events, recommends a secret key, and has no API-version step. | Update it from §7-§8 when implementing. |
| C2 | Low | The mock can't simulate disputes, refunds, async payment methods, duplicate open sessions, or lost responses. | Extend `stripemock` alongside the A1/A3/B1 fixes so each has a regression test. |
| C3 | Info | Tests never touch real Stripe. Before launch, run the same scenarios once against test mode (`acct_1UCreIGpvdUG1rIZ`) with `stripe listen`. | Add a manual/opt-in `-tags stripetest` suite. |

**Fix order:** B1 and A1 (money correctness), then A2, A3, A10, A12 (cheap hardening), then A4/A5 with the
Accounts v2 and Enterprise decisions, and the rest with their phases.

### Implementation status (2026-10-09)

- **B1:** usage is reported in fixed, epoch-aligned windows (`billing.stripe.window`, default 1h). The identifier is
  sha256(project, customer, from, to), which is the same on every retry and every replica. The cursor advances by
  compare-and-swap, and a `job_leases` DB lease makes one replica the writer. Tests: lost-response retry (no double count),
  two concurrent replicas with skewed clocks, and window math.
- **A1:** one open Checkout Session per org (`org_billing.checkout_session_id/plan`). It is reused on a second click,
  expired on a plan switch, and expired once the org is subscribed. Idempotency keys are derived from the request
  state. Before creating a session, the customer's subscriptions are listed: a live one is adopted (409), and an
  `incomplete` one is canceled. A second live subscription arriving by webhook is **canceled** (audit + alert), and
  events for non-current subscriptions are ignored. Mock + live tests.
- **A2:** `Stripe-Version: 2026-09-30.endive` on every call (cloud client and meter sync), overridable with `api_version`.
- **A3:** disputes, refunds and early fraud warnings are audited and alerted. An open dispute flags the org. A lost
  dispute or an early fraud warning suspends paid limits until `POST /admin/api/billing/risk/clear` (operator). A won
  dispute clears its own flag. The dashboard shows a banner.
- **A9:** the portal configuration `bpc_...` is passed (`cloud.stripe.portal_configuration`).
- **A10:** events whose livemode differs from the key mode get 400. **A12:** a unique-insert claim (`processing` → `done`),
  released on failure, with stale-claim takeover after 5 minutes.
- **Found in the live run:** on 2026-09-30, portal "cancel at period end" sets `cancel_at`, not `cancel_at_period_end`.
  It is now handled, and the mock behaves the same way.
- B4/B5: duplicates are matched on `code=resource_already_exists` first (English text kept as a fallback), and the
  version is pinned. Retry/backoff and permanent-error classification are still open.
