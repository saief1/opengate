# Migrating from LiteLLM Proxy

Both are OpenAI-compatible proxies, so client code only changes its base URL and key. This guide maps LiteLLM concepts to
OpenGate. LiteLLM terms follow its docs as of October 2026 (`config.yaml`, `/key/generate`).

## Why teams switch

* **Supply chain.** OpenGate is one static Go binary with four direct dependencies. There is no Python environment and
  no PyPI package on the path of every key and prompt. (LiteLLM's PyPI package was compromised in March 2026.)
* **Hard limits before the call.** OpenGate reserves the worst-case cost of a request atomically before forwarding it,
  so concurrent agents can't overshoot a budget. Spend is checked per key, per **agent** label and per project.
* **Kill switch.** One call blocks a key or project and aborts its in-flight streams, on every replica.

## Concept map

| LiteLLM | OpenGate |
|---|---|
| `model_list[].model_name` | an alias in `routes:` (or call `provider/model` directly) |
| `litellm_params.model: openai/gpt-4.1` | `openai/gpt-4.1` (same prefix style) |
| `litellm_params.api_key: os.environ/OPENAI_API_KEY` | `providers.openai.api_key_env`, or store it encrypted per org in the dashboard |
| `litellm_params.api_base` | `providers.<name>.base_url` (`type: openai` for any compatible server) |
| `router_settings` / `fallbacks` | ordered list per alias, e.g. `smart: [anthropic/…, openai/…]`, plus `retry:` |
| `general_settings.master_key` | `OPENGATE_ADMIN_TOKEN` (env only) |
| `database_url` | `database:` / `OPENGATE_DATABASE` (SQLite by default, Postgres optional) |
| `POST /key/generate` | `POST /admin/api/keys` |
| `max_budget` + `budget_duration` | `limits.daily_usd` / `monthly_usd` / `total_usd` on key, agent or project |
| `rpm_limit` | `limits.rpm` |
| `models: [...]` on a key | `allowed_models` / `denied_models` (globs) on key or project |
| teams | projects (budgets, policies, guardrails) inside organizations (people, roles) |
| `metadata.user` / `user` | `agent` and `user` labels on the key; end customers via `X-OpenGate-Customer` |
| Slack alerting | `alerts.webhook_url` (any Slack-compatible webhook) |
| `litellm_settings.cache` | per-project exact-match response cache |
| custom pricing (`input_cost_per_token`) | `pricing_file` YAML (per-1M-token prices, cache read/write, long-context tiers) |

## Translate a config

LiteLLM:

```yaml
model_list:
  - model_name: smart
    litellm_params: {model: anthropic/claude-sonnet-4-5, api_key: os.environ/ANTHROPIC_API_KEY}
  - model_name: smart
    litellm_params: {model: openai/gpt-4.1, api_key: os.environ/OPENAI_API_KEY}
  - model_name: local
    litellm_params: {model: openai/llama3, api_base: http://localhost:11434/v1}
general_settings:
  master_key: os.environ/LITELLM_MASTER_KEY
  database_url: os.environ/DATABASE_URL
```

OpenGate:

```yaml
database: ${DATABASE_URL}                 # or omit for SQLite
providers:
  ollama: {type: openai, base_url: http://localhost:11434/v1}
routes:
  smart: [anthropic/claude-sonnet-4-5, openai/gpt-4.1]   # ordered failover
  local: [ollama/llama3]
```

`OPENAI_API_KEY` and `ANTHROPIC_API_KEY` are picked up from the environment automatically.

## Recreate keys and budgets

```bash
# LiteLLM: curl -X POST $LITELLM/key/generate -d '{"models":["smart"],"max_budget":10,"budget_duration":"30d","rpm_limit":60}'
curl -s localhost:8080/admin/api/keys -H "Authorization: Bearer $OPENGATE_ADMIN_TOKEN" \
  -d '{"name":"research-bot","agent":"planner","limits":{"monthly_usd":10,"rpm":60}}'
# then restrict models:
curl -s -X PATCH localhost:8080/admin/api/keys/key_… -H "Authorization: Bearer $OPENGATE_ADMIN_TOKEN" \
  -d '{"allowed_models":["smart"]}'
```

Keys can't be imported. LiteLLM stores hashes, and OpenGate keys have their own `og-` format. Issue new keys, then roll
them out per service. You can run both proxies side by side during the switch.

## Differences to plan for

* Budget periods are calendar-based in UTC (`daily` resets at 00:00, `monthly` on the 1st). There is no rolling `budget_duration`.
* Providers are OpenAI, Anthropic, Gemini, OpenRouter and any OpenAI-compatible endpoint (vLLM, Ollama, Together…).
  LiteLLM supports far more native providers. Use OpenRouter or a compatible endpoint for the rest.
* Endpoints: chat completions, embeddings, Responses (OpenAI) and models. There's no image or audio passthrough yet.
* Callbacks to Langfuse and similar don't exist yet. Use the request log, `/metrics` (Prometheus) and JSON logs.
