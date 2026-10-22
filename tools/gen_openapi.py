#!/usr/bin/env python3
"""Generates docs/openapi.yaml for the OpenGate admin API.
Run: python3 tools/gen_openapi.py  (TestOpenAPICoversAllRoutes keeps it in sync)."""
import json, re, os

S = lambda name: {"$ref": f"#/components/schemas/{name}"}
obj = lambda props, req=None: {"type": "object", "properties": props, **({"required": req} if req else {})}
s, i, n, b = {"type": "string"}, {"type": "integer"}, {"type": "number"}, {"type": "boolean"}
arr = lambda x: {"type": "array", "items": x}
listOf = lambda x: obj({"data": arr(x)})

schemas = {
    "Error": obj({"error": obj({"message": s, "type": s, "code": s, "param": {"type": "string", "nullable": True}})}),
    "ModelPolicy": obj({"allowed_models": arr(s), "denied_models": arr(s), "max_tokens": i}),
    "Project": obj({"id": s, "org_id": s, "name": s, "log_bodies": b, "killed": b, "created_at": {"type": "string", "format": "date-time"},
                    "allowed_models": arr(s), "denied_models": arr(s), "max_tokens": i, "cache_ttl_seconds": i,
                    "allowed_features": arr({"type": "string", "enum": ["background", "n", "builtin_tools", "store"]}),
                    "trust_customer_header": b, "guardrails": S("Guardrails")}),
    "ProjectPatch": obj({"log_bodies": b, "cache_ttl_seconds": i, "allowed_models": arr(s), "denied_models": arr(s), "max_tokens": i,
                         "allowed_features": arr(s), "trust_customer_header": b}),
    "VirtualKey": obj({"id": s, "project_id": s, "name": s, "agent": s, "user": s, "prefix": s, "killed": b, "revoked": b,
                       "created_at": {"type": "string", "format": "date-time"}, "allowed_models": arr(s), "denied_models": arr(s),
                       "max_tokens": i, "customer_id": s}),
    "KeyCreate": obj({"project_id": s, "name": s, "agent": s, "user": s, "limits": S("LimitValues")}),
    "KeyCreated": obj({"key": {"type": "string", "description": "Plaintext og-... key, shown once"}, "virtual_key": S("VirtualKey")}),
    "KeyPatch": obj({"allowed_models": arr(s), "denied_models": arr(s), "max_tokens": i, "customer_id": s}),
    "LimitValues": obj({"daily_usd": n, "monthly_usd": n, "total_usd": n, "rpm": i}),
    "Limit": obj({"scope": {"type": "string", "enum": ["key", "agent", "project"]}, "scope_id": s, "daily_usd": n, "monthly_usd": n,
                  "total_usd": n, "rpm": i}, ["scope", "scope_id"]),
    "RequestLog": obj({"id": s, "ts": {"type": "string", "format": "date-time"}, "endpoint": s, "project_id": s, "key_id": s, "agent": s,
                       "model": s, "provider": s, "provider_model": s, "status": i, "prompt_tokens": i, "completion_tokens": i,
                       "cached_tokens": i, "cost_usd": n, "billed_usd": n, "latency_ms": i, "stream": b, "customer_id": s,
                       "guard": s, "guard_action": s, "cache_hit": b, "usage_estimated": b, "error": s,
                       "request_body": {"type": "string", "description": "Only when the project logs bodies"}, "response_body": s}),
    "ProviderKey": obj({"id": s, "provider": s, "label": s, "hint": s, "created_at": {"type": "string", "format": "date-time"}}),
    "RoutingRule": obj({"id": s, "name": s, "priority": i, "enabled": b, "project_id": s,
                        "match": obj({"models": arr(s), "min_prompt_tokens": i, "max_prompt_tokens": i, "tags": arr(s), "headers": {"type": "object", "additionalProperties": s},
                                      "agent": s, "max_cost_usd": n}),
                        "target": {"type": "string", "description": "provider/model, an alias, or auto"}, "fallbacks": arr(s)}),
    "Guardrails": obj({"pii": {"type": "string", "enum": ["off", "log", "redact", "block"]}, "categories": arr(s), "responses": b,
                       "injection": {"type": "string", "enum": ["off", "log", "block"]}}),
    "Customer": obj({"project_id": s, "customer_id": s, "name": s, "stripe_customer_id": s, "requests": i, "prompt_tokens": i,
                     "completion_tokens": i, "cost_usd": n, "billed_usd": n}),
    "Markup": obj({"id": s, "project_id": s, "customer_id": s, "model": s, "markup_pct": n, "per_mtok_usd": {"type": "number", "description": "Flat USD fee per million tokens, added to the percentage markup"}}),
    "Org": obj({"id": s, "name": s, "created_at": {"type": "string", "format": "date-time"}, "role": s}),
    "Member": obj({"id": s, "email": s, "name": s, "role": {"type": "string", "enum": ["owner", "admin", "member", "viewer"], "description": "viewer = read-only (\"read-only\" is accepted as an alias)"},
                   "joined_at": {"type": "string", "format": "date-time"}}),
    "Invite": obj({"id": s, "email": s, "role": s, "expires_at": {"type": "string", "format": "date-time"}}),
    "AuditEntry": obj({"id": s, "ts": {"type": "string", "format": "date-time"}, "actor": s, "action": s, "target": s, "ip": s,
                       "actor_type": s, "actor_email": s, "status": i, "detail": {"type": "object"},
                       "before": {"type": "object", "description": "State before the change"}, "after": {"type": "object", "description": "State after the change"}}),
    "MCPServer": obj({"id": s, "name": s, "url": s, "transport": {"type": "string", "enum": ["http", "sse"]}, "auth_header": s, "has_secret": b,
                      "secret_hint": s, "default_rpm": i, "enabled": b, "gateway_path": s}),
    "MCPServerInput": obj({"name": s, "url": s, "transport": s, "auth_header": s, "secret": {"type": "string", "description": "Write-only upstream secret (stored encrypted, never returned)"},
                           "default_rpm": i, "enabled": b, "clear_secret": b}),
    "MCPCredential": obj({"id": s, "server_id": s, "agent": s, "header": s, "secret_hint": s}),
    "MCPPolicy": obj({"id": s, "server_id": s, "agent": {"type": "string", "description": "empty = every agent"}, "tool": {"type": "string", "description": "exact name or prefix*"},
                      "action": {"type": "string", "enum": ["allow", "deny"]}, "rpm": i}, ["server_id", "tool", "action"]),
    "MCPCall": obj({"id": s, "ts": {"type": "string", "format": "date-time"}, "agent": s, "key_id": s, "server_name": s, "method": s, "tool": s,
                    "args": {"type": "string", "description": "Redacted, truncated JSON arguments"}, "redacted": b, "status": i, "error_code": s, "latency_ms": i}),
    "Me": obj({"kind": {"type": "string", "enum": ["token", "session"]}, "user": obj({"id": s, "email": s, "name": s}), "org_id": s,
               "role": s, "orgs": arr(S("Org"))}),
}

R = []  # (method, path, tag, summary, role, request schema, response schema/code)
def r(m, p, tag, summ, role=None, req=None, resp=None, code="200"):
    R.append((m, p, tag, summ, role, req, resp, code))

# Auth (public, then session-carrying)
r("GET", "/admin/api/auth/config", "Auth", "Login options (signup enabled, cloud mode, etc.)", "public", resp=obj({"allow_signup": b, "has_users": b, "cloud": b, "version": s}))
r("POST", "/admin/api/auth/login", "Auth", "Email + password login; sets the og_session cookie", "public", obj({"email": s, "password": s}, ["email", "password"]), S("Me"))
r("POST", "/admin/api/auth/logout", "Auth", "Ends the session", "public")
r("GET", "/admin/api/auth/me", "Auth", "Current principal, role and orgs", "public", resp=S("Me"))
r("POST", "/admin/api/auth/switch-org", "Auth", "Switch the session's active org", "public", obj({"org_id": s}, ["org_id"]), S("Me"))
r("POST", "/admin/api/auth/invites/preview", "Auth", "Preview an invite (token in the body, never in the URL)", "public", obj({"token": s}, ["token"]), obj({"email": s, "org": s, "role": s, "existing_user": b}))
r("POST", "/admin/api/auth/invites/accept", "Auth", "Accept an invite (single use)", "public", obj({"token": s, "name": s, "password": s}, ["token", "password"]), S("Me"))
r("POST", "/admin/api/auth/signup", "Auth", "Self-service signup (only with auth.allow_signup)", "public", obj({"email": s, "password": s, "name": s, "org_name": s}), S("Me"), "201")
r("POST", "/admin/api/auth/password", "Auth", "Change own password", "public", obj({"current_password": s, "new_password": s}))
r("GET", "/admin/api/whoami", "Auth", "Who is calling", resp=S("Me"))
# Monitoring
r("GET", "/admin/api/overview", "Monitor", "Spend/requests summary, charts, top models/keys/routes/customers")
r("GET", "/admin/api/series", "Monitor", "Rolling-window summary, p50/p95 latency and spend series grouped by provider, agent, key, customer, model or block reason")
r("GET", "/admin/api/alerts", "Budgets", "Budget alert webhook status (host only) and recent threshold alerts")
r("GET", "/admin/api/logs", "Monitor", "Request logs (filters: key_id, project_id, agent, model, customer_id, errors=1, guard=1, limit, offset)", resp=listOf(S("RequestLog")))
r("GET", "/admin/api/logs/{id}", "Monitor", "One request log incl. bodies when logged", resp=S("RequestLog"))
r("GET", "/admin/api/audit", "Monitor", "Append-only audit log of admin actions (filters: actor, action prefix, target, since, until, limit, offset). Team plan.", resp=listOf(S("AuditEntry")))
r("GET", "/admin/api/audit/export.csv", "Monitor", "Audit log as CSV (same filters, up to 5000 rows). Team plan.")
r("GET", "/admin/api/plan", "Auth", "Effective plan, Team feature flags, caller role and coming-soon features")
r("GET", "/admin/api/mcp/servers", "MCP", "MCP servers (secrets never returned)", resp=listOf(S("MCPServer")))
r("POST", "/admin/api/mcp/servers", "MCP", "Register an MCP server", None, S("MCPServerInput"), S("MCPServer"), "201")
r("PATCH", "/admin/api/mcp/servers/{id}", "MCP", "Update / rotate secret / enable-disable an MCP server", None, S("MCPServerInput"), S("MCPServer"))
r("DELETE", "/admin/api/mcp/servers/{id}", "MCP", "Delete an MCP server with its policies and credentials", code="204")
r("GET", "/admin/api/mcp/servers/{id}/credentials", "MCP", "Per-agent upstream credentials (hints only)", resp=listOf(S("MCPCredential")))
r("PUT", "/admin/api/mcp/servers/{id}/credentials", "MCP", "Set an agent's upstream credential", None, obj({"agent": s, "header": s, "secret": s}, ["agent", "secret"]), S("MCPCredential"))
r("DELETE", "/admin/api/mcp/servers/{id}/credentials/{cid}", "MCP", "Delete an agent credential", code="204")
r("GET", "/admin/api/mcp/policies", "MCP", "Tool allow/deny and rate-limit rules (filter: server_id)", resp=listOf(S("MCPPolicy")))
r("POST", "/admin/api/mcp/policies", "MCP", "Add a tool rule", None, S("MCPPolicy"), S("MCPPolicy"), "201")
r("DELETE", "/admin/api/mcp/policies/{id}", "MCP", "Delete a tool rule", code="204")
r("POST", "/admin/api/mcp/check", "MCP", "Dry run: would this agent be allowed to call this tool?", "viewer", obj({"server_id": s, "agent": s, "tool": s}), obj({"allowed": b, "rpm": i}))
r("GET", "/admin/api/mcp/calls", "MCP", "MCP tool-call log (filters: server_id, agent, tool, errors=1)", resp=listOf(S("MCPCall")))
# Projects & keys
r("GET", "/admin/api/projects", "Projects", "List projects in the org", resp=listOf(S("Project")))
r("POST", "/admin/api/projects", "Projects", "Create project", None, obj({"name": s, "log_bodies": b}, ["name"]), S("Project"), "201")
r("PATCH", "/admin/api/projects/{id}", "Projects", "Update body logging, model policy, cache TTL, allowed_features, trust_customer_header", None, S("ProjectPatch"), S("Project"))
r("POST", "/admin/api/projects/{id}/kill", "Projects", "Kill switch: block and abort in-flight", "member", resp=obj({"aborted_inflight": i}))
r("POST", "/admin/api/projects/{id}/unkill", "Projects", "Re-enable", "member")
r("PUT", "/admin/api/projects/{id}/cache", "Projects", "Set response-cache TTL", None, obj({"ttl_seconds": i}), S("Project"))
r("PUT", "/admin/api/projects/{id}/guardrails", "Guardrails", "Set project guardrails (null clears to server defaults)", None, S("Guardrails"))
r("GET", "/admin/api/keys", "Keys", "List virtual keys", resp=listOf(S("VirtualKey")))
r("POST", "/admin/api/keys", "Keys", "Create a virtual key (plaintext returned once)", "member", S("KeyCreate"), S("KeyCreated"), "201")
r("PATCH", "/admin/api/keys/{id}", "Keys", "Update key model policy / bind to a customer", None, S("KeyPatch"), S("VirtualKey"))
r("DELETE", "/admin/api/keys/{id}", "Keys", "Revoke a key")
r("POST", "/admin/api/keys/{id}/kill", "Keys", "Kill switch", "member", resp=obj({"aborted_inflight": i}))
r("POST", "/admin/api/keys/{id}/unkill", "Keys", "Re-enable", "member")
# Budgets
r("GET", "/admin/api/limits", "Budgets", "Limits with current spend", resp=listOf(S("Limit")))
r("PUT", "/admin/api/limits", "Budgets", "Create or replace a limit", None, S("Limit"), S("Limit"))
r("DELETE", "/admin/api/limits", "Budgets", "Delete a limit (?scope=&scope_id=)")
# Providers & pricing
r("GET", "/admin/api/providers", "Providers", "Configured providers, routes and key sources")
r("GET", "/admin/api/provider-keys", "Providers", "Stored provider keys (hints only)", resp=listOf(S("ProviderKey")))
r("POST", "/admin/api/provider-keys", "Providers", "Store an encrypted provider key", None, obj({"provider": s, "key": s, "label": s}, ["provider", "key"]), S("ProviderKey"), "201")
r("DELETE", "/admin/api/provider-keys/{id}", "Providers", "Delete a provider key")
r("GET", "/admin/api/pricing", "Providers", "Active price table")
r("POST", "/admin/api/pricing/reload", "Providers", "Reload the price table (owner)", "owner")
# Routing
r("GET", "/admin/api/routing/rules", "Routing", "List routing rules", resp=listOf(S("RoutingRule")))
r("POST", "/admin/api/routing/rules", "Routing", "Create a rule", None, S("RoutingRule"), S("RoutingRule"))
r("PUT", "/admin/api/routing/rules/{id}", "Routing", "Replace a rule", None, S("RoutingRule"), S("RoutingRule"))
r("DELETE", "/admin/api/routing/rules/{id}", "Routing", "Delete a rule")
r("POST", "/admin/api/routing/preview", "Routing", "Dry-run routing for a sample request", "viewer", obj({"model": s, "messages": arr({"type": "object"}), "tags": arr(s)}))
r("GET", "/admin/api/routing/auto", "Routing", "auto alias configuration and tiers")
# Guardrails
r("GET", "/admin/api/guardrails", "Guardrails", "Server defaults and per-project settings")
r("POST", "/admin/api/guardrails/test", "Guardrails", "Scan sample text (dry run)", "viewer", obj({"text": s, "categories": arr(s)}),
  obj({"redacted": s, "findings": {"type": "object"}, "injection_score": n, "injection_flagged": b}))
# Cache
r("GET", "/admin/api/cache", "Cache", "Cache stats for the org or ?project_id=")
r("DELETE", "/admin/api/cache", "Cache", "Purge a project's cache (?project_id=)", resp=obj({"deleted": i}))
# Billing
r("GET", "/admin/api/customers", "Billing", "Per-customer usage and billed amount (?from=&to=)", resp=listOf(S("Customer")))
r("PUT", "/admin/api/customers/{project}/{id}", "Billing", "Set customer name / Stripe customer ID", None, obj({"name": s, "stripe_customer_id": s}), S("Customer"))
r("GET", "/admin/api/billing/markups", "Billing", "Markup rules", resp=listOf(S("Markup")))
r("POST", "/admin/api/billing/markups", "Billing", "Create markup rule", None, S("Markup"), S("Markup"), "201")
r("DELETE", "/admin/api/billing/markups/{id}", "Billing", "Delete markup rule")
r("GET", "/admin/api/billing/export.csv", "Billing", "Monthly usage CSV (?month=YYYY-MM)", resp="csv")
r("GET", "/admin/api/billing/stripe", "Billing", "Stripe meter sync status")
r("POST", "/admin/api/billing/stripe/sync", "Billing", "Sync this org's usage to Stripe now", resp=obj({"customers": i, "events": i, "errors": arr(s)}))
# Cloud (only when cloud.enabled)
plan = obj({"id": s, "name": s, "price_usd": n, "interval": s, "contact_sales": b, "self_serve": b, "max_requests_month": i,
            "max_members": i, "max_projects": i, "log_retention_days": i, "features": arr(s)})
r("GET", "/admin/api/billing", "Cloud", "Plan, subscription status, usage and the plan catalog (cloud mode)", "viewer",
  resp=obj({"plan": s, "effective_plan": plan, "status": s, "current_period_end": s, "cancel_at_period_end": b, "has_subscription": b,
            "usage": obj({"period": s, "requests": i, "projects": i, "members": i, "pending_invites": i}), "plans": arr(plan), "sales_url": s, "can_manage": b,
            "risk_status": s, "suspended": b}))
r("POST", "/admin/api/billing/checkout", "Cloud", "Start (or reuse the open) Stripe Checkout for a plan (owner); 409 if a subscription exists", "owner", obj({"plan": s}, ["plan"]), obj({"url": s, "id": s, "reused": b}))
r("POST", "/admin/api/billing/portal", "Cloud", "Open the Stripe Customer Portal (owner)", "owner", resp=obj({"url": s}))
r("PUT", "/admin/api/billing/plan", "Cloud", "Assign a plan by hand (operator admin token only; e.g. Enterprise)", "admin token", obj({"plan": s}, ["plan"]))
r("POST", "/admin/api/billing/risk/clear", "Cloud", "Lift a dispute / fraud-warning suspension after review (operator admin token only)", "admin token")
# Team
r("GET", "/admin/api/orgs", "Team", "Orgs the caller belongs to", resp=listOf(S("Org")))
r("POST", "/admin/api/orgs", "Team", "Create an org (caller becomes owner)", None, obj({"name": s}, ["name"]), S("Org"), "201")
r("GET", "/admin/api/org", "Team", "Current org", resp=S("Org"))
r("PATCH", "/admin/api/org", "Team", "Rename current org (owner)", None, obj({"name": s}), S("Org"))
r("GET", "/admin/api/members", "Team", "Members of the current org", resp=listOf(S("Member")))
r("PATCH", "/admin/api/members/{id}", "Team", "Change a member's role", None, obj({"role": s}, ["role"]), S("Member"))
r("DELETE", "/admin/api/members/{id}", "Team", "Remove a member")
r("GET", "/admin/api/invites", "Team", "Pending invites", resp=listOf(S("Invite")))
r("POST", "/admin/api/invites", "Team", "Invite by email (role <= inviter's)", None, obj({"email": s, "role": s}, ["email"]),
  obj({"invite": S("Invite"), "token": s, "url": s}), "201")
r("DELETE", "/admin/api/invites/{id}", "Team", "Revoke an invite")
r("GET", "/admin/api/settings", "Team", "Instance and org settings (read-only view)")

paths = {}
for m, p, tag, summ, role, req, resp, code in R:
    op = {"tags": [tag], "summary": summ, "operationId": (m.lower() + re.sub(r"[^A-Za-z0-9]+", "_", p.replace("/admin/api", ""))).rstrip("_")}
    if role == "public":
        op["security"] = [{}, {"session": []}]
    else:
        op["x-min-role"] = role or ("viewer" if m == "GET" else "admin")
    params = [{"name": x, "in": "path", "required": True, "schema": s} for x in re.findall(r"{(\w+)}", p)]
    if params:
        op["parameters"] = params
    if req:
        op["requestBody"] = {"required": True, "content": {"application/json": {"schema": req}}}
    if resp == "csv":
        ok = {"description": "CSV", "content": {"text/csv": {"schema": s}}}
    elif resp:
        ok = {"description": "OK", "content": {"application/json": {"schema": resp}}}
    else:
        ok = {"description": "OK"}
    err = {"description": "Error", "content": {"application/json": {"schema": S("Error")}}}
    op["responses"] = {code: ok, "400": err, "401": err, "403": err, "404": {"description": "Not found (also for other orgs' IDs)", "content": err["content"]}}
    paths.setdefault(p, {})[m.lower()] = op

spec = {
    "openapi": "3.0.3",
    "info": {"title": "OpenGate Admin API", "version": "1.0.0",
             "description": "Admin API of the OpenGate AI gateway. Every route is scoped to the caller's organization: "
             "a session's active org, or with the admin token the org in X-OpenGate-Org (default org otherwise). "
             "Cookie-authenticated writes must send X-Requested-With: opengate. x-min-role gives the minimum role "
             "(viewer < member < admin < owner). The gateway itself (/v1/*) is OpenAI-compatible and not described here.",
             "license": {"name": "Apache-2.0"}},
    "servers": [{"url": "http://localhost:8080"}],
    "security": [{"adminToken": []}, {"session": []}],
    "tags": [{"name": t} for t in dict.fromkeys(x[2] for x in R)],
    "paths": paths,
    "components": {
        "securitySchemes": {
            "adminToken": {"type": "http", "scheme": "bearer", "description": "OPENGATE_ADMIN_TOKEN; pick the org with X-OpenGate-Org"},
            "session": {"type": "apiKey", "in": "cookie", "name": "og_session"}},
        "schemas": schemas},
}

def dump(v, ind=0):
    pad = "  " * ind
    if isinstance(v, dict):
        if not v:
            return " {}"
        out = ""
        for k, x in v.items():
            key = json.dumps(k) if re.search(r"[^A-Za-z0-9_\-/.$]", k) or k in ("y", "n", "yes", "no", "on", "off") or k[:1].isdigit() else k
            if isinstance(x, (dict, list)) and x:
                out += f"\n{pad}{key}:" + dump(x, ind + 1)
            else:
                out += f"\n{pad}{key}:" + dump(x, ind + 1)
        return out
    if isinstance(v, list):
        if not v:
            return " []"
        out = ""
        for x in v:
            if isinstance(x, dict) and x:
                first, *rest = dump(x, ind + 1).lstrip("\n").split("\n")
                out += f"\n{pad}- " + first.strip() + "".join("\n" + l for l in rest)
            else:
                out += f"\n{pad}-" + dump(x, ind + 1)
        return out
    return " " + json.dumps(v)

root = os.path.join(os.path.dirname(__file__), "..")
with open(os.path.join(root, "docs", "openapi.yaml"), "w") as f:
    f.write("# Generated by tools/gen_openapi.py - edit that file, not this one.\n" + dump(spec).lstrip("\n") + "\n")
print(len(R), "operations")
