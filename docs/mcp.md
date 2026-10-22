# MCP tool gateway

OpenGate proxies [Model Context Protocol](https://modelcontextprotocol.io)
traffic, so agents reach their tools through the same gateway, key, kill
switch and budget as their model calls.

```
agent ──Bearer og-…──▶ OpenGate /mcp/{server} ──upstream secret──▶ MCP server
```

## Set up

1. **Register a server** (dashboard: *MCP tools → + Add server*, or the API):
   ```bash
   curl -X POST $OG/admin/api/mcp/servers -H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' \
     -d '{"name":"github","url":"https://mcp.example.com/mcp","transport":"http","secret":"Bearer ghp_…","default_rpm":60}'
   ```
   The secret is encrypted with the master key, injected into upstream
   requests in `auth_header` (default `Authorization`) and never returned.
2. **Point the agent at OpenGate**: `url: https://gateway.example.com/mcp/github`,
   header `Authorization: Bearer og-…` (the agent's virtual key).
3. **Add tool rules** (*Tool policies* tab or `POST /admin/api/mcp/policies`).

## Policies

A rule is `{server_id, agent, tool, action, rpm}`:

- `agent` empty = every agent; otherwise the key's agent name (or key ID for keys without one).
- `tool` exact name or prefix wildcard (`delete_*`, `*`).
- The most specific rule wins: agent-specific > all agents, exact > prefix > `*`; at a tie `deny` wins.
- No matching rule → allowed, unless `mcp.default_deny: true`.
- `rpm` (or the server's `default_rpm`) limits calls per agent and tool per minute (HTTP 429, JSON-RPC error `-32002`).
- Denied tools are removed from `tools/list` results and calls get HTTP 403 with JSON-RPC error `-32001`.
- Dry run: `POST /admin/api/mcp/check {"server_id","agent","tool"}`.

## Per-agent credentials

`PUT /admin/api/mcp/servers/{id}/credentials {"agent":"planner","header":"Authorization","secret":"Bearer …"}`
overrides the server secret for one agent (e.g. a least-privilege token per agent).

## Enforcement

- **Kill switch**: killed keys/projects get 403 (`-32003`); open SSE streams are aborted.
- **Budgets and request rate limits** of the key/agent/project apply to every `tools/call`
  (an agent over budget cannot keep acting through tools).
- **Logging**: every `tools/call` is stored in `mcp_calls` with agent, server, tool,
  status, error code, latency and arguments with PII/secrets replaced by
  `[REDACTED_*]` placeholders (the same detectors as the guardrails, plus
  fields named like password/secret/token), truncated to `mcp.args_log_max_bytes`.
- Every MCP config change is in the audit log (secrets appear only as hints).

## Transports

- **Streamable HTTP** (`transport: http`): POST/GET/DELETE `/mcp/{server}` are forwarded
  with `Mcp-Session-Id` / `Mcp-Protocol-Version`; JSON and SSE responses are streamed.
- **HTTP+SSE (legacy)** (`transport: sse`): `GET /mcp/{server}` opens the event stream; the
  `endpoint` event is rewritten to `/mcp/{server}/…` so follow-up POSTs go through OpenGate.
  For this transport tool results arrive on the stream, so the log records the POST
  status (202) rather than the tool's result.

## Config (`mcp:`)

| Key | Default | |
|---|---|---|
| `disabled` | false | turn the `/mcp` routes off |
| `timeout` | 60s | per upstream POST |
| `stream_timeout` | 10m | GET event streams |
| `args_log_max_bytes` | 4096 | logged argument size |
| `default_deny` | false | tools without an allow rule are blocked |

Upstream URLs must be `https://` (plain `http://` only for loopback, unless
`allow_insecure_upstream`).
