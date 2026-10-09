// OpenGate dashboard: dependency-free vanilla JS. All DOM is built with
// el() (no innerHTML with data), so log content can never inject markup.
"use strict";

const TOKEN_KEY = "opengate_admin_token";
const COLORS = ["#079A6A", "#60a5fa", "#f59e0b", "#06b6d4", "#ec4899", "#a855f7", "#94a3b8", "#84cc16"];
const state = { projects: [], keys: [] };

// ---------- helpers ----------
function el(tag, attrs, ...children) {
  const svg = ["svg", "rect", "g", "line", "text", "title", "path"].includes(tag);
  const n = svg ? document.createElementNS("http://www.w3.org/2000/svg", tag) : document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === undefined || v === null || v === false) continue;
    if (k.startsWith("on")) n.addEventListener(k.slice(2), v);
    else if (k === "class") n.setAttribute("class", v);
    else if (k === "checked" || k === "value" || k === "selected") n[k] = v;
    else n.setAttribute(k, v === true ? "" : v);
  }
  for (const c of children.flat()) {
    if (c === null || c === undefined || c === false) continue;
    n.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return n;
}
const $ = (id) => document.getElementById(id);
const usd = (v, d) => "$" + Number(v || 0).toLocaleString(undefined, { minimumFractionDigits: d ?? 2, maximumFractionDigits: d ?? 2 });
const usdSmart = (v) => (v > 0 && v < 0.01 ? usd(v, 5) : usd(v));
const num = (v) => Number(v || 0).toLocaleString();
const ago = (ts) => {
  const s = (Date.now() - new Date(ts).getTime()) / 1000;
  if (s < 60) return Math.max(0, Math.floor(s)) + "s ago";
  if (s < 3600) return Math.floor(s / 60) + "m ago";
  if (s < 86400) return Math.floor(s / 3600) + "h ago";
  return new Date(ts).toLocaleDateString();
};
const fmtTime = (ts) => new Date(ts).toLocaleString(undefined, { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit", second: "2-digit" });
const projName = (id) => (state.projects.find((p) => p.id === id) || {}).name || id;
const keyName = (id) => { const k = state.keys.find((k) => k.id === id); return k ? k.name : id; };

function toast(msg, bad) {
  const t = $("toast");
  t.textContent = msg;
  t.className = "toast" + (bad ? " bad" : "");
  clearTimeout(toast.t);
  toast.t = setTimeout(() => t.classList.add("hidden"), 3500);
}

async function api(path, opts = {}) {
  const res = await fetch("/admin/api" + path, {
    method: opts.method || "GET",
    headers: { Authorization: "Bearer " + sessionStorage.getItem(TOKEN_KEY), "Content-Type": "application/json" },
    body: opts.body ? JSON.stringify(opts.body) : undefined,
  });
  if (res.status === 401) { logout(); throw new Error("unauthorized"); }
  const data = await res.json().catch(() => ({}));
  if (!res.ok) { const m = (data.error && data.error.message) || res.statusText; toast(m, true); throw new Error(m); }
  return data;
}

function statusBadge(l) {
  if (l.status < 400) return el("span", { class: "badge ok" }, l.status);
  const blocked = ["budget_exceeded", "key_killed", "project_killed", "killed", "killed_mid_stream", "rate_limit_exceeded", "model_not_allowed", "max_tokens_exceeded", "guardrail_blocked", "prompt_injection"].includes(l.error_code);
  return el("span", { class: "badge " + (blocked ? "warn" : "err"), title: l.error_message || "" }, l.status + " " + (l.error_code || ""));
}

function modal(...content) {
  const body = $("modal-body");
  body.replaceChildren(...content, el("div", { class: "actions section-gap" }, el("button", { class: "btn", onclick: closeModal }, "Close")));
  $("modal").classList.remove("hidden");
}
function closeModal() { $("modal").classList.add("hidden"); }

function card(title, ...children) { return el("div", { class: "card" }, title ? el("h2", {}, title) : null, ...children); }
function table(headers, rows, empty) {
  if (!rows.length) return el("div", { class: "empty" }, empty || "Nothing here yet.");
  return el("div", { class: "table-wrap" }, el("table", {},
    el("thead", {}, el("tr", {}, headers.map((h) => el("th", { class: h.startsWith("#") ? "num" : "" }, h.replace(/^#/, ""))))),
    el("tbody", {}, rows)));
}
function sw(checked, onchange) {
  return el("label", { class: "switch" }, el("input", { type: "checkbox", checked, onchange: (e) => onchange(e.target.checked) }), el("span", {}));
}
function bar(spent, limit, neutral) {
  const pct = limit > 0 ? Math.min(100, (spent / limit) * 100) : 0;
  const b = el("div", { class: "bar" + (neutral ? "" : pct >= 100 ? " over" : pct >= 80 ? " warn" : "") }, el("span", {}));
  b.firstChild.style.width = pct + "%";
  return b;
}

// ---------- chart ----------
function spendChart(series, days, since) {
  const W = 900, H = 330, P = { l: 56, r: 12, t: 12, b: 28 };
  const start = new Date(since).getTime();
  const buckets = [];
  for (let i = 0; i < days; i++) buckets.push({ t: start + i * 86400000, parts: {} });
  const providers = [...new Set(series.filter((p) => p.cost_usd > 0).map((p) => p.provider))].sort();
  for (const p of series) {
    const i = Math.round((new Date(p.bucket).getTime() - start) / 86400000);
    if (buckets[i]) buckets[i].parts[p.provider] = (buckets[i].parts[p.provider] || 0) + p.cost_usd;
  }
  const max = Math.max(0.01, ...buckets.map((b) => Object.values(b.parts).reduce((a, c) => a + c, 0)));
  const nice = (() => { const e = Math.pow(10, Math.floor(Math.log10(max))); for (const m of [1, 2, 2.5, 5, 10]) if (m * e >= max) return m * e; return 10 * e; })();
  const iw = W - P.l - P.r, ih = H - P.t - P.b, bw = iw / days;
  const svg = el("svg", { class: "chart", viewBox: `0 0 ${W} ${H}`, preserveAspectRatio: "none" });
  for (let g = 0; g <= 4; g++) {
    const y = P.t + ih - (ih * g) / 4;
    svg.append(el("line", { class: "grid-line", x1: P.l, x2: W - P.r, y1: y, y2: y }));
    svg.append(el("text", { x: P.l - 8, y: y + 4, "text-anchor": "end" }, usd((nice * g) / 4, nice < 10 ? 2 : 0)));
  }
  buckets.forEach((b, i) => {
    let y = P.t + ih;
    const x = P.l + i * bw + bw * 0.18;
    const total = Object.values(b.parts).reduce((a, c) => a + c, 0);
    const g = el("g", {}, el("title", {}, new Date(b.t).toLocaleDateString() + " — " + usdSmart(total) + "\n" +
      providers.filter((p) => b.parts[p]).map((p) => `${p}: ${usdSmart(b.parts[p])}`).join("\n")));
    providers.forEach((p, pi) => {
      const v = b.parts[p] || 0;
      if (!v) return;
      const h = (v / nice) * ih;
      y -= h;
      g.append(el("rect", { x, y, width: Math.max(1, bw * 0.64), height: Math.max(0.5, h), rx: 2, fill: COLORS[pi % COLORS.length] }));
    });
    svg.append(g);
    const every = days > 14 ? 5 : days > 7 ? 2 : 1;
    if (i % every === 0 || i === days - 1)
      svg.append(el("text", { x: P.l + i * bw + bw / 2, y: H - 8, "text-anchor": "middle" },
        new Date(b.t).toLocaleDateString(undefined, { month: "short", day: "numeric", timeZone: "UTC" })));
  });
  const legend = el("div", { class: "legend" }, providers.map((p, i) => {
    const sw = el("i", {}); sw.style.background = COLORS[i % COLORS.length];
    return el("span", {}, sw, p);
  }));
  return el("div", {}, svg, legend);
}

// ---------- pages ----------
const pages = {
  async overview(root, actions) {
    const range = el("select", { onchange: (e) => { pages.days = +e.target.value; renderPageOnly(); } },
      [7, 14, 30, 90].map((d) => el("option", { value: d, selected: d === (pages.days || 30) }, `Last ${d} days`)));
    actions.append(range);
    const o = await api("/overview?days=" + (pages.days || 30));
    const stat = (label, value, sub, bad) => el("div", { class: "card stat" }, el("div", { class: "label" }, label), el("div", { class: "value" }, value), el("div", { class: "sub" + (bad ? " bad" : "") }, sub));
    const errRate = o.today.requests ? ((o.today.errors / o.today.requests) * 100).toFixed(1) : "0.0";
    root.append(
      el("div", { class: "grid stats" },
        stat("Spend today (UTC)", usd(o.today.cost_usd), num(o.today.tokens) + " tokens"),
        stat("Spend this month", usd(o.month.cost_usd), num(o.month.requests) + " requests"),
        stat("Requests today", num(o.today.requests), errRate + "% errors", o.today.errors > 0 && errRate > 5),
        stat("Blocked by guardrails today", num(o.today.blocked), "avg latency " + Math.round(o.today.avg_latency_ms || 0) + " ms"),
        savedStat(o.month)),
      el("div", { class: "grid two" },
        card(`Spend over time — ${usd(o.range.cost_usd)} in ${o.days} days`, spendChart(o.series, o.days, o.since)),
        el("div", { class: "grid" },
          card("Top agents", ...topList(o.top_agents)),
          card("Top models", ...topList(o.top_models)))),
    );
    const recent = await api("/logs?limit=8");
    root.append(el("div", { class: "section-gap" }, card("Recent requests", logTable(recent.data))));
  },

  async logs(root) {
    const f = pages.logFilter || (pages.logFilter = { project_id: "", agent: "", errors: false, offset: 0 });
    const proj = el("select", { onchange: (e) => { f.project_id = e.target.value; f.offset = 0; render(); } },
      el("option", { value: "" }, "All projects"), state.projects.map((p) => el("option", { value: p.id, selected: p.id === f.project_id }, p.name)));
    const agent = el("input", { placeholder: "Agent", value: f.agent, onchange: (e) => { f.agent = e.target.value.trim(); f.offset = 0; render(); } });
    const errs = el("label", {}, el("input", { type: "checkbox", checked: f.errors, onchange: (e) => { f.errors = e.target.checked; f.offset = 0; render(); } }), "Errors & blocked only");
    const q = new URLSearchParams({ limit: 50, offset: f.offset, project_id: f.project_id, agent: f.agent, errors: f.errors ? "1" : "" });
    const r = await api("/logs?" + q);
    const pager = el("div", { class: "filters section-gap" },
      el("button", { class: "btn small", disabled: f.offset === 0, onclick: () => { f.offset = Math.max(0, f.offset - 50); render(); } }, "← Newer"),
      el("span", { class: "muted" }, `${r.total ? f.offset + 1 : 0}–${Math.min(f.offset + 50, r.total)} of ${num(r.total)}`),
      el("button", { class: "btn small", disabled: f.offset + 50 >= r.total, onclick: () => { f.offset += 50; render(); } }, "Older →"));
    root.append(card(null, el("div", { class: "filters" }, proj, agent, errs), logTable(r.data), pager));
  },

  async keys(root, actions) {
    actions.append(el("button", { class: "btn primary", onclick: newKeyModal }, "+ New virtual key"));
    const rows = state.keys.map((k) => el("tr", { class: k.killed ? "killed" : "" },
      el("td", {}, el("strong", {}, k.name), el("div", { class: "muted pill" }, k.prefix + "…")),
      el("td", {}, projName(k.project_id)),
      el("td", {}, k.agent || el("span", { class: "muted" }, "—")),
      el("td", {}, k.user || el("span", { class: "muted" }, "—")),
      el("td", { class: "num" }, usdSmart(k.spend_today)),
      el("td", { class: "num" }, usdSmart(k.spend_total)),
      el("td", {}, k.killed ? el("span", { class: "badge err" }, "KILLED") : el("span", { class: "badge ok" }, "active")),
      el("td", {}, el("div", { class: "actions" },
        k.killed ? el("button", { class: "btn small", onclick: () => killKey(k, false) }, "Re-enable")
                 : el("button", { class: "btn danger small", onclick: () => killKey(k, true) }, "Kill switch"),
        el("button", { class: "btn ghost small", onclick: () => limitModal("key", k.id, k.name) }, "Budget"),
        el("button", { class: "btn ghost small", onclick: () => revokeKey(k) }, "Revoke")))));
    root.append(card(null, table(["Key", "Project", "Agent", "User", "#Today", "#Total", "Status", "Actions"], rows, "No keys yet — create one to start sending traffic.")));
  },

  async projects(root, actions) {
    actions.append(el("button", { class: "btn primary", onclick: newProjectModal }, "+ New project"));
    const r = await api("/projects");
    const rows = r.data.map((p) => el("tr", { class: p.killed ? "killed" : "" },
      el("td", {}, el("strong", {}, p.name), el("div", { class: "muted pill" }, p.id)),
      el("td", { class: "num" }, p.keys),
      el("td", { class: "num" }, usdSmart(p.spend_today)),
      el("td", { class: "num" }, usdSmart(p.spend_month)),
      el("td", {}, sw(p.log_bodies, async (on) => { await api("/projects/" + p.id, { method: "PATCH", body: { log_bodies: on } }); toast("Body logging " + (on ? "enabled" : "disabled") + " for " + p.name); })),
      el("td", {}, p.killed ? el("span", { class: "badge err" }, "KILLED") : el("span", { class: "badge ok" }, "active")),
      el("td", {}, el("div", { class: "actions" },
        p.killed ? el("button", { class: "btn small", onclick: () => killProject(p, false) }, "Re-enable")
                 : el("button", { class: "btn danger small", onclick: () => killProject(p, true) }, "Kill switch"),
        el("button", { class: "btn ghost small", onclick: () => limitModal("project", p.id, p.name) }, "Budget")))));
    root.append(card(null, table(["Project", "#Keys", "#Today", "#Month", "Log bodies", "Status", "Actions"], rows)),
      el("p", { class: "muted" }, "Request/response bodies are never stored unless body logging is switched on for a project."));
  },

  async budgets(root, actions) {
    actions.append(el("button", { class: "btn primary", onclick: () => limitModal() }, "+ Add budget"));
    const r = await api("/limits");
    const cell = (spent, limit) => limit > 0
      ? el("td", { class: "limit-cell" }, `${usdSmart(spent)} / ${usd(limit)}`, bar(spent, limit))
      : el("td", { class: "muted" }, "—");
    const rows = r.data.map((l) => el("tr", {},
      el("td", {}, el("span", { class: "badge info" }, l.scope)),
      el("td", {}, l.label || l.scope_id),
      cell(l.daily_spent, l.daily_usd), cell(l.monthly_spent, l.monthly_usd), cell(l.total_spent, l.total_usd),
      el("td", { class: "num" }, l.rpm || "—"),
      el("td", {}, el("div", { class: "actions" },
        el("button", { class: "btn ghost small", onclick: () => limitModal(l.scope, l.scope_id, l.label, l) }, "Edit"),
        el("button", { class: "btn ghost small", onclick: async () => { await api(`/limits?scope=${l.scope}&scope_id=${encodeURIComponent(l.scope_id)}`, { method: "DELETE" }); render(); } }, "Remove")))));
    root.append(card(null, table(["Scope", "Target", "Daily", "Monthly", "Total", "#RPM", ""], rows, "No budgets yet. Without a budget, spend is unlimited.")),
      el("p", { class: "muted" }, "Requests are rejected with HTTP 429 (code budget_exceeded) once any applicable key, agent or project budget is reached. Periods reset at 00:00 UTC / the 1st of the month."));
  },

  async providers(root) {
    const [p, pk] = await Promise.all([api("/providers"), api("/provider-keys")]);
    const prow = p.data.map((x) => el("tr", {},
      el("td", {}, el("strong", {}, x.name)), el("td", {}, x.type), el("td", { class: "pill" }, x.base_url),
      el("td", {}, x.key_source === "none" ? el("span", { class: "badge warn" }, "no key") : el("span", { class: "badge ok" }, x.key_source)),
      el("td", {}, x.timeout)));
    const krow = pk.data.map((k) => el("tr", {},
      el("td", {}, k.provider), el("td", {}, k.label || "—"), el("td", { class: "pill" }, k.hint || "••••"), el("td", {}, ago(k.created_at)),
      el("td", {}, el("button", { class: "btn ghost small", onclick: async () => { if (confirm("Delete this provider key?")) { await api("/provider-keys/" + k.id, { method: "DELETE" }); render(); } } }, "Delete"))));
    const prov = el("select", {}, p.data.map((x) => el("option", { value: x.name }, x.name)));
    const label = el("input", { placeholder: "Label (optional)" });
    const key = el("input", { type: "password", placeholder: "sk-…", autocomplete: "off" });
    const routes = Object.entries(p.routes || {}).map(([a, t]) => el("div", { class: "list-row" }, el("span", { class: "pill" }, a), el("span", { class: "muted" }, t.join("  →  "))));
    root.append(
      card("Providers", table(["Name", "Type", "Base URL", "Key", "Timeout"], prow)),
      el("div", { class: "grid half section-gap" },
        card("Provider keys (AES-256-GCM encrypted at rest)", table(["Provider", "Label", "Key", "Added", ""], krow, "No stored keys; env vars are used if set."),
          el("form", { class: "form section-gap", onsubmit: async (e) => { e.preventDefault(); await api("/provider-keys", { method: "POST", body: { provider: prov.value, label: label.value, key: key.value } }); toast("Key stored encrypted"); render(); } },
            el("label", {}, "Provider", prov), el("label", {}, "Label", label), el("label", {}, "API key", key), el("button", { class: "btn primary", type: "submit" }, "Add key"))),
        card(`Routes & failover (retry ×${p.retry.max_attempts}, backoff ${p.retry.initial_backoff}–${p.retry.max_backoff})`, routes.length ? routes : el("div", { class: "empty" }, "No aliases configured. Use provider/model names directly."))));
  },
};

function savedStat(m) {
  const total = (m.saved_usd || 0) + (m.cache_saved_usd || 0);
  return el("div", { class: "card stat saved" }, el("div", { class: "label" }, "Saved this month"),
    el("div", { class: "value" }, usd(total)),
    el("div", { class: "sub" }, `routing ${usdSmart(m.saved_usd)} · cache ${num(m.cache_hits)} hits`));
}

function topList(items) {
  if (!items || !items.length) return [el("div", { class: "empty" }, "No traffic yet.")];
  const max = Math.max(...items.map((i) => i.cost_usd), 0.000001);
  return items.slice(0, 6).map((i) => el("div", { class: "list-row" },
    el("div", { class: "grow" }, el("div", {}, i.name), bar(i.cost_usd, max, true)),
    el("div", { class: "num" }, el("div", {}, usdSmart(i.cost_usd)), el("div", { class: "muted" }, num(i.requests) + " req"))));
}

function logTable(logs) {
  const rows = logs.map((l) => el("tr", { class: "clickable", onclick: () => logDetail(l.id) },
    el("td", { title: fmtTime(l.ts) }, ago(l.ts)),
    el("td", {}, projName(l.project_id)),
    el("td", {}, l.agent || keyName(l.key_id)),
    el("td", {}, l.model, l.provider && (l.provider + "/" + l.provider_model) !== l.model ? el("div", { class: "muted pill" }, "→ " + l.provider + "/" + l.provider_model) : null),
    el("td", { class: "num" }, num(l.prompt_tokens + l.completion_tokens) + (l.usage_estimated ? "*" : "")),
    el("td", { class: "num" }, usdSmart(l.cost_usd)),
    el("td", { class: "num" }, num(l.latency_ms) + " ms"),
    el("td", { class: "tags" }, l.stream ? el("span", { class: "badge info" }, "stream") : null,
      l.cache_hit ? el("span", { class: "badge ok" }, "cache") : null,
      l.route ? el("span", { class: "badge route", title: "Smart routing: " + l.route }, l.route.replace(/^rule:/, "")) : null,
      l.guard ? el("span", { class: "badge " + (l.guard_action === "blocked" ? "err" : "warn"), title: l.guard_action + ": " + l.guard }, "guard") : null),
    el("td", { class: "num" }, l.attempts > 1 ? el("span", { class: "badge warn" }, "×" + l.attempts) : l.attempts),
    el("td", {}, statusBadge(l))));
  return table(["When", "Project", "Agent / key", "Model", "#Tokens", "#Cost", "#Latency", "", "#Tries", "Status"], rows, "No requests yet.");
}

async function logDetail(id) {
  const l = await api("/logs/" + id);
  const kv = [["Request ID", l.id], ["Time", fmtTime(l.ts)], ["Project", projName(l.project_id)], ["Key", keyName(l.key_id)], ["Agent / user", (l.agent || "—") + " / " + (l.user || "—")],
    ["Requested model", l.model], ["Served by", l.provider ? l.provider + "/" + l.provider_model : "—"], ["Tokens", `${num(l.prompt_tokens)} in / ${num(l.completion_tokens)} out${l.usage_estimated ? " (estimated)" : ""}`],
    ["Cost", usd(l.cost_usd, 6) + (l.baseline_cost_usd > l.cost_usd ? `  (requested model: ${usd(l.baseline_cost_usd, 6)}, saved ${usd(l.baseline_cost_usd - l.cost_usd, 6)})` : "")],
    ["Customer", l.customer_id || "—"], ["Billed", l.billed_usd ? usd(l.billed_usd, 6) : "—"], ["Route", l.route || (l.cache_hit ? "cache hit" : "—")],
    ["Guardrails", l.guard ? `${l.guard_action}: ${l.guard}` : "—"], ["Latency", l.latency_ms + " ms"], ["Attempts", l.attempts], ["Status", l.status + (l.error_code ? " " + l.error_code : "")]];
  modal(el("h2", {}, "Request detail"), el("div", { class: "kv" }, kv.flatMap(([k, v]) => [el("div", {}, k), el("div", {}, v)])),
    l.error_message ? el("div", {}, el("h2", {}, "Error"), el("pre", {}, l.error_message)) : null,
    l.request_body ? el("div", {}, el("h2", {}, "Request body"), el("pre", {}, pretty(l.request_body))) : el("p", { class: "muted" }, "Bodies are not logged for this project (opt-in per project)."),
    l.response_body ? el("div", {}, el("h2", {}, "Response body"), el("pre", {}, pretty(l.response_body))) : null);
}
function pretty(s) { try { return JSON.stringify(JSON.parse(s), null, 2); } catch { return s; } }

async function killKey(k, kill) {
  if (kill && !confirm(`Kill key "${k.name}"? All its requests (including in-flight streams) will be rejected immediately.`)) return;
  const r = await api(`/keys/${k.id}/${kill ? "kill" : "unkill"}`, { method: "POST" });
  toast(kill ? `Key killed (${r.aborted_inflight} in-flight aborted)` : "Key re-enabled");
  render();
}
async function killProject(p, kill) {
  if (kill && !confirm(`Kill project "${p.name}"? Every key in it stops working immediately.`)) return;
  const r = await api(`/projects/${p.id}/${kill ? "kill" : "unkill"}`, { method: "POST" });
  toast(kill ? `Project killed (${r.aborted_inflight} in-flight aborted)` : "Project re-enabled");
  render();
}
async function revokeKey(k) {
  if (!confirm(`Permanently revoke "${k.name}"? This cannot be undone.`)) return;
  await api("/keys/" + k.id, { method: "DELETE" });
  render();
}

function newKeyModal() {
  const f = { project: el("select", {}, state.projects.map((p) => el("option", { value: p.id }, p.name))), name: el("input", { placeholder: "e.g. support-bot-prod", required: true }),
    agent: el("input", { placeholder: "agent label (optional)" }), user: el("input", { placeholder: "user label (optional)" }),
    daily: el("input", { type: "number", step: "0.01", min: "0", placeholder: "0 = none" }), monthly: el("input", { type: "number", step: "0.01", min: "0", placeholder: "0 = none" }),
    total: el("input", { type: "number", step: "0.01", min: "0", placeholder: "0 = none" }), rpm: el("input", { type: "number", min: "0", placeholder: "0 = none" }) };
  const form = el("form", { class: "form", onsubmit: async (e) => {
    e.preventDefault();
    const limits = { daily_usd: +f.daily.value || 0, monthly_usd: +f.monthly.value || 0, total_usd: +f.total.value || 0, rpm: +f.rpm.value || 0 };
    const r = await api("/keys", { method: "POST", body: { project_id: f.project.value, name: f.name.value, agent: f.agent.value, user: f.user.value,
      limits: Object.values(limits).some((v) => v > 0) ? limits : undefined } });
    modal(el("h2", {}, "Key created"), el("p", {}, "Copy it now — OpenGate stores only a hash and can't show it again."), el("div", { class: "secret" }, r.key));
    await loadState(); renderPageOnly();
  } },
    el("label", {}, "Project", f.project), el("label", {}, "Name", f.name), el("label", {}, "Agent", f.agent), el("label", {}, "User", f.user),
    el("label", {}, "Daily budget $", f.daily), el("label", {}, "Monthly budget $", f.monthly), el("label", {}, "Total budget $", f.total), el("label", {}, "Requests / min", f.rpm),
    el("button", { class: "btn primary", type: "submit" }, "Create key"));
  modal(el("h2", {}, "New virtual key"), form);
}

function newProjectModal() {
  const name = el("input", { placeholder: "e.g. research-agents", required: true });
  const bodies = el("input", { type: "checkbox" });
  modal(el("h2", {}, "New project"), el("form", { class: "form", onsubmit: async (e) => {
    e.preventDefault(); await api("/projects", { method: "POST", body: { name: name.value, log_bodies: bodies.checked } }); closeModal(); await loadState(); render();
  } }, el("label", {}, "Name", name), el("label", {}, el("span", {}, "Log request bodies"), bodies), el("button", { class: "btn primary", type: "submit" }, "Create")));
}

function limitModal(scope, scopeId, label, existing) {
  const e = existing || {};
  const scopeSel = el("select", {}, ["key", "agent", "project"].map((s) => el("option", { value: s, selected: s === (scope || "key") }, s)));
  const target = el("div", {});
  const buildTarget = () => {
    const s = scopeSel.value;
    let input;
    if (scopeId && s === scope) input = el("select", { disabled: true }, el("option", { value: scopeId }, label || scopeId));
    else if (s === "key") input = el("select", {}, state.keys.map((k) => el("option", { value: k.id }, `${k.name} (${projName(k.project_id)})`)));
    else if (s === "project") input = el("select", {}, state.projects.map((p) => el("option", { value: p.id }, p.name)));
    else {
      const agents = [...new Set(state.keys.filter((k) => k.agent).map((k) => k.project_id + "/" + k.agent))];
      input = el("select", {}, agents.map((a) => el("option", { value: a }, a.split("/")[1] + " @ " + projName(a.split("/")[0]))));
    }
    target.replaceChildren(el("label", {}, "Target", input));
    target.input = input;
  };
  scopeSel.addEventListener("change", buildTarget); buildTarget();
  const n = (v, step) => el("input", { type: "number", min: "0", step: step || "0.01", value: v || "", placeholder: "0 = none" });
  const d = n(e.daily_usd), m = n(e.monthly_usd), t = n(e.total_usd), r = n(e.rpm, "1");
  modal(el("h2", {}, existing ? "Edit budget" : "Set budget"), el("form", { class: "form", onsubmit: async (ev) => {
    ev.preventDefault();
    await api("/limits", { method: "PUT", body: { scope: scopeSel.value, scope_id: target.input.value, daily_usd: +d.value || 0, monthly_usd: +m.value || 0, total_usd: +t.value || 0, rpm: +r.value || 0 } });
    closeModal(); toast("Budget saved"); location.hash = "#budgets"; render();
  } }, el("label", {}, "Scope", scopeSel), target, el("label", {}, "Daily $", d), el("label", {}, "Monthly $", m), el("label", {}, "Total $", t), el("label", {}, "Requests / min", r),
    el("button", { class: "btn primary", type: "submit" }, "Save")));
}

// ---------- shell ----------
async function loadState() {
  const [p, k] = await Promise.all([api("/projects"), api("/keys")]);
  state.projects = p.data; state.keys = k.data;
}
const titles = { overview: "Overview", logs: "Request logs", keys: "Virtual keys", projects: "Projects", budgets: "Budgets & limits", providers: "Providers & routing" };
let rendering = 0;
async function renderPageOnly() {
  const page = (location.hash || "#overview").slice(1);
  const name = pages[page] ? page : "overview";
  const my = ++rendering;
  document.querySelectorAll("#nav a").forEach((a) => a.classList.toggle("active", a.dataset.page === name));
  $("page-title").textContent = titles[name];
  const root = el("div", {}), actions = el("div", { class: "actions" });
  try { await pages[name](root, actions); } catch (e) { root.append(el("div", { class: "card error" }, String(e.message || e))); }
  if (my !== rendering) return;
  $("page").replaceChildren(root);
  $("page-actions").replaceChildren(actions);
}
async function render() { try { await loadState(); } catch (e) { return; } await renderPageOnly(); }

async function start() {
  if (!sessionStorage.getItem(TOKEN_KEY)) { $("login").classList.remove("hidden"); $("app").classList.add("hidden"); return; }
  try { const w = await api("/whoami"); $("version").textContent = "v" + w.version.replace(/^v/, ""); } catch { return; }
  $("login").classList.add("hidden"); $("app").classList.remove("hidden");
  await render();
}
function logout() { sessionStorage.removeItem(TOKEN_KEY); $("app").classList.add("hidden"); $("login").classList.remove("hidden"); }

$("login-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  sessionStorage.setItem(TOKEN_KEY, $("token").value.trim());
  $("login-error").textContent = "";
  await start();
  if (!sessionStorage.getItem(TOKEN_KEY)) $("login-error").textContent = "Invalid admin token.";
});
$("logout").addEventListener("click", logout);
$("modal").addEventListener("click", (e) => { if (e.target.id === "modal") closeModal(); });
window.addEventListener("hashchange", renderPageOnly);
setInterval(() => { if (!document.hidden && sessionStorage.getItem(TOKEN_KEY) && $("modal").classList.contains("hidden") && ["#overview", "#logs", ""].includes(location.hash)) render(); }, 15000);
start();
