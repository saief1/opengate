# Deploying OpenGate

OpenGate is one static Go binary (dashboard embedded) plus a database:
SQLite on a persistent volume for a single instance, or PostgreSQL for
several replicas. Nothing here deploys anything for you, buys anything or
touches DNS: these are the steps you run.

## What you need to decide / provide

| Item | Notes |
|---|---|
| Hosting | A VPS, Fly.io or Render (below). **Not Vercel** (see why). |
| Domain + TLS | e.g. `gateway.example.com`; TLS via Caddy (VPS), or the platform's certificate. |
| Secrets | `OPENGATE_MASTER_KEY`, `OPENGATE_ADMIN_TOKEN` (`opengate keygen`). Keep them in the platform's secret store, never in git. |
| Database | SQLite volume (simplest) or managed PostgreSQL (replicas, managed backups). |
| Stripe | Test-mode key (`sk_test_…`) only. Live keys are refused unless `OPENGATE_STRIPE_ALLOW_LIVE=true`. |
| Email | `RESEND_API_KEY` (preferred) or `SMTP_HOST`, plus `MAIL_FROM`. Unset logs the message and skips verification. See [docs/LAUNCH.md](docs/LAUNCH.md). |

All configuration is documented in [`.env.example`](.env.example) and
[docs/configuration.md](docs/configuration.md).

## Production checklist

- [ ] `opengate keygen` → store both values as secrets; back up the master key separately from the database.
- [ ] HTTPS in front (Caddy / platform TLS). Set `trust_proxy_headers: true` and `trusted_proxies` to the proxy CIDRs so client IPs (lockout, audit log) are correct, and `auth.secure_cookies: true`.
- [ ] `OPENGATE_LOG_FORMAT=json` (structured logs to stdout/stderr).
- [ ] Health checks: liveness `GET /livez` (or `/healthz`), readiness `GET /readyz` (checks the database). The image's `HEALTHCHECK` runs `opengate healthcheck`.
- [ ] `/metrics` protected with `OPENGATE_METRICS_TOKEN` (or disabled).
- [ ] CORS: leave `OPENGATE_CORS_ORIGINS` empty unless browsers call `/v1` or `/mcp` directly.
- [ ] Database migrations run automatically at start (versioned, transactional, recorded in `schema_migrations`). Take a backup before upgrading.
- [ ] Admin auth is rate limited (`admin_lockout`, default 10 failures / 5 min → 15 min lockout) and every admin change lands in the append-only audit log.
- [ ] Graceful shutdown: SIGTERM stops accepting, waits up to 30 s for in-flight requests (give the platform a ≥ 30 s kill timeout).

## Option A: a VPS (Hetzner, DigitalOcean, Lightsail…) with Docker Compose

```bash
git clone https://github.com/saief1/opengate && cd opengate
cp .env.example .env && docker run --rm $(docker build -q .) keygen >> .env   # then edit .env
docker compose up -d                                  # SQLite on the opengate-data volume
# or PostgreSQL:
echo "POSTGRES_PASSWORD=$(openssl rand -hex 16)" >> .env
docker compose -f docker-compose.yml -f docker-compose.postgres.yml up -d
```

The compose file binds `127.0.0.1:8080` only. Put Caddy in front for TLS
(`deploy/Caddyfile`, automatic Let's Encrypt once your DNS A record points
at the server):

```
gateway.example.com {
  reverse_proxy 127.0.0.1:8080 { flush_interval -1 }   # keep SSE streaming
}
```

Firewall: allow 22/80/443 only. Updates: `git pull && docker compose up -d --build`.

## Option B: Fly.io

`deploy/fly.toml` is ready (one machine, SQLite volume, health checks).

```bash
fly launch --no-deploy --copy-config --config deploy/fly.toml
fly volumes create opengate_data --size 1
fly secrets set OPENGATE_MASTER_KEY=... OPENGATE_ADMIN_TOKEN=...
fly deploy --config deploy/fly.toml
```

The public test site uses `deploy/fly-demo/` instead (mock providers, seeded by
`scripts/fly-demo-seed.sh`). It sets `OPENGATE_DEMO_MOCK_LISTEN` (demo mode: `/public/live`,
dashboard *Try it*), `OPENGATE_DEMO_TRAFFIC=1` (in-process traffic generator, one
request every 3-7s while the machine is running; demo rows kept 30 days, capped at 60k per table), and
`OPENGATE_PUBLIC_PLAYGROUND=1` (mock-only). Never set these on a real deployment.
`OPENGATE_ENV=production` refuses them.

Production is a separate config, `deploy/fly-prod/fly.toml` (app `opengate-prod`,
`min_machines_running = 1`, health checks, no demo flags, no demo volume).
Backups, email, Stripe live keys, and the uptime probe are in
[docs/LAUNCH.md](docs/LAUNCH.md). That file does not deploy or change DNS.

Keep one machine with SQLite (volumes are per machine). For more than one,
attach Fly Postgres / a managed Postgres and set `OPENGATE_DATABASE`.

## Option C: Render

`deploy/render.yaml` is a Blueprint (Docker web service + persistent disk).
Create it from the dashboard ("New → Blueprint"), then set the two secrets
as environment variables. Persistent disks need a paid instance type; or use
Render PostgreSQL and drop the disk.

## Why not Vercel

Vercel runs request-scoped serverless functions. OpenGate needs a
long-running process: in-memory kill-switch fan-out and in-flight stream
cancellation, background jobs (Stripe usage sync, pruning, kill sweeps),
long-lived SSE streams for model output and MCP, and a local SQLite file or a
pooled Postgres connection. Function timeouts, no persistent disk and cold
starts break these. Host the gateway on a VPS / Fly / Render; a marketing site
(the `site/` folder) can still live on Vercel.

## Kubernetes

`deploy/k8s/opengate.yaml` (Deployment + Service + PVC) with
`deploy/k8s/secret.example.env`. Use PostgreSQL for `replicas > 1`.

## Backups

- **SQLite**: the database is `/data/opengate.db` (WAL mode). Use
  `sqlite3 /data/opengate.db ".backup '/backup/opengate-$(date +%F).db'"`
  (safe while running), or snapshot the volume. Copy backups off the host
  (restic / rclone to object storage) and test a restore.
- **PostgreSQL**: managed point-in-time recovery, or
  `pg_dump -Fc "$OPENGATE_DATABASE" > opengate-$(date +%F).dump` nightly.
- **Master key**: stored separately from database backups (password manager /
  secret manager). A backup without the key cannot decrypt provider keys or
  MCP secrets; a key without the backup is useless to an attacker.
- The audit log is append-only by database trigger; restores bring it back
  as it was at backup time.

## Upgrading

Migrations are forward-only and applied at start. Back up, deploy the new
image, watch `/readyz` and the logs (`migration N` errors abort start-up
without partial schema changes).
