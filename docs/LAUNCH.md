# Launch checklist

This is the operator checklist for taking OpenGate Cloud from the public demo
(`deploy/fly-demo`, Fly app `opengate`) to a separate production app
(`deploy/fly-prod`, placeholder app `opengate-prod`, public URL
`https://opengatehq.com`). Nothing in this repository deploys, changes DNS, or
stores live Stripe keys. Do those steps yourself, in the platform's secret
store.

Legal pages at `/terms`, `/privacy`, and `/pricing` are drafts marked **FOR
LEGAL REVIEW** (OpenGate, Quebec, Canada). Replace them before accepting
customers.

## 1. App and database

Production config is `deploy/fly-prod/fly.toml`. It sets `OPENGATE_ENV=production`,
`min_machines_running = 1`, `auto_stop_machines = "off"`, and health checks on
`/readyz` and `/livez`. It does not set `OPENGATE_DEMO_MOCK_LISTEN`,
`OPENGATE_DEMO_TRAFFIC`, or `OPENGATE_PUBLIC_PLAYGROUND`. With
`OPENGATE_ENV=production` the process refuses those flags and refuses to serve
a database path that contains `demo.db`.

Create a new Fly app. Do not deploy onto the demo app and do not mount the
demo volume `opengate_data`.

```bash
fly apps create opengate-prod
fly secrets set -a opengate-prod \
  OPENGATE_MASTER_KEY="$(go run ./cmd/opengate keygen | sed -n 's/^OPENGATE_MASTER_KEY=//p')" \
  OPENGATE_ADMIN_TOKEN="$(openssl rand -hex 24)"
```

Keep a copy of `OPENGATE_MASTER_KEY` outside the database. Losing it makes
stored provider keys unreadable. `OPENGATE_ADMIN_TOKEN` is the operator token.
Customer signup (`/signup`) is a separate password login.

### Postgres (preferred)

Point `OPENGATE_DATABASE` at a fresh managed database (Fly Postgres, RDS, or
another host). Use `sslmode=require` for anything that is not on a private
network. The entrypoint skips Litestream for `postgres://` and `postgresql://`
URLs. Migrations run on start and are recorded in `schema_migrations` (current
version includes email verification, migration 10).

Backups are the database host's job (the provider's automated snapshots, or
`pg_dump` on a schedule you run). Restore by loading a dump into a new
database, setting `OPENGATE_DATABASE` to that URL, and starting the app. Do
not restore a demo database into this app.

### SQLite + Litestream

Use this when you want one machine and a file database. Create a **new**
volume and uncomment the `[[mounts]]` block in `deploy/fly-prod/fly.toml`
(`opengate_prod_data` → `/data`). Set:

| Variable | Purpose |
|---|---|
| `OPENGATE_DATABASE` | `/data/opengate.db` |
| `LITESTREAM_BUCKET` | Bucket name. Replica path is `s3://$LITESTREAM_BUCKET/opengate`. |
| `LITESTREAM_ACCESS_KEY_ID` | S3-compatible access key. Mapped to `AWS_ACCESS_KEY_ID` if that is unset. |
| `LITESTREAM_SECRET_ACCESS_KEY` | Secret key. Mapped to `AWS_SECRET_ACCESS_KEY` if that is unset. |
| `LITESTREAM_ENDPOINT` | Optional. Required for non-AWS S3 (Tigris, R2, MinIO), for example `fly.storage.tigris.dev`. |
| `LITESTREAM_REGION` | Optional region (`auto` for Tigris). |
| `LITESTREAM_FORCE_PATH_STYLE` | `true` for providers that need path-style URLs. |

`Dockerfile.prod` installs Litestream 0.5.17. On boot the entrypoint runs
`litestream restore -if-replica-exists -if-db-not-exists` and then
`litestream replicate -exec` so OpenGate is the child process. A missing
bucket in production exits before serving. `OPENGATE_SQLITE_NO_REPLICA=1`
acknowledges the risk and is not a backup.

Manual restore onto an empty file (stop OpenGate first so nothing else has
the database open):

```bash
export AWS_ACCESS_KEY_ID="$LITESTREAM_ACCESS_KEY_ID"
export AWS_SECRET_ACCESS_KEY="$LITESTREAM_SECRET_ACCESS_KEY"
export OPENGATE_DATABASE=/data/opengate.db
export LITESTREAM_REPLICA_URL="s3://$LITESTREAM_BUCKET/opengate"
# add ?endpoint=...&region=...&force-path-style=true when you set those env vars
litestream restore -config deploy/fly-prod/litestream.yml "$OPENGATE_DATABASE"
```

Point-in-time: add `-timestamp 2026-10-26T00:00:00Z` (UTC). The restored file
includes Litestream's `_litestream_lock` table; leave it in place.

## 2. Email

Signup verification and password reset send mail only when a provider is
configured. With neither Resend nor SMTP set, the process logs the message
and treats the account as already verified so local development still works.
Production must set a provider or customers are not asked to confirm email.

| Variable | Purpose |
|---|---|
| `RESEND_API_KEY` | Preferred. HTTPS API, no SMTP. |
| `MAIL_FROM` | From address, for example `OpenGate <noreply@opengatehq.com>`. `RESEND_FROM` and `SMTP_FROM` are aliases. Required when a provider is set. |
| `SMTP_HOST` | Fallback when `RESEND_API_KEY` is unset. |
| `SMTP_PORT` | Default `587` (STARTTLS). |
| `SMTP_USER` / `SMTP_PASSWORD` | Optional SMTP auth. |

Verify the sending domain with the provider before launch. Verification links
expire in 24 hours, reset links in 1 hour, and each token works once.
`/forgot-password` always answers the same way whether or not the address
exists.

## 3. Stripe: test mode, then live

Checkout (`POST /admin/api/billing/checkout`), the customer portal
(`POST /admin/api/billing/portal`), and `POST /cloud/stripe/webhook` are
implemented. The webhook checks the Stripe signature (5 minute tolerance),
rejects a tampered body, a wrong secret, and a stale timestamp, ignores
replays, and refuses a livemode that does not match the API key. Live keys
(`sk_live_`, `rk_live_`) are refused unless `OPENGATE_STRIPE_ALLOW_LIVE=true`.

Stay on test keys until every box below is done. Test-mode object IDs from
the sandbox write-up are in [stripe-test-setup.md](stripe-test-setup.md).
Integration coverage is `integration/cloud_test.go` (checkout, portal,
webhook). Repeat checkout against the mock or your Stripe test account
before flipping the live switch.

- [ ] `OPENGATE_PUBLIC_URL` is the real `https://` origin (placeholder in the prod config is `https://opengatehq.com`). Stripe success and portal return URLs use it.
- [ ] Test-mode product, price, and Customer Portal configuration exist. `STRIPE_PRICE_TEAM` is the test price. `cloud.stripe.portal_configuration` is the test `bpc_...` if you do not want the account default.
- [ ] Webhook endpoint in **test mode**: `https://<public host>/cloud/stripe/webhook`. Subscribe at least `checkout.session.completed`, `customer.subscription.created`, `customer.subscription.updated`, `customer.subscription.deleted`, `invoice.paid`, and `invoice.payment_failed`.
- [ ] `STRIPE_API_KEY=sk_test_...` and `STRIPE_WEBHOOK_SECRET=whsec_...` (the test endpoint's secret). Confirm an unsigned POST returns 400 and a signed test event updates the org plan.
- [ ] Create the **live** product, price, and portal configuration (do not reuse test IDs). Put the live price in `STRIPE_PRICE_TEAM` and the live `bpc_...` in the portal configuration.
- [ ] Add a **live** webhook endpoint to the same path. Its `whsec_` is different from the test secret.
- [ ] Replace the secrets: `STRIPE_API_KEY=sk_live_...`, `STRIPE_WEBHOOK_SECRET` (live), `STRIPE_PRICE_TEAM` (live). Then set `OPENGATE_STRIPE_ALLOW_LIVE=true`.
- [ ] Run one live checkout for a real card you control, open the portal, and cancel in test-of-the-live-flow only after the webhook shows `200` in the Stripe dashboard. Turn Stripe Tax on only after a Quebec registration exists (`automatic_tax` stays false in the template).

Never commit these values. `OPENGATE_STRIPE_ALLOW_LIVE` left unset or `false`
is the safe default.

## 4. Monitoring and uptime

`OPENGATE_ALERT_WEBHOOK_URL` is the existing Slack-compatible webhook. Budget
and kill-switch events still go there. HTTP 5xx responses also send one event
per path and status per minute:

```json
{"kind":"http_error","status":500,"path":"/v1/chat/completions","method":"POST","request_id":"...","text":"OpenGate HTTP 500 POST /v1/chat/completions"}
```

`GET /metrics` (bearer `OPENGATE_METRICS_TOKEN`) includes
`opengate_http_errors_total{class,path}` for both 4xx and 5xx. `path` is a
coarse label (`/v1`, `/admin`, `/cloud`, `/public`, `/mcp`, `/account`,
`other`), not the raw URL.

Uptime, outside the Fly checks already in `deploy/fly-prod/fly.toml`:

1. Probe `GET https://opengatehq.com/readyz` every minute from a service you
   run (Better Stack, UptimeRobot, or a synthetic check). Non-200 means the
   process is up but the database is not.
2. Probe `GET https://opengatehq.com/livez` the same way. Non-200 means the
   process is down.
3. Alert when either check fails twice in a row, and when the webhook
   receives `kind=http_error`.
4. Do not point the probe at `/public/playground` or at a path that spends a
   provider key.

## 5. What stays off

- `POST /public/playground` is unregistered unless `OPENGATE_PUBLIC_PLAYGROUND`
  is on **and** `OPENGATE_DEMO_MOCK_LISTEN` is set. The handler still returns
  403 unless every provider base URL is loopback. The prod config sets neither
  variable. The demo config (`deploy/fly-demo/fly.toml`) opts in.
- Customer signup is on in cloud mode (`auth.allow_signup`). It is rate
  limited (5 account creates per 10 minutes per IP, 30 per hour globally,
  plus the admin lockout). Password reset is 5 per hour per IP and 3 per hour
  per email.
- HTML responses, including `/signup`, `/terms`, `/privacy`, and `/pricing`,
  send `Content-Security-Policy` (`default-src 'self'`) and
  `X-Frame-Options: DENY`.

## 6. Deploy command (you run this)

```bash
fly deploy -c deploy/fly-prod/fly.toml
```

Confirm `/readyz` is 200, create one account through `/signup`, open the
verification link, and sign in. Operator access remains the admin token on
the dashboard, not the customer form.
