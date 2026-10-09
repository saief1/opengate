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
    "Markup": obj({"id": s, "project_id": s, "customer_id": s, "model": s, "markup_pct": n, "fixed_usd_per_1k_requests": n}),
    "Org": obj({"id": s, "name": s, "created_at": {"type": "string", "format": "date-time"}, "role": s}),
    "Member": obj({"id": s, "email": s, "name": s, "role": {"type": "string", "enum": ["owner", "admin", "member", "viewer"]},
                   "joined_at": {"type": "string", "format": "date-time"}}),
    "Invite": obj({"id": s, "email": s, "role": s, "expires_at": {"type": "string", "format": "date-time"}}),
    "AuditEntry": obj({"id": s, "ts": {"type": "string", "format": "date-time"}, "actor": s, "action": s, "target": s, "ip": s,
                       "detail": {"type": "object"}}),
    "Me": obj({"kind": {"type": "string", "enum": ["token", "session"]}, "user": obj({"id": s, "email": s, "name": s}), "org_id": s,
               "role": s, "orgs": arr(S("Org"))}),
}

R = []  # (method, path, tag, summary, role, request schema, response schema/code)
def r(m, p, tag, summ, role=None, req=None, resp=None, code="200"):
    R.append((m, p, tag, summ, role, req, resp, code))

# Auth (public, then session-carrying)
r("GET", "/admin/api/auth/config", "Auth", "Login options (signup enabled, etc.)", "public", resp=obj({"allow_signup": b}))
r("POST", "/admin/api/auth/login", "Auth", "Email + password login; sets the og_session cookie", "public", obj({"email": s, "password": s}, ["email", "password"]), S("Me"))
r("POST", "/admin/api/auth/logout", "Auth", "Ends the session", "public")
r("GET", "/admin/api/auth/me", "Auth", "Current principal, role and orgs", "public", resp=S("Me"))
r("POST", "/admin/api/auth/switch-org", "Auth", "Switch the session's active org", "public", obj({"org_id": s}, ["org_id"]), S("Me"))
r("GET", "/admin/api/auth/invites/{token}", "Auth", "Preview an invite", "public", resp=obj({"email": s, "org": s, "role": s}))
r("POST", "/admin/api/auth/invites/accept", "Auth", "Accept an invite (single use)", "public", obj({"token": s, "name": s, "password": s}, ["token", "password"]), S("Me"))
r("POST", "/admin/api/auth/signup", "Auth", "Self-service signup (only with auth.allow_signup)", "public", obj({"email": s, "password": s, "name": s, "org_name": s}), S("Me"), "201")
r("POST", "/admin/api/auth/password", "Auth", "Change own password", "public", obj({"current_password": s, "new_password": s}))
r("GET", "/admin/api/whoami", "Auth", "Who is calling", resp=S("Me"))
# Monitoring
r("GET", "/admin/api/overview", "Monitor", "Spend/requests summary, charts, top models/keys/routes/customers")
r("GET", "/admin/api/logs", "Monitor", "Request logs (filters: key_id, project_id, agent, model, customer_id, errors=1, guard=1, limit, offset)", resp=listOf(S("RequestLog")))
r("GET", "/admin/api/logs/{id}", "Monitor", "One request log incl. bodies when logged", resp=S("RequestLog"))
r("GET", "/admin/api/audit", "Monitor", "Audit log of admin actions", resp=listOf(S("AuditEntry")))
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
