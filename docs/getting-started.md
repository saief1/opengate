# Getting started

OpenGate is one static binary (`opengate`) that serves an OpenAI-compatible gateway on `/v1/*`, an admin API on
`/admin/api/*` and a dashboard on `/ui/`. It needs no external services: SQLite is built in, and Postgres is optional.

## 1. Install

```bash
# from a release (GoReleaser builds linux/darwin/windows × amd64/arm64)
tar xzf opengate_1.0.0_linux_amd64.tar.gz && sudo mv opengate /usr/local/bin/

# or from source (Go 1.27+, no CGO)
git clone https://github.com/opengate-dev/opengate && cd opengate && make build   # -> bin/opengate

# or the container (distroless, non-root, read-only rootfs)
docker compose up -d
```

## 2. Try it without any API keys

```bash
make demo
# Dashboard http://127.0.0.1:8080/ui/
#   email demo@example.com / demo-only-password, or admin token demo-admin-token-change-me
```

`make demo` starts the bundled mock provider (it imitates OpenAI, Anthropic and Gemini, including streaming,
failures and rate limits), seeds 30 days of history and sends live traffic: routing rules, guardrails, customers,
a runaway agent hitting its budget, and a killed key.

## 3. Run it for real

```bash
eval "$(opengate keygen | sed 's/^/export /')"   # OPENGATE_MASTER_KEY + OPENGATE_ADMIN_TOKEN, put them in your secret store
export OPENAI_API_KEY=sk-... ANTHROPIC_API_KEY=sk-ant-...   # or store them encrypted from the dashboard (Providers)
opengate serve -config examples/opengate.example.yaml
```

Secrets are only read from the environment, never from YAML.

## 4. Create a virtual key with a budget

```bash
curl -s localhost:8080/admin/api/keys -H "Authorization: Bearer $OPENGATE_ADMIN_TOKEN" \
  -d '{"name":"planner-prod","agent":"planner","limits":{"daily_usd":5,"monthly_usd":60,"rpm":60}}'
# {"key":"og-…","virtual_key":{…}}   <- shown once; only its hash is stored
```

## 5. Point your SDK at OpenGate

```python
from openai import OpenAI
client = OpenAI(base_url="http://localhost:8080/v1", api_key="og-…")
client.chat.completions.create(model="anthropic/claude-sonnet-4-5", messages=[{"role": "user", "content": "hi"}])
```

Models are `provider/model` (`openai/gpt-4.1`, `anthropic/claude-sonnet-4-5`, `gemini/gemini-2.5-flash`,
`openrouter/meta-llama/llama-3.3-70b-instruct`), an alias from `routes:` (`smart`, with ordered failover), or `auto`
when smart routing is enabled. `/v1/chat/completions` (streaming and non-streaming), `/v1/embeddings`, `/v1/responses`
(OpenAI) and `/v1/models` are supported.

## 6. Dashboard accounts

```bash
OPENGATE_NEW_PASSWORD='a long passphrase' opengate create-user -config opengate.yaml -email you@company.com -name You -role owner
```

Sign in at `/ui/`, then invite teammates from **Team**. Roles: viewer < member < admin < owner. Each organization has
its own projects, keys, provider keys, rules, customers and audit log.

## 7. Production checklist

* Put TLS in front (Caddy, nginx, a load balancer). Set `trust_proxy_headers: true` and `trusted_proxies: [<proxy CIDR>]`.
* Use Postgres for backups and multiple replicas: `OPENGATE_DATABASE=postgres://…?sslmode=verify-full`. See
  [security model › replicas](security-model.md#budgets-and-replicas) for how hard limits behave across replicas.
* Give Prometheus its own `OPENGATE_METRICS_TOKEN` (`GET /metrics`). Probes go to `GET /healthz` and `GET /readyz`.
* Set `budgets.strict: true` and a limit on every key.
