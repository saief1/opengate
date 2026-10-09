// Minimal typed client for the OpenGate admin API (Node 18+ / browsers / Deno /
// Bun; uses global fetch, no dependencies). Types mirror docs/openapi.yaml;
// for full coverage generate them with `npx openapi-typescript docs/openapi.yaml`.

export interface VirtualKey {
  id: string; project_id: string; name: string; agent: string; user: string;
  prefix: string; killed: boolean; revoked: boolean; created_at: string;
  allowed_models: string[]; denied_models: string[]; max_tokens: number; customer_id: string;
}
export interface Limit { scope: "key" | "agent" | "project"; scope_id: string; daily_usd?: number; monthly_usd?: number; total_usd?: number; rpm?: number }
export interface RequestLog { id: string; ts: string; model: string; provider: string; status: number; cost_usd: number; latency_ms: number; key_id: string; agent: string; customer_id?: string }
export class OpenGateError extends Error { constructor(public status: number, public code: string, message: string) { super(message); } }

export class OpenGateAdmin {
  constructor(private baseURL: string, private token: string, private org?: string) {}

  private async req<T>(method: string, path: string, body?: unknown): Promise<T> {
    const headers: Record<string, string> = { Authorization: `Bearer ${this.token}`, "Content-Type": "application/json" };
    if (this.org) headers["X-OpenGate-Org"] = this.org;
    const res = await fetch(`${this.baseURL}/admin/api${path}`, { method, headers, body: body === undefined ? undefined : JSON.stringify(body) });
    const data: any = res.status === 204 ? {} : await res.json().catch(() => ({}));
    if (!res.ok) throw new OpenGateError(res.status, data?.error?.code ?? "", data?.error?.message ?? res.statusText);
    return data as T;
  }

  createKey(input: { name: string; agent?: string; project_id?: string; limits?: Omit<Limit, "scope" | "scope_id"> }) {
    return this.req<{ key: string; virtual_key: VirtualKey }>("POST", "/keys", input);
  }
  listKeys() { return this.req<{ data: VirtualKey[] }>("GET", "/keys"); }
  kill(keyId: string) { return this.req<{ aborted_inflight: number }>("POST", `/keys/${encodeURIComponent(keyId)}/kill`); }
  unkill(keyId: string) { return this.req<unknown>("POST", `/keys/${encodeURIComponent(keyId)}/unkill`); }
  bindCustomer(keyId: string, customerId: string) { return this.req<VirtualKey>("PATCH", `/keys/${encodeURIComponent(keyId)}`, { customer_id: customerId }); }
  putLimit(limit: Limit) { return this.req<Limit>("PUT", "/limits", limit); }
  logs(q: { key_id?: string; model?: string; errors?: boolean; limit?: number } = {}) {
    const p = new URLSearchParams();
    for (const [k, v] of Object.entries(q)) if (v !== undefined) p.set(k, k === "errors" ? (v ? "1" : "") : String(v));
    return this.req<{ data: RequestLog[] }>("GET", `/logs?${p}`);
  }
}

// Example: kill every key whose spend today crossed $10.
export async function killRunaways(admin: OpenGateAdmin): Promise<string[]> {
  const killed: string[] = [];
  for (const k of (await admin.listKeys()).data) {
    const recent = (await admin.logs({ key_id: k.id, limit: 200 })).data;
    const today = new Date().toISOString().slice(0, 10);
    const spend = recent.filter((l) => l.ts.startsWith(today)).reduce((s, l) => s + l.cost_usd, 0);
    if (!k.killed && spend > 10) { await admin.kill(k.id); killed.push(k.id); }
  }
  return killed;
}
