# OpenGate Cloud (hosted, multi-tenant)

Cloud mode turns one OpenGate deployment into a hosted product. Anyone can sign up, gets an organization on the
free plan, and can upgrade with Stripe Checkout. It runs the same single binary with `cloud.enabled: true`;
there is no separate service.

| | |
|---|---|
| **Sign-up** | Public `/signup`, `/login`, and `/logout` (separate from the operator admin token). Creates a user and an org they own. Rate limited. When Resend or SMTP is configured, the account stays signed out until the single-use verification link is consumed; password reset is `/forgot-password`. See [LAUNCH.md](LAUNCH.md). |
| **Plans** | Configurable catalog (`cloud.plans`). Limits: requests per month, seats, projects, log retention. Draft defaults: Free, Team ($49/mo), Enterprise (contact sales). |
| **Payments** | Stripe Checkout (subscription mode), the Stripe Customer Portal (card, invoices, cancel, plan change), and signed webhooks. |
| **Enforcement** | Gateway requests over quota get `429 insufficient_quota / plan_quota_exceeded` (OpenAI-SDK compatible). Seats and projects over the limit get `402 plan_limit_reached`. Logs older than the plan's retention are deleted hourly. |
| **Tenancy** | Bring-your-own provider keys per org (encrypted). Tenants can never use the operator's env provider keys or Stripe meter sync (refused at startup). |

Screenshots: [`screenshots/cloud/`](screenshots/cloud/) (sign-up, billing on Free, mock Checkout, billing on Team, mock portal).

## Try it locally (no Stripe account)

```bash
make cloud-demo      # mock provider + mock Stripe + OpenGate in cloud mode
# open http://127.0.0.1:8080/ui/  ·  demo@example.com / demo-only-password  ·  Plan & billing  ·  Upgrade to Team
```

`opengate mock-stripe` runs a fake Stripe: customers, Checkout Sessions with a hosted "Subscribe" page, portal sessions
with *cancel*, *cancel at period end*, *simulate failed renewal* and *recover* buttons, subscription retrieval, and
**signed** webhook delivery back to OpenGate. It is for development and tests only. The integration tests drive
the same mock (`integration/cloud_test.go`).

## How billing works

```
Owner clicks Upgrade ──► POST /admin/api/billing/checkout ──► Stripe customer (one per org, idempotent)
                                                         ├─► list the customer's subscriptions: adopt a live one (409),
                                                         │   cancel an incomplete one
                                                         └─► reuse the org's open Checkout Session, or expire it and
                                                             create one (idempotency key; client_reference_id = org,
                                                             subscription_data.metadata.org_id = org) ──► redirect
Stripe ──► POST /cloud/stripe/webhook (signed) ──► verify ─► livemode matches key ─► atomic claim of event id
        ──► GET /v1/subscriptions/{id}  (fresh state; never trust event payload order)
        ──► org_billing: plan (by price id), status, period end, cancel_at_period_end ──► audit log ("stripe")
```

* **Effective plan:** an org flagged by a lost dispute or a Radar early fraud warning drops to `default_plan` until the
  operator clears it (`POST /admin/api/billing/risk/clear`, admin token). Otherwise `active`/`trialing` keep the paid plan. `past_due` keeps it for `cloud.grace_period` (default 7 days)
  while Stripe retries the payment. `unpaid`/`canceled`/`incomplete_expired` fall back to `default_plan`. Plans assigned
  by the operator (`PUT /admin/api/billing/plan`, admin token only; e.g. Enterprise invoiced offline) always apply.
* **Webhook safety:**
  * HMAC-SHA256 signatures with 5-minute replay tolerance, constant-time compare, and multiple `v1` signatures accepted (secret rotation).
  * Every request pins `Stripe-Version: 2026-09-30.endive`; create the webhook endpoint at the same version.
  * Events whose `livemode` doesn't match the key (`sk_live_`/`rk_live_` vs. test) are rejected with 400.
  * Event ids are claimed with a unique insert before processing, so concurrent deliveries are handled once;
    a failed handler releases the claim so Stripe's retry reprocesses it.
  * **No double subscriptions:** one payable Checkout Session per org (reused on a second click, expired on a plan
    switch or once the org is subscribed), idempotency keys on every create, a live subscription the webhook missed
    is adopted instead of opening another Checkout, and a second live subscription for the same customer is canceled
    automatically (audited + alert; refund its first invoice in the Dashboard if it was paid).
  * **Risk events:** `charge.dispute.created|updated|closed`, `charge.refunded` and `radar.early_fraud_warning.created`
    are audited and sent to the alert webhook. An open dispute only flags the org; a lost dispute or an early fraud
    warning suspends paid limits; a won dispute clears its flag.
  * Cancellation scheduled in the portal is recognized whether Stripe sets `cancel_at_period_end` or (current API
    versions) `cancel_at`.
  * Subscriptions are re-fetched, so stale or out-of-order events can't resurrect a canceled plan.
  * A Stripe customer is bound to exactly one org (unique index). Subscriptions of a different customer, unknown prices and unknown orgs are ignored and logged.
  * Processing failures answer 5xx, so Stripe retries for up to 3 days.
* **Who can pay:** only org **owners** (or the operator) can open Checkout or the portal. Admins and viewers see the plan
  and usage read-only. Stripe price ids are never exposed to the browser.
* **Usage counter:** every authenticated gateway request (including cache hits and requests rejected later by budgets
  or guardrails) increments `org_usage` for the calendar month (UTC). The operator's own `org_default` is never limited.

## Stripe setup (test mode first, then live)

1. **Product and price.** Create a product "OpenGate Cloud Team" with a recurring monthly price ($49, or your final
   price). Copy the `price_...` id into `STRIPE_PRICE_TEAM`. Add more plans by adding entries to `cloud.plans` with
   their own `stripe_price_id`.
2. **Customer portal:** create a configuration via the API (reproducible; see [stripe-test-setup.md](stripe-test-setup.md)):
   payment-method update, invoice history, cancel at period end (with reasons), plan switching off for now. Put its
   `bpc_...` id in `cloud.stripe.portal_configuration`.
3. **Webhook endpoint** (Developers → Webhooks), API version `2026-09-30.endive`:
   `https://<your-domain>/cloud/stripe/webhook`, with events `checkout.session.completed`, `checkout.session.expired`,
   `customer.subscription.created|updated|deleted|trial_will_end`, `invoice.paid`, `invoice.payment_failed`,
   `invoice.payment_action_required`, `charge.dispute.created|updated|closed`, `charge.refunded`,
   `radar.early_fraud_warning.created`. Copy the signing secret into `STRIPE_WEBHOOK_SECRET`.
4. **API key.** Prefer a *restricted* key (`rk_...`): **write** Customers, Checkout Sessions, Customer portal and
   Subscriptions (needed to cancel duplicate/incomplete subscriptions); **read** Charges and Disputes. Put it in
   `STRIPE_API_KEY`.
5. **Local testing against real Stripe test mode:** `stripe listen --forward-to localhost:8080/cloud/stripe/webhook`
   prints a `whsec_...` for local use. Pay with card `4242 4242 4242 4242`. (Events from `stripe trigger` carry no org
   metadata and are correctly ignored.) `examples/cloud-stripe-test.yaml` is the config used for the recorded live
   test-mode run ([stripe-test-setup.md](stripe-test-setup.md)).
6. **Trial:** the launch default is 14 days with no card (`trial_days: 14`, `trial_without_card: true`). Only an org's
   first subscription gets a trial; a trial that ends without a card is canceled and the org returns to Free.
7. Optional: `cloud.stripe.automatic_tax: true` once a Stripe Tax registration exists; `allow_promotion_codes`.

## Deploy

Templates live in [`deploy/`](../deploy/). Each one runs **one replica on PostgreSQL**: budget reservations are
per-process, so more replicas can briefly overshoot spend limits (see [security model](security-model.md)).

| Target | Files |
|---|---|
| Any VM (Docker) | `deploy/docker-compose.cloud.yml` + `deploy/Caddyfile` (automatic HTTPS) + `examples/env.cloud.example` |
| Fly.io | `deploy/fly.toml` (Fly Postgres; `auto_stop_machines = "off"` so background jobs run) |
| Render | `deploy/render.yaml` (Blueprint with managed Postgres) |
| Kubernetes | `deploy/k8s/opengate.yaml` (Deployment `Recreate`, non-root, read-only rootfs; Service; nginx Ingress with SSE buffering off) + `secret.example.env` |

All of them use [`deploy/cloud.yaml`](../deploy/cloud.yaml) (strict budgets, trusted private proxy ranges,
`max_inflight`, and the draft plan catalog). `TestDeployTemplates` loads it in CI so it can't drift from the config schema.

Required environment: `OPENGATE_DATABASE` (postgres), `OPENGATE_PUBLIC_URL` (https), `OPENGATE_ADMIN_TOKEN`,
`OPENGATE_MASTER_KEY` (back it up!), `STRIPE_API_KEY`, `STRIPE_WEBHOOK_SECRET`, `STRIPE_PRICE_TEAM`.

## Go-live checklist

- [ ] Domain and TLS, with `OPENGATE_PUBLIC_URL` set to it (cookies become `Secure` automatically).
- [ ] Managed Postgres with backups; `sslmode=verify-full` when it isn't on a private network.
- [ ] Master key stored in a secret manager *and* an offline backup.
- [ ] Stripe live mode: product and price, portal settings, webhook endpoint (live secret), restricted live key.
- [ ] Run one real live-mode purchase and refund it.
- [ ] `/metrics` scraped (`OPENGATE_METRICS_TOKEN`), alerts on 5xx and on `stripe webhook processing failed` log lines.
- [ ] Terms of service, privacy policy, DPA and a refund policy linked from the landing page and sign-up.
- [ ] Support and sales addresses (`cloud.sales_url`) and a security contact (`SECURITY.md`).

## Not yet in Cloud (deliberately deferred)

* **Email:** address verification, password reset and invite emails need a transactional email provider (invites are
  links you share yourself today). Until then, sign-ups are throttled per IP but emails are unverified.
* Usage-based overage billing (metered prices) and annual plans. Today hitting the quota blocks until you upgrade.
* In-app invoices (the portal has them), account deletion and data export, SSO and 2FA.
* Exact spend limits across several replicas (DB-backed reservations). Run one replica until then.
