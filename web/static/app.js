// OpenGate dashboard (design v2). Dependency-free vanilla JS served by the Go
// binary. All DOM is built with el() (never innerHTML with data), so request
// log content can never inject markup. Every number on screen comes from the
// admin API; when there is no data the page says so.
"use strict";

// ---------- config ----------
// Draft pricing shown on self-hosted installs (OpenGate Cloud reads its plans
// from the server config instead). Edit here; nothing else depends on it.
const DRAFT_PLANS = [
  { id: "free", name: "Free", price_usd: 0, sub: "forever", features: ["10k requests / month", "3 team members", "2 projects", "7-day request logs"] },
  { id: "team", name: "Team", price_usd: 49, sub: "/ month · draft", features: ["1M requests / month", "25 team members", "Unlimited projects", "30-day request logs", "Email support"] },
  { id: "enterprise", name: "Enterprise", contact_sales: true, sub: "contact sales", features: ["Unlimited requests", "SSO & custom contracts", "1-year request logs", "Priority support"] },
];
const PRICING_DRAFT = true; // shows the "Draft pricing · not final" label
const TOKEN_KEY = "opengate_admin_token", ORG_KEY = "opengate_org", PLAN_KEY = "opengate_signup_plan";
const BLOCK_CODES = ["budget_exceeded", "key_killed", "project_killed", "killed", "killed_mid_stream", "rate_limit_exceeded", "model_not_allowed", "max_tokens_exceeded", "guardrail_blocked", "prompt_injection"];
const PALETTE = ["#2BD49A", "#7AA2F7", "#B58CF5", "#E8B24A", "#F06A6A", "#5FB3C9", "#A1A1A8"];
const MONO_BG = ["#1F6F55", "#35508F", "#5B3E8A", "#6B4A2A", "#2E5E6B", "#6B2E45", "#3A3A40"];

// ---------- helpers ----------
const SVG_TAGS = new Set(["svg", "path", "rect", "circle", "line", "g", "defs", "linearGradient", "stop", "text", "polyline"]);
function el(tag, attrs, ...children) {
  const n = SVG_TAGS.has(tag) ? document.createElementNS("http://www.w3.org/2000/svg", tag) : document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === undefined || v === null || v === false) continue;
    if (k.startsWith("on")) n.addEventListener(k.slice(2), v);
    else if (k === "style") Object.assign(n.style, v); // CSSOM: allowed by the strict CSP
    else if (k === "class") n.setAttribute("class", v);
    else if (["checked", "value", "selected", "disabled"].includes(k)) n[k] = v;
    else n.setAttribute(k, v === true ? "" : v);
  }
  for (const c of children.flat(Infinity)) {
    if (c === null || c === undefined || c === false) continue;
    n.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return n;
}
const $ = (id) => document.getElementById(id);
const num = (v) => Number(v || 0).toLocaleString("en-US");
const compact = (v) => { v = Number(v || 0); if (v >= 1e9) return (v / 1e9).toFixed(2) + "B"; if (v >= 1e6) return (v / 1e6).toFixed(2) + "M"; if (v >= 1e4) return (v / 1e3).toFixed(1) + "k"; return num(v); };
function usd(v, d) {
  v = Number(v || 0);
  if (d === undefined) d = v !== 0 && Math.abs(v) < 0.01 ? 4 : 2;
  return (v < 0 ? "-$" : "$") + Math.abs(v).toLocaleString("en-US", { minimumFractionDigits: d, maximumFractionDigits: d });
}
// $4,812.40 with dimmed cents, like the design.
function usdBig(v) {
  const s = usd(v, Math.abs(v) > 0 && Math.abs(v) < 1 ? 4 : 2); const i = s.indexOf(".");
  return i < 0 ? s : [s.slice(0, i), el("span", { class: "dim" }, s.slice(i))];
}
const pct = (a, b) => (b > 0 ? Math.round((a / b) * 100) : 0);
const ms = (v) => num(Math.round(v || 0)) + "ms";
const hhmmss = (ts) => new Date(ts).toLocaleTimeString("en-GB", { hour12: false });
const dateShort = (ts) => new Date(ts).toLocaleDateString("en-US", { month: "short", day: "numeric" });
function ago(ts) {
  if (!ts || String(ts).startsWith("0001")) return "never";
  const s = (Date.now() - new Date(ts).getTime()) / 1000;
  if (s < 60) return "now"; if (s < 3600) return Math.floor(s / 60) + "m ago"; if (s < 86400) return Math.floor(s / 3600) + "h ago";
  if (s < 172800) return "Yesterday"; return dateShort(ts);
}
function delta(cur, prev, suffix) {
  if (!prev) return null;
  const d = ((cur - prev) / prev) * 100;
  return el("span", { class: d >= 0 ? "up" : "dn" }, (d >= 0 ? "▲ " : "▼ ") + Math.abs(d).toFixed(1) + "%" + (suffix || ""));
}
const initials = (s) => String(s || "?").replace(/[^A-Za-z0-9 _-]/g, "").split(/[\s_-]+/).filter(Boolean).slice(0, 2).map((w) => w[0]).join("").toUpperCase() || "?";
function hash(s) { let h = 0; for (const c of String(s)) h = (h * 31 + c.charCodeAt(0)) | 0; return Math.abs(h); }
function monoFor(name, letter) { return el("span", { class: "mono", style: { background: MONO_BG[hash(name) % MONO_BG.length] } }, letter || initials(name)[0]); }
function modelChip(model, sub) {
  const short = String(model || "—");
  const prov = short.includes("/") ? short.split("/")[0] : short;
  return el("span", { style: { display: "inline-flex", alignItems: "center", gap: "8px" } }, monoFor(prov, prov[0] ? prov[0].toUpperCase() : "?"),
    el("span", { class: "m", style: { fontSize: "12px" } }, short), sub ? el("span", { class: "mut", style: { fontSize: "11px" } }, sub) : null);
}
const st = (cls, t, title) => el("span", { class: "st " + cls, title }, t);
const SOON = () => el("span", { class: "soon" }, "Coming soon");
const kvr = (rows) => rows.filter(Boolean).map(([a, b]) => el("div", { class: "kvr" }, el("span", {}, a), b instanceof Node ? b : el("span", {}, b ?? "—")));
const pb = (p, cls) => el("div", { class: "pb " + (cls || "") }, el("i", { style: { width: Math.max(0, Math.min(100, p)) + "%" } }));
const pbClass = (p) => (p >= 100 ? "d" : p >= 75 ? "w" : "");
function card(title, inner, opts = {}) {
  return el("div", { class: "card " + (opts.class || ""), style: opts.style },
    title !== null ? el("div", { class: "hd" }, el("h3", {}, title), opts.sub ? el("span", { class: "sub" }, opts.sub) : null, opts.right ? el("div", { class: "r" }, opts.right) : null) : null, inner);
}
function kpis(items) {
  return el("div", { class: "card kpis k" + items.length, style: items.length === 4 ? {} : { gridTemplateColumns: `repeat(${items.length},1fr)` } },
    items.map(([l, v, d]) => el("div", { class: "kpi" }, el("div", { class: "kl" }, l), el("div", { class: "kv" }, v), el("div", { class: "kd" }, d || "\u00a0"))));
}
function table(headers, rows, opts = {}) {
  if (!rows.length) return opts.empty || empty("Nothing here yet");
  return el("div", { class: "tscroll" }, el("table", { class: opts.cls }, el("thead", {}, el("tr", {}, headers.map((h) => el("th", { class: h.startsWith(">") ? "r" : "" }, h.replace(/^>/, ""))))),
    el("tbody", {}, rows)));
}
function empty(title, text, action) { return el("div", { class: "empty" }, el("b", {}, title), text ? el("div", {}, text) : null, action || null); }
function toast(msg, bad) {
  const t = $("toast"); t.textContent = msg; t.className = "toast" + (bad ? " bad" : "");
  clearTimeout(toast.t); toast.t = setTimeout(() => t.classList.add("hidden"), 3500);
}
async function copy(text, what) { try { await navigator.clipboard.writeText(text); toast((what || "Copied") + " to clipboard"); } catch { toast("Copy failed; select and copy manually", true); } }
function modal(title, ...content) {
  $("modal-body").className = "modal-body";
  $("modal-body").replaceChildren(el("h2", {}, title), ...content);
  $("modal").classList.remove("hidden");
}
function closeModal() { $("modal").classList.add("hidden"); }
const csv = (s) => String(s || "").split(",").map((x) => x.trim()).filter(Boolean);
function download(name, text, type) {
  const a = el("a", { href: URL.createObjectURL(new Blob([text], { type: type || "text/csv" })), download: name });
  document.body.append(a); a.click(); a.remove();
}
const toCSV = (rows) => rows.map((r) => r.map((c) => { const s = String(c ?? ""); return /[",\n]/.test(s) ? '"' + s.replace(/"/g, '""') + '"' : s; }).join(",")).join("\n");

// ---------- API ----------
const me = { kind: "", role: "viewer", orgs: [], org_id: "", user: null };
const ROLES = { viewer: 1, member: 2, admin: 3, owner: 4 };
const can = (role) => (ROLES[me.role] || 0) >= (ROLES[role] || 9);
async function api(path, opts = {}) {
  const headers = { "Content-Type": "application/json", "X-Requested-With": "opengate" };
  const tok = sessionStorage.getItem(TOKEN_KEY);
  if (tok) { headers.Authorization = "Bearer " + tok; const o = sessionStorage.getItem(ORG_KEY); if (o) headers["X-OpenGate-Org"] = o; }
  const res = await fetch("/admin/api" + path, { method: opts.method || "GET", headers, credentials: "same-origin", body: opts.body !== undefined ? JSON.stringify(opts.body) : undefined });
  if (opts.raw) return res;
  if (res.status === 401 && !opts.quiet) { showAuth(); throw new Error("unauthorized"); }
  const data = res.status === 204 ? {} : await res.json().catch(() => ({}));
  if (!res.ok) { const m = (data.error && data.error.message) || res.statusText; if (!opts.quiet) toast(m, true); const e = new Error(m); e.status = res.status; throw e; }
  return data;
}
const soft = (p, fb) => api(p, { quiet: true }).catch(() => fb); // optional data: degrade to an empty state

// Shared state, refreshed on every navigation.
const S = { projects: [], keys: [], limits: [], cfg: {}, health: null, counts: {} };
const projName = (id) => (S.projects.find((p) => p.id === id) || {}).name || id || "—";
const keyById = (id) => S.keys.find((k) => k.id === id);
const keyName = (id) => (keyById(id) || {}).name || id || "—";
const keyLabel = (k) => k.prefix + "…";

// ---------- charts ----------
// Builds an SVG line/area chart from points [{bucket, name, value}].
// Empty input renders an honest empty state instead of a flat fake line.
function lineChart(points, opts = {}) {
  const h = opts.height || 150, W = 800;
  const names = opts.names || topNames(points, opts.max || 3);
  const buckets = opts.buckets || [...new Set(points.map((p) => +new Date(p.bucket)))].sort((a, b) => a - b);
  const wrap = el("div", { class: "chart-wrap" });
  if (!points.length || !buckets.length) {
    wrap.append(el("div", { class: "empty", style: { height: h + "px", display: "grid", placeContent: "center" } }, el("b", {}, opts.emptyTitle || "No traffic in this window"), opts.emptyText || "Send a request through the gateway and it shows up here."));
    return wrap;
  }
  const val = {}; for (const p of points) { const k = p.name + "\x00" + +new Date(p.bucket); val[k] = (val[k] || 0) + p.value; }
  const series = names.map((n, i) => ({ name: n, color: (opts.colors || PALETTE)[i % PALETTE.length], ys: buckets.map((b) => val[n + "\x00" + b] || 0) }));
  let max = Math.max(...series.flatMap((s) => s.ys), opts.ref || 0) || 1; max *= 1.15;
  const x = (i) => (buckets.length === 1 ? W / 2 : (i * W) / (buckets.length - 1)), y = (v) => h - 6 - ((h - 16) * v) / max;
  const svg = el("svg", { class: "chart", viewBox: `0 0 ${W} ${h}`, preserveAspectRatio: "none", height: h });
  const gid = "g" + Math.random().toString(36).slice(2, 8);
  svg.append(el("defs", {}, el("linearGradient", { id: gid, x1: 0, y1: 0, x2: 0, y2: 1 }, el("stop", { offset: 0, "stop-color": "#2BD49A", "stop-opacity": ".3" }), el("stop", { offset: 1, "stop-color": "#079A6A", "stop-opacity": "0" }))),
    el("g", { stroke: "rgba(255,255,255,.05)" }, el("path", { d: `M0 ${h * 0.25}H${W}M0 ${h * 0.5}H${W}M0 ${h * 0.75}H${W}` })));
  if (opts.ref) svg.append(el("path", { d: `M0 ${y(opts.ref)}H${W}`, stroke: "#E8B24A", "stroke-width": 1.2, "stroke-dasharray": "4 4", fill: "none", "vector-effect": "non-scaling-stroke" }));
  series.forEach((s, i) => {
    const d = "M" + s.ys.map((v, j) => `${x(j).toFixed(1)} ${y(v).toFixed(1)}`).join(" L");
    if (i === 0 && opts.fill !== false) svg.append(el("path", { d: `${d} L${x(s.ys.length - 1)} ${h} L${x(0)} ${h}Z`, fill: `url(#${gid})` }));
    svg.append(el("path", { d, fill: "none", stroke: s.color, "stroke-width": 1.5, "stroke-linejoin": "round", "vector-effect": "non-scaling-stroke" }));
  });
  const tip = el("div", { class: "tip hidden" });
  const fmt = opts.fmt || ((v) => usd(v));
  svg.addEventListener("mousemove", (e) => {
    const r = svg.getBoundingClientRect(); const i = Math.max(0, Math.min(buckets.length - 1, Math.round(((e.clientX - r.left) / r.width) * (buckets.length - 1))));
    const bs = opts.bucketSeconds || 3600;
    tip.replaceChildren(el("div", { class: "mut" }, bs >= 86400 ? dateShort(buckets[i]) : new Date(buckets[i]).toLocaleString("en-US", { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" })),
      ...series.map((s) => el("div", {}, el("i", { class: "dot", style: { background: s.color, marginRight: "6px" } }), s.name + "  " + fmt(s.ys[i]))));
    tip.classList.remove("hidden"); tip.style.left = Math.min(r.width - 160, e.clientX - r.left + 12) + "px"; tip.style.top = "8px";
  });
  svg.addEventListener("mouseleave", () => tip.classList.add("hidden"));
  wrap.append(el("div", { class: "legend" }, series.map((s) => el("span", {}, el("i", { style: { background: s.color } }), s.name)), opts.ref ? el("span", {}, el("i", { style: { background: "#E8B24A" } }), opts.refLabel || "limit") : null), svg, tip);
  return wrap;
}
function topNames(points, n) {
  const tot = {}; for (const p of points) tot[p.name] = (tot[p.name] || 0) + p.value;
  return Object.entries(tot).sort((a, b) => b[1] - a[1]).slice(0, n).map(([k]) => k);
}
// Fills missing buckets so lines are continuous.
function bucketRange(since, now, seconds) {
  const out = []; const step = seconds * 1000; for (let t = Math.floor(+new Date(since) / step) * step; t <= +new Date(now); t += step) out.push(t); return out;
}
function seriesPoints(r, metric, nameMap) {
  return (r.series || []).map((p) => ({ bucket: p.bucket, name: nameMap ? nameMap(p.name) : p.name, value: metric === "requests" ? p.requests : p.cost_usd }));
}
function spark(values, off) {
  const max = Math.max(...values, 0);
  return el("div", { class: "spark" + (off ? " off" : "") }, values.map((v) => el("i", { style: { height: (max ? Math.max(2, (v / max) * 28) : 2) + "px" } })));
}

// ---------- shell ----------
const ICONS = {
  overview: [["rect", { x: 3, y: 3, width: 7, height: 7, rx: 1.5 }], ["rect", { x: 14, y: 3, width: 7, height: 7, rx: 1.5 }], ["rect", { x: 3, y: 14, width: 7, height: 7, rx: 1.5 }], ["rect", { x: 14, y: 14, width: 7, height: 7, rx: 1.5 }]],
  requests: [["path", { d: "M4 6h16M4 12h16M4 18h10" }]],
  keys: [["circle", { cx: 7.5, cy: 15.5, r: 4.5 }], ["path", { d: "m10.7 12.3 9.3-9.3M17 6l3 3" }]],
  agents: [["rect", { x: 4, y: 8, width: 16, height: 12, rx: 3 }], ["path", { d: "M12 4v4" }]],
  customers: [["circle", { cx: 9, cy: 8, r: 4 }], ["path", { d: "M2 21a7 7 0 0 1 14 0M16 4a4 4 0 0 1 0 8M22 21a7 7 0 0 0-4-6" }]],
  budgets: [["circle", { cx: 12, cy: 12, r: 9 }], ["path", { d: "M12 7v5l3 2" }]],
  policies: [["path", { d: "M12 3 4 6v6c0 5 3.5 8 8 9 4.5-1 8-4 8-9V6z" }]],
  providers: [["circle", { cx: 6, cy: 19, r: 2 }], ["circle", { cx: 18, cy: 5, r: 2 }], ["path", { d: "M8 19h8a3 3 0 0 0 0-6H8a3 3 0 0 1 0-6h8" }]],
  kill: [["path", { d: "M12 2v10M18.4 6.6a9 9 0 1 1-12.8 0" }]],
  setup: [["path", { d: "m16 18 6-6-6-6M8 6l-6 6 6 6" }]],
  settings: [["circle", { cx: 12, cy: 12, r: 3 }], ["path", { d: "M12 2v3M12 19v3M2 12h3M19 12h3" }]],
  billing: [["rect", { x: 3, y: 5, width: 18, height: 14, rx: 2 }], ["path", { d: "M3 10h18" }]],
  mcp: [["rect", { x: 3, y: 4, width: 7, height: 7, rx: 1.5 }], ["rect", { x: 14, y: 13, width: 7, height: 7, rx: 1.5 }], ["path", { d: "M10 7.5h4a3 3 0 0 1 3 3V13M14 16.5h-4a3 3 0 0 1-3-3V11" }]],
  audit: [["path", { d: "M6 3h9l4 4v14H6z" }], ["path", { d: "M14 3v5h5M9 13h7M9 17h5" }]],
};
const svgIcon = (name) => el("svg", { viewBox: "0 0 24 24" }, ICONS[name].map(([t, a]) => el(t, a)));
const logoSvg = (s) => el("div", { class: "logo" }, el("svg", { width: s || 12, height: s || 12, viewBox: "0 0 24 24", fill: "none", stroke: "#fff", "stroke-width": 2.6, "stroke-linecap": "round" }, el("path", { d: "M5 20V9l7-5 7 5v11" }), el("path", { d: "M10 20v-5h4v5" })));
const NAV = [
  ["overview", "Overview"], ["requests", "Requests", "requests"], ["keys", "Keys", "keys"], ["agents", "Agents", "agents"], ["customers", "Customers", "customers"],
  "Control", ["budgets", "Budgets"], ["policies", "Policies"], ["mcp", "MCP tools", "mcp"], ["providers", "Providers & models"], ["kill", "Kill switch", null, "policies"],
  "Workspace", ["setup", "Setup & SDK"], ["settings", "Settings", null, "team"], ["audit", "Audit log"], ["billing", "Plan & billing"],
];
const TITLES = { overview: "Overview", requests: "Requests", keys: "Keys", agents: "Agents", customers: "Cost per customer", budgets: "Budgets & alerts", policies: "Kill switch & policies",
  providers: "Providers & models", setup: "Setup & SDK", team: "Team & settings", billing: "Plan & billing", mcp: "MCP tool gateway", audit: "Audit log" };
// Roles as shown to people ("viewer" is stored; "read-only" is its label).
const ROLE_LABEL = { owner: "Owner", admin: "Admin", member: "Member", viewer: "Read-only" };
const teamOn = (f) => !me.plan || !me.plan.features || me.plan.features[f] !== false;
function teamLocked(title, text) {
  return el("div", { class: "empty" }, el("b", {}, title), el("div", {}, text), el("div", { style: { marginTop: "10px" } }, st("vi", "Team plan"), " ", el("a", { href: "#billing", class: "grn" }, "See plans →")));
}

function route() {
  const h = (location.hash || "#overview").slice(1);
  const [path, query] = h.split("?");
  const parts = path.split("/");
  return { page: TITLES[parts[0]] ? parts[0] : "overview", id: parts[1] ? decodeURIComponent(parts[1]) : "", q: new URLSearchParams(query || "") };
}
function sidebar(active) {
  const org = me.orgs.find((o) => o.id === me.org_id) || { name: me.org_id };
  const items = NAV.map((n) => {
    if (typeof n === "string") return el("div", { class: "sec" }, n);
    const [ic, label, count, target] = n; const page = target || ic;
    const on = ic === "kill" ? false : page === active;
    return el("a", { class: "it" + (on ? " on" : ""), href: "#" + page }, svgIcon(ic), label, count && S.counts[count] !== undefined ? el("span", { class: "n" }, compact(S.counts[count])) : null);
  });
  const orgPick = me.orgs.length > 1
    ? el("select", { "aria-label": "Organization", onchange: (e) => switchOrg(e.target.value) }, me.orgs.map((o) => el("option", { value: o.id, selected: o.id === me.org_id }, o.name)))
    : el("span", { class: "pill", title: (org.name || "") + " · " + me.role }, org.name || "org");
  const hl = S.health;
  const user = me.kind === "user" ? (me.user.name || me.user.email) : "Admin token";
  return el("aside", { class: "sb" },
    el("div", { class: "ws" }, logoSvg(), el("b", {}, "OpenGate"), orgPick),
    el("button", { class: "srch", onclick: palette }, el("svg", { width: 13, height: 13, viewBox: "0 0 24 24", fill: "none", stroke: "currentColor", "stroke-width": 2 }, el("circle", { cx: 11, cy: 11, r: 7 }), el("path", { d: "m20 20-3.5-3.5" })), "Search or jump to…", el("span", { class: "kbd", style: { marginLeft: "auto" } }, "⌘K")),
    items,
    el("div", { class: "health" + (hl && !hl.ok ? " bad" : "") },
      el("div", { class: "h1r" }, el("span", { class: "gdot" + (!hl ? " idle" : hl.ok ? "" : " bad") }), !hl ? "Checking gateway…" : hl.ok ? "Gateway healthy" : "Gateway not ready"),
      el("div", { class: "h2r" }, hl && hl.p50 ? `p50 latency ${ms(hl.p50)} · 24h` : hl ? "no traffic in the last 24h" : "\u00a0", S.cfg.version ? " · " + String(S.cfg.version).replace(/^v?/, "v") : "")),
    el("div", { class: "me" }, el("span", { class: "mono av" }, initials(user)), el("span", { class: "nm", title: me.user ? me.user.email : "operator" }, user), el("button", { onclick: logout, title: "Sign out" }, "Sign out")));
}
async function switchOrg(id) {
  if (me.kind === "token") sessionStorage.setItem(ORG_KEY, id); else await api("/auth/switch-org", { method: "POST", body: { org_id: id } });
  await loadMe(); render();
}
function topbar(crumb, actions) {
  const org = me.orgs.find((o) => o.id === me.org_id) || { name: "" };
  return el("div", { class: "top" }, el("span", { class: "crumb" }, (org.name || "") + " / ", crumb), el("div", { class: "acts" }, actions));
}

let renderSeq = 0, poller = null;
async function render() {
  const my = ++renderSeq; clearInterval(poller); poller = null;
  const r = route();
  const [p, k, l] = await Promise.all([api("/projects"), api("/keys"), soft("/limits", { data: [] })]).catch(() => [null]);
  if (!p) return;
  S.projects = p.data; S.keys = k.data; S.limits = l.data || [];
  const agents = new Set(S.keys.filter((x) => !x.revoked && x.agent).map((x) => x.project_id + "/" + x.agent));
  S.counts.keys = S.keys.filter((x) => !x.revoked).length; S.counts.agents = agents.size;
  const actions = [], crumb = [];
  const body = el("div", { class: "body" });
  try { await PAGES[r.page](body, actions, crumb, r); } catch (e) { if (e.message !== "unauthorized") body.replaceChildren(el("div", { class: "card err-card" }, String(e.message || e))); }
  if (my !== renderSeq) return;
  $("root").replaceChildren(el("div", { class: "app" }, sidebar(r.page), el("div", { class: "main" }, topbar(crumb.length ? crumb : el("b", {}, TITLES[r.page]), actions), body)));
  window.scrollTo(0, 0);
  refreshChrome();
  Live.ensure();
}
// Sidebar counters + health, loaded after the page so navigation stays fast.
async function refreshChrome() {
  const [logs, cust, ready, ser] = await Promise.all([soft("/logs?limit=1", {}), soft("/customers", { data: [] }), fetch("/readyz").then((r) => r.ok).catch(() => false), soft("/series?hours=24&by=provider", null)]);
  const changed = S.counts.requests !== logs.total || S.counts.customers !== (cust.data || []).length || !S.health;
  S.counts.requests = logs.total; S.counts.customers = new Set((cust.data || []).map((c) => c.customer_id)).size;
  S.health = { ok: ready, p50: ser && ser.latency ? ser.latency.p50_ms : 0 };
  if (changed) { const sb = document.querySelector(".sb"); if (sb) sb.replaceWith(sidebar(route().page)); }
}

// ---------- ⌘K palette ----------
function palette() {
  const items = [
    ...Object.entries(TITLES).map(([k, t]) => ({ label: t, hint: "page", go: "#" + k })),
    ...S.keys.filter((k) => !k.revoked).map((k) => ({ label: k.name, hint: "key · " + keyLabel(k), go: "#keys" })),
    ...[...new Set(S.keys.filter((k) => k.agent).map((k) => k.agent))].map((a) => ({ label: a, hint: "agent", go: "#requests?q=" + encodeURIComponent("agent:" + a) })),
    ...S.projects.map((p) => ({ label: p.name, hint: "project", go: "#policies?project=" + p.id })),
  ];
  const input = el("input", { placeholder: "Search pages, keys, agents, projects or paste a request ID…", autocomplete: "off" });
  const list = el("div", { class: "items" }); let sel = 0, shown = [];
  const draw = () => {
    const q = input.value.trim().toLowerCase();
    shown = items.filter((i) => !q || (i.label + " " + i.hint).toLowerCase().includes(q)).slice(0, 12);
    if (/^req_[\w]+$/.test(input.value.trim())) shown.unshift({ label: "Open request " + input.value.trim(), hint: "request", go: "#requests/" + input.value.trim() });
    sel = Math.min(sel, Math.max(0, shown.length - 1));
    list.replaceChildren(...(shown.length ? shown.map((i, n) => el("div", { class: "pi" + (n === sel ? " sel" : ""), onclick: () => pick(i) }, i.label, el("span", { class: "k" }, i.hint))) : [el("div", { class: "empty" }, "No matches")]));
  };
  const pick = (i) => { closeModal(); location.hash = i.go; };
  input.addEventListener("input", () => { sel = 0; draw(); });
  input.addEventListener("keydown", (e) => {
    if (e.key === "ArrowDown") { sel = Math.min(shown.length - 1, sel + 1); draw(); e.preventDefault(); }
    if (e.key === "ArrowUp") { sel = Math.max(0, sel - 1); draw(); e.preventDefault(); }
    if (e.key === "Enter" && shown[sel]) pick(shown[sel]);
  });
  $("modal-body").className = "modal-body pal"; $("modal-body").replaceChildren(input, list); $("modal").classList.remove("hidden"); draw(); input.focus();
}
document.addEventListener("keydown", (e) => {
  if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k" && !document.querySelector(".auth")) { e.preventDefault(); palette(); }
  if (e.key === "Escape") closeModal();
});
$("modal").addEventListener("click", (e) => { if (e.target.id === "modal") closeModal(); });

// ---------- shared bits ----------
function statusTag(l) {
  if (l.status < 400) return st("ok", l.status);
  return st(BLOCK_CODES.includes(l.error_code) ? "bk" : "rt", l.status, l.error_message || "");
}
const ERR_LABEL = { budget_exceeded: "Over budget", key_killed: "Kill switch", project_killed: "Kill switch", killed: "Kill switch", killed_mid_stream: "Killed mid-stream",
  rate_limit_exceeded: "Rate limited", model_not_allowed: "Model not allowed", max_tokens_exceeded: "Max tokens", guardrail_blocked: "Guardrail blocked", prompt_injection: "Prompt injection", plan_quota_exceeded: "Plan quota" };
function logTags(l) {
  const t = [];
  if (l.error_code) t.push(st(BLOCK_CODES.includes(l.error_code) ? "bk" : "rt", ERR_LABEL[l.error_code] || l.error_code, l.error_message));
  if (l.attempts > 1) t.push(st("rt", `Failover · ${l.attempts} attempts`));
  if (l.cache_hit) t.push(st("bl", "Cache hit"));
  if (l.route && l.baseline_cost_usd > l.cost_usd) t.push(st("ok", "Routed −" + usd(l.baseline_cost_usd - l.cost_usd)));
  else if (l.route) t.push(st("mu", l.route.replace(/^rule:/, "")));
  if (l.guard_action === "redacted") t.push(st("vi", "PII redacted"));
  else if (l.guard && l.guard_action !== "blocked") t.push(st("vi", "Flagged: " + l.guard));
  return t;
}
const modelSub = (l) => (l.provider_model && l.provider + "/" + l.provider_model !== l.model ? "→ " + l.provider_model.split("/").pop() : "");
function feedRow(l) {
  const who = [l.agent ? "agent/" + l.agent : "key/" + keyName(l.key_id), l.customer_id || null, l.prompt_tokens + l.completion_tokens ? num(l.prompt_tokens + l.completion_tokens) + " tok" : l.error_code || null].filter(Boolean).join(" · ");
  return el("div", { class: "row", "data-id": l.id, onclick: () => (location.hash = "#requests/" + l.id) }, el("span", { class: "t" }, hhmmss(l.ts)), el("span", { class: "md" }, l.model),
    el("span", { class: "st m " + (l.status < 400 ? "ok" : BLOCK_CODES.includes(l.error_code) ? "bk" : "rt") }, l.status < 400 ? `${l.status} · ${usd(l.cost_usd, 4)}` : `${l.status} · ${(ERR_LABEL[l.error_code] || l.error_code || "error").toLowerCase()}`),
    el("span", { class: "s" }, who));
}
function liveCard(title, logs) {
  return card(title, el("div", { class: "feed" }, logs.length ? logs.map(feedRow) : empty("Waiting for the first request", "Point any OpenAI SDK at this gateway with a virtual key.", el("a", { class: "btn", href: "#setup" }, "Setup & SDK"))),
    { right: el("span", { class: "m mut", style: { fontSize: "11px" } }, "updated " + hhmmss(Date.now())) });
}
function seg(options, value, onpick) {
  return el("div", { class: "seg" }, options.map(([v, label]) => el("button", { class: v === value ? "a" : "", onclick: () => onpick(v) }, label)));
}
function setQuery(patch) {
  const r = route(); for (const [k, v] of Object.entries(patch)) { if (v === "" || v === null || v === undefined) r.q.delete(k); else r.q.set(k, v); }
  const qs = r.q.toString(); location.hash = "#" + r.page + (r.id ? "/" + r.id : "") + (qs ? "?" + qs : "");
}
const RANGES = [["1", "1h"], ["24", "24h"], ["168", "7d"], ["720", "30d"]];
const rangeLabel = (h) => ({ 1: "1h", 24: "24h", 168: "7d", 720: "30d" }[h] || h + "h");
let pricingCache = null;
// Returns a lookup table: exact entries plus "provider/prefix*" patterns (longest prefix wins).
async function pricing() {
  if (!pricingCache) {
    const r = await soft("/pricing", { models: {}, patterns: {} });
    pricingCache = { ...(r.models || {}) };
    Object.defineProperty(pricingCache, "_patterns", { value: Object.entries(r.patterns || {}).map(([k, v]) => [k.replace(/\*$/, ""), v]).sort((a, b) => b[0].length - a[0].length) });
  }
  return pricingCache;
}
function priceOf(P, model) {
  if (!model) return null; if (P[model]) return P[model];
  const hit = (P._patterns || []).find(([pre]) => model.startsWith(pre)); return hit ? hit[1] : null;
}

// ---------- pages ----------
const PAGES = {};

PAGES.overview = async (root, actions, crumb, r) => {
  const hours = +(r.q.get("h") || 24);
  const [ser, models, logs, rules, P] = await Promise.all([api(`/series?hours=${hours}&by=provider`), api(`/series?hours=${hours}&by=model`), api("/logs?limit=11"), soft("/routing/rules", { data: [] }), pricing()]);
  const prev = ser.previous, sm = ser.summary;
  const monthSpend = (await soft("/overview?days=1", { month: {} })).month.cost_usd || 0;
  const projLimits = S.limits.filter((l) => l.scope === "project" && l.monthly_usd > 0);
  const cap = projLimits.reduce((a, l) => a + l.monthly_usd, 0), capSpent = projLimits.reduce((a, l) => a + l.monthly_spent, 0);
  const anyKilled = S.projects.some((p) => p.killed);
  actions.push(liveIndicator(), tryItButton(),
    el("span", { class: "seg" }, RANGES.map(([v, l]) => el("button", { class: +v === hours ? "a" : "", onclick: () => setQuery({ h: v === "24" ? "" : v }) }, l))),
    can("member") ? el("a", { class: "btn k", href: "#policies" }, svgIconSmall("kill"), anyKilled ? "Kill switch · active" : "Kill switch") : null);
  const k = kpis([
    ["Spend · " + rangeLabel(hours), usdBig(sm.cost_usd), [delta(sm.cost_usd, prev.cost_usd), prev.cost_usd ? " vs prior " + rangeLabel(hours) : "no prior traffic"]],
    ["Requests", compact(sm.requests), [delta(sm.requests, prev.requests), " · " + (sm.requests / (hours * 3600)).toFixed(2) + " rps avg"]],
    ["Tokens", compact(sm.tokens), sm.errors ? `${num(sm.errors)} errors · ${num(sm.blocked)} blocked` : "no errors"],
    ["Budget used · " + new Date().toLocaleString("en-US", { month: "short" }), cap ? [pct(capSpent, cap), el("span", { class: "dim" }, "%")] : el("span", { class: "dim" }, "—"),
      cap ? `${usd(capSpent, 0)} of ${usd(cap, 0)} project caps` : `${usd(monthSpend)} this month · no monthly project cap`],
  ]);
  const buckets = bucketRange(ser.since, ser.now, ser.bucket_seconds);
  const chart = card("Spend by provider", lineChart(seriesPoints(ser), { buckets, bucketSeconds: ser.bucket_seconds, height: 150 }), { sub: "USD / " + (ser.bucket_seconds >= 86400 ? "day" : "hour") });
  const mt = {}; for (const p of models.series) { const m = (mt[p.name] ||= { cost: 0, req: 0 }); m.cost += p.cost_usd; m.req += p.requests; }
  const tot = Object.values(mt).reduce((a, m) => a + m.cost, 0);
  const rows = Object.entries(mt).sort((a, b) => b[1].cost - a[1].cost || b[1].req - a[1].req).slice(0, 6).map(([name, m]) => {
    const pr = priceOf(P, name); const share = pct(m.cost, tot);
    return el("tr", { class: "click", onclick: () => (location.hash = "#requests?q=" + encodeURIComponent("model:" + name)) }, el("td", {}, modelChip(name)), el("td", { class: "r m" }, num(m.req)),
      el("td", { class: "r m" }, pr ? `${usd(pr.input)} · ${usd(pr.output)}` : el("span", { class: "mut", title: "Alias or model without a price-table entry" }, "—")),
      el("td", { class: "r m" }, usd(m.cost)), el("td", { class: "m" }, share + "%", el("span", { class: "bar" }, el("i", { style: { width: share + "%" } }))));
  });
  const top = card("Top models", table(["Model", ">Requests", ">$ / 1M in · out", ">Cost", "Share"], rows, { cls: "tight", empty: empty("No requests in this window") }), { sub: "requested in last " + rangeLabel(hours), right: el("a", { class: "m mut", href: "#requests", style: { fontSize: "11px" } }, "View all →") });
  const rule = rules.data.find((x) => x.enabled);
  const policy = card("Routing policy", rule ? el("div", { class: "code" },
    el("span", { class: "c4" }, `# ${rules.data.length} rule${rules.data.length === 1 ? "" : "s"}, first match wins\n`), el("span", { class: "c3" }, "rule"), `: `, el("span", { class: "c2" }, JSON.stringify(rule.name)), "\n",
    "match: ", el("span", { class: "c5" }, JSON.stringify(rule.match)), "\n", "target: ", el("span", { class: "c2" }, JSON.stringify(rule.target)), "\n",
    rule.fallbacks.length ? ["fallbacks: ", el("span", { class: "c2" }, JSON.stringify(rule.fallbacks)), "\n"] : null, "scope: ", el("span", { class: "c1" }, rule.project_id ? projName(rule.project_id) : "all projects"))
    : empty("No routing rules yet", "Requests go to the model they ask for, with alias failover.", el("a", { class: "btn", href: "#policies" }, "Add a rule")), { right: rule ? el("span", { class: "st ok" }, "active") : null, sub: rule ? "priority " + rule.priority : "" });
  const feed = el("div", {}, liveCard("Live requests", logs.data));
  root.append(el("div", { class: "g split" }, el("div", { class: "g" }, k, chart, top), el("div", { class: "g" }, feed, policy)));
  poller = setInterval(async () => { if (Live.state === "live" || document.hidden || !$("modal").classList.contains("hidden")) return; const l = await soft("/logs?limit=11", null); if (l) feed.replaceChildren(liveCard("Live requests", l.data)); }, 5000);
};
function svgIconSmall(n) { const s = svgIcon(n); s.setAttribute("width", 13); s.setAttribute("height", 13); s.setAttribute("fill", "none"); s.setAttribute("stroke", "currentColor"); s.setAttribute("stroke-width", 2); return s; }

// Requests: free-text filter "agent:x model:y key:z customer:c project:p"
function parseFilter(q) {
  const f = {}; for (const m of String(q || "").matchAll(/(\w+):(\S+)/g)) f[m[1]] = m[2]; return f;
}
PAGES.requests = async (root, actions, crumb, r) => {
  if (r.id) return requestDetail(root, actions, crumb, r.id);
  const q = r.q.get("q") || "", view = r.q.get("view") || "all", page = +(r.q.get("p") || 0), per = 25;
  const f = parseFilter(q);
  const keyId = f.key ? (S.keys.find((k) => k.name === f.key || k.id === f.key || k.prefix === f.key) || {}).id || f.key : "";
  const projId = f.project ? (S.projects.find((p) => p.name === f.project || p.id === f.project) || {}).id || f.project : "";
  const params = new URLSearchParams({ limit: per, offset: page * per });
  if (f.agent) params.set("agent", f.agent); if (f.model) params.set("model", f.model); if (keyId) params.set("key_id", keyId);
  if (f.customer) params.set("customer_id", f.customer); if (projId) params.set("project_id", projId);
  if (view === "errors") params.set("errors", "1"); if (view === "guard") params.set("guard", "1");
  const [logs, ser] = await Promise.all([api("/logs?" + params), api("/series?hours=24&by=provider")]);
  const sm = ser.summary;
  actions.push(liveIndicator(), tryItButton(), el("span", { class: "btn", style: { cursor: "default" } }, "Last 24h"));
  const input = el("input", { class: "inp m", value: q, placeholder: "agent:planner model:openai/gpt-4o-mini customer:acme key:name project:name", style: { flex: 1, border: 0, background: "transparent", boxShadow: "none" } });
  const filterBar = el("form", { class: "card", style: { display: "flex", alignItems: "center", gap: "8px", padding: "4px 12px" }, onsubmit: (e) => { e.preventDefault(); setQuery({ q: input.value.trim(), p: "" }); } },
    el("span", { class: "m mut" }, "›"), input, el("span", { class: "kbd" }, "↵"));
  const exportCsv = () => download(`opengate-requests-${new Date().toISOString().slice(0, 10)}.csv`, toCSV([["id", "time", "project", "key", "agent", "customer", "model", "provider", "provider_model", "prompt_tokens", "completion_tokens", "cost_usd", "billed_usd", "latency_ms", "status", "error_code", "attempts", "route", "cache_hit", "guard"],
    ...logs.data.map((l) => [l.id, l.ts, projName(l.project_id), keyName(l.key_id), l.agent, l.customer_id, l.model, l.provider, l.provider_model, l.prompt_tokens, l.completion_tokens, l.cost_usd, l.billed_usd, l.latency_ms, l.status, l.error_code, l.attempts, l.route, l.cache_hit, l.guard])]));
  const rows = logs.data.map((l) => el("tr", { class: "click", "data-id": l.id, onclick: () => (location.hash = "#requests/" + l.id) },
    el("td", { class: "m t2" }, l.id), el("td", { class: "m mut" }, hhmmss(l.ts)), el("td", {}, l.agent || el("span", { class: "mut" }, keyName(l.key_id))), el("td", {}, l.customer_id || el("span", { class: "mut" }, "—")),
    el("td", {}, modelChip(l.model, modelSub(l))), el("td", { class: "r m" }, l.prompt_tokens + l.completion_tokens ? `${num(l.prompt_tokens)} / ${num(l.completion_tokens)}` : "– / –"),
    el("td", { class: "r m" }, l.cost_usd ? usd(l.cost_usd, 4) : "–"), el("td", { class: "r m" }, l.latency_ms ? ms(l.latency_ms) : "–"), el("td", {}, statusTag(l)),
    el("td", {}, el("span", { style: { display: "inline-flex", gap: "6px" } }, logTags(l)))));
  const pager = el("span", { class: "m mut", style: { fontSize: "11px", display: "flex", gap: "8px", alignItems: "center" } },
    logs.total ? `${page * per + 1}–${Math.min(logs.total, page * per + logs.data.length)} of ${num(logs.total)}` : "0 results",
    page > 0 ? el("button", { class: "btn sm", onclick: () => setQuery({ p: page - 1 || "" }) }, "←") : null,
    (page + 1) * per < logs.total ? el("button", { class: "btn sm", onclick: () => setQuery({ p: page + 1 }) }, "→") : null);
  root.append(el("div", { class: "g" },
    kpis([["Requests · 24h", num(sm.requests), [delta(sm.requests, ser.previous.requests), " · " + (sm.requests / 86400).toFixed(2) + " rps avg"]],
      ["Cost", usdBig(sm.cost_usd), sm.requests ? "avg " + usd(sm.cost_usd / sm.requests, 4) + " / req" : "no requests"],
      ["P50 latency", ser.latency.samples ? [num(Math.round(ser.latency.p50_ms)), el("small", {}, " ms")] : el("span", { class: "dim" }, "—"), ser.latency.samples ? "p95 " + ms(ser.latency.p95_ms) + " · successful, uncached" : "no successful requests"],
      ["Errors", sm.requests ? [((sm.errors / sm.requests) * 100).toFixed(1), el("span", { class: "dim" }, "%")] : el("span", { class: "dim" }, "—"), `${num(sm.errors)} errors · ${num(sm.blocked)} blocked by policy`]]),
    el("div", { style: { display: "flex", gap: "8px", alignItems: "stretch" } }, el("div", { style: { flex: 1 } }, filterBar),
      seg([["all", "All"], ["errors", "Errors"], ["guard", "Guardrail"]], view, (v) => setQuery({ view: v === "all" ? "" : v, p: "" })),
      el("button", { class: "btn", onclick: exportCsv, disabled: !logs.data.length, title: "Exports the rows on this page" }, "Export CSV")),
    card("Request log", table(["Request ID", "Time", "Agent", "Customer", "Model", ">Tokens", ">Cost", ">Latency", "Status", ""], rows,
      { cls: "tight", empty: empty(q || view !== "all" ? "No requests match this filter" : "No requests yet", q || view !== "all" ? "Clear the filter to see everything." : "Requests appear here as soon as traffic flows through the gateway.") }), { sub: "newest first", right: pager })));
};

async function requestDetail(root, actions, crumb, id) {
  const [l, P] = await Promise.all([api("/logs/" + encodeURIComponent(id)), pricing()]);
  const proj = S.projects.find((p) => p.id === l.project_id) || {};
  crumb.push(el("a", { href: "#requests" }, "Requests"), " / ", el("b", {}, l.id));
  const pretty = (s) => { try { return JSON.stringify(JSON.parse(s), null, 2); } catch { return s; } };
  const curl = () => {
    const path = l.endpoint === "embeddings" ? "/v1/embeddings" : l.endpoint === "responses" ? "/v1/responses" : "/v1/chat/completions";
    const body = l.request_body || JSON.stringify({ model: l.model, messages: [{ role: "user", content: "…" }] });
    return `curl ${location.origin}${path} \\\n  -H "Authorization: Bearer $OPENGATE_KEY" \\\n  -H "Content-Type: application/json" \\\n` + (l.customer_id ? `  -H "X-OpenGate-Customer: ${l.customer_id}" \\\n` : "") + `  -d '${body.replace(/'/g, "'\\''")}'`;
  };
  actions.push(statusTag(l), l.attempts > 1 ? st("rt", `Failover · ${l.attempts} attempts`) : null, el("button", { class: "btn", onclick: () => copy(curl(), "cURL") }, "Copy as cURL"));
  const tok = l.prompt_tokens + l.completion_tokens;
  const k = kpis([
    ["Cost", l.cost_usd ? usdBig(l.cost_usd) : el("span", { class: "dim" }, "$0"), l.billed_usd ? "billed " + usd(l.billed_usd, 4) : l.cache_hit ? "served from cache" : l.usage_estimated ? "usage estimated" : "not billed to a customer"],
    ["Latency", l.latency_ms ? [num(l.latency_ms), el("small", {}, " ms")] : el("span", { class: "dim" }, "—"), l.stream ? "streamed" : "non-streaming"],
    ["Tokens", tok ? num(tok) : el("span", { class: "dim" }, "0"), `${num(l.prompt_tokens)} in · ${num(l.cached_tokens)} cached · ${num(l.completion_tokens)} out`],
    ["Attempts", num(l.attempts), l.attempts > 1 ? "failed over to " + (l.provider || "—") : l.attempts === 0 ? "refused before any provider" : "first attempt succeeded"],
  ]);
  // Timeline built only from logged facts (per-attempt detail isn't stored).
  const t = [];
  t.push(["", "Received", `${l.endpoint} · key ${keyName(l.key_id)} · project ${proj.name || "—"}`, l.ts]);
  if (l.guard) t.push([l.guard_action === "blocked" ? "r" : "w", "Guardrail · " + l.guard, l.guard_action || "flagged"]);
  if (l.cache_hit) t.push(["", "Cache hit", "response served from the exact-match cache"]);
  if (l.route) t.push(["", "Routed · " + l.route.replace(/^rule:/, "rule "), `${l.model} → ${l.provider ? l.provider + "/" : ""}${l.provider_model || "?"}` + (l.baseline_cost_usd > l.cost_usd ? ` · saved ${usd(l.baseline_cost_usd - l.cost_usd, 4)}` : "")]);
  if (l.attempts > 1) t.push(["w", `${l.attempts - 1} failed attempt${l.attempts > 2 ? "s" : ""}`, "retried / failed over (per-attempt detail is not logged)"]);
  if (l.status < 400) t.push(["", `${l.provider || "provider"} · ${l.status}`, `${l.provider_model || l.model}${l.latency_ms ? " · " + ms(l.latency_ms) : ""}`]);
  else t.push([BLOCK_CODES.includes(l.error_code) ? "r" : "w", `${l.status} · ${ERR_LABEL[l.error_code] || l.error_code || "error"}`, l.error_message || ""]);
  if (l.status < 400) t.push(["", "Reconciled", `actual ${usd(l.cost_usd, 4)}${l.usage_estimated ? " (estimated usage)" : ""}`]);
  const timeline = card("What happened", el("div", {}, el("div", { class: "timeline" }, t.map(([c, a, b, ts]) => el("div", { class: "tl" }, el("span", { class: "d " + c }), el("div", {}, el("div", { class: "tt" }, a), el("div", { class: "ts" }, b)), el("span", { class: "tm" }, ts ? hhmmss(ts) : "")))),
    el("div", { class: "kvr", style: { borderTop: "1px solid var(--ln)" } }, el("span", {}, "OpenTelemetry trace export"), SOON())));
  const pr = priceOf(P, l.provider && l.provider_model ? l.provider + "/" + l.provider_model : l.model);
  const fresh = Math.max(0, l.prompt_tokens - l.cached_tokens);
  const line = (label, rate, n) => el("div", { class: "kvr" }, el("span", {}, `${label} · ${usd(rate)} / 1M`), el("span", {}, usd((n * rate) / 1e6, 4)));
  const cost = card("Cost breakdown", el("div", {},
    pr && tok ? [line("Prompt", pr.input, fresh), l.cached_tokens ? line("Cached", pr.cache_read ?? pr.input, l.cached_tokens) : null, line("Completion", pr.output, l.completion_tokens)] : el("div", { class: "empty" }, pr ? "No tokens were used." : "No price-table entry for this model."),
    kvr([["Provider cost (logged)", usd(l.cost_usd, 4)]]),
    l.billed_usd ? el("div", { class: "kvr", style: { background: "rgba(43,212,154,.04)" } }, el("span", {}, "Billed" + (l.customer_id ? " to " + l.customer_id : "") + (l.cost_usd ? ` · ${(l.billed_usd / l.cost_usd).toFixed(2)}× markup` : "")), el("span", { class: "grn" }, usd(l.billed_usd, 4))) : null),
    { sub: "priced from " + (S.cfg.pricing_source || "price table") });
  let tab = "request";
  const bodyBox = el("div");
  const drawBody = () => {
    const txt = tab === "request" ? l.request_body : l.response_body;
    bodyBox.replaceChildren(txt ? el("div", { class: "code wrap" }, pretty(txt)) : empty(proj.log_bodies ? "No " + tab + " body stored for this call" : "Body logging is off for project " + (proj.name || "—"),
      proj.log_bodies ? "Refused requests and streams may not carry a body." : "Turn it on per project in Team & settings → Security (stored up to the configured cap)."));
    tabs.querySelectorAll("button").forEach((b) => b.classList.toggle("a", b.dataset.t === tab));
  };
  const tabs = el("div", { class: "tabs" }, ["request", "response"].map((x) => el("button", { "data-t": x, onclick: () => { tab = x; drawBody(); } }, x[0].toUpperCase() + x.slice(1))));
  drawBody();
  const bodies = el("div", { class: "card" }, el("div", { class: "hd" }, tabs, el("div", { class: "r" }, el("span", { class: "m mut", style: { fontSize: "11px" } }, `${Math.round((S.cfg.body_log_max_bytes || 65536) / 1024)} KB cap · opt-in per project`))), bodyBox);
  const details = card("Details", el("div", {}, kvr([
    ["Provider", l.provider ? `${l.provider} / ${l.provider_model}` : "—"], ["Requested model", l.model], ["Route", l.route || "—"],
    ["Key", el("a", { href: "#keys", class: "m" }, keyName(l.key_id))], ["Agent", l.agent || "—"], ["Customer", l.customer_id || "—"], ["Project", proj.name || "—"],
    ["Endpoint", "/v1/" + (l.endpoint === "chat" ? "chat/completions" : l.endpoint)], ["Stream", l.stream ? "yes" : "no"], ["Guardrail", l.guard ? `${l.guard} · ${l.guard_action || "flagged"}` : "—"], ["Time", new Date(l.ts).toLocaleString()]])));
  const resp = card("Response", el("div", {}, kvr([["Status", String(l.status)], ["Error", l.error_code || "—"], ["Output", l.completion_tokens ? num(l.completion_tokens) + " tokens" + (l.stream ? " · streamed" : "") : "—"],
    ["Cache", l.cache_hit ? "hit" : l.cached_tokens ? `prompt prefix hit · ${num(l.cached_tokens)}` : "miss"], ["Usage", l.usage_estimated ? "estimated" : "reported by provider"]])));
  root.append(el("div", { class: "g split" }, el("div", { class: "g" }, k, el("div", { class: "g", style: { gridTemplateColumns: "1.25fr 1fr" } }, timeline, cost), bodies), el("div", { class: "g" }, details, resp)));
}

// ---------- keys ----------
function limitFor(scope, id) { return S.limits.find((l) => l.scope === scope && l.scope_id === id); }
function budgetOf(l) {
  if (!l) return null;
  if (l.daily_usd > 0) return { spent: l.daily_spent, cap: l.daily_usd, per: "daily" };
  if (l.monthly_usd > 0) return { spent: l.monthly_spent, cap: l.monthly_usd, per: "monthly" };
  if (l.total_usd > 0) return { spent: l.total_spent, cap: l.total_usd, per: "total" };
  return null;
}
function budgetCell(b) {
  if (!b) return el("span", { class: "mut" }, "No spend limit");
  const p = pct(b.spent, b.cap);
  return el("div", { style: { minWidth: "150px" } }, el("div", { style: { display: "flex", justifyContent: "space-between", gap: "8px", marginBottom: "5px" } }, el("span", { class: "m" }, usd(b.spent)), el("span", { class: "mut", style: { fontSize: "11.5px" } }, `of ${usd(b.cap)} ${b.per}`)), pb(p, pbClass(p)));
}
async function killKey(k, kill) {
  if (kill && !confirm(`Kill key ${k.name}? New requests get 403 and in-flight streams are aborted.`)) return;
  await api(`/keys/${k.id}/${kill ? "kill" : "unkill"}`, { method: "POST" }); toast(kill ? "Key killed" : "Key revived"); render();
}
PAGES.keys = async (root, actions) => {
  const ser = await api("/series?hours=24&by=key");
  const live = S.keys.filter((k) => !k.revoked);
  const killed = live.filter((k) => k.killed);
  const lastKill = (await soft("/audit?limit=50", { data: [] })).data.find((a) => a.action === "key.kill");
  const today = live.reduce((a, k) => a + (k.spend_today || 0), 0);
  actions.push(el("span", { class: "m mut", style: { fontSize: "11px" } }, "stored only as SHA-256 hashes"), can("member") ? el("button", { class: "btn gr", onclick: () => document.querySelector("#nk-name")?.focus() }, "+ New key") : null);
  const rows = live.map((k) => {
    const rl = limitFor("key", k.id);
    return el("tr", {}, el("td", {}, el("div", {}, k.name), el("div", { class: "m mut", style: { fontSize: "11px", marginTop: "2px" } }, keyLabel(k) + " · " + projName(k.project_id))),
      el("td", { class: "m" }, k.agent || el("span", { class: "mut" }, "—")), el("td", {}, budgetCell(budgetOf(rl))), el("td", { class: "r m" }, rl && rl.rpm ? num(rl.rpm) : "—"),
      el("td", {}, k.killed ? st("bk", "Killed") : st("ok", "Active")),
      el("td", { class: "r" }, can("member") ? el("span", { style: { display: "inline-flex", gap: "6px" } },
        k.killed ? el("button", { class: "btn sm", onclick: () => killKey(k, false) }, "Revive") : el("button", { class: "btn k sm", onclick: () => killKey(k, true) }, "Kill"),
        can("admin") ? el("button", { class: "btn sm", title: "Edit or revoke", onclick: () => keyModal(k) }, "⋯") : null) : null));
  });
  const chart = card("Spend by key", lineChart(seriesPoints(ser, "cost", keyName), { buckets: bucketRange(ser.since, ser.now, ser.bucket_seconds), height: 140 }), { sub: "USD / hour · 24h" });
  root.append(el("div", { class: "g split" },
    el("div", { class: "g" },
      kpis([["Active keys", num(live.length - killed.length), `across ${new Set(live.map((k) => k.project_id)).size} projects`],
        ["Killed", num(killed.length), lastKill ? `${keyName(lastKill.target)} · ${ago(lastKill.ts)}` : "none killed"],
        ["Spend today", usdBig(today), "UTC day · all keys"]]),
      card("Virtual keys", table(["Key", "Agent", "Budget", ">RPM", "Status", ""], rows, { cls: "tight", empty: empty("No keys yet", "Create a key on the right and use it as the API key in any OpenAI SDK.") })), chart),
    el("div", { class: "g" }, newKeyCard(), apiCard(), scopesCard(live))));
};
function keyModal(k) {
  const a = el("input", { class: "inp", value: (k.allowed_models || []).join(", "), placeholder: "all models" }), d = el("input", { class: "inp", value: (k.denied_models || []).join(", ") });
  const cust = el("input", { class: "inp", value: k.customer_id || "", placeholder: "bill all calls to this customer" }), mt = el("input", { class: "inp", type: "number", min: 0, value: k.max_tokens || "", placeholder: "none" });
  modal("Key · " + k.name, el("form", { class: "form", onsubmit: async (e) => { e.preventDefault(); await api("/keys/" + k.id, { method: "PATCH", body: { allowed_models: csv(a.value), denied_models: csv(d.value), customer_id: cust.value, max_tokens: +mt.value || 0 } }); closeModal(); toast("Key updated"); render(); } },
    el("div", { class: "hint" }, `${keyLabel(k)} · ${projName(k.project_id)} · created ${dateShort(k.created_at)} · lifetime spend ${usd(k.spend_total)}`),
    el("div", { class: "row2" }, el("label", {}, "Allowed models", a), el("label", {}, "Denied models", d)), el("div", { class: "row2" }, el("label", {}, "Customer", cust), el("label", {}, "Max tokens / request", mt)),
    el("div", { class: "acts2" }, el("button", { class: "btn k", type: "button", style: { marginRight: "auto" }, onclick: async () => { if (confirm(`Revoke ${k.name}? This cannot be undone.`)) { await api("/keys/" + k.id, { method: "DELETE" }); closeModal(); render(); } } }, "Revoke key"),
      el("button", { class: "btn", type: "button", onclick: closeModal }, "Cancel"), el("button", { class: "btn gr", type: "submit" }, "Save"))));
}
function newKeyCard() {
  if (!can("member")) return card("New key", empty("Viewers can't create keys", "Ask an admin for member access."));
  const f = { name: el("input", { class: "inp", id: "nk-name", required: true, placeholder: "support-bot · prod" }), agent: el("input", { class: "inp", placeholder: "support-bot" }),
    proj: el("select", { class: "inp" }, S.projects.map((p) => el("option", { value: p.id }, p.name))),
    daily: el("input", { class: "inp", type: "number", min: 0, step: "0.01", placeholder: "none" }), monthly: el("input", { class: "inp", type: "number", min: 0, step: "0.01", placeholder: "none" }),
    rpm: el("input", { class: "inp", type: "number", min: 0, placeholder: "none" }) };
  const out = el("div");
  const form = el("form", { class: "form pad", onsubmit: async (e) => {
    e.preventDefault();
    const r = await api("/keys", { method: "POST", body: { project_id: f.proj.value, name: f.name.value, agent: f.agent.value, limits: { daily_usd: +f.daily.value || 0, monthly_usd: +f.monthly.value || 0, rpm: +f.rpm.value || 0 } } });
    out.replaceChildren(card("Key created", el("div", {}, el("div", { class: "pad" }, el("div", { class: "t2", style: { fontSize: "12px", marginBottom: "10px" } }, "Shown once. Only the SHA-256 hash is stored."),
      el("div", { class: "copyline g" }, el("span", {}, r.key), el("button", { class: "btn sm", type: "button", onclick: () => copy(r.key, "Key") }, "Copy"))),
      kvr([["Daily limit", +f.daily.value ? usd(+f.daily.value) : "—"], ["Monthly limit", +f.monthly.value ? usd(+f.monthly.value) : "—"], ["Rate limit", +f.rpm.value ? f.rpm.value + " rpm" : "—"], ["Agent label", f.agent.value || "—"], ["Project", projName(f.proj.value)]])),
      { right: el("span", { class: "st ok" }, "new") }));
    form.reset(); toast("Key created; copy it now");
    const k = await api("/keys"); S.keys = k.data;
  } },
    el("label", {}, "Name", f.name), el("div", { class: "row2" }, el("label", {}, "Project", f.proj), el("label", {}, "Agent label", f.agent)),
    el("div", { class: "row2" }, el("label", {}, "Daily USD", f.daily), el("label", {}, "Monthly USD", f.monthly)), el("label", {}, "Rate limit (rpm)", f.rpm),
    el("button", { class: "btn gr", type: "submit" }, "Create key"));
  return el("div", { class: "g" }, out, card("New key", form, { sub: "plaintext shown once" }));
}
function apiCard() {
  const o = location.origin;
  return card("Create via API", el("div", { class: "code" }, el("span", { class: "c1" }, "curl"), ` -X POST ${o}/admin/api/keys \\\n  -H `, el("span", { class: "c2" }, '"Authorization: Bearer $OG_ADMIN"'), " \\\n  -d ",
    el("span", { class: "c2" }, `'{"project_id":"${(S.projects[0] || {}).id || "prj_…"}",\n      "name":"planner","agent":"planner",\n      "limits":{"daily_usd":20}}'`)), { sub: "bash" });
}
function scopesCard(live) {
  return card("Scopes", el("div", {}, kvr([["Projects", S.projects.map((p) => p.name).join(" · ") || "—"], ["Admin API", "enabled"], ["Key prefix", "og-"], ["Revoked keys", num(S.keys.length - live.length)], ["Rotation", "manual"]])));
}

// ---------- agents ----------
PAGES.agents = async (root, actions) => {
  const ser = await api("/series?hours=24&by=agent");
  actions.push(el("span", { class: "m mut", style: { fontSize: "11px" } }, "agent labels group keys and share one budget"));
  const groups = {};
  for (const k of S.keys.filter((k) => !k.revoked && k.agent)) (groups[k.project_id + "/" + k.agent] ||= { id: k.project_id + "/" + k.agent, name: k.agent, project: k.project_id, keys: [] }).keys.push(k);
  const buckets = bucketRange(ser.since, ser.now, ser.bucket_seconds);
  const by = {}; for (const p of ser.series) { const a = (by[p.name] ||= { cost: 0, req: 0, b: {} }); a.cost += p.cost_usd; a.req += p.requests; a.b[+new Date(p.bucket)] = (a.b[+new Date(p.bucket)] || 0) + p.requests; }
  const list = Object.values(groups).map((g) => {
    g.killed = g.keys.every((k) => k.killed); g.today = g.keys.reduce((a, k) => a + (k.spend_today || 0), 0);
    g.limit = budgetOf(limitFor("agent", g.id)); g.s = by[g.name] || { cost: 0, req: 0, b: {} }; return g;
  }).sort((a, b) => a.killed - b.killed || b.today - a.today);
  const running = list.filter((g) => !g.killed);
  const closest = list.filter((g) => g.limit).sort((a, b) => b.limit.spent / b.limit.cap - a.limit.spent / a.limit.cap)[0];
  const sharedLimits = (g) => { const k = g.keys.map((k) => budgetOf(limitFor("key", k.id))).filter(Boolean); return k.length ? `${k.length} key limit${k.length > 1 ? "s" : ""}` : "no agent limit · project budget applies"; };
  const killAgent = async (g, kill) => {
    if (kill && !confirm(`Kill all ${g.keys.length} key(s) of agent ${g.name}?`)) return;
    for (const k of g.keys) await api(`/keys/${k.id}/${kill ? "kill" : "unkill"}`, { method: "POST" });
    toast(kill ? "Agent killed" : "Agent revived"); render();
  };
  const cardFor = (g) => {
    const p = g.limit ? pct(g.limit.spent, g.limit.cap) : 0;
    return el("div", { class: "card agent-card" + (g.killed ? " killed" : "") },
      el("div", { class: "ah" }, el("span", { class: "ab" }, initials(g.name)), el("div", { style: { minWidth: 0 } }, el("div", {}, g.name), el("div", { class: "m mut", style: { fontSize: "11px" } }, `${projName(g.project)} · ${g.keys.length} key${g.keys.length > 1 ? "s" : ""}`)),
        el("span", { style: { marginLeft: "auto", display: "flex", gap: "6px", alignItems: "center" } }, g.killed ? st("bk", "Killed") : st("ok", "Running"),
          can("member") ? el("button", { class: "btn sm" + (g.killed ? "" : " k"), onclick: () => killAgent(g, !g.killed) }, g.killed ? "Revive" : "Kill") : null)),
      el("div", { style: { display: "flex", alignItems: "flex-end", justifyContent: "space-between" } },
        el("div", {}, el("div", { class: "kl" }, "Today"), el("div", { class: "kv2", style: { fontSize: "20px", fontWeight: 500, margin: "6px 0 2px" } }, usd(g.today)), el("div", { class: "m mut", style: { fontSize: "11px" } }, num(g.s.req) + " requests · 24h")),
        spark(buckets.slice(-14).map((b) => g.s.b[b] || 0), g.killed)),
      g.killed ? el("div", { class: "m mut", style: { fontSize: "11px" } }, "Killed · 403 on every call")
        : g.limit ? [el("div", { class: "m", style: { display: "flex", justifyContent: "space-between", fontSize: "11px", color: "var(--t2)" } }, el("span", {}, p + "%"), el("span", {}, `of ${usd(g.limit.cap, 0)} ${g.limit.per}`)), pb(p, pbClass(p))]
        : el("div", { class: "m mut", style: { fontSize: "11px", borderTop: "1px solid var(--ln)", paddingTop: "8px" } }, sharedLimits(g)));
  };
  const totalToday = list.reduce((a, g) => a + g.today, 0);
  root.append(el("div", { class: "g" },
    kpis([["Running", num(running.length), `${list.length - running.length} killed`], ["Spend today", usdBig(totalToday), "UTC day · labelled keys"],
      ["Requests · 24h", num(Object.values(by).reduce((a, x) => a + x.req, 0)), `across ${S.keys.filter((k) => !k.revoked && k.agent).length} labelled keys`],
      ["Closest to limit", closest ? [pct(closest.limit.spent, closest.limit.cap), el("span", { class: "dim" }, "%")] : el("span", { class: "dim" }, "—"), closest ? closest.name : "no agent limits set"]]),
    list.length ? el("div", { class: "g g3" }, list.map(cardFor)) : card(null, empty("No agents yet", "Give a key an agent label (e.g. support-bot) to group its spend and give it a shared budget.", el("a", { class: "btn", href: "#keys" }, "Create a labelled key"))),
    card("Spend by agent", lineChart(seriesPoints(ser), { buckets, height: 140 }), { sub: "USD / hour · 24h" }),
    el("div", { class: "roadmap" }, el("span", { class: "pill" }, "ROADMAP"), "Approval workflows for over-budget requests: let a human approve an agent's next $50 instead of a hard stop.", SOON())));
};

// ---------- customers ----------
const monthKey = (d) => d.toISOString().slice(0, 7);
PAGES.customers = async (root, actions, crumb, r) => {
  const now = new Date(), cur = monthKey(now), prevM = monthKey(new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth() - 1, 1)));
  const month = r.q.get("month") || cur;
  const mStart = new Date(month + "-01T00:00:00Z");
  const hours = Math.min(24 * 365, Math.ceil((Date.now() - mStart) / 3600e3) + 1);
  const [c, mk, sp, ser] = await Promise.all([api("/customers?month=" + month), soft("/billing/markups", { data: [] }), soft("/billing/stripe", { customers: [], enabled: false }), api(`/series?by=customer&bucket=day&hours=${hours}`)]);
  const mLabel = mStart.toLocaleString("en-US", { month: "long", timeZone: "UTC" });
  const custom = month !== cur && month !== prevM;
  actions.push(el("span", { class: "seg" }, el("button", { class: month === cur ? "a" : "", onclick: () => setQuery({ month: "" }) }, new Date(cur + "-01T00:00:00Z").toLocaleString("en-US", { month: "long", timeZone: "UTC" })),
    el("button", { class: month === prevM ? "a" : "", onclick: () => setQuery({ month: prevM }) }, new Date(prevM + "-01T00:00:00Z").toLocaleString("en-US", { month: "long", timeZone: "UTC" })),
    el("button", { class: custom ? "a" : "", onclick: () => { const m = prompt("Month (YYYY-MM)", month); if (/^\d{4}-\d{2}$/.test(m || "")) setQuery({ month: m }); } }, custom ? month : "Custom")),
    el("button", { class: "btn", onclick: async () => { const res = await api("/billing/export.csv?month=" + month, { raw: true }); if (!res.ok) return toast("Export failed", true); download(`opengate-customers-${month}.csv`, await res.text()); } }, "Monthly CSV"));
  const rows = c.data, cost = rows.reduce((a, x) => a + x.cost_usd, 0), billed = rows.reduce((a, x) => a + x.billed_usd, 0);
  const sync = {}; for (const s of sp.customers || []) sync[s.project_id + "/" + s.customer_id] = s;
  const stripeTag = (x) => {
    if (!x.stripe_customer_id) return st("mu", "No Stripe customer");
    if (!sp.enabled) return st("mu", "Stripe sync off", "Set billing.stripe in the config to push usage to Stripe");
    const s = sync[x.project_id + "/" + x.customer_id];
    return s && !String(s.synced_to).startsWith("0001") ? st("ok", "Meter synced", "synced to " + new Date(s.synced_to).toLocaleString()) : st("rt", "Sync pending");
  };
  const trs = rows.map((x) => el("tr", { class: "click", onclick: () => customerModal(x) },
    el("td", {}, el("span", { style: { display: "flex", alignItems: "center", gap: "10px" } }, el("span", { class: "mono", style: { width: "24px", height: "24px", background: MONO_BG[hash(x.customer_id) % MONO_BG.length] } }, initials(x.name || x.customer_id)),
      el("span", {}, el("div", {}, x.name || x.customer_id), el("div", { class: "m mut", style: { fontSize: "11px" } }, x.customer_id + " · " + x.project)))),
    el("td", { class: "r m" }, num(x.requests)), el("td", { class: "r m" }, usd(x.cost_usd)), el("td", { class: "r m" }, x.billed_usd ? usd(x.billed_usd) : el("span", { class: "mut" }, "Not billed")),
    el("td", { class: "r m" }, x.cost_usd && x.billed_usd ? (x.billed_usd / x.cost_usd).toFixed(2) + "×" : "—"), el("td", {}, stripeTag(x))));
  const buckets = bucketRange(mStart, ser.now, 86400).filter((b) => monthKey(new Date(b)) === month);
  const pts = seriesPoints(ser).filter((p) => monthKey(new Date(p.bucket)) === month);
  const def = mk.data.find((m) => !m.project_id && !m.customer_id && !m.model);
  const lastPush = (sp.customers || []).map((s) => +new Date(s.synced_to)).filter((t) => t > 0).sort((a, b) => b - a)[0];
  root.append(el("div", { class: "g split380" },
    el("div", { class: "g" },
      kpis([["AI cost · " + mLabel.slice(0, 3), usdBig(cost), "tagged via " + (c.header || "customer header")], ["Billed with markup", usdBig(billed), billed ? el("span", { class: "up" }, "+" + usd(billed - cost) + " spread") : "no markup billed"],
        ["Customers", num(rows.length), `${rows.filter((x) => x.stripe_customer_id).length} linked to Stripe`]]),
      card("Customers", table(["Customer", ">Requests", ">AI cost", ">Billed", ">Markup", "Stripe billing meter"], trs,
        { empty: empty("No customer-tagged traffic in " + mLabel, `Send ${c.header || "X-OpenGate-Customer"}: <id> with requests (projects must trust the header) or set a customer on a key.`) }), { sub: mLabel }),
      card("Daily AI cost · top customers", lineChart(pts, { buckets, bucketSeconds: 86400, height: 140, emptyTitle: "No customer spend this month" }), { sub: mLabel })),
    el("div", { class: "g" },
      card("Margin per customer", el("div", { class: "pad t2", style: { fontSize: "12.5px", lineHeight: 1.6 } }, "Stripe revenue minus AI cost, per customer. Planned, not built yet. Billed-with-markup totals on the left are real."), { right: SOON() }),
      card("Markup rules", el("div", {}, mk.data.length ? kvr([["Default", def ? markupLabel(def) : "none (billed at cost)"],
        ...mk.data.filter((m) => m !== def).slice(0, 5).map((m) => [[m.customer_id || null, m.project_id ? projName(m.project_id) : null, m.model || null].filter(Boolean).join(" · ") || "rule", markupLabel(m)]),
        ["Header", c.header || "X-OpenGate-Customer"]]) : empty("No markup rules", "Customers are billed at cost until you add one.")), { sub: "applied to billed_usd", right: can("admin") ? (teamOn("customer_billing") ? el("button", { class: "btn sm", onclick: () => markupModal(mk.data) }, "+ Rule") : st("vi", "Team plan")) : null }),
      card("Stripe sync", el("div", {}, kvr([["Status", sp.enabled ? st("ok", "enabled") : st("mu", "disabled in config")], ["Last push", lastPush ? ago(lastPush) : "never"], ["Meter event", sp.event_name || "—"],
        ["Unit", sp.unit || "—"], ["Interval", sp.interval_seconds ? Math.round(sp.interval_seconds / 60) + " min" : "—"], ["Linked customers", num((sp.customers || []).length)]])),
        { right: can("admin") && sp.enabled ? el("button", { class: "btn sm", onclick: async () => { await api("/billing/stripe/sync", { method: "POST" }); toast("Sync started"); render(); } }, "Sync now") : null }))));
};
const markupLabel = (m) => (1 + m.markup_pct / 100).toFixed(2) + "×" + (m.per_mtok_usd ? " + " + usd(m.per_mtok_usd) + "/1M tok" : "");
function markupModal(rules) {
  const cust = el("input", { class: "inp", placeholder: "any customer" }), model = el("input", { class: "inp", placeholder: "any model, e.g. openai/*" });
  const proj = el("select", { class: "inp" }, el("option", { value: "" }, "any project"), S.projects.map((p) => el("option", { value: p.id }, p.name)));
  const pctI = el("input", { class: "inp", type: "number", step: "0.1", value: "20" }), tok = el("input", { class: "inp", type: "number", step: "0.01", min: 0, value: "0" });
  modal("New markup rule", el("form", { class: "form", onsubmit: async (e) => {
    e.preventDefault();
    await api("/billing/markups", { method: "POST", body: { customer_id: cust.value.trim(), model: model.value.trim(), project_id: proj.value, markup_pct: +pctI.value || 0, per_mtok_usd: +tok.value || 0 } });
    closeModal(); toast("Markup rule added"); render();
  } }, el("div", { class: "row2" }, el("label", {}, "Customer", cust), el("label", {}, "Project", proj)), el("label", {}, "Model pattern", model),
    el("div", { class: "row2" }, el("label", {}, "Markup %", pctI), el("label", {}, "Per 1M tokens (USD)", tok)),
    el("div", { class: "hint" }, "billed = cost × (1 + markup%) + tokens ÷ 1M × fee. The most specific rule wins (customer > model > project). Billed amounts are pushed to Stripe (test mode) when billing.stripe is enabled."),
    rules.length ? el("div", { class: "hint" }, rules.length + " existing rule" + (rules.length === 1 ? "" : "s")) : null,
    el("div", { class: "acts2" }, el("button", { class: "btn", type: "button", onclick: closeModal }, "Cancel"), el("button", { class: "btn gr", type: "submit" }, "Add rule"))));
}
function customerModal(r) {
  if (!can("admin")) return;
  const name = el("input", { class: "inp", value: r.name || "" }), sid = el("input", { class: "inp", value: r.stripe_customer_id || "", placeholder: "cus_…" });
  modal("Customer " + r.customer_id, el("form", { class: "form", onsubmit: async (e) => {
    e.preventDefault(); await api(`/customers/${r.project_id}/${encodeURIComponent(r.customer_id)}`, { method: "PUT", body: { name: name.value, stripe_customer_id: sid.value } }); closeModal(); render();
  } }, el("label", {}, "Display name", name), el("label", {}, "Stripe customer", sid), el("div", { class: "hint" }, "Usage is reported to Stripe from the moment a Stripe customer ID is set (no backfill)."),
    el("div", { class: "acts2" }, el("button", { class: "btn", type: "button", onclick: closeModal }, "Cancel"), el("button", { class: "btn gr", type: "submit" }, "Save"))));
}

// ---------- budgets ----------
function limitModal(existing) {
  const l = existing || { scope: "key", scope_id: "", daily_usd: 0, monthly_usd: 0, total_usd: 0, rpm: 0 };
  const scope = el("select", { class: "inp", disabled: !!existing }, ["key", "agent", "project"].map((s) => el("option", { value: s, selected: s === l.scope }, s)));
  const target = el("select", { class: "inp", disabled: !!existing });
  const fill = () => {
    const s = scope.value;
    const opts = s === "key" ? S.keys.filter((k) => !k.revoked).map((k) => [k.id, `${k.name} (${keyLabel(k)})`]) : s === "project" ? S.projects.map((p) => [p.id, p.name])
      : [...new Set(S.keys.filter((k) => k.agent && !k.revoked).map((k) => k.project_id + "/" + k.agent))].map((id) => [id, id.split("/")[1] + " @ " + projName(id.split("/")[0])]);
    target.replaceChildren(...opts.map(([v, t]) => el("option", { value: v, selected: v === l.scope_id }, t)));
    if (existing && !opts.length) target.append(el("option", { value: l.scope_id }, l.label || l.scope_id));
  };
  scope.addEventListener("change", fill); fill();
  const inp = (v, step) => el("input", { class: "inp", type: "number", min: 0, step: step || "0.01", value: v || "", placeholder: "none" });
  const f = { d: inp(l.daily_usd), m: inp(l.monthly_usd), t: inp(l.total_usd), r: inp(l.rpm, "1") };
  modal(existing ? "Edit limit · " + (l.label || l.scope_id) : "New limit", el("form", { class: "form", onsubmit: async (e) => {
    e.preventDefault();
    await api("/limits", { method: "PUT", body: { scope: scope.value, scope_id: target.value, daily_usd: +f.d.value || 0, monthly_usd: +f.m.value || 0, total_usd: +f.t.value || 0, rpm: +f.r.value || 0 } });
    closeModal(); toast("Limit saved"); render();
  } }, el("div", { class: "row2" }, el("label", {}, "Scope", scope), el("label", {}, "Applies to", target)),
    el("div", { class: "row2" }, el("label", {}, "Daily USD", f.d), el("label", {}, "Monthly USD", f.m)), el("div", { class: "row2" }, el("label", {}, "Lifetime USD", f.t), el("label", {}, "Requests / min", f.r)),
    el("div", { class: "hint" }, "Checked before every request reaches a provider. Daily resets 00:00 UTC, monthly on the 1st (UTC)."),
    el("div", { class: "acts2" }, existing && can("admin") ? el("button", { class: "btn k", type: "button", style: { marginRight: "auto" }, onclick: async () => { await api(`/limits?scope=${l.scope}&scope_id=${encodeURIComponent(l.scope_id)}`, { method: "DELETE" }); closeModal(); render(); } }, "Remove") : null,
      el("button", { class: "btn", type: "button", onclick: closeModal }, "Cancel"), el("button", { class: "btn gr", type: "submit" }, "Save limit"))));
}
PAGES.budgets = async (root, actions, crumb, r) => {
  const scope = r.q.get("scope") || "all";
  const [blocked, ov, alerts] = await Promise.all([api("/series?hours=24&by=blocked"), api("/overview?days=14"), soft("/alerts", { configured: false, recent: [], thresholds: [] })]);
  if (can("admin")) actions.push(el("button", { class: "btn gr", onclick: () => limitModal() }, "+ New limit"));
  const ls = S.limits.filter((l) => scope === "all" || l.scope === scope.replace(/s$/, ""));
  const atRisk = S.limits.filter((l) => { const b = budgetOf(l); return b && b.spent / b.cap >= 0.75; });
  const budgetBlocks = blocked.series.filter((p) => p.name === "budget_exceeded").reduce((a, p) => a + p.requests, 0);
  const allBlocks = blocked.series.reduce((a, p) => a + p.requests, 0);
  const alertFor = (l) => alerts.recent.find((a) => a.scope === l.scope && a.scope_id === l.scope_id);
  const per = { daily: "daily · resets 00:00 UTC", monthly: "monthly · resets on the 1st UTC", total: "total · never resets" };
  const rows = ls.map((l) => {
    const b = budgetOf(l); const p = b ? pct(b.spent, b.cap) : 0; const a = alertFor(l);
    return el("div", { class: "kvr", style: { display: "grid", gridTemplateColumns: "1fr 260px 120px", gap: "16px", padding: "12px 16px", cursor: can("admin") ? "pointer" : "default" }, onclick: () => can("admin") && limitModal(l) },
      el("div", { style: { minWidth: 0 } }, el("div", { style: { display: "flex", gap: "8px", alignItems: "center", color: "var(--tx)" } }, l.label || l.scope_id, el("span", { class: "pill", style: { fontSize: "9.5px" } }, l.scope)),
        el("div", { class: "m mut", style: { fontSize: "11px", marginTop: "3px" } }, b ? per[b.per] : l.rpm ? `rate limit only · ${l.rpm} rpm` : "no caps set")),
      b ? el("div", {}, el("div", { class: "m", style: { display: "flex", justifyContent: "space-between", fontSize: "12px", marginBottom: "6px", color: "var(--tx)" } }, el("span", {}, usd(b.spent)), el("span", { class: "mut" }, "/ " + usd(b.cap))), pb(p, pbClass(p))) : el("span", { class: "mut" }, "—"),
      el("span", { style: { textAlign: "right" } }, !b ? "" : p >= 100 ? st("bk", "Limit hit") : a && p >= 75 ? st("rt", Math.round(a.threshold * 100) + "% alert sent") : el("span", { class: "m", style: { color: "var(--t2)", fontSize: "12.5px" } }, p + "%")));
  });
  const days = {}; for (const p of ov.series) { const k = +new Date(p.bucket); days[k] = (days[k] || 0) + p.cost_usd; }
  const dayBuckets = bucketRange(ov.since, new Date(), 86400);
  const projDaily = S.limits.filter((l) => l.scope === "project" && l.daily_usd > 0).reduce((a, l) => a + l.daily_usd, 0);
  root.append(el("div", { class: "g split" },
    el("div", { class: "g" },
      kpis([["Limits", num(S.limits.length), "keys, agents, projects"], ["At risk", num(atRisk.length), "≥ 75% used"], ["Blocked · 24h", num(budgetBlocks), `budget refusals · ${num(allBlocks)} blocked total`],
        ["Org spend · " + new Date().toLocaleString("en-US", { month: "short" }), usdBig(ov.month.cost_usd), "month to date (UTC)"]]),
      card("Limits", rows.length ? el("div", {}, rows) : empty(scope === "all" ? "No limits yet" : "No " + scope + " limits", "Cap spend per key, agent or project. Over-limit calls are refused before they reach a provider.", can("admin") ? el("button", { class: "btn gr", onclick: () => limitModal() }, "+ New limit") : null),
        { sub: "checked before every request reaches a provider", right: seg([["all", "All"], ["keys", "Keys"], ["agents", "Agents"], ["projects", "Projects"]], scope, (v) => setQuery({ scope: v === "all" ? "" : v })) }),
      card("Org spend vs daily limit", lineChart(dayBuckets.map((b) => ({ bucket: b, name: "spend", value: days[b] || 0 })).filter(() => ov.range.requests > 0),
        { buckets: dayBuckets, bucketSeconds: 86400, height: 150, ref: projDaily || 0, refLabel: "project daily caps " + usd(projDaily, 0) }), { sub: "last 14 days" })),
    el("div", { class: "g" },
      card("Alerts webhook", el("div", { class: "pad" }, el("div", { class: "mut", style: { fontSize: "12px", marginBottom: "8px" } }, "Budget threshold webhook (Slack-compatible)"),
        el("div", { class: "copyline" }, el("span", {}, alerts.configured ? "https://" + alerts.webhook_host + "/…" : "Not configured")),
        el("div", { class: "chips", style: { marginTop: "10px" } }, (alerts.thresholds || []).map((t) => el("span", { class: "chip" + (alerts.configured ? " on" : "") }, (alerts.configured ? "✓ " : "") + Math.round(t * 100) + "%"))),
        alerts.configured ? null : el("div", { class: "m mut", style: { fontSize: "11px", marginTop: "10px" } }, "Set alerts.webhook_url or OPENGATE_ALERT_WEBHOOK_URL."))),
      card("Recent alerts", alerts.recent.length ? el("div", {}, alerts.recent.slice(0, 6).map((a) => el("div", { class: "kvr", style: { alignItems: "flex-start" } },
        el("span", { style: { display: "flex", gap: "10px" } }, el("span", { class: "dot", style: { background: a.threshold >= 1 ? "var(--rd)" : "var(--am)", marginTop: "5px" } }),
          el("span", {}, el("div", { style: { color: "var(--tx)" } }, `${a.label} crossed ${Math.round(a.threshold * 100)}% of ${a.period} limit`), el("div", { class: "m mut", style: { fontSize: "11px" } }, ago(a.sent_at) + " · webhook"))), el("span", {}, ""))))
        : empty("No alerts sent yet", alerts.configured ? "Alerts fire when a limit crosses a threshold." : "Configure a webhook to get threshold alerts.")),
      card("Enforcement", el("div", {}, el("div", { class: "kvr" }, el("span", {}, el("div", {}, "Strict mode"), el("div", { class: "mut", style: { fontSize: "11px", marginTop: "2px" } }, "Refuse if worst case exceeds what's left · set in config")), el("span", { class: "tog" + (S.cfg.budgets && S.cfg.budgets.strict ? " on" : ""), title: "Read-only: budgets.strict in the config" })),
        kvr([["Budget sync", S.cfg.budgets && S.cfg.budgets.sync_interval !== "0s" ? "every " + S.cfg.budgets.sync_interval : "single instance"]]), el("div", { class: "kvr" }, el("span", {}, "Budget-aware downgrades"), SOON()))))));
};

// ---------- policies & kill switch ----------
const CAT_LABEL = { email: "Emails", phone: "Phone numbers", credit_card: "Cards (Luhn)", iban: "IBAN", ssn: "SSN", sin: "SIN", secret: "API keys & secrets" };
const FEATURES = [["background", "Background responses"], ["builtin_tools", "Hosted web / file search"], ["n", "n > 1"], ["store", "Stored responses"]];
function ruleText(m) {
  const p = [];
  if (m.models && m.models.length) p.push("model = " + m.models.join(" | "));
  if (m.min_prompt_tokens) p.push(`prompt ≥ ${num(m.min_prompt_tokens)} tok`);
  if (m.max_prompt_tokens) p.push(`prompt < ${num(m.max_prompt_tokens)} tok`);
  if (m.tags && m.tags.length) p.push("tag " + m.tags.join(" + "));
  if (m.agents && m.agents.length) p.push("agent = " + m.agents.join(" | "));
  if (m.est_cost_above_usd) p.push(`est. cost > ${usd(m.est_cost_above_usd)}`);
  return p.join(" and ") || "every request";
}
function ruleModal(r) {
  r = r || { name: "", priority: 100, enabled: true, match: {}, target: "", fallbacks: [], project_id: "" };
  const m = r.match || {}, i = (v, ph, t) => el("input", { class: "inp", value: v ?? "", placeholder: ph || "", type: t || "text" });
  const f = { name: i(r.name, "long prompts to Flash"), prio: i(r.priority, "", "number"), proj: el("select", { class: "inp" }, el("option", { value: "" }, "all projects"), S.projects.map((p) => el("option", { value: p.id, selected: p.id === r.project_id }, p.name))),
    models: i((m.models || []).join(", "), "openai/*, smart"), minp: i(m.min_prompt_tokens || "", "", "number"), maxp: i(m.max_prompt_tokens || "", "", "number"), tags: i((m.tags || []).join(", "), "batch"),
    agents: i((m.agents || []).join(", "), "triage"), cost: i(m.est_cost_above_usd || "", "0.05", "number"), target: i(r.target, "openai/gpt-4.1-mini or auto"), fb: i(r.fallbacks.join(", "), "anthropic/claude-haiku-4-5") };
  f.cost.step = "0.0001";
  modal(r.id ? "Edit routing rule" : "New routing rule", el("form", { class: "form", onsubmit: async (e) => {
    e.preventDefault();
    const body = { name: f.name.value, priority: +f.prio.value || 0, enabled: r.enabled, project_id: f.proj.value, target: f.target.value, fallbacks: csv(f.fb.value),
      match: { models: csv(f.models.value), min_prompt_tokens: +f.minp.value || 0, max_prompt_tokens: +f.maxp.value || 0, tags: csv(f.tags.value), agents: csv(f.agents.value), est_cost_above_usd: +f.cost.value || 0 } };
    await api(r.id ? "/routing/rules/" + r.id : "/routing/rules", { method: r.id ? "PUT" : "POST", body }); closeModal(); toast("Rule saved"); render();
  } }, el("div", { class: "row2" }, el("label", {}, "Name", f.name), el("label", {}, "Priority (low first)", f.prio)), el("label", {}, "Project", f.proj),
    el("div", { class: "row2" }, el("label", {}, "When model is", f.models), el("label", {}, "Agents", f.agents)), el("div", { class: "row2" }, el("label", {}, "Prompt ≥ tokens", f.minp), el("label", {}, "Prompt < tokens", f.maxp)),
    el("div", { class: "row2" }, el("label", {}, "Tags (all)", f.tags), el("label", {}, "Est. cost above $", f.cost)), el("div", { class: "row2" }, el("label", {}, "Route to", f.target), el("label", {}, "Fallbacks", f.fb)),
    el("div", { class: "acts2" }, r.id ? el("button", { class: "btn k", type: "button", style: { marginRight: "auto" }, onclick: async () => { if (confirm("Delete rule " + r.name + "?")) { await api("/routing/rules/" + r.id, { method: "DELETE" }); closeModal(); render(); } } }, "Delete") : null,
      el("button", { class: "btn", type: "button", onclick: closeModal }, "Cancel"), el("button", { class: "btn gr", type: "submit" }, "Save rule"))));
}
PAGES.policies = async (root, actions, crumb, r) => {
  const proj = S.projects.find((p) => p.id === r.q.get("project")) || S.projects.find((p) => p.keys > 0) || S.projects[0];
  if (!proj) { root.append(card(null, empty("No projects", "Create a project via the admin API to get started."))); return; }
  const [rules, blocked, guards] = await Promise.all([api("/routing/rules"), api("/series?hours=24&by=blocked"), soft("/guardrails", { categories: Object.keys(CAT_LABEL), projects: [] })]);
  const pkeys = S.keys.filter((k) => k.project_id === proj.id && !k.revoked);
  actions.push(el("select", { class: "inp", style: { padding: "5px 26px 5px 10px" }, onchange: (e) => setQuery({ project: e.target.value }) }, S.projects.map((p) => el("option", { value: p.id, selected: p.id === proj.id }, "Project · " + p.name))));
  const isAdmin = can("admin"), isMember = can("member");
  const killProj = async (kill) => {
    if (kill && !confirm(`Kill project ${proj.name}? Every key in it gets 403 and in-flight streams are aborted.`)) return;
    await api(`/projects/${proj.id}/${kill ? "kill" : "unkill"}`, { method: "POST" }); toast(kill ? "Project killed" : "Project revived"); render();
  };
  const killbox = el("div", { class: "card killbox" }, el("div", { class: "kh" }, el("span", { class: "ki" }, svgIconSmall("kill")), el("b", { style: { fontWeight: 500 } }, "Kill switch"),
    el("span", { class: "m mut", style: { fontSize: "11px" } }, `${proj.name} · ${pkeys.length} key${pkeys.length === 1 ? "" : "s"} · ${pkeys.filter((k) => k.killed).length} killed`), proj.killed ? st("bk", "Project killed", "") : null),
    el("div", { class: "big" }, proj.killed ? "Every request in this project is refused." : "Stop every request in this project, now."),
    el("div", { class: "t2", style: { fontSize: "12.5px" } }, "New requests get 403. In-flight streams are aborted here, and on other replicas at their next budget sync."),
    el("div", { style: { display: "flex", gap: "8px", marginTop: "14px" } }, isMember ? (proj.killed ? el("button", { class: "btn gr", onclick: () => killProj(false) }, "Revive project") : el("button", { class: "btn kf", onclick: () => killProj(true) }, "Kill project")) : el("span", { class: "mut" }, "Members and above can use the kill switch."),
      isMember ? el("a", { class: "btn", href: "#keys" }, "Kill one key…") : null));
  const ordered = rules.data.slice().sort((a, b) => a.priority - b.priority);
  const ruleList = card("Routing rules", ordered.length ? el("div", { class: "rules" }, ordered.map((x, i) => el("div", { class: "rr", style: { opacity: x.enabled ? 1 : 0.5, cursor: isAdmin ? "pointer" : "default" }, onclick: () => isAdmin && ruleModal(x) },
    el("span", { class: "ix" }, i + 1), el("span", {}, ruleText(x.match), el("span", { class: "mut", style: { fontSize: "11px", marginLeft: "8px" } }, x.name + (x.project_id ? " · " + projName(x.project_id) : ""))),
    isAdmin ? el("button", { class: "tog" + (x.enabled ? " on" : ""), title: x.enabled ? "Disable" : "Enable", onclick: async (e) => { e.stopPropagation(); await api("/routing/rules/" + x.id, { method: "PUT", body: { ...x, enabled: !x.enabled } }); render(); } }) : el("span"),
    el("span", { class: "flow" }, "→ ", x.target === "auto" ? el("span", { class: "m" }, "cheapest that qualifies") : modelChip(x.target)))))
    : empty("No routing rules", "Requests go to the model they ask for. Add rules to downgrade, tag-route or cap per-request cost."),
    { sub: "top to bottom, first match wins", right: isAdmin ? el("button", { class: "btn sm", onclick: () => ruleModal() }, "+ Add rule") : null });
  const group = (c) => (["key_killed", "project_killed", "killed", "killed_mid_stream"].includes(c) ? "kill switch" : c === "budget_exceeded" ? "budget" : c === "rate_limit_exceeded" ? "rate limit" : "policy");
  const blk = card("Blocked by reason", lineChart(blocked.series.map((p) => ({ bucket: p.bucket, name: group(p.name), value: p.requests })),
    { buckets: bucketRange(blocked.since, blocked.now, blocked.bucket_seconds), height: 120, max: 4, fill: false, colors: ["#E8B24A", "#B58CF5", "#F06A6A", "#7AA2F7"], fmt: (v) => num(v), emptyTitle: "Nothing blocked in the last 24h", emptyText: "Budget, policy and kill-switch refusals show up here." }), { sub: "requests · 24h" });
  // guardrails
  const gp = guards.projects.find((g) => g.project_id === proj.id) || { effective: proj.guardrails || { pii: "off", injection: "off" } };
  const g = { pii: "off", injection: "off", responses: false, categories: [], ...(gp.effective || {}) };
  const cats = g.categories && g.categories.length ? g.categories : guards.categories;
  const saveG = async (patch) => { await api(`/projects/${proj.id}/guardrails`, { method: "PUT", body: { pii: g.pii, injection: g.injection, responses: g.responses, categories: g.categories || [], ...patch } }); toast("Guardrails saved"); render(); };
  const guardCard = card("Guardrails", el("div", {},
    el("div", { class: "pad chips" }, guards.categories.map((c) => { const on = g.pii !== "off" && cats.includes(c); return el(isAdmin ? "button" : "span", { class: "chip" + (on ? " on" : ""), onclick: isAdmin ? () => { const set = new Set(cats); set.has(c) ? set.delete(c) : set.add(c); saveG({ categories: [...set], pii: g.pii === "off" ? "redact" : g.pii }); } : null }, (on ? "✓ " : "") + (CAT_LABEL[c] || c)); })),
    el("div", { class: "kvr" }, el("span", {}, "On match"), isAdmin ? seg([["off", "Off"], ["log", "Log"], ["redact", "Redact"], ["block", "Block"]], g.pii, (v) => saveG({ pii: v })) : el("span", {}, g.pii)),
    el("div", { class: "kvr" }, el("span", {}, "Scan responses too"), el(isAdmin ? "button" : "span", { class: "tog" + (g.responses ? " on" : ""), onclick: isAdmin ? () => saveG({ responses: !g.responses }) : null })),
    el("div", { class: "kvr" }, el("span", {}, "Prompt-injection"), isAdmin ? seg([["off", "Off"], ["log", "Flag"], ["block", "Block"]], g.injection, (v) => saveG({ injection: v })) : el("span", {}, g.injection)),
    gp.override === false ? el("div", { class: "note" }, "Using server defaults; changing anything here sets a project override.") : null), { sub: "requests and responses" });
  const models = card("Model allow-list", el("div", {}, (proj.allowed_models.length || proj.denied_models.length)
    ? kvr([...proj.allowed_models.map((m) => [m, st("ok", "Allowed")]), ...proj.denied_models.map((m) => [m, st("bk", "Blocked")])]) : empty("Every model allowed", "No allow- or deny-list on this project."),
    isAdmin ? el("div", { class: "note" }, el("button", { class: "btn sm", onclick: () => modelListModal(proj) }, "Edit lists")) : null));
  const pl = limitFor("project", proj.id);
  const feats = new Set(proj.allowed_features || []);
  const lim = card("Limits & passthrough", el("div", {}, kvr([["Project rate limit", pl && pl.rpm ? pl.rpm + " rpm" : "—"], ["Max tokens / request", proj.max_tokens ? num(proj.max_tokens) : "—"],
    ...FEATURES.map(([k, label]) => [label, feats.has(k) ? st("ok", "Allowed") : st("bk", "Refused")]), ["Response cache TTL", proj.cache_ttl_seconds ? proj.cache_ttl_seconds + "s" : "off"]])));
  const blockedTotal = blocked.series.reduce((a, p) => a + p.requests, 0);
  root.append(el("div", { class: "g g2" },
    el("div", { class: "g" }, killbox, ruleList, blk, el("div", { class: "roadmap" }, el("span", { class: "pill" }, "ROADMAP"), "Approval workflows: a human approves the next $50 instead of a hard stop.", SOON())),
    el("div", { class: "g" }, kpis([["Blocked · 24h", num(blockedTotal), "budget, policy, kill"], ["Killed", num(S.keys.filter((k) => k.killed && !k.revoked).length + S.projects.filter((p) => p.killed).length), `${S.projects.filter((p) => p.killed).length} projects · ${S.keys.filter((k) => k.killed && !k.revoked).length} keys`]]),
      guardCard, el("div", { class: "g g2" }, models, lim))));
};
function modelListModal(p) {
  const a = el("input", { class: "inp", value: p.allowed_models.join(", "), placeholder: "smart, openai/*" }), d = el("input", { class: "inp", value: p.denied_models.join(", "), placeholder: "openai/gpt-5-pro" });
  modal("Model lists · " + p.name, el("form", { class: "form", onsubmit: async (e) => { e.preventDefault(); await api("/projects/" + p.id, { method: "PATCH", body: { allowed_models: csv(a.value), denied_models: csv(d.value) } }); closeModal(); render(); } },
    el("label", {}, "Allowed (empty = all)", a), el("label", {}, "Denied", d), el("div", { class: "hint" }, "Globs allowed, e.g. openai/*. Requests for other models get 403 model_not_allowed."),
    el("div", { class: "acts2" }, el("button", { class: "btn", type: "button", onclick: closeModal }, "Cancel"), el("button", { class: "btn gr", type: "submit" }, "Save"))));
}

// ---------- providers ----------
PAGES.providers = async (root, actions) => {
  const [p, pk, ser, auto, recent, P] = await Promise.all([api("/providers"), soft("/provider-keys", { data: [] }), api("/series?hours=24&by=provider"), soft("/routing/auto", { enabled: false, models: [] }), api("/logs?limit=500"), pricing()]);
  const by = {}; for (const x of ser.series) { const a = (by[x.name] ||= { req: 0, cost: 0, b: {} }); a.req += x.requests; a.cost += x.cost_usd; a.b[+new Date(x.bucket)] = (a.b[+new Date(x.bucket)] || 0) + x.requests; }
  const buckets = bucketRange(ser.since, ser.now, ser.bucket_seconds).slice(-12);
  const keysFor = (name) => pk.data.filter((k) => k.provider === name);
  const failovers = recent.data.filter((l) => l.attempts > 1);
  const routes = Object.entries(p.routes || {});
  const rows = p.data.map((x) => {
    const s = by[x.name] || { req: 0, cost: 0, b: {} }; const ks = keysFor(x.name);
    return el("tr", {}, el("td", {}, el("span", { style: { display: "flex", alignItems: "center", gap: "10px" } }, el("span", { class: "mono", style: { width: "24px", height: "24px", background: MONO_BG[hash(x.name) % MONO_BG.length] } }, x.name[0].toUpperCase()),
      el("span", {}, el("div", {}, x.name, el("span", { class: "pill", style: { marginLeft: "8px", fontSize: "9.5px" } }, x.type)), el("div", { class: "m mut", style: { fontSize: "11px" } }, x.base_url)))),
      el("td", { class: "r m" }, num(s.req)), el("td", {}, el("span", { style: { display: "inline-flex", alignItems: "center", gap: "10px" } }, spark(buckets.map((b) => s.b[b] || 0), !s.req), el("span", { class: "m", style: { fontSize: "12px" } }, usd(s.cost)))),
      el("td", {}, st("mu", x.key_source === "stored" ? `Key · AES-256 · ${ks.length}` : x.key_source === "env" ? "Key · env var" : x.key_source || "no key", ks.map((k) => k.label + " " + k.hint).join(", ") || undefined)));
  });
  const keyRows = pk.data.map((k) => el("div", { class: "kvr" }, el("span", {}, `${k.provider} · ${k.label || "key"}`), el("span", { style: { display: "flex", gap: "8px", alignItems: "center" } }, k.hint,
    can("admin") ? el("button", { class: "btn sm", onclick: async () => { if (confirm("Delete this provider key?")) { await api("/provider-keys/" + k.id, { method: "DELETE" }); render(); } } }, "Delete") : null)));
  const connect = () => {
    const prov = el("select", { class: "inp" }, p.data.map((x) => el("option", { value: x.name }, x.name))), label = el("input", { class: "inp", placeholder: "prod" }), key = el("input", { class: "inp", type: "password", required: true, autocomplete: "off", placeholder: "sk-…" });
    modal("Connect a provider key", el("form", { class: "form", onsubmit: async (e) => { e.preventDefault(); await api("/provider-keys", { method: "POST", body: { provider: prov.value, label: label.value, key: key.value } }); closeModal(); toast("Key stored (encrypted)"); render(); } },
      el("div", { class: "row2" }, el("label", {}, "Provider", prov), el("label", {}, "Label", label)), el("label", {}, "API key", key), el("div", { class: "hint" }, "Encrypted with AES-256-GCM using OPENGATE_MASTER_KEY; only the last 4 characters are shown again. New provider types are added in the config file."),
      el("div", { class: "acts2" }, el("button", { class: "btn", type: "button", onclick: closeModal }, "Cancel"), el("button", { class: "btn gr", type: "submit" }, "Store key"))));
  };
  const priced = [...new Set([...routes.flatMap(([, t]) => t), ...(auto.models || []).map((m) => m.model)])].filter((m) => priceOf(P, m)).slice(0, 6);
  root.append(el("div", { class: "g split380" },
    el("div", { class: "g" },
      kpis([["Connected", num(p.data.length), `${pk.data.length} stored key${pk.data.length === 1 ? "" : "s"} · ${p.data.filter((x) => x.key_source === "env").length} from env`],
        ["Failovers", num(failovers.length), `in the last ${num(recent.data.length)} requests`], ["Models priced", num(Object.keys(P).length + (P._patterns || []).length), `entries · via ${routes.length + (auto.enabled ? 1 : 0)} alias${routes.length + (auto.enabled ? 1 : 0) === 1 ? "" : "es"}`]]),
      card("Providers", table(["Provider", ">Requests 24h", "Spend 24h", "Credentials"], rows, { cls: "tight", empty: empty("No providers configured") }), { sub: "OpenAI-compatible · translated both ways", right: can("admin") ? el("button", { class: "btn sm", onclick: connect }, "+ Connect") : null }),
      card("Aliases with ordered failover", routes.length || auto.enabled ? el("div", {}, routes.map(([a, t]) => el("div", { class: "alias" }, el("span", { class: "an" }, a), el("span", { class: "flow" }, "→", t.map((m, i) => [i ? el("span", {}, "then") : null, el("span", { class: "mdl" }, modelChip(m))])))),
        auto.enabled ? el("div", { class: "alias" }, el("span", { class: "an" }, auto.alias || "auto"), el("span", { class: "flow" }, "→ cheapest model meeting the difficulty tier · " + auto.models.length + " candidates")) : null)
        : empty("No aliases", "Add routes in the config file to give clients stable names like smart or cheap."), { sub: "config: routes" }),
      pk.data.length ? card("Stored provider keys", el("div", {}, keyRows), { sub: "AES-256-GCM at rest" }) : null),
    el("div", { class: "g" },
      card("Retries", el("div", {}, kvr([["Max attempts per target", String(p.retry.max_attempts)], ["Initial backoff", p.retry.initial_backoff], ["Max backoff", p.retry.max_backoff], ["Retry on", "429 · 5xx · timeout"]]))),
      card("Pricing table", priced.length ? el("div", { class: "code" }, el("span", { class: "c4" }, `# ${S.cfg.pricing_source || "pricing"} · USD per 1M tokens\n`), priced.map((m) => [el("span", { class: "c3" }, m + ":"), ` { in: `, el("span", { class: "c5" }, priceOf(P, m).input.toFixed(2)), `, out: `, el("span", { class: "c5" }, priceOf(P, m).output.toFixed(2)), ` }\n`]))
        : empty("No priced models in your routes"), { sub: "per 1M tokens in / out", right: me.kind === "token" ? el("button", { class: "btn sm", onclick: async () => { await api("/pricing/reload", { method: "POST" }); pricingCache = null; toast("Pricing reloaded"); render(); } }, "Reload") : null }),
      card("Learned routing", el("div", { class: "pad t2", style: { fontSize: "12.5px", lineHeight: 1.6 } }, "Quality tiers informed by your own evals and feedback.", el("div", { class: "mut" }, "Also planned: distillation of opted-in traffic into smaller models.")), { right: SOON() }),
      card("Recent failovers", failovers.length ? el("div", {}, failovers.slice(0, 6).map((l) => el("div", { class: "kvr", style: { cursor: "pointer" }, onclick: () => (location.hash = "#requests/" + l.id) }, el("span", {}, `${l.model} → ${l.provider ? l.provider + "/" + l.provider_model : "failed"}`), el("span", {}, `${l.attempts} attempts · ${ago(l.ts)}`))))
        : empty("No failovers recently", "Requests that needed a retry or fallback show up here."), { sub: "last " + num(recent.data.length) + " requests" }))));
};

// ---------- setup ----------
PAGES.setup = async (root, actions) => {
  const logs = await api("/logs?limit=1");
  const base = location.origin + "/v1";
  const key = S.keys.find((k) => !k.revoked && !k.killed);
  const hasLimit = S.limits.length > 0, hasReq = logs.total > 0;
  actions.push(el("a", { class: "btn", href: "https://github.com/saief1/opengate#readme", target: "_blank", rel: "noopener" }, "Docs ↗"));
  const c = (t, k) => el("span", { class: "c" + k }, t);
  const SNIP = {
    Python: () => [c("from", 1), " openai ", c("import", 1), " ", c("OpenAI", 5), "\n", c("import", 1), " os\n\nclient = ", c("OpenAI", 5), "(\n    base_url=", c(`"${base}"`, 2), ",\n    api_key=os.environ[", c('"OPENGATE_KEY"', 2), "],  ", c("# og-…", 4), "\n)\n\nclient.chat.completions.create(\n    model=", c('"smart"', 2), ",  ", c("# or any provider/model", 4), "\n    messages=[{", c('"role"', 3), ": ", c('"user"', 2), ", ", c('"content"', 3), ": ", c('"Hi"', 2), "}],\n    extra_headers={", c('"X-OpenGate-Customer"', 2), ": ", c('"acme-co"', 2), "},  ", c("# optional", 4), "\n)"],
    TypeScript: () => [c("import", 1), " OpenAI ", c("from", 1), " ", c('"openai"', 2), ";\n\n", c("const", 1), " client = ", c("new", 1), " ", c("OpenAI", 5), "({\n  baseURL: ", c(`"${base}"`, 2), ",\n  apiKey: process.env.", c("OPENGATE_KEY", 5), ",\n});\n\n", c("await", 1), " client.chat.completions.create({\n  model: ", c('"smart"', 2), ",\n  messages: [{ role: ", c('"user"', 2), ", content: ", c('"Hi"', 2), " }],\n}, { headers: { ", c('"X-OpenGate-Customer"', 2), ": ", c('"acme-co"', 2), " } });"],
    curl: () => [c("curl", 1), ` ${base}/chat/completions \\\n  -H `, c('"Authorization: Bearer $OPENGATE_KEY"', 2), " \\\n  -H ", c('"Content-Type: application/json"', 2), " \\\n  -d ", c(`'{"model":"smart","messages":[{"role":"user","content":"Hi"}]}'`, 2)],
    Docker: () => [c("# self-host: gateway + SQLite in one container", 4), "\n", c("git", 1), " clone https://github.com/saief1/opengate && ", c("cd", 1), " opengate\n", c("docker", 1), " compose up -d\n\n", c("# then point any OpenAI SDK at", 4), "\nbase_url = ", c('"http://localhost:8080/v1"', 2)],
  };
  let lang = "Python"; const code = el("div", { class: "code", style: { minHeight: "250px" } });
  const tabs = el("div", { class: "tabs" }, Object.keys(SNIP).map((l) => el("button", { "data-l": l, onclick: () => { lang = l; draw(); } }, l)));
  const draw = () => { code.replaceChildren(...SNIP[lang]()); tabs.querySelectorAll("button").forEach((b) => b.classList.toggle("a", b.dataset.l === lang)); };
  draw();
  const step = (done, cur, n, t, s) => el("div", { class: "sp" }, el("span", { class: "n" + (done ? " done" : cur ? " cur" : "") }, done ? "✓" : n), el("div", {}, el("div", { class: "tt", style: { color: done || cur ? "var(--tx)" : "var(--t2)" } }, t), el("div", { class: "ts" }, s)));
  const doneN = [!!key, !!key, hasReq, hasLimit].filter(Boolean).length;
  const tail = el("div");
  const drawTail = (l) => tail.replaceChildren(el("div", { class: "code" }, el("span", { class: "mut" }, "$ tail · " + location.host + "\n"),
    l ? [el("span", { class: "grn" }, "● "), `${hhmmss(l.ts)} ${l.model} ${l.status} ${num(l.prompt_tokens)} / ${num(l.completion_tokens)} tok ${usd(l.cost_usd, 4)}`] : [el("span", { class: "grn" }, "● "), "waiting for first request ", el("span", { class: "cursor" })]));
  drawTail(logs.data[0]);
  poller = setInterval(async () => { if (document.hidden) return; const l = await soft("/logs?limit=1", null); if (l) drawTail(l.data[0]); }, 4000);
  const first = logs.data[0];
  root.append(el("div", { class: "g split" },
    el("div", { class: "g" },
      el("div", {}, el("div", { class: "m grn", style: { fontSize: "11px", letterSpacing: ".06em", marginBottom: "8px" } }, "TWO MINUTES · ONE LINE CHANGED"), el("div", { class: "h1", style: { fontSize: "26px" } }, hasReq ? "Your gateway is live" : "Connect your first agent")),
      el("div", { class: "card" }, el("div", { class: "hd" }, tabs, el("div", { class: "r" }, el("button", { class: "btn sm", onclick: () => copy(code.textContent, "Snippet") }, "Copy"))), code),
      el("div", { class: "g g2" },
        card("Prefer self-hosting?", el("div", { class: "pad" }, el("div", { class: "t2", style: { marginBottom: "10px" } }, "Same binary, Apache-2.0, a single static Go executable."), el("div", { class: "copyline" }, el("span", {}, el("span", { class: "grn" }, "$ "), "docker compose up -d")))),
        card("Works with", el("div", { class: "pad chips" }, ["Python", "TypeScript", "Go", "cURL", "any OpenAI-compatible client"].map((x) => el("span", { class: "chip" }, x))))),
      card("Latest request", first ? el("div", { class: "code", style: { cursor: "pointer" }, onclick: () => (location.hash = "#requests/" + first.id) }, el("span", { class: "c4" }, "# the newest row in Requests\n"),
        `${hhmmss(first.ts)}  `, el("span", { class: "c2" }, first.model), first.provider_model ? " → " + first.provider_model : "", "  ", el("span", { class: first.status < 400 ? "c2" : "c5" }, String(first.status)), `  ${num(first.prompt_tokens)} / ${num(first.completion_tokens)} tok  `, el("span", { class: "c5" }, usd(first.cost_usd, 4)), `  ${ms(first.latency_ms)}`)
        : empty("No requests yet", "Your first call shows up here and in Requests."))),
    el("div", { class: "g" },
      card("Setup", el("div", { class: "steps" }, step(!!key, !key, 1, "Create a virtual key", key ? `${keyLabel(key)} · ${key.name}` : "Keys → New key"), step(hasReq, !!key && !hasReq, 2, "Swap the base URL", "Point any OpenAI SDK at OpenGate"),
        step(hasReq, !!key && !hasReq, 3, "Send your first request", hasReq ? num(logs.total) + " requests logged" : el("span", {}, "Waiting for traffic… ", el("span", { class: "gdot" }))), step(hasLimit, hasReq && !hasLimit, 4, "Set a budget", hasLimit ? S.limits.length + " limits active" : "Optional · per key, agent or project")), { sub: `${doneN} of 4` }),
      card("Base URL", el("div", { class: "pad" }, el("div", { class: "copyline" }, el("span", {}, base), el("button", { class: "btn sm", onclick: () => copy(base, "Base URL") }, "Copy")),
        el("div", { class: "m mut", style: { fontSize: "11px", marginTop: "10px", lineHeight: 1.7 } }, "Endpoints:", el("br"), "/v1/chat/completions · /v1/embeddings", el("br"), "/v1/responses · /v1/models"))),
      card("Live tail", tail, { right: el("span", { class: "gdot" }) }))));
};

// ---------- team & settings ----------
PAGES.team = async (root) => {
  const [m, inv, audit, org, billing] = await Promise.all([api("/members"), soft("/invites", { data: [] }), soft("/audit?limit=8", { data: [] }), api("/org"), S.authCfg.cloud ? soft("/billing", null) : null]);
  const plan = billing && billing.effective_plan;
  const pending = inv.data.filter((i) => String(i.accepted_at).startsWith("0001"));
  const email = el("input", { class: "inp", type: "email", required: true, placeholder: "name@company.com", style: { flex: 1 } });
  const roleOpts = teamOn("roles") ? ["viewer", "member", "admin"] : ["admin"];
  const role = el("select", { class: "inp", title: teamOn("roles") ? "" : "Member and read-only roles are part of the Team plan" }, roleOpts.map((r) => el("option", { value: r, selected: r === (teamOn("roles") ? "member" : "admin") }, ROLE_LABEL[r])));
  const invite = can("admin") ? el("form", { style: { display: "flex", gap: "8px", padding: "12px 16px", borderBottom: "1px solid var(--ln)" }, onsubmit: async (e) => {
    e.preventDefault(); const r = await api("/invites", { method: "POST", body: { email: email.value, role: role.value } });
    const link = location.origin + "/ui/#invite=" + r.token;
    modal("Invite created", el("div", { class: "form" }, el("div", { class: "t2" }, `Send this link to ${r.invite ? r.invite.email : email.value}. It works once and expires in ${S.cfg.auth ? S.cfg.auth.invite_ttl : "7 days"}.`), el("div", { class: "copyline g" }, el("span", {}, link), el("button", { class: "btn sm", onclick: () => copy(link, "Invite link") }, "Copy")),
      el("div", { class: "acts2" }, el("button", { class: "btn", onclick: () => { closeModal(); render(); } }, "Done"))));
  } }, email, role, el("button", { class: "btn gr", type: "submit" }, "Invite")) : null;
  const ROLE_ST = { owner: "ok", admin: "bl", member: "mu", viewer: "mu" };
  const rows = [...m.data.map((u) => el("tr", {}, el("td", {}, el("span", { style: { display: "flex", alignItems: "center", gap: "10px" } }, el("span", { class: "mono", style: { width: "24px", height: "24px", borderRadius: "50%", background: MONO_BG[hash(u.email) % MONO_BG.length] } }, initials(u.name || u.email)),
    el("span", {}, el("div", {}, u.name || u.email.split("@")[0]), el("div", { class: "m mut", style: { fontSize: "11px" } }, u.email)))),
    el("td", { class: "mut" }, me.user && me.user.id === u.id ? "Now" : ago(u.last_login_at)),
    el("td", {}, can("admin") && u.role !== "owner" ? el("select", { class: "inp", style: { padding: "3px 24px 3px 8px", fontSize: "11.5px" }, onchange: async (e) => { await api("/members/" + u.id, { method: "PATCH", body: { role: e.target.value } }); toast("Role updated"); render(); } },
      [...(teamOn("roles") || u.role === "viewer" ? ["viewer"] : []), ...(teamOn("roles") || u.role === "member" ? ["member"] : []), "admin", ...(can("owner") ? ["owner"] : [])].map((r) => el("option", { value: r, selected: r === u.role }, ROLE_LABEL[r]))) : st(ROLE_ST[u.role] || "mu", ROLE_LABEL[u.role] || u.role)),
    el("td", { class: "r" }, can("admin") && u.role !== "owner" ? el("button", { class: "btn sm", onclick: async () => { if (confirm("Remove " + u.email + "?")) { await api("/members/" + u.id, { method: "DELETE" }); render(); } } }, "Remove") : null))),
  ...pending.map((i) => el("tr", {}, el("td", {}, el("span", { class: "t2" }, i.email), " ", st("rt", "invited")), el("td", { class: "mut" }, "expires " + dateShort(i.expires_at)), el("td", {}, st("mu", ROLE_LABEL[i.role] || i.role)),
    el("td", { class: "r" }, can("admin") ? el("button", { class: "btn sm", onclick: async () => { await api("/invites/" + i.id, { method: "DELETE" }); render(); } }, "Revoke") : null)))];
  const auditRows = audit.data.map((a) => el("div", { class: "kvr" }, el("span", { style: { color: "var(--tx)" } }, el("b", { style: { fontWeight: 500 } }, a.actor_email || (a.actor_type === "token" ? "admin token" : a.actor_type)), " ",
    el("span", { class: "t2" }, a.action.replace(/\./g, " ")), " ", el("span", { class: "m grn", style: { fontSize: "12px" } }, a.target ? (keyById(a.target) ? keyById(a.target).name : a.target) : "")), el("span", { class: "mut" }, ago(a.ts))));
  const exportAudit = () => exportAuditCSV("");
  const toggleBodies = (p) => can("admin") ? el("button", { class: "tog" + (p.log_bodies ? " on" : ""), title: p.name, onclick: async () => { await api("/projects/" + p.id, { method: "PATCH", body: { log_bodies: !p.log_bodies } }); toast(`Body logging ${p.log_bodies ? "off" : "on"} for ${p.name}`); render(); } }) : el("span", { class: "tog" + (p.log_bodies ? " on" : "") });
  const seats = plan && plan.max_members ? [num(m.data.length), el("small", {}, " / " + plan.max_members)] : num(m.data.length);
  const pw = me.kind === "user" ? (() => { const cur = el("input", { class: "inp", type: "password", autocomplete: "current-password", placeholder: "current" }), nw = el("input", { class: "inp", type: "password", autocomplete: "new-password", minlength: 10, placeholder: "new (10+ chars)" });
    return el("form", { class: "kvr", style: { gap: "8px" }, onsubmit: async (e) => { e.preventDefault(); await api("/auth/password", { method: "POST", body: { current_password: cur.value, new_password: nw.value } }); toast("Password changed; other sessions signed out"); e.target.reset(); } },
      el("span", {}, "Password"), el("span", { style: { display: "flex", gap: "6px" } }, cur, nw, el("button", { class: "btn sm", type: "submit" }, "Change"))); })() : null;
  root.append(el("div", { class: "g split420" },
    el("div", { class: "g" },
      kpis([["Seats", seats, plan ? plan.name + " plan" : "self-hosted · no seat limit"], ["Projects", num(S.projects.length), plan ? (plan.max_projects ? `of ${plan.max_projects} on ${plan.name}` : "unlimited on " + plan.name) : "unlimited"], ["Pending invites", num(pending.length), "invite by email link"]]),
      card("Members", el("div", {}, invite, table(["Member", "Last active", "Role", ""], rows), el("div", { class: "note" }, "read-only views everything · member creates keys and pulls kill switches · admin manages budgets, policies, MCP and the org · owner manages billing and owners", teamOn("roles") ? null : [" · ", st("vi", "Team plan"), " unlocks member and read-only roles"])), { sub: `${m.data.length} member${m.data.length === 1 ? "" : "s"}` + (plan && plan.max_members ? ` of ${plan.max_members} seats` : "") }),
      card("Audit log", !teamOn("audit_log") ? teamLocked("Audit log is a Team feature", "Admin actions are still recorded; upgrade to view and export them.") : auditRows.length ? el("div", {}, auditRows) : empty("No audit events yet"),
        { right: teamOn("audit_log") ? el("span", { style: { display: "flex", gap: "6px" } }, el("button", { class: "btn sm", onclick: exportAudit }, "Export"), el("a", { class: "btn sm", href: "#audit" }, "Open")) : null })),
    el("div", { class: "g" },
      card("Organization", el("div", {}, kvr([["Name", org.name], ["ID", org.id], ["Your role", me.kind === "token" ? "operator (admin token)" : ROLE_LABEL[org.role] || org.role], ["Projects", S.projects.map((p) => p.name).join(" · ") || "—"], ["Database", S.cfg.database || "—"]])),
        { right: can("owner") ? el("button", { class: "btn sm", onclick: async () => { const n = prompt("Organization name", org.name); if (n && n !== org.name) { await api("/org", { method: "PATCH", body: { name: n } }); await loadMe(); render(); } } }, "Rename") : null }),
      card("Security", el("div", {},
        S.projects.map((p) => el("div", { class: "kvr" }, el("span", {}, "Request body logging", el("span", { class: "m mut", style: { fontSize: "11px", marginLeft: "8px" } }, p.name)), toggleBodies(p))),
        kvr([["Session idle timeout", S.cfg.auth ? S.cfg.auth.session_ttl.replace(/0m0s$/, "").replace(/m0s$/, "m") : "—"], ["Log retention", plan && plan.log_retention_days ? `${plan.log_retention_days} days · ${plan.name} plan` : "unlimited (self-hosted)"],
          ["Admin lockout", S.cfg.admin_lockout ? `${S.cfg.admin_lockout.max_failures} failures / ${S.cfg.admin_lockout.window}` : "—"]]),
        pw, el("div", { class: "kvr" }, el("span", {}, "SSO · OIDC / SAML"), SOON()), el("div", { class: "kvr" }, el("span", {}, "Two-factor auth"), SOON()), el("div", { class: "kvr" }, el("span", {}, "Master-key rotation"), SOON()),
        el("div", { class: "kvr" }, el("span", {}, "OpenTelemetry export"), SOON()), el("div", { class: "kvr" }, el("span", {}, "Approval workflows"), SOON())), { sub: "argon2id passwords · HttpOnly sessions" }))));
};

// ---------- plan & billing ----------
function planCard(p, current, cta) {
  return el("div", { class: "card plan" + (current ? " cur" : "") }, el("div", { style: { display: "flex", alignItems: "center" } }, el("span", { style: { fontSize: "14px" } }, p.name), current ? el("span", { class: "st ok", style: { marginLeft: "auto" } }, "Current plan") : null),
    el("div", { class: "pr" }, p.contact_sales ? "Custom" : usd(p.price_usd || 0, 0)), el("div", { class: "m mut", style: { fontSize: "11px", marginTop: "-6px" } }, p.contact_sales ? "contact sales" : p.price_usd ? `/ ${p.interval || "month"}${PRICING_DRAFT ? " · draft" : ""}` : "forever"),
    el("ul", {}, (p.features || []).map((f) => el("li", {}, f, /SSO/.test(f) ? [" ", SOON()] : null))), cta);
}
PAGES.billing = async (root, actions) => {
  if (PRICING_DRAFT) actions.push(el("span", { class: "ex" }, "Draft pricing · not final"));
  if (!S.authCfg.cloud) {
    const ov = await api("/overview?days=1");
    actions.push(el("span", { class: "m mut", style: { fontSize: "11px" } }, "self-hosted · no OpenGate bill"));
    root.append(el("div", { class: "g" },
      el("div", { class: "card", style: { padding: "18px" } }, el("div", { class: "kl" }, "Requests this month"), el("div", { style: { display: "flex", alignItems: "baseline", gap: "8px", margin: "8px 0 14px" } }, el("span", { style: { fontSize: "30px", fontWeight: 500, letterSpacing: "-.02em" } }, num(ov.month.requests)), el("span", { class: "t2" }, "no quota (self-hosted)")),
        el("div", { class: "g g3", style: { borderTop: "1px solid var(--ln)", paddingTop: "14px" } }, [["Seats", "unlimited"], ["Projects", num(S.projects.length) + " / ∞"], ["Retention", "unlimited"]].map(([a, b]) => el("div", {}, el("div", { class: "kl" }, a), el("div", { style: { fontSize: "18px", marginTop: "6px" } }, b))))),
      el("div", { class: "g g3" }, DRAFT_PLANS.map((p) => planCard(p, false, el("button", { class: "btn full", disabled: true, title: "OpenGate Cloud only" }, p.contact_sales ? "Contact sales" : "Cloud only")))),
      el("div", { class: "g g2" }, card("Invoices", empty("No invoices", "Self-hosted OpenGate never bills you. Provider costs go to your own provider accounts.")),
        el("div", { class: "card pad", style: { display: "flex", gap: "14px" } }, el("span", { class: "ki mono", style: { width: "32px", height: "32px", background: "rgba(43,212,154,.08)", border: "1px solid rgba(43,212,154,.25)", color: "var(--g2)" } }, "{}"),
          el("div", {}, el("div", { style: { fontSize: "13px", fontWeight: 500 } }, "Self-hosting stays free"), el("div", { class: "t2", style: { fontSize: "12.5px", marginTop: "4px", lineHeight: 1.5 } }, "The open-source core is Apache-2.0, with no phone-home and no seat limits. Cloud prices above are draft defaults and may change."))))));
    return;
  }
  const b = await api("/billing");
  const eff = b.effective_plan, u = b.usage;
  const go = async (path, body) => { const r = await api(path, { method: "POST", body }); location.href = r.url; };
  actions.push(el("span", { class: "m mut", style: { fontSize: "11px" } }, "OpenGate Cloud · billed through Stripe"));
  const banners = [];
  if (b.suspended) banners.push(el("div", { class: "banner bad" }, "Paid features are paused because of a payment dispute or fraud warning on this account. Contact support to restore your plan."));
  else if (b.status === "trialing" && !b.cancel_at_period_end) banners.push(el("div", { class: "banner" }, `Your ${eff.name} trial runs until ${dateShort(b.current_period_end)}. Add a card in the billing portal to keep it afterwards.`));
  else if (b.status === "past_due") banners.push(el("div", { class: "banner bad" }, "Your last payment failed. Stripe retries automatically; update your card in the billing portal."));
  else if (b.cancel_at_period_end) banners.push(el("div", { class: "banner" }, `Your subscription ends on ${dateShort(b.current_period_end)}.`));
  const quota = eff.max_requests_month, used = u.requests, p = quota ? pct(used, quota) : 0;
  const nextReset = new Date(Date.UTC(new Date().getUTCFullYear(), new Date().getUTCMonth() + 1, 1));
  const usage = el("div", { class: "card", style: { padding: "18px" } },
    el("div", { style: { display: "flex", alignItems: "flex-start" } }, el("div", {}, el("div", { class: "kl" }, "Requests this month"), el("div", { style: { display: "flex", alignItems: "baseline", gap: "6px", marginTop: "8px" } }, el("span", { style: { fontSize: "30px", fontWeight: 500, letterSpacing: "-.02em" } }, num(used)), el("span", { class: "t2" }, quota ? "of " + num(quota) : "· unlimited"))),
      el("div", { class: "m mut", style: { marginLeft: "auto", textAlign: "right", fontSize: "11px", lineHeight: 1.7 } }, "Resets " + dateShort(nextReset), el("br"), quota ? "Over quota → 429 plan_quota_exceeded" : "")),
    quota ? el("div", { style: { margin: "14px 0" } }, pb(p, pbClass(p))) : el("div", { style: { height: "14px" } }),
    el("div", { class: "g g3", style: { borderTop: "1px solid var(--ln)", paddingTop: "14px" } }, [["Seats", `${u.members}${eff.max_members ? " / " + eff.max_members : " / ∞"}`], ["Projects", `${u.projects}${eff.max_projects ? " / " + eff.max_projects : " / ∞"}`], ["Retention", eff.log_retention_days ? eff.log_retention_days + " days" : "—"]].map(([a, v]) => el("div", {}, el("div", { class: "kl" }, a), el("div", { style: { fontSize: "18px", marginTop: "6px" } }, v)))));
  const cta = (pl) => {
    if (pl.id === eff.id) return b.has_subscription && b.can_manage ? el("button", { class: "btn p full", onclick: () => go("/billing/portal") }, "Manage in Stripe portal") : el("button", { class: "btn full", disabled: true }, "Current plan");
    if (pl.contact_sales) return b.sales_url ? el("a", { class: "btn full", href: b.sales_url, target: "_blank", rel: "noopener" }, "Contact sales") : el("button", { class: "btn full", disabled: true }, "Contact sales");
    if (!b.can_manage) return el("button", { class: "btn full", disabled: true, title: "Only owners can change the plan" }, "Owners only");
    if (!pl.self_serve) return b.has_subscription ? el("button", { class: "btn full", onclick: () => go("/billing/portal") }, "Downgrade") : el("button", { class: "btn full", disabled: true }, "Included");
    if (b.has_subscription) return el("button", { class: "btn full", onclick: () => go("/billing/portal") }, "Change in portal");
    return el("button", { class: "btn gr full", onclick: (e) => { e.target.disabled = true; e.target.textContent = "Redirecting to Stripe…"; go("/billing/checkout", { plan: pl.id }).catch(() => { e.target.disabled = false; e.target.textContent = "Upgrade to " + pl.name; }); } }, "Upgrade to " + pl.name);
  };
  root.append(el("div", { class: "g" }, banners, usage, el("div", { class: "g g3" }, b.plans.map((pl) => planCard(pl, pl.id === eff.id, cta(pl)))),
    el("div", { class: "g g2" },
      card("Invoices", empty("Invoices live in Stripe", "Open the billing portal to download invoices and receipts.", b.has_customer && b.can_manage ? el("button", { class: "btn", onclick: () => go("/billing/portal") }, "Open billing portal") : null)),
      card("Payment", el("div", {}, kvr([["Status", b.status || "—"], ["Subscription", b.has_subscription ? "active in Stripe" : "none"], ["Renews", b.current_period_end && !String(b.current_period_end).startsWith("0001") ? dateShort(b.current_period_end) : "—"],
        ["Card & billing email", b.has_customer ? "managed in Stripe portal" : "—"]]))))));
};

// ---------- auth (13-signup) ----------
S.authCfg = {};
let invite = null;
function showAuth(mode) {
  clearInterval(poller);
  const cfg = S.authCfg;
  mode = mode || (invite ? "invite" : "signin");
  if (mode === "signup" && !cfg.allow_signup) mode = "signin";
  const f = { email: el("input", { class: "inp", type: "email", autocomplete: "username", required: mode !== "token" && mode !== "invite", placeholder: "you@company.com" }),
    pw: el("input", { class: "inp", type: "password", autocomplete: mode === "signin" ? "current-password" : "new-password", required: mode !== "token", minlength: mode === "signin" ? null : 10 }),
    name: el("input", { class: "inp", autocomplete: "name", placeholder: "Your name" }), org: el("input", { class: "inp", placeholder: "Acme Inc." }),
    token: el("input", { class: "inp", type: "password", autocomplete: "off", required: mode === "token", placeholder: "OPENGATE_ADMIN_TOKEN" }) };
  const err = el("div", { class: "err" });
  const plan = sessionStorage.getItem(PLAN_KEY);
  const submit = async (e) => {
    e.preventDefault(); err.textContent = "";
    const btn = e.target.querySelector("button[type=submit]"); btn.disabled = true;
    try {
      if (mode === "invite") { await api("/auth/invites/accept", { method: "POST", quiet: true, body: { token: invite.token, name: f.name.value, password: f.pw.value } }); invite = null; history.replaceState(null, "", "#overview"); }
      else if (mode === "token") { sessionStorage.setItem(TOKEN_KEY, f.token.value.trim()); try { await api("/auth/me", { quiet: true }); } catch { sessionStorage.removeItem(TOKEN_KEY); throw new Error("Invalid admin token."); } }
      else if (mode === "signup") {
        await api("/auth/signup", { method: "POST", quiet: true, body: { email: f.email.value, name: f.name.value, password: f.pw.value, org_name: f.org.value } });
        sessionStorage.removeItem(PLAN_KEY);
        if (plan && plan !== "free") { try { location.href = (await api("/billing/checkout", { method: "POST", quiet: true, body: { plan } })).url; return; } catch { location.hash = "#billing"; } }
      } else await api("/auth/login", { method: "POST", quiet: true, body: { email: f.email.value, password: f.pw.value } });
      await boot();
    } catch (x) { err.textContent = x.message; btn.disabled = false; }
  };
  const title = { signin: ["Sign in to your", "gateway."], signup: ["Create your", "gateway."], token: ["Operator", "access."], invite: ["Join", invite ? invite.org + "." : "your team."] }[mode];
  const lead = { signin: "Every AI call priced, logged and capped.", signup: cfg.cloud ? (plan && plan !== "free" ? `Start on Free, then continue to Stripe Checkout for ${plan}.` : "Free plan, no card. Every AI call priced, logged and capped.") : "Creates your account and a new organization.",
    token: "Use the OPENGATE_ADMIN_TOKEN. It stays in this tab's session storage.", invite: invite ? `Invited as ${invite.role} (${invite.email}). ` + (invite.existing_user ? "Confirm with your password." : "Choose a password to create your account.") : "" }[mode];
  const L = (t, i) => [el("label", {}, t), i];
  const fields = mode === "token" ? L("Admin token", f.token) : mode === "invite" ? [invite && !invite.existing_user ? L("Your name", f.name) : null, L("Password", f.pw)]
    : mode === "signup" ? [L("Work email", f.email), L("Your name", f.name), L("Password", f.pw), L("Organization", f.org)] : [L("Email", f.email), L("Password", f.pw)];
  const tabs = mode === "invite" ? null : el("div", { class: "tabs" }, [["signin", "Sign in"], cfg.allow_signup ? ["signup", "Create account"] : null, ["token", "Admin token"]].filter(Boolean).map(([m, t]) => el("button", { type: "button", class: m === mode ? "a" : "", onclick: () => showAuth(m) }, t)));
  const left = el("div", { class: "l" },
    el("div", { class: "lh" }, logoSvg(13), el("b", {}, "OpenGate"), el("span", { class: "alt" }, mode === "signup" ? ["Already have one? ", el("button", { onclick: () => showAuth("signin") }, "Sign in")] : cfg.allow_signup && mode !== "invite" ? ["New here? ", el("button", { onclick: () => showAuth("signup") }, "Create account")] : el("a", { href: "landing.html" }, "About OpenGate"))),
    el("form", { class: "f", onsubmit: submit }, el("h1", {}, title[0], el("br"), el("span", { class: "gt" }, title[1])), el("p", { class: "lead" }, lead), tabs, fields,
      el("button", { class: "bp", type: "submit" }, mode === "signup" ? "Create account →" : mode === "invite" ? "Join " + (invite ? invite.org : "") + " →" : "Sign in →"), err,
      mode !== "token" ? el("div", { class: "sso" }, "Continue with SSO", SOON()) : null,
      !cfg.allow_signup && mode === "signin" ? el("div", { class: "info" }, "Accounts are invite-only on this gateway. Ask an admin for an invite link, or use the admin token.") : null,
      el("div", { class: "self" }, "Prefer to run it yourself?", el("div", { class: "cmd" }, el("span", { class: "grn" }, "$ "), "docker compose up -d"))),
    el("div", { class: "foot" }, "Open-source core · Apache-2.0" + (cfg.version ? " · " + String(cfg.version).replace(/^v?/, "v") : "")));
  const sc = el("div", { class: "sc" },
    el("div", { class: "sh" }, el("span", { class: "dot", style: { width: "8px", height: "8px", background: "#F06A6A", boxShadow: "0 0 10px #F06A6A" } }), el("span", { class: "m", style: { fontSize: "12px" } }, "looping-agent · 03:12 UTC"), el("span", { class: "st bk", style: { marginLeft: "auto" } }, "Killed")),
    el("div", { class: "big" }, "Looping agent stopped at ", el("span", { class: "grn" }, "its $250 cap"), ",", el("br"), el("span", { class: "mut" }, "not at the end of the month.")),
    el("div", { style: { padding: "10px 20px 14px" } }, el("svg", { width: "100%", height: 90, viewBox: "0 0 400 90", preserveAspectRatio: "none" },
      el("defs", {}, el("linearGradient", { id: "rg", x1: 0, y1: 0, x2: 0, y2: 1 }, el("stop", { offset: 0, "stop-color": "#2BD49A", "stop-opacity": ".3" }), el("stop", { offset: 1, "stop-color": "#2BD49A", "stop-opacity": "0" }))),
      el("path", { d: "M0 85 L60 80 L120 70 L180 55 L240 30 L260 22 L400 22", fill: "none", stroke: "#2BD49A", "stroke-width": 1.6 }), el("path", { d: "M0 85 L60 80 L120 70 L180 55 L240 30 L260 22 L400 22 V90 H0Z", fill: "url(#rg)" }),
      el("path", { d: "M260 22 L400 -60", stroke: "rgba(240,106,106,.5)", "stroke-dasharray": "4 4", fill: "none" }), el("path", { d: "M0 22H400", stroke: "rgba(232,178,74,.5)", "stroke-dasharray": "3 4" }), el("circle", { cx: 260, cy: 22, r: 4, fill: "#F06A6A" }))),
    el("div", { style: { borderTop: "1px solid var(--ln)", padding: "12px 0" } },
      el("div", { class: "ln" }, el("span", { class: "mut" }, "03:12:04"), " budget check · key=og-… ", el("span", { class: "amb" }, "at limit")),
      el("div", { class: "ln" }, el("span", { class: "mut" }, "03:12:05"), " ", el("span", { class: "red" }, "403 budget_exceeded"), " · streams aborted"),
      el("div", { class: "ln" }, el("span", { class: "mut" }, "03:12:05"), " alert → webhook")),
    el("div", { class: "sf" }, "Hard budget hit · in-flight streams aborted", el("span", { class: "amb", style: { marginLeft: "auto" } }, "Example scenario")));
  $("root").replaceChildren(el("div", { class: "auth" }, left, el("div", { class: "r" }, el("div", { class: "gridbg" }), sc)));
  (mode === "token" ? f.token : mode === "invite" ? f.pw : f.email).focus();
}

// ---------- MCP tool gateway ----------
async function exportAuditCSV(qs) {
  const res = await api("/audit/export.csv" + (qs ? "?" + qs : ""), { raw: true });
  if (!res.ok) return toast(res.status === 402 ? "Audit export is a Team plan feature" : "Export failed", true);
  download("opengate-audit.csv", await res.text());
}
function mcpServerModal(existing) {
  const m = existing || { transport: "http", auth_header: "Authorization", default_rpm: 0, enabled: true };
  const name = el("input", { class: "inp m", value: m.name || "", required: true, disabled: !!existing, placeholder: "github", pattern: "[a-z0-9][a-z0-9_-]*" });
  const url = el("input", { class: "inp m", value: m.url || "", required: true, placeholder: "https://mcp.example.com/mcp" });
  const tr = el("select", { class: "inp" }, [["http", "Streamable HTTP"], ["sse", "HTTP + SSE (legacy)"]].map(([v, t]) => el("option", { value: v, selected: v === m.transport }, t)));
  const hdr = el("input", { class: "inp m", value: m.auth_header || "Authorization" });
  const secret = el("input", { class: "inp m", type: "password", autocomplete: "off", placeholder: existing && m.has_secret ? "stored " + m.secret_hint + " · leave blank to keep" : "e.g. Bearer ghp_… (optional)" });
  const rpm = el("input", { class: "inp", type: "number", min: 0, value: m.default_rpm || "", placeholder: "unlimited" });
  modal(existing ? "Edit MCP server · " + m.name : "Add MCP server", el("form", { class: "form", onsubmit: async (e) => {
    e.preventDefault();
    const body = { url: url.value.trim(), transport: tr.value, auth_header: hdr.value.trim(), default_rpm: +rpm.value || 0 };
    if (secret.value) body.secret = secret.value;
    if (existing) await api("/mcp/servers/" + m.id, { method: "PATCH", body }); else await api("/mcp/servers", { method: "POST", body: { ...body, name: name.value.trim() } });
    closeModal(); toast(existing ? "Server updated" : "Server added"); render();
  } }, el("div", { class: "row2" }, el("label", {}, "Name (URL slug)", name), el("label", {}, "Transport", tr)), el("label", {}, "Upstream URL", url),
    el("div", { class: "row2" }, el("label", {}, "Credential header", hdr), el("label", {}, "Default calls / min per agent+tool", rpm)), el("label", {}, "Upstream secret", secret),
    el("div", { class: "hint" }, "The secret is encrypted with the master key and injected server-side. Agents never see it and the API never returns it."),
    el("div", { class: "acts2" }, existing && m.has_secret ? el("button", { class: "btn k", type: "button", style: { marginRight: "auto" }, onclick: async () => { await api("/mcp/servers/" + m.id, { method: "PATCH", body: { clear_secret: true } }); closeModal(); render(); } }, "Clear secret") : null,
      el("button", { class: "btn", type: "button", onclick: closeModal }, "Cancel"), el("button", { class: "btn gr", type: "submit" }, existing ? "Save" : "Add server"))));
}
function mcpPolicyModal(servers, serverId) {
  const agents = [...new Set(S.keys.filter((k) => k.agent && !k.revoked).map((k) => k.agent))];
  const srv = el("select", { class: "inp" }, servers.map((x) => el("option", { value: x.id, selected: x.id === serverId }, x.name)));
  const agent = el("input", { class: "inp m", placeholder: "every agent", list: "og-agents" });
  const tool = el("input", { class: "inp m", required: true, placeholder: "search · delete_* · *" });
  const action = el("select", { class: "inp" }, el("option", { value: "allow" }, "Allow"), el("option", { value: "deny" }, "Deny"));
  const rpm = el("input", { class: "inp", type: "number", min: 0, placeholder: "server default" });
  modal("New tool rule", el("form", { class: "form", onsubmit: async (e) => {
    e.preventDefault(); await api("/mcp/policies", { method: "POST", body: { server_id: srv.value, agent: agent.value.trim(), tool: tool.value.trim(), action: action.value, rpm: +rpm.value || 0 } });
    closeModal(); toast("Rule added"); render();
  } }, el("datalist", { id: "og-agents" }, agents.map((a) => el("option", { value: a }))),
    el("div", { class: "row2" }, el("label", {}, "Server", srv), el("label", {}, "Agent", agent)), el("div", { class: "row2" }, el("label", {}, "Tool", tool), el("label", {}, "Action", action)),
    el("label", {}, "Calls / min (allow rules)", rpm),
    el("div", { class: "hint" }, "Most specific rule wins: a rule for one agent beats an all-agents rule, an exact tool beats a wildcard, deny beats allow at a tie. Denied tools are also hidden from tools/list."),
    el("div", { class: "acts2" }, el("button", { class: "btn", type: "button", onclick: closeModal }, "Cancel"), el("button", { class: "btn gr", type: "submit" }, "Add rule"))));
}
function mcpCredModal(server) {
  const agent = el("input", { class: "inp m", required: true, placeholder: "agent name (or key id)" }), hdr = el("input", { class: "inp m", value: server.auth_header || "Authorization" });
  const secret = el("input", { class: "inp m", type: "password", required: true, autocomplete: "off" });
  modal("Agent credential · " + server.name, el("form", { class: "form", onsubmit: async (e) => {
    e.preventDefault(); await api(`/mcp/servers/${server.id}/credentials`, { method: "PUT", body: { agent: agent.value.trim(), header: hdr.value.trim(), secret: secret.value } }); closeModal(); toast("Credential stored"); render();
  } }, el("div", { class: "row2" }, el("label", {}, "Agent", agent), el("label", {}, "Header", hdr)), el("label", {}, "Secret", secret),
    el("div", { class: "hint" }, "Overrides the server secret for this agent only (e.g. a per-agent GitHub token). Write-only."),
    el("div", { class: "acts2" }, el("button", { class: "btn", type: "button", onclick: closeModal }, "Cancel"), el("button", { class: "btn gr", type: "submit" }, "Save credential"))));
}
PAGES.mcp = async (root, actions, crumb, r) => {
  const tab = r.q.get("tab") || "servers", srvF = r.q.get("server") || "";
  const [sv, pol, calls] = await Promise.all([api("/mcp/servers"), api("/mcp/policies"), api("/mcp/calls?limit=50" + (r.q.get("errors") ? "&errors=1" : "") + (srvF ? "&server_id=" + srvF : ""))]);
  const servers = sv.data, byId = Object.fromEntries(servers.map((x) => [x.id, x]));
  actions.push(liveIndicator());
  if (can("admin")) actions.push(tab === "policies" && servers.length ? el("button", { class: "btn gr", onclick: () => mcpPolicyModal(servers, srvF) }, "+ Tool rule") : el("button", { class: "btn gr", onclick: () => mcpServerModal() }, "+ Add server"));
  const errs = calls.data.filter((c) => c.error_code).length;
  const lat = calls.data.filter((c) => !c.error_code).map((c) => c.latency_ms).sort((a, b) => a - b);
  const head = kpis([["MCP servers", num(servers.length), `${servers.filter((x) => x.enabled).length} enabled`], ["Tool rules", num(pol.data.length), pol.default_deny ? "default: deny" : "default: allow"],
    ["Tool calls", num(calls.total), "logged with redacted args"], ["Blocked / failed", num(errs), lat.length ? "p50 " + ms(lat[Math.floor(lat.length / 2)]) + " on success" : "last 50 calls"]]);
  const tabs = seg([["servers", "Servers"], ["policies", "Tool policies"], ["calls", "Call log"]], tab, (v) => setQuery({ tab: v === "servers" ? "" : v }));
  let body;
  if (tab === "servers") {
    const rows = servers.map((x) => el("tr", {},
      el("td", {}, el("span", { style: { display: "flex", alignItems: "center", gap: "10px" } }, monoFor(x.name), el("span", {}, el("div", {}, x.name), el("div", { class: "m mut", style: { fontSize: "11px" } }, x.url)))),
      el("td", { class: "m t2" }, x.gateway_path), el("td", {}, st("mu", x.transport === "sse" ? "HTTP+SSE" : "Streamable HTTP")),
      el("td", {}, x.has_secret ? st("ok", "Secret " + x.secret_hint) : st("mu", "No secret")), el("td", { class: "r m" }, x.default_rpm ? x.default_rpm + "/min" : "—"),
      el("td", {}, can("admin") ? el("button", { class: "tog" + (x.enabled ? " on" : ""), title: x.enabled ? "Disable" : "Enable", onclick: async () => { await api("/mcp/servers/" + x.id, { method: "PATCH", body: { enabled: !x.enabled } }); render(); } }) : el("span", { class: "tog" + (x.enabled ? " on" : "") })),
      el("td", { class: "r" }, can("admin") ? el("span", { style: { display: "inline-flex", gap: "6px" } }, el("button", { class: "btn sm", onclick: () => mcpCredModal(x) }, "Agent secret"), el("button", { class: "btn sm", onclick: () => mcpServerModal(x) }, "Edit"),
        el("button", { class: "btn sm", onclick: async () => { if (confirm(`Delete ${x.name} and its rules?`)) { await api("/mcp/servers/" + x.id, { method: "DELETE" }); render(); } } }, "Delete")) : null)));
    const origin = location.origin;
    body = el("div", { class: "g split380" },
      card("MCP servers", table(["Server", "Gateway URL", "Transport", "Credential", ">Default limit", "On", ""], rows, { empty: empty("No MCP servers yet", "Add an upstream MCP server; agents then connect to /mcp/<name> on this gateway with their OpenGate key.", can("admin") ? el("button", { class: "btn gr", onclick: () => mcpServerModal() }, "+ Add server") : null) })),
      el("div", { class: "g" },
        card("Connect an agent", el("div", { class: "pad t2", style: { fontSize: "12.5px", lineHeight: 1.6 } }, "Point your MCP client at the gateway and authenticate with the agent's virtual key:",
          el("pre", { class: "code" }, `{\n  "mcpServers": {\n    "${servers[0] ? servers[0].name : "github"}": {\n      "url": "${origin}/mcp/${servers[0] ? servers[0].name : "github"}",\n      "headers": { "Authorization": "Bearer og-…" }\n    }\n  }\n}`),
          "Kill switch, budgets and request rate limits of the key apply to tool calls; upstream secrets stay on the server.")),
        card("What OpenGate enforces", el("div", {}, kvr([["Kill switch", st("ok", "key + project")], ["Budgets", st("ok", "over-budget agents can't call tools")], ["Allow / deny", st("ok", "per agent, wildcards")], ["Rate limit", st("ok", "per agent + tool")], ["Argument logging", st("vi", "PII redacted")], ["Approval workflows", SOON()]])))));
  } else if (tab === "policies") {
    const rows = pol.data.map((p) => el("tr", {}, el("td", {}, (byId[p.server_id] || {}).name || p.server_id), el("td", {}, p.agent || el("span", { class: "mut" }, "every agent")), el("td", { class: "m" }, p.tool),
      el("td", {}, st(p.action === "allow" ? "ok" : "bk", p.action === "allow" ? "Allow" : "Deny")), el("td", { class: "r m" }, p.rpm ? p.rpm + "/min" : "—"), el("td", { class: "mut" }, ago(p.created_at)),
      el("td", { class: "r" }, can("admin") ? el("button", { class: "btn sm", onclick: async () => { await api("/mcp/policies/" + p.id, { method: "DELETE" }); render(); } }, "Remove") : null)));
    const agentI = el("input", { class: "inp m", placeholder: "agent", style: { flex: 1 } }), toolI = el("input", { class: "inp m", placeholder: "tool", style: { flex: 1 } });
    const srvI = el("select", { class: "inp" }, servers.map((x) => el("option", { value: x.id }, x.name)));
    const out = el("span", { class: "mut" }, "");
    const tester = el("form", { style: { display: "flex", gap: "8px", padding: "12px 16px", alignItems: "center" }, onsubmit: async (e) => {
      e.preventDefault(); const d = await api("/mcp/check", { method: "POST", body: { server_id: srvI.value, agent: agentI.value.trim(), tool: toolI.value.trim() } });
      out.replaceChildren(st(d.allowed ? "ok" : "bk", d.allowed ? "Allowed" : "Denied"), d.rpm ? " " + d.rpm + "/min" : "", d.rule ? el("span", { class: "m mut", style: { fontSize: "11px", marginLeft: "6px" } }, "rule " + d.rule.tool + (d.rule.agent ? " @ " + d.rule.agent : "")) : el("span", { class: "mut", style: { marginLeft: "6px" } }, "no rule · default"));
    } }, srvI, agentI, toolI, el("button", { class: "btn sm", type: "submit", disabled: !servers.length }, "Test"), out);
    body = el("div", { class: "g" }, card("Tool policy", table(["Server", "Agent", "Tool", "Action", ">Limit", "Added", ""], rows, { empty: empty("No tool rules", (pol.default_deny ? "Default deny is on: every tool is blocked" : "Every tool is allowed") + " until you add rules. Typical setup: deny write tools (delete_*, push_*) for all agents, then allow them for one trusted agent.") }),
      { sub: pol.default_deny ? "default deny" : "default allow" }), card("Policy tester", tester, { sub: "dry run, no call is made" }));
  } else {
    const rows = calls.data.map((c) => el("tr", { "data-id": c.id }, el("td", { class: "m mut" }, hhmmss(c.ts)), el("td", {}, c.agent), el("td", {}, c.server_name), el("td", { class: "m" }, c.tool || c.method),
      el("td", { class: "m mut", style: { maxWidth: "320px", overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap", fontSize: "11px" }, title: c.args }, c.args || "—", c.redacted ? [" ", st("vi", "redacted")] : null),
      el("td", { class: "r m" }, ms(c.latency_ms)), el("td", {}, c.error_code ? st(c.status >= 400 ? "bk" : "rt", ERR_LABEL[c.error_code] || c.error_code.replace(/_/g, " "), c.error_message) : st("ok", c.status))));
    const srvSel = el("select", { class: "inp", onchange: (e) => setQuery({ server: e.target.value }) }, el("option", { value: "" }, "All servers"), servers.map((x) => el("option", { value: x.id, selected: x.id === srvF }, x.name)));
    body = card("Tool-call log", table(["Time", "Agent", "Server", "Tool", "Arguments (redacted)", ">Latency", "Status"], rows, { cls: "tight", empty: empty("No tool calls yet", "Calls appear here as soon as an agent uses a tool through /mcp/<server>.") }),
      { sub: `${num(calls.total)} calls · newest first`, right: el("span", { style: { display: "flex", gap: "6px" } }, srvSel, el("button", { class: "btn sm" + (r.q.get("errors") ? " gr" : ""), onclick: () => setQuery({ errors: r.q.get("errors") ? "" : "1" }) }, "Errors only"),
        el("button", { class: "btn sm", disabled: !calls.data.length, onclick: () => download("opengate-mcp-calls.csv", toCSV([["id", "ts", "agent", "server", "tool", "status", "error_code", "latency_ms", "args"], ...calls.data.map((c) => [c.id, c.ts, c.agent, c.server_name, c.tool, c.status, c.error_code, c.latency_ms, c.args])])) }, "CSV")) });
  }
  root.append(el("div", { class: "g" }, head, el("div", {}, tabs), body));
};
ERR_LABEL.tool_not_allowed = "Tool denied"; ERR_LABEL.tool_error = "Tool error";

// ---------- audit log ----------
PAGES.audit = async (root, actions, crumb, r) => {
  if (!teamOn("audit_log")) { root.append(card("Audit log", teamLocked("The audit log is part of the Team plan", "Every admin action (budgets, keys, kill switch, roles, policies, MCP) is recorded append-only. Upgrade to search and export it."))); return; }
  const q = r.q, page = +(q.get("p") || 0), per = 50;
  const params = new URLSearchParams({ limit: per, offset: page * per });
  for (const k of ["actor", "action", "since", "until"]) if (q.get(k)) params.set(k, q.get(k));
  const a = await api("/audit?" + params);
  const f = { actor: el("input", { class: "inp", value: q.get("actor") || "", placeholder: "actor email" }),
    action: el("select", { class: "inp" }, [["", "All actions"], ["key.", "Keys"], ["limit.", "Budgets"], ["project.", "Projects & kill switch"], ["member.", "Members & roles"], ["invite", "Invites"], ["mcp.", "MCP"], ["markup.", "Markups"], ["guardrails.", "Guardrails"], ["routing_rule.", "Routing"], ["billing.", "Billing"], ["auth.", "Sign-ins"]].map(([v, t]) => el("option", { value: v, selected: v === (q.get("action") || "") }, t))),
    since: el("input", { class: "inp", type: "date", value: q.get("since") || "" }), until: el("input", { class: "inp", type: "date", value: q.get("until") || "" }) };
  const apply = (e) => { e.preventDefault(); setQuery({ actor: f.actor.value.trim(), action: f.action.value, since: f.since.value, until: f.until.value, p: "" }); };
  const filters = el("form", { class: "card", style: { display: "flex", gap: "8px", padding: "10px 12px", alignItems: "center", flexWrap: "wrap" }, onsubmit: apply },
    f.actor, f.action, el("span", { class: "mut" }, "from"), f.since, el("span", { class: "mut" }, "to"), f.until, el("button", { class: "btn sm gr", type: "submit" }, "Apply"),
    el("button", { class: "btn sm", type: "button", onclick: () => setQuery({ actor: "", action: "", since: "", until: "", p: "" }) }, "Clear"));
  const qs = [...params].filter(([k]) => k !== "limit" && k !== "offset").map(([k, v]) => k + "=" + encodeURIComponent(v)).join("&");
  actions.push(el("button", { class: "btn", onclick: () => exportAuditCSV(qs) }, "Export CSV"));
  const diff = (x) => {
    if (!x.before && !x.after) return x.detail ? el("span", { class: "m mut", style: { fontSize: "11px" } }, JSON.stringify(x.detail).slice(0, 120)) : el("span", { class: "mut" }, "—");
    const b = x.before || {}, af = x.after || {};
    const keys = [...new Set([...Object.keys(b), ...Object.keys(af)])].filter((k) => JSON.stringify(b[k]) !== JSON.stringify(af[k])).slice(0, 4);
    if (!keys.length) return el("span", { class: "mut" }, "no change");
    return el("span", { style: { display: "flex", flexDirection: "column", gap: "2px" } }, keys.map((k) => el("span", { class: "m", style: { fontSize: "11px" } }, el("span", { class: "mut" }, k + " "),
      el("span", { class: "red" }, b[k] === undefined ? "∅" : JSON.stringify(b[k])), " → ", el("span", { class: "grn" }, af[k] === undefined ? (x.after ? "∅" : "deleted") : JSON.stringify(af[k])))));
  };
  const rows = a.data.map((x) => el("tr", {}, el("td", { class: "m mut", title: x.ts }, new Date(x.ts).toLocaleString("en-US", { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit", second: "2-digit" })),
    el("td", {}, x.actor_email || (x.actor_type === "token" ? "admin token" : x.actor_type)), el("td", { class: "m" }, x.action), el("td", { class: "m t2", style: { fontSize: "11px" } }, x.target ? (keyById(x.target) ? keyById(x.target).name : x.target) : "—"),
    el("td", {}, diff(x)), el("td", {}, st(x.status < 400 ? "ok" : "bk", x.status)), el("td", { class: "m mut", style: { fontSize: "11px" } }, x.ip || "—")));
  const pager = el("span", { class: "m mut", style: { fontSize: "11px", display: "flex", gap: "8px", alignItems: "center" } }, a.total ? `${page * per + 1}–${Math.min(a.total, page * per + a.data.length)} of ${num(a.total)}` : "0 events",
    page > 0 ? el("button", { class: "btn sm", onclick: () => setQuery({ p: page - 1 || "" }) }, "←") : null, (page + 1) * per < a.total ? el("button", { class: "btn sm", onclick: () => setQuery({ p: page + 1 }) }, "→") : null);
  root.append(el("div", { class: "g" }, filters,
    card("Admin actions", table(["Time", "Actor", "Action", "Target", "Before → after", "Status", "IP"], rows, { cls: "tight", empty: empty("No audit events match", "Admin actions are recorded here as they happen.") }), { sub: "append-only · the database rejects edits and deletes", right: pager })));
};

// ---------- boot ----------
async function loadMe() {
  const r = await api("/auth/me", { quiet: true });
  Object.assign(me, r);
  if (r.kind === "token") me.org_id = sessionStorage.getItem(ORG_KEY) || "org_default";
  if (r.kind === "user") { const o = r.orgs.find((o) => o.id === r.org_id); if (o) me.role = o.role; } else me.role = "owner";
  S.cfg = await soft("/settings", {});
  me.plan = await soft("/plan", null);
}
async function boot() {
  S.authCfg = await api("/auth/config", { quiet: true }).catch(() => ({}));
  const su = /^#signup(?:\?plan=([\w-]+))?/.exec(location.hash);
  const inv = /#invite=([\w-]+)/.exec(location.hash);
  if (inv) {
    try { invite = { token: inv[1], ...(await api("/auth/invites/preview", { method: "POST", quiet: true, body: { token: inv[1] } })) }; return showAuth("invite"); }
    catch { invite = null; history.replaceState(null, "", "#overview"); showAuth("signin"); toast("This invite link is invalid, used or expired.", true); return; }
  }
  try { await loadMe(); } catch {
    if (su) { if (su[1]) sessionStorage.setItem(PLAN_KEY, su[1]); history.replaceState(null, "", "#overview"); return showAuth("signup"); }
    return showAuth("signin");
  }
  if (su) history.replaceState(null, "", "#overview");
  await render();
}
async function logout() {
  await api("/auth/logout", { method: "POST", quiet: true }).catch(() => {});
  Live.stop(); sessionStorage.removeItem(TOKEN_KEY); sessionStorage.removeItem(ORG_KEY); showAuth("signin");
}
window.addEventListener("hashchange", () => { if (/#invite=|#signup/.test(location.hash)) return boot(); if (document.querySelector(".app")) { closeModal(); render(); } });
window.addEventListener("DOMContentLoaded", boot);
