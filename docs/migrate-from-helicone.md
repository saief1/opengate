# Migrating from Helicone

Helicone is primarily observability: a hosted proxy (`oai.helicone.ai`, legacy) or its AI Gateway (`ai-gateway.helicone.ai`)
that logs requests. OpenGate is a self-hosted gateway focused on **control** (budgets, kill switch, routing and
guardrails), and it logs every request as well. Request details follow Helicone's docs as of October 2026.

## Swap the client

Helicone (legacy proxy):

```python
client = OpenAI(api_key=os.environ["OPENAI_API_KEY"], base_url="https://oai.helicone.ai/v1",
                default_headers={"Helicone-Auth": f"Bearer {os.environ['HELICONE_API_KEY']}"})
```

Helicone AI Gateway:

```python
client = OpenAI(api_key=os.environ["HELICONE_API_KEY"], base_url="https://ai-gateway.helicone.ai")
```

OpenGate:

```python
client = OpenAI(api_key=os.environ["OPENGATE_KEY"],   # og-…, one per agent or service
                base_url="https://gateway.internal.example/v1")
```

Provider keys no longer live in your apps. Store them once (env or dashboard, AES-256-GCM encrypted), and every app
gets a revocable virtual key with its own budget instead.

## Header map

| Helicone | OpenGate |
|---|---|
| `Helicone-Auth: Bearer …` | not needed (the virtual key authenticates) |
| `Helicone-User-Id` | `X-OpenGate-Customer` (per-customer usage and billing; enable `trust_customer_header` on the project) or the OpenAI `user` field |
| `Helicone-Property-<Name>` | `X-OpenGate-Tags: a,b` (used by routing rules), or `agent` labels on keys |
| `Helicone-Cache-Enabled: true` | per-project `cache_ttl_seconds`. `X-OpenGate-Cache: off` bypasses the cache, and `X-OpenGate-Cache-TTL` shortens it |
| `Helicone-Retry-Enabled` | always on: `retry:` with backoff on 429/5xx, then the next fallback |
| `Helicone-Fallbacks` | ordered `routes:` aliases, or routing-rule fallbacks |
| `Helicone-RateLimit-Policy` | `limits.rpm` (and USD limits) per key, agent or project |
| `Helicone-Session-*` | not yet. Use agent labels plus the request log filters |
| `Helicone-Target-URL` | `providers.<name>.base_url` |

## Observability you keep

* The **request log** has model, provider, tokens (including cached), cost, latency, attempts, status, route, guard findings and
  customer. Bodies are stored only when a project opts in, and are redacted first.
* The **overview** shows spend over time, top models, keys, routes and customers, routing and cache savings, and budget burn.
* **Prometheus** `/metrics` covers requests, latency histograms, tokens, cost, rejections and in-flight requests. Logs are JSON.

Helicone's hosted features (evaluations, prompt management, experiments) have no equivalent. Keep Helicone for those if you
need them: OpenGate can sit in front of it by pointing a provider's `base_url` at Helicone.

## What you gain

* Hard per-agent spend limits enforced **before** the call, with a kill switch that aborts in-flight streams.
* PII and secret redaction or blocking, and prompt-injection flags, inside your own network.
* Smart routing (`auto`) to cheaper models, with the savings tracked.
* Customer billing with markups, CSV export and Stripe meter sync.
