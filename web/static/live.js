// OpenGate dashboard live updates: an SSE feed from /admin/api/stream (read
// with fetch so the admin-token mode can send its Authorization header),
// in-place page refreshes with row highlights, toasts, and a polling
// fallback when streaming is unavailable. Loaded after app.js.
"use strict";
const LIVE_PAGES = new Set(["overview", "requests", "mcp", "budgets", "customers", "agents", "policies", "keys"]);
const REFRESH_ON = {
  request: ["overview", "requests", "customers", "agents"], mcp: ["mcp"], tick: ["overview", "requests", "budgets"],
  alert: ["budgets", "overview"], kill: ["overview", "policies", "keys", "agents"],
};
const Live = { state: "off", ctrl: null, fails: 0, last: 0, timer: null, pollT: null, retryT: null, totals: null, demo: false, highlight: "", quiet: {} };
const reducedMotion = () => window.matchMedia && window.matchMedia("(prefers-reduced-motion: reduce)").matches;

function liveIndicator() {
  const label = { live: "Live", polling: "Polling", connecting: "Connecting…", off: "Live" }[Live.state];
  const title = { live: "Streaming updates from the gateway", polling: "Live stream unavailable; refreshing every 5s", connecting: "Connecting to the live stream", off: "" }[Live.state];
  return el("span", { class: "live-ind " + Live.state, title }, el("span", { class: "ldot" }), label);
}
function setLiveState(s) {
  if (Live.state === s) return;
  Live.state = s;
  document.querySelectorAll(".live-ind").forEach((n) => n.replaceWith(liveIndicator()));
}

// ---------- toasts ----------
function liveToast(kind, title, text) {
  let box = $("live-toasts");
  if (!box) { box = el("div", { id: "live-toasts", "aria-live": "polite" }); document.body.append(box); }
  const t = el("div", { class: "lt " + kind, role: "status" }, el("span", { class: "li" }), el("div", {}, el("b", {}, title), text ? el("div", { class: "lx" }, text) : null),
    el("button", { class: "lc", title: "Dismiss", onclick: () => t.remove() }, "×"));
  box.prepend(t);
  while (box.children.length > 4) box.lastChild.remove();
  setTimeout(() => { t.classList.add("out"); setTimeout(() => t.remove(), 300); }, 6500);
}
// once per key per window, so a runaway agent doesn't flood the screen
function throttled(key, ms) {
  const now = Date.now();
  if (Live.quiet[key] && now - Live.quiet[key] < ms) return false;
  Live.quiet[key] = now; return true;
}

// ---------- events ----------
function onLiveEvent(type, d) {
  if (type === "hello") { Live.demo = !!d.demo; return; }
  if (type === "request") {
    if (d.error_code === "budget_exceeded" && throttled("budget:" + (d.agent || d.key_id), 90000))
      liveToast("warn", "Budget limit reached", `${d.agent ? "agent/" + d.agent : keyName(d.key_id)} was refused · ${d.model}`);
  } else if (type === "mcp") {
    if (d.error_code === "tool_not_allowed" && throttled("tool:" + d.agent + d.tool, 30000))
      liveToast("bad", "Tool call denied", `${d.agent} → ${d.server_name}/${d.tool} blocked by policy`);
  } else if (type === "alert") {
    if (d.kind === "budget_threshold") {
      const p = Math.round((d.threshold || 0) * 100);
      liveToast(p >= 100 ? "bad" : "warn", p >= 100 ? "Budget exhausted" : "Budget near the limit", `${d.scope_name || d.scope_id} at ${p}% of ${usd(d.limit_usd)} ${d.period || ""}`.trim());
    } else if (d.text) liveToast("warn", "Alert", d.text);
  } else if (type === "kill") {
    const txt = (d.text || "").replace(/^:[a-z_]+:\s*/, "").replace(/^OpenGate:\s*/, "");
    const killed = /KILLED/.test(txt);
    liveToast(killed ? "bad" : "ok", killed ? (d.scope === "project" ? "Project killed" : "Agent killed") : "Kill switch released", txt || d.scope_name);
  }
  if (type === "tick" && route().page !== "overview" && route().page !== "requests") { if (throttled("chrome", 30000)) refreshChrome(); }
  if ((REFRESH_ON[type] || []).includes(route().page)) scheduleRefresh();
}

// ---------- in-place refresh ----------
function scheduleRefresh(now) {
  const wait = now ? 0 : Math.max(0, 2500 - (Date.now() - Live.last));
  if (Live.timer && !now) return;
  clearTimeout(Live.timer);
  Live.timer = setTimeout(() => { Live.timer = null; softRefresh(); }, wait);
}
async function softRefresh() {
  const r = route();
  if (!LIVE_PAGES.has(r.page) || r.id || +(r.q.get("p") || 0) > 0) return;
  const body = document.querySelector(".main > .body");
  if (!body || document.hidden) return;
  const ae = document.activeElement;
  if (!$("modal").classList.contains("hidden") || (ae && /^(INPUT|SELECT|TEXTAREA)$/.test(ae.tagName) && body.contains(ae))) { Live.last = Date.now(); return scheduleRefresh(); }
  Live.last = Date.now();
  const seq = renderSeq, nb = el("div", { class: "body" });
  const keep = poller; poller = null;
  try { await PAGES[r.page](nb, [], [], r); } catch { poller = keep; return; }
  if (seq !== renderSeq || route().page !== r.page) { clearInterval(poller); poller = keep; return; }
  clearInterval(keep);
  const prevIds = new Set([...body.querySelectorAll("[data-id]")].map((n) => n.dataset.id));
  const prevKv = [...body.querySelectorAll(".kv")].map((n) => n.textContent);
  body.replaceChildren(...nb.childNodes);
  body.querySelectorAll("[data-id]").forEach((n) => {
    if (n.dataset.id === Live.highlight) n.classList.add("flash", "mine");
    else if (prevIds.size && !prevIds.has(n.dataset.id)) n.classList.add("flash");
  });
  body.querySelectorAll(".kv").forEach((n, i) => { if (prevKv[i] !== undefined && prevKv[i] !== n.textContent) n.classList.add("bump"); });
}

// ---------- stream + polling fallback ----------
function authHeaders() {
  const h = { "X-Requested-With": "opengate", Accept: "text/event-stream" };
  const tok = sessionStorage.getItem(TOKEN_KEY);
  if (tok) { h.Authorization = "Bearer " + tok; const o = sessionStorage.getItem(ORG_KEY); if (o) h["X-OpenGate-Org"] = o; }
  return h;
}
Live.ensure = () => { if (!Live.ctrl && !Live.retryT) liveConnect(); };
Live.stop = () => { if (Live.ctrl) Live.ctrl.abort(); Live.ctrl = null; clearTimeout(Live.retryT); Live.retryT = null; stopPolling(); setLiveState("off"); };
async function liveConnect() {
  Live.retryT = null;
  const ctrl = new AbortController(); Live.ctrl = ctrl;
  if (Live.state === "off") setLiveState("connecting");
  try {
    const res = await fetch("/admin/api/stream", { headers: authHeaders(), credentials: "same-origin", cache: "no-store", signal: ctrl.signal });
    if (res.status === 401) { Live.ctrl = null; setLiveState("off"); return; }
    if (!res.ok || !res.body || !(res.headers.get("Content-Type") || "").startsWith("text/event-stream")) throw new Error("stream " + res.status);
    setLiveState("live"); Live.fails = 0; stopPolling();
    const rd = res.body.getReader(), dec = new TextDecoder();
    let buf = "";
    for (;;) {
      const { value, done } = await rd.read();
      if (done) break;
      buf += dec.decode(value, { stream: true });
      let i;
      while ((i = buf.indexOf("\n\n")) >= 0) {
        const chunk = buf.slice(0, i); buf = buf.slice(i + 2);
        let type = "message", data = "";
        for (const line of chunk.split("\n")) {
          if (line.startsWith("event: ")) type = line.slice(7);
          else if (line.startsWith("data: ")) data += line.slice(6);
        }
        if (!data) continue;
        try { onLiveEvent(type, JSON.parse(data)); } catch (e) { console.warn("live event", e); }
      }
    }
    throw new Error("stream closed");
  } catch (e) {
    if (ctrl.signal.aborted) return;
    Live.ctrl = null; Live.fails++;
    if (Live.fails >= 2) startPolling(); else setLiveState("connecting");
    Live.retryT = setTimeout(liveConnect, Math.min(60000, 1500 * 2 ** Live.fails));
  }
}
function startPolling() {
  setLiveState("polling");
  if (Live.pollT) return;
  Live.pollT = setInterval(async () => {
    if (document.hidden) return;
    const [l, m] = await Promise.all([soft("/logs?limit=1", null), soft("/mcp/calls?limit=1", null)]);
    const t = (l ? l.total : 0) + ":" + (m ? m.total : 0);
    if (Live.totals !== null && t !== Live.totals) softRefresh();
    Live.totals = t;
  }, 5000);
}
function stopPolling() { clearInterval(Live.pollT); Live.pollT = null; Live.totals = null; }
document.addEventListener("visibilitychange", () => { if (!document.hidden && Live.state !== "off") scheduleRefresh(true); });

// ---------- "Try it" (demo deployments) ----------
function tryItButton() {
  if (!(me.plan && me.plan.demo) || !can("member")) return null;
  const b = el("button", { class: "btn gr try-it", title: "Send a test request through a demo key", onclick: async () => {
    b.disabled = true; b.classList.add("busy");
    try {
      const d = await api("/demo/try", { method: "POST", body: {} });
      Live.highlight = d.request_id;
      liveToast(d.status < 400 ? "ok" : "warn", d.status < 400 ? "Test request routed" : "Test request " + d.status,
        `${d.requested_model} → ${d.model || "—"} · ${usd(d.cost_usd, 5)} · ${ms(d.latency_ms)}${d.saved_usd ? " · saved " + usd(d.saved_usd, 5) : ""}`);
      if (route().page !== "overview" && route().page !== "requests") location.hash = "#requests"; else scheduleRefresh(true);
    } catch { /* api() already toasted */ } finally { b.disabled = false; b.classList.remove("busy"); }
  } }, "▶ Try it");
  return b;
}
