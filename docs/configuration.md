# Configuration reference

OpenGate reads one YAML file (`opengate serve -config path.yaml`). Every field is optional. `${VAR}` is expanded from the
environment. The fully commented reference is [`examples/opengate.example.yaml`](../examples/opengate.example.yaml).
Send `SIGHUP` to reload the price table.

## Environment variables

| Variable | Purpose |
|---|---|
| `OPENGATE_ADMIN_TOKEN` | **Required.** Admin API bearer token (≥ 16 chars; `opengate keygen`). |
| `OPENGATE_MASTER_KEY` | **Required.** 32-byte base64 key that encrypts provider keys (AES-256-GCM). |
| `OPENGATE_LISTEN` | Listen address (default `127.0.0.1:8080`, or `0.0.0.0:8080` in the image). |
| `OPENGATE_DATABASE` | SQLite path or `postgres://…` DSN. |
| `OPENGATE_PRICING_FILE` | Custom price table (YAML). Empty means the built-in, source-cited table (`opengate pricing`). |
| `OPENGATE_METRICS_TOKEN` | Bearer token for `/metrics` (the admin token is not accepted). |
| `OPENGATE_ALERT_WEBHOOK_URL` | Slack-compatible webhook for budget and kill-switch alerts. |
| `OPENGATE_LOG_LEVEL` / `OPENGATE_LOG_FORMAT` | `debug\|info\|warn\|error` / `json\|text`. |
| `OPENGATE_RATE_LIMIT_BACKEND` | `database` (persisted, shared) or `memory`. |
| `OPENGATE_PROVIDER_<NAME>_BASE_URL` | Override a provider's base URL. |
| `OPENGATE_ALLOW_INSECURE_UPSTREAM` | `true` = same as `allow_insecure_upstream` (e.g. the compose mock). |
| `OPENAI_API_KEY`, `ANTHROPIC_API_KEY`, `GEMINI_API_KEY`, `OPENROUTER_API_KEY` | Provider keys for the default org (or store them in the dashboard). |
| `STRIPE_API_KEY` | With `billing.stripe.enabled` (your customers' metering) **or** `cloud.enabled` (subscriptions); never both. |
| `STRIPE_WEBHOOK_SECRET` | Cloud mode: `whsec_...` signing secret of the webhook endpoint. |
| `OPENGATE_PUBLIC_URL` | Cloud mode: external `https://` base URL (Stripe redirects back here). |
| `OPENGATE_PLAN` | Empty (default, everything unlocked) / `team` / `enterprise`, or `free` to gate Team features. |
| `OPENGATE_CORS_ORIGINS` | Comma-separated origins allowed to call `/v1` and `/mcp` from browsers (`*` = any). Empty = no CORS. |
| `OPENGATE_STRIPE_ALLOW_LIVE` | `true` to accept live-mode Stripe keys (refused by default). |
| `OPENGATE_NEW_PASSWORD` | Read by `opengate create-user` instead of stdin. |

## Top level

| Key | Default | Notes |
|---|---|---|
| `listen` | `127.0.0.1:8080` | |
| `database` | `opengate.db` | SQLite (WAL) or Postgres. Migrations run on start. |
| `pricing_file` | built-in | See `opengate pricing` for the format (input, output, cache read/write, long-context tiers). |
| `max_request_bytes` | 10 MiB | |
| `body_log_max_bytes` | 64 KiB | Applies only to projects with body logging. |
| `default_max_tokens` | 4096 | Used when a client omits `max_tokens` (reservations, Anthropic). |
| `disable_dashboard` | false | |
| `trust_proxy_headers` | false | Use `X-Forwarded-For` / `-Proto`. |
| `trusted_proxies` | `[]` | CIDRs; XFF is honored only from these peers, and the client is the rightmost untrusted hop. |
| `allow_insecure_upstream` | false | Allow `http://` provider, Stripe and webhook URLs to non-loopback hosts. |
| `max_inflight` | 0 | Instance-wide concurrent gateway request cap (503 when full). |

## `providers` and `routes`

```yaml
providers:
  openai:     {base_url: https://api.openai.com/v1, api_key_env: OPENAI_API_KEY, timeout: 120s}
  local-llm:  {type: openai, base_url: http://localhost:11434/v1}   # any OpenAI-compatible server
routes:
  smart: [anthropic/claude-sonnet-4-5, openai/gpt-4.1]               # ordered failover
retry: {max_attempts: 2, initial_backoff: 250ms, max_backoff: 4s}
```

Types are `openai`, `anthropic`, `gemini` and `openrouter`. `timeout` limits time-to-first-byte and the gap between stream chunks.

## `budgets`, `rate_limit`, `alerts`

| Key | Default | Notes |
|---|---|---|
| `budgets.strict` | false | Refuse requests whose worst-case cost exceeds the remaining budget. |
| `budgets.sync_interval` | 0 (SQLite) / 5s (Postgres) | How often committed spend is re-read from the DB. |
| `rate_limit.backend` | `database` | `memory` is per process. |
| `alerts.webhook_url` | — | `alerts.thresholds: [0.8, 1.0]`. |

Limits themselves (`daily_usd`, `monthly_usd`, `total_usd`, `rpm` per key, agent or project) are managed through the API and dashboard.

## `metrics`, `log`, `admin_lockout`

```yaml
metrics: {disabled: false, public: false}
log: {level: info, format: json}
admin_lockout: {max_failures: 10, window: 5m, lockout: 15m}
```

## `auth`

| Key | Default | |
|---|---|---|
| `session_ttl` | 12h | idle timeout |
| `session_max_age` | 168h | absolute |
| `invite_ttl` | 168h | |
| `allow_signup` | false | self-service account + org creation (hosted mode) |
| `secure_cookies` | false | forced on automatically over TLS or a trusted `X-Forwarded-Proto: https` |

## `smart_routing`

```yaml
smart_routing:
  auto:
    enabled: true
    alias: auto
    baseline: openai/gpt-4.1          # savings are reported against this
    models:
      - {model: openai/gpt-4.1-nano, tier: 1}
      - {model: gemini/gemini-2.5-flash, tier: 2}
      - {model: anthropic/claude-sonnet-4-5, tier: 3}
```

Routing rules (match on model patterns, prompt-token range, `X-OpenGate-Tags`, headers, agent or cost ceiling; target plus
fallbacks) are stored in the DB and managed in the dashboard or through `/admin/api/routing/rules`.

## `guardrails` (server defaults; projects override)

```yaml
guardrails: {pii: off, categories: [], responses: false, injection: off}   # pii: off|log|redact|block, injection: off|log|block
```

## `billing`

```yaml
billing:
  customer_header: X-OpenGate-Customer   # honored only for projects with trust_customer_header, or keys bound to a customer
  ignore_user_field: false
  stripe: {enabled: false, event_name: opengate_usage, unit: cents, interval: 1h, lag: 15m, base_url: https://api.stripe.com,
           window: 1h,          # fixed, epoch-aligned reporting window (>= 1s, divides 24h); one meter event per customer per window
           api_version: ""}     # default: the pinned Stripe-Version (2026-09-30.endive)
```

## `cloud` (hosted, multi-tenant mode; see [cloud.md](cloud.md))

```yaml
cloud:
  enabled: false
  public_url: https://cloud.example.com   # or OPENGATE_PUBLIC_URL; https required (loopback http allowed)
  default_plan: free                      # must have no stripe_price_id
  max_orgs_per_user: 3
  grace_period: 168h                      # past_due keeps the paid plan this long
  sales_url: "mailto:sales@example.com"   # linked from contact_sales plans
  stripe:
    base_url: https://api.stripe.com
    api_version: ""                       # default: pinned 2026-09-30.endive (sent as Stripe-Version on every call)
    portal_configuration: ""              # bpc_... Customer Portal configuration (default: the account default)
    trial_days: 14                        # first subscription per org only; 0 = no trial
    trial_without_card: true              # payment_method_collection=if_required; trial ends -> canceled if no card
    allow_promotion_codes: false
    automatic_tax: false                  # only after a Stripe Tax registration exists
  plans:                                  # default: Free / Team / Enterprise (draft)
    - {id: team, name: Team, price_usd: 49, interval: month, stripe_price_id: "${STRIPE_PRICE_TEAM}",
       max_requests_month: 1000000, max_members: 25, max_projects: 0, log_retention_days: 30, features: ["..."]}
```

Limits of `0` mean unlimited. Enabling cloud mode turns on `auth.allow_signup`, and also `auth.secure_cookies` for an https
`public_url`. It **refuses to start** with `share_env_provider_keys` or `billing.stripe.enabled`, because tenants
must never spend the operator's provider keys or be metered into the operator's Stripe account. It needs
`STRIPE_API_KEY` and `STRIPE_WEBHOOK_SECRET`.

## `mcp`

MCP tool gateway; see [mcp.md](mcp.md). Keys: `disabled`, `timeout` (60s), `stream_timeout` (10m),
`args_log_max_bytes` (4096), `default_deny` (false).

## `plan`, `cors_origins`

`plan: free` gates the Team features (audit log viewer/export, member and read-only roles, markup and Stripe sync
writes) with HTTP 402. Empty (the default) leaves a self-hosted install fully unlocked. In cloud mode the org's
subscription decides (any non-free plan is unlocked). `cors_origins` lists browser origins for `/v1` and `/mcp`.

## `cache`

```yaml
cache: {disabled: false, max_ttl: 24h, max_entry_bytes: 1048576, share_across_customers: false, share_across_keys: false}
```

Caching is enabled per project (`cache_ttl_seconds`). Clients can send `X-OpenGate-Cache: off` or a shorter `X-OpenGate-Cache-TTL`.

## Per-project settings (API and dashboard)

| Field | Meaning |
|---|---|
| `log_bodies` | Store truncated request and response bodies (after redaction). |
| `allowed_models` / `denied_models` / `max_tokens` | Model policy (glob patterns). Also settable per key. |
| `cache_ttl_seconds` | Response cache opt-in. |
| `allowed_features` | Opt into passthrough features whose cost can't be budgeted: `background`, `n`, `builtin_tools`, `store`. |
| `trust_customer_header` | Honor client-asserted customer IDs. |
| guardrails | `pii`, `categories`, `responses`, `injection`. |

## Request headers

| Header | Effect |
|---|---|
| `X-OpenGate-Tags: batch,nightly` | Input for routing rules. |
| `X-OpenGate-Customer: cus-123` | End-customer attribution (see `trust_customer_header`). |
| `X-OpenGate-Cache: off` / `X-OpenGate-Cache-TTL: 60` | Bypass the cache, or shorten its TTL. |

Response headers: `X-OpenGate-Request-Id`, `X-OpenGate-Route`, `X-OpenGate-Cache: HIT|MISS`, `Retry-After` on 429.
