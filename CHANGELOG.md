# Changelog

All notable changes to OpenGate. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the
project uses [Semantic Versioning](https://semver.org/).

## [Unreleased] (v2-dev)

### Added: OpenGate Cloud (hosted, multi-tenant mode)
- `cloud:` config. Self-serve sign-up (per-IP throttle) and a plan catalog (Free / Team / Enterprise drafts) with monthly
  request quotas, seat, project and orgs-per-user limits, and per-plan log retention.
- Stripe Checkout (subscriptions), the Customer Portal, and signed and de-duplicated webhooks that re-fetch subscription
  state. A past-due grace period, operator-assigned plans, and billing changes recorded in the audit log.
- Dashboard: a "Plan & billing" page (usage meters, plan cards, upgrade/manage) and sign-up deep links (`#signup?plan=team`).
- `opengate mock-stripe` (a fake Stripe with hosted checkout and portal pages that sends signed webhooks) and `make cloud-demo`.
- Deploy templates: Docker Compose + Caddy, Fly.io, Render and Kubernetes (`deploy/`); see `docs/cloud.md`.
- Migration v6 (`org_billing`, `org_usage`, `stripe_events`). Admin API: `GET /admin/api/billing`,
  `POST /admin/api/billing/checkout`, `POST /admin/api/billing/portal`, `PUT /admin/api/billing/plan`; webhook `POST /cloud/stripe/webhook`.

### Security
- Cloud mode refuses to start with `share_env_provider_keys` or operator Stripe meter sync (tenant isolation).

## [1.0.1] - 2026-10-09

### Security
- Routing rules are evaluated per organization only. Before this fix, an enabled org-wide rule in one org could win evaluation for another org's requests and was then discarded, so the other org's own rules silently stopped applying. Covers both live traffic and the routing preview.
- The invite preview is now `POST /admin/api/auth/invites/preview` with the token in the body, so invite tokens no longer appear in URLs or access logs. The old `GET /admin/api/auth/invites/{token}` route is removed.
- Argon2id password work (login, sign-up, invite accept, password change) is capped at 2×CPU concurrent operations. Excess requests get `429 auth_busy`. This prevents memory exhaustion and keeps parallel guessing from outrunning the lockout.
- Accepting an invite never changes an existing membership: the higher of the current and invited roles is kept, so an owner can't be demoted (and an org can't lose its last owner) through an invite.
- `GET /admin/api/logs/{id}` now fails closed if the log's project can't be resolved.

### Changed
- The demo account is now `demo@example.com` / `demo-only-password`.

## [1.0.0] - 2026-10-09

First public release.

### Gateway
- OpenAI-compatible `/v1/chat/completions` (streaming SSE and non-streaming), `/v1/embeddings`, `/v1/responses` (OpenAI passthrough), `/v1/models`.
- Adapters for OpenAI, Anthropic (full Messages API translation incl. tools and streaming), Google Gemini, OpenRouter and any OpenAI-compatible server.
- Aliases with ordered failover, and retries with backoff on 429, 5xx and timeouts.
- An editable, source-cited price table with cache-read/write and long-context pricing, hot-reloaded on `SIGHUP`.

### Spend control
- Daily, monthly and total USD limits plus RPM per key, agent and project. Worst-case cost is reserved atomically before every call and reconciled afterwards.
- A kill switch per key and project that aborts in-flight streams, and propagates across replicas within ~2 s.
- Slack-compatible webhook alerts at configurable thresholds.
- Model allow/deny lists and `max_tokens` caps per key and project. Per-project opt-in for unbudgetable passthrough features (`background`, `n`, hosted tools, `store`).

### Differentiators
- Smart routing: rules by prompt length, tags, headers, agent and cost ceiling, with per-rule fallbacks, plus an `auto` alias that picks the cheapest model meeting the request's difficulty tier. Savings are tracked against the requested model.
- Guardrails: PII and secret detection (email, phone, Luhn-checked cards, IBAN, SSN/SIN, API keys, JWTs, private keys) with off/log/redact/block modes on requests and responses, plus prompt-injection heuristics.
- Customer billing: end-customer attribution (trusted header or key binding), markup rules, monthly CSV export, Stripe Billing Meter sync.
- An opt-in exact-match response cache scoped per project, key and customer.

### Teams and operations
- Organizations, argon2id user accounts, sessions, owner/admin/member/viewer roles, invites, an append-only audit log, and per-org provider keys.
- An embedded dashboard (vanilla JS, strict CSP) with overview, logs, keys, projects, budgets, routing, guardrails, customers and billing, providers, team, audit and settings pages.
- SQLite (default) or PostgreSQL storage with migrations. Shared rate limits survive restarts.
- Prometheus `/metrics`, JSON logs, `/healthz`, `/livez` and `/readyz`.
- An OpenAPI 3 spec for the admin API, plus Go and TypeScript client examples.
- A distroless non-root Docker image, compose examples (SQLite and Postgres), and GoReleaser config for linux/darwin/windows/freebsd.

### Security
- Fixes from an internal security review before release: org scoping of every admin route, `max_tokens` normalization, passthrough feature allowlist, client-asserted customer IDs off by default, trusted-proxy aware client IPs, a lockout that can't lock out the real credential and is bounded, a dedicated metrics token, https-only upstreams, generic error messages, a kill re-check, and an optional `max_inflight` cap. See `SECURITY.md`.

[1.0.0]: https://github.com/opengate-dev/opengate/releases/tag/v1.0.0
