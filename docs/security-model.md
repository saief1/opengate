# Security model

OpenGate holds every provider key you own and sees every prompt, so it's designed to be small, auditable and
secure by default. To report a vulnerability, see [SECURITY.md](../SECURITY.md).

## Assets and trust boundaries

| Asset | Protection |
|---|---|
| Provider API keys | AES-256-GCM under `OPENGATE_MASTER_KEY`, with a random nonce and AAD (row id + provider) so ciphertexts can't be moved between rows. Never returned (only a 4-char hint), never logged, sent only in the upstream auth header. |
| Virtual keys (`og-…`) | 192-bit random values. Only SHA-256 hashes are stored, and lookup is by hash. |
| Admin token | Env only, at least 16 chars, compared in constant time. |
| User passwords | argon2id (OWASP parameters, 16-byte salt, PHC format), with bounded parameters on verify and a dummy verify for unknown users. |
| Sessions and invites | 256-bit random tokens, stored hashed. Sessions have an idle TTL plus an absolute max age. Invites are single-use, expire, and can't grant a role above the inviter's. |
| Prompts and responses | Not stored unless a project enables body logging. They then pass through guardrail redaction and are truncated. |
| Spend | Enforced before the request leaves (see below). |

Fully trusted: whoever holds `OPENGATE_MASTER_KEY` plus the database, or `OPENGATE_ADMIN_TOKEN`. Out of scope: a
compromised host (process memory holds decrypted keys). TLS termination is delegated to your proxy.

## Supply chain

One static, pure-Go binary (no CGO), built with `-trimpath` and `go mod verify` in a distroless non-root image with a
read-only rootfs and no capabilities. Direct dependencies: `modernc.org/sqlite`, `github.com/jackc/pgx/v5`, `gopkg.in/yaml.v3`
and `golang.org/x/crypto`. The dashboard is vanilla JS with no npm packages, embedded with `go:embed`.

## Authentication and tenancy

* **Gateway:** `Authorization: Bearer og-…`. Client headers are never forwarded upstream, and neither is the virtual key.
* **Admin API:** admin token (bearer) or a dashboard session cookie (`HttpOnly`, `SameSite=Strict`, `Secure` over TLS).
  Cookie-authenticated writes also need `X-Requested-With: opengate`. There is no CORS, so cross-origin reads are blocked.
* **Roles:** viewer < member < admin < owner, checked per route. Global operations (pricing reload) need owner.
* **Organizations:** every admin route is scoped to the caller's org. IDs from another org answer `404`
  (`integration/security_test.go` checks every object route). Provider keys are per org, and env keys serve only the
  default org unless `share_env_provider_keys` is set.
* **Brute force:** wrong admin tokens lock out per client IP, passwords per account+IP (with a 5× looser per-IP spray
  limit). The correct credential always works, so nobody can lock the operator out. The table is bounded (50k entries)
  and expires. With `trust_proxy_headers`, `X-Forwarded-For` is read from the right, and with `trusted_proxies` only
  from those peers.
* **Audit log:** every state-changing admin action, sign-in and invite is appended with actor, IP and target. Secrets
  are never recorded.

## Budgets and replicas

* Before each request, its worst-case cost (prompt estimate + `max_tokens` × `n` × the most expensive candidate target)
  is reserved atomically against every applicable limit (key, agent, project) and reconciled with actual usage afterwards.
* `max_tokens` and `max_completion_tokens` are collapsed into one field holding the larger value before the cap check
  and before forwarding.
* Passthrough features OpenGate can't price or bound are refused unless the project allows them (`allowed_features`):
  Responses `background`, provider-hosted tools (web or file search, code interpreter, image generation, MCP, computer
  use), audio output, chat `n > 1` and `store: true`. A success without usage is charged at the worst case.
* Unknown models are charged at the table's `default` price. A startup warning fires if a custom table has none and
  `budgets.strict` is off.
* **Multiple replicas (Postgres):** committed spend, rate-limit buckets and kill flags are shared through the database.
  New requests for a killed key are refused everywhere immediately, and in-flight streams on other replicas are aborted
  within ~2 s (each replica polls the kill flags of its in-flight requests). **Reservations are per process**, so with N
  replicas a limit can be overshot by up to N × (concurrent requests) × (request cost) within one `budgets.sync_interval`.
  Run a single replica when limits must be exact to the cent. DB-backed reservations are on the roadmap.

## Cloud mode (hosted billing)

* Tenants bring their own provider keys. Cloud mode refuses to start if env provider keys are shared with all orgs, or if
  operator-level Stripe meter sync is on; either would let tenants spend, or be billed through, the operator's accounts.
* Stripe webhooks need a valid `Stripe-Signature` (HMAC-SHA256, 5-minute tolerance, constant-time compare). Events are
  de-duplicated by id and treated as notifications only: OpenGate re-fetches the subscription from the Stripe API before
  changing a plan. A Stripe customer maps to at most one org (unique index), and subscriptions of another customer,
  unknown prices and unknown orgs are ignored.
* Only org owners (or the operator token) can start Checkout or open the Customer Portal. Card data never touches
  OpenGate. Price ids and Stripe secrets are never sent to the browser.
* Sign-up is throttled per client IP. Email addresses are **not verified yet** (no email provider), so treat an
  org's email as unverified until that ships.

## Customer attribution and cache

* `X-OpenGate-Customer` and the `user` field are client-asserted. They're ignored unless the project sets
  `trust_customer_header`, which you should only do when the keys are held by your own backend. Alternatively, bind a key
  to one customer (`customer_id`): it's then always attributed to that customer, and conflicting headers get a 403.
* Cache entries are keyed by project, endpoint, key, customer and the exact body. Caching must be enabled by the project
  (clients can only shorten the TTL or bypass it). Cache hits still count against rate limits and require budget.

## Network and HTTP hardening

* Listens on `127.0.0.1` by default. Provider, Stripe and webhook URLs must be `https://` (loopback excepted) unless
  `allow_insecure_upstream`.
* Size limits: 10 MiB requests, 1 MiB admin JSON, 64 MiB upstream responses, 16 MiB SSE lines. Header, read and idle
  timeouts are set. `max_inflight` optionally caps concurrency.
* Every response: strict CSP (`default-src 'self'`, `script-src 'self'`, no inline code, `frame-ancestors 'none'`),
  `X-Frame-Options: DENY`, `nosniff`, `Referrer-Policy: no-referrer`, HSTS over TLS. Public pages (`/signup`, legal)
  load `/public.css` only. Dashboard DOM is built with `textContent`.
* `/metrics` needs its own `OPENGATE_METRICS_TOKEN`. Internal errors return generic messages with a request ID, and the
  detail goes to the server log.

## Guardrails are best-effort

PII and secret detection uses RE2 regexes (no ReDoS) plus validators (Luhn, IBAN mod-97, SSN and SIN rules).
Prompt-injection detection is a heuristic score. Treat both as defense in depth, not a guarantee. Streaming responses are
scanned after delivery (log only), and streaming `/v1/responses` isn't scanned.

## Known gaps

* No master-key rotation command yet (re-enter provider keys after rotating).
* Per-process reservations on multi-replica deployments (above).
* No SSO/SAML or 2FA for dashboard accounts yet.
