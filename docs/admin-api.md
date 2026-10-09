# Admin API

The full contract is in [`openapi.yaml`](openapi.yaml), an OpenAPI 3.0 spec that a test checks against the routes the server
registers. Browse it with any OpenAPI viewer, for example `npx @redocly/cli preview-docs docs/openapi.yaml`, or generate clients
with `openapi-generator` / `openapi-typescript`.

## Authentication

| Caller | How | Org |
|---|---|---|
| Operator / automation | `Authorization: Bearer $OPENGATE_ADMIN_TOKEN` | default org, or `X-OpenGate-Org: org_…` |
| Dashboard user | `og_session` cookie from `POST /admin/api/auth/login` (`HttpOnly`, `SameSite=Strict`) | the session's active org (`POST /admin/api/auth/switch-org`) |

* Cookie-authenticated writes must send `X-Requested-With: opengate` (CSRF guard).
* Roles: `viewer` (read) < `member` (create keys, pull kill switches) < `admin` (everything in the org) < `owner` (rename org, manage owners, reload pricing). The spec's `x-min-role` gives the minimum role for each operation.
* **Everything is org-scoped.** IDs that belong to another org answer `404`, exactly like IDs that don't exist.
* Errors use the OpenAI error shape: `{"error":{"message","type","code","param"}}`.
* Repeated bad credentials get `429` with `Retry-After`. The correct credential still works.

## Go

A complete, dependency-free example is in [`examples/admin-client-go`](../examples/admin-client-go/main.go):

```go
c := &Client{BaseURL: "http://localhost:8080", Token: os.Getenv("OPENGATE_ADMIN_TOKEN"), HTTP: http.DefaultClient}

var created struct {
	Key        string     `json:"key"` // plaintext og-… key, shown once
	VirtualKey VirtualKey `json:"virtual_key"`
}
_ = c.do("POST", "/keys", map[string]any{"name": "nightly-summarizer", "agent": "summarizer",
	"limits": map[string]any{"daily_usd": 5, "rpm": 60}}, &created)

_ = c.do("POST", "/keys/"+created.VirtualKey.ID+"/kill", nil, nil) // blocks + aborts in-flight streams
```

```bash
OPENGATE_ADMIN_TOKEN=... go run ./examples/admin-client-go
```

## TypeScript

[`examples/admin-client-ts/client.ts`](../examples/admin-client-ts/client.ts) is a typed `fetch` client (Node 18+, Deno, Bun, browsers):

```ts
import { OpenGateAdmin } from "./client";

const admin = new OpenGateAdmin("http://localhost:8080", process.env.OPENGATE_ADMIN_TOKEN!);
const { key, virtual_key } = await admin.createKey({ name: "support-bot", agent: "support", limits: { daily_usd: 20 } });
await admin.bindCustomer(virtual_key.id, "cus-acme");   // all traffic billed to this customer
await admin.putLimit({ scope: "project", scope_id: virtual_key.project_id, monthly_usd: 500 });
const errors = await admin.logs({ errors: true, limit: 50 });
await admin.kill(virtual_key.id);
```

## curl

```bash
H="Authorization: Bearer $OPENGATE_ADMIN_TOKEN"
curl -s localhost:8080/admin/api/overview -H "$H" | jq .today
curl -s -X PATCH localhost:8080/admin/api/projects/prj_… -H "$H" \
  -d '{"allowed_features":["n"],"trust_customer_header":true,"cache_ttl_seconds":300}'
curl -s "localhost:8080/admin/api/billing/export.csv?month=2026-10" -H "$H" -o usage.csv
```
