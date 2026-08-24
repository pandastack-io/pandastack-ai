// SPDX-License-Identifier: Apache-2.0
import { isStubAuth, STUB_USER_EMAIL, STUB_USER_ID } from "@/lib/auth-mode";
import { createClient } from "@/lib/supabase/client";

// Self-hosted by default: point at a control plane on localhost unless the
// deployment sets NEXT_PUBLIC_PANDASTACK_API (the Dockerfile takes it as a
// build ARG). Never default to a hosted vendor endpoint — a self-hosted
// dashboard must not talk to someone else's control plane.
export const API_BASE =
  process.env.NEXT_PUBLIC_PANDASTACK_API ?? "http://localhost:8080";

export async function getAuthHeaders(): Promise<Record<string, string>> {
  if (typeof window === "undefined") return {};

  const orgId = window.localStorage.getItem("pandastack_org_id");
  const headers: Record<string, string> = {};

  if (isStubAuth()) {
    headers["X-Stub-User"] = STUB_USER_EMAIL;
    headers["X-Fcs-Workspace"] = STUB_USER_ID;
    if (orgId) headers["X-Pandastack-Org"] = orgId;
    return headers;
  }

  const {
    data: { session },
  } = await createClient().auth.getSession();

  const token = session?.access_token;
  const userId = session?.user?.id;

  if (token && userId) {
    headers.Authorization = `Bearer ${token}`;
    headers["X-Fcs-Workspace"] = userId;
  }
  // Only send X-Pandastack-Org if the stored org belongs to the current user.
  // Guards against stale localStorage from a previously logged-in account.
  const orgUser = window.localStorage.getItem("pandastack_org_user");
  if (orgId && userId && orgUser === userId) headers["X-Pandastack-Org"] = orgId;

  return headers;
}

export type Sandbox = {
  id: string;
  template: string;
  cpu: number;
  memory_mb: number;
  status:
    | "creating"
    | "running"
    | "paused"
    | "stopping"
    | "deleted"
    | "failed"
    | "hibernated";
  guest_ip: string;
  host_tap: string;
  mac: string;
  vsock_cid: number;
  from_snapshot?: string;
  metadata?: Record<string, string>;
  created_at: string;
};

export type CreateRequest = {
  template: string;
  from_snapshot?: string;
  ttl_seconds?: number;
};

// Managed PostgreSQL database (a postgres-16 sandbox wrapped with DB ergonomics).
export type DatabaseInfo = {
  id: string;
  status: string;
  template: string;
  label?: string;
  created_at?: number;
  host?: string;
  port?: number;
  database?: string;
  username?: string;
  password?: string;
  connection_url?: string;
  broker_token?: string;
  broker_url?: string;
  // Set when the VM is up but postgres failed to publish credentials.
  error?: string;
  // Failover availability (item 15: populated when status="failed")
  failover_available?: boolean;
  failover_reason?: string;
  failover_eta_seconds?: number;
  // Backing sandbox id (today identical to the database id).
  sandbox_id?: string;
  // When true, the database is exempt from idle auto-suspend.
  always_on?: boolean;
};

export type CreateDatabaseRequest = {
  cpu?: number;
  memory_mb?: number;
  label?: string;
  // Exempt from idle auto-suspend from creation.
  always_on?: boolean;
};

// Live stats snapshot for a managed database (GET /v1/databases/{id}/stats).
export type DatabaseStats = {
  postgres_version?: string;
  db_size_bytes: number;
  connections: number;
  max_connections: number;
  uptime_seconds: number;
  cache_hit_ratio: number;
  disk_size_bytes: number;
  disk_used_bytes: number;
  disk_avail_bytes: number;
  disk_used_pct: number;
};

// Historical CPU + memory + net + disk I/O for one database. Bucketed on the
// server per range; the client just plots the points as returned.
export type DatabaseMetricPoint = {
  ts: string;              // "YYYY-MM-DD HH:MM:SS" UTC (analytics bucket start)
  cpu_pct: number;         // 0–100 (avg over bucket)
  mem_bytes: number;       // avg guest working-set bytes
  net_rx_bytes: number;    // sum over bucket
  net_tx_bytes: number;
  disk_rd_bytes: number;
  disk_wr_bytes: number;
};

export type DatabaseMetrics = {
  range: "1h" | "24h" | "7d" | "30d";
  bucket: string;          // "15s" | "1m" | "5m" | "30m" | "2h"
  bucket_seconds: number;
  from: string;            // RFC3339
  to: string;
  points: DatabaseMetricPoint[];
  empty: boolean;          // true when the pipeline is wired but has no samples yet
};

export type Template = {
  name: string;
  rootfs_path: string;
  size_bytes: number;
  cpu: number;
  memory_mb: number;
  // Curated catalog metadata (control-plane registry). Global templates are
  // first-party and non-deletable; custom templates belong to a workspace.
  is_global?: boolean;
  workspace?: string;
  label?: string;
  description?: string;
  category?: string;
  base?: string;
  tools?: string[];
  meta?: Record<string, unknown>;
};

export type ApiToken = {
  prefix: string;
  label: string;
  created_at: string;
};

export type NewApiToken = ApiToken & {
  token: string;
};

export type ExecResult = {
  stdout: string;
  stderr: string;
  exit_code: number;
};

export type OrgRole = "owner" | "admin" | "member";

export type Org = {
  id: string;
  slug: string;
  name: string;
  role: OrgRole;
  created_at: string;
  member_count?: number;
};

export type OrgMember = {
  user_id: string;
  email: string;
  role: OrgRole;
  joined_at: string;
};

export type InviteResponse = {
  invite_url: string;
  expires_at: string;
};

export type Me = {
  user_id: string;
  email: string;
  current_org_id?: string | null;
  orgs: Org[];
};

export type DirEntry = {
  name: string;
  is_dir: boolean;
  size: number;
  mode: string;
  mtime: number;
};

export type Metrics = {
  pid: number;
  uptime_seconds: number;
  host_cpu_percent: number;
  host_rss_bytes: number;
  host_vsz_bytes: number;
  threads: number;
};

async function call<T>(path: string, init?: RequestInit): Promise<T> {
  const headers = new Headers(init?.headers);
  if (!headers.has("content-type") && !(init?.body instanceof FormData)) {
    headers.set("content-type", "application/json");
  }
  Object.entries(await getAuthHeaders()).forEach(([key, value]) => {
    headers.set(key, value);
  });

  // Parameterless mutating requests (pause/resume/hibernate/accept-invite) have
  // no body; some runtimes then omit Content-Length, which an edge proxy rejects
  // with 411. Send an explicit empty body so Content-Length: 0 is always set.
  // (Mirrors the SDK fix.)
  const method = (init?.method ?? "GET").toUpperCase();
  const mutating = method !== "GET" && method !== "HEAD" && method !== "DELETE";
  const body = init?.body ?? (mutating ? "" : undefined);

  const r = await fetch(`${API_BASE}/v1${path}`, {
    ...init,
    headers,
    body,
    cache: "no-store",
  });
  if (!r.ok) {
    let msg = `${r.status} ${r.statusText}`;
    try {
      const j = await r.json();
      if (j?.error) msg = j.error;
    } catch {}
    // Clear stale org from localStorage if the server rejects it — avoids
    // permanent "not a member of requested org" loops on re-login.
    if (r.status === 403 && msg.includes("not a member")) {
      window.localStorage.removeItem("pandastack_org_id");
      window.localStorage.removeItem("pandastack_org_user");
    }
    throw new Error(msg);
  }
  if (r.status === 204) return undefined as T;
  const ct = r.headers.get("content-type") ?? "";
  if (ct.includes("application/json")) return r.json();
  return r.text() as unknown as T;
}

export async function listApiTokens(): Promise<{ items: ApiToken[] }> {
  return call<{ items: ApiToken[] }>("/me/tokens");
}

export async function createApiToken(label: string): Promise<NewApiToken> {
  return call<NewApiToken>("/me/tokens", {
    method: "POST",
    body: JSON.stringify({ label }),
  });
}

export async function revokeApiToken(prefix: string): Promise<void> {
  return call<void>(`/me/tokens/${encodeURIComponent(prefix)}`, { method: "DELETE" });
}

function slugifyOrgName(name: string): string {
  const slug = name
    .trim()
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "")
    .slice(0, 48);
  return slug || "org";
}

export async function listOrgs(): Promise<Org[]> {
  return call<Org[]>("/orgs");
}

export async function createOrg(name: string): Promise<Org> {
  return call<Org>("/orgs", {
    method: "POST",
    body: JSON.stringify({ name, slug: slugifyOrgName(name) }),
  });
}

export async function getOrgMembers(orgId: string): Promise<OrgMember[]> {
  return call<OrgMember[]>(`/orgs/${encodeURIComponent(orgId)}/members`);
}

export async function inviteMember(orgId: string, email: string, role: "admin" | "member"): Promise<InviteResponse> {
  return call<InviteResponse>(`/orgs/${encodeURIComponent(orgId)}/members`, {
    method: "POST",
    body: JSON.stringify({ email, role }),
  });
}

export async function removeMember(orgId: string, userId: string): Promise<void> {
  return call<void>(`/orgs/${encodeURIComponent(orgId)}/members/${encodeURIComponent(userId)}`, { method: "DELETE" });
}

export async function acceptInvite(token: string): Promise<{ org_id: string; role: string }> {
  return call<{ org_id: string; role: string }>(`/orgs/invites/${encodeURIComponent(token)}/accept`, { method: "POST" });
}

export async function getMe(): Promise<Me> {
  return call<Me>("/me");
}

function base64EncodeBytes(bytes: Uint8Array): string {
  let binary = "";
  const chunkSize = 0x8000;
  for (let i = 0; i < bytes.length; i += chunkSize) {
    binary += String.fromCharCode(...bytes.subarray(i, i + chunkSize));
  }
  if (typeof btoa === "function") return btoa(binary);
  const fallback = (globalThis as { Buffer?: { from(data: Uint8Array): { toString(encoding: string): string } } }).Buffer;
  if (fallback) return fallback.from(bytes).toString("base64");
  throw new Error("Base64 encoding is unavailable in this environment");
}

export async function setCurrentOrg(orgId: string): Promise<void> {
  if (typeof window !== "undefined") {
    window.localStorage.setItem("pandastack_org_id", orgId);
    // Tag with current user so cross-account stale orgs are ignored.
    const { data: { session } } = await createClient().auth.getSession();
    if (session?.user?.id) {
      window.localStorage.setItem("pandastack_org_user", session.user.id);
    }
  }

  const headers = new Headers({ "content-type": "application/json" });
  Object.entries(await getAuthHeaders()).forEach(([key, value]) => headers.set(key, value));
  const r = await fetch(`${API_BASE}/v1/me/current-org`, {
    method: "POST",
    headers,
    body: JSON.stringify({ org_id: orgId }),
    cache: "no-store",
  });
  if (r.ok || r.status === 404 || r.status === 405) return;
  throw new Error(`${r.status} ${r.statusText}`);
}

export const api = {
  list: () => call<Sandbox[] | null>("/sandboxes"),
  get: (id: string) => call<Sandbox>(`/sandboxes/${id}`),
  create: (req: CreateRequest) =>
    call<Sandbox>("/sandboxes", { method: "POST", body: JSON.stringify(req) }),
  remove: (id: string) => call<void>(`/sandboxes/${id}`, { method: "DELETE" }),
  pause: (id: string) =>
    call<void>(`/sandboxes/${id}/pause`, { method: "POST" }),
  resume: (id: string) =>
    call<void>(`/sandboxes/${id}/resume`, { method: "POST" }),
  snapshot: (id: string) =>
    call<{ id: string; sandbox_id: string; created_at: string }>(
      `/sandboxes/${id}/snapshots`,
      { method: "POST" }
    ),

  // Phase 2: templates
  templates: () => call<Template[]>("/templates"),

  // Phase 1: filesystem
  readFile: (id: string, path: string) =>
    call<string>(`/sandboxes/${id}/fs?path=${encodeURIComponent(path)}`),
  writeFile: (id: string, path: string, body: string) =>
    call<{ path: string; bytes: number }>(
      `/sandboxes/${id}/fs?path=${encodeURIComponent(path)}`,
      {
        method: "PUT",
        headers: { "content-type": "application/octet-stream" },
        body,
      }
    ),
  deletePath: (id: string, path: string) =>
    call<void>(`/sandboxes/${id}/fs?path=${encodeURIComponent(path)}`, {
      method: "DELETE",
    }),
  listDir: (id: string, path: string) =>
    call<{ path: string; entries: DirEntry[] | null }>(
      `/sandboxes/${id}/fs/dir?path=${encodeURIComponent(path)}`
    ),

  // Phase 1: exec
  exec: (id: string, cmd: string) =>
    call<ExecResult>(`/sandboxes/${id}/exec`, {
      method: "POST",
      body: JSON.stringify({ cmd }),
    }),

  // Phase 2: logs (snapshot fetch — follow=1 uses SSE, see useLogs hook)
  logs: (id: string) => call<string>(`/sandboxes/${id}/logs`),

  // Phase 2: metrics
  metrics: (id: string) => call<Metrics>(`/sandboxes/${id}/metrics`),

  // ClickHouse-backed time-series. `series` is a map of metric-name → array
  // of [bucket-iso, value] tuples. Empty arrays when CH has no data yet.
  metricsOverview: (params?: {
    from?: string;
    to?: string;
    step?: "15s" | "1m" | "5m" | "1h";
  }) =>
    call<{
      from: string;
      to: string;
      step: string;
      series: Record<string, Array<[string, number | null]>>;
    }>(
      `/metrics/overview${
        params
          ? "?" +
            new URLSearchParams(
              Object.entries(params).filter(([, v]) => v != null) as [
                string,
                string,
              ][],
            ).toString()
          : ""
      }`,
    ),

  metricsSandbox: (
    id: string,
    params?: { from?: string; to?: string; step?: "15s" | "1m" | "5m" | "1h" },
  ) =>
    call<{
      from: string;
      to: string;
      step: string;
      series: Record<string, Array<[string, number | null]>>;
    }>(
      `/metrics/sandbox/${id}${
        params
          ? "?" +
            new URLSearchParams(
              Object.entries(params).filter(([, v]) => v != null) as [
                string,
                string,
              ][],
            ).toString()
          : ""
      }`,
    ),

  // Phase 3: fork + hibernate + wake
  fork: (id: string, count: number) =>
    call<ForkResult>(`/sandboxes/${id}/fork`, {
      method: "POST",
      body: JSON.stringify({ count }),
    }),
  hibernate: (id: string) =>
    call<{ status: string }>(`/sandboxes/${id}/hibernate`, { method: "POST" }),
  wake: (id: string) =>
    call<{ status: string }>(`/sandboxes/${id}/wake`, { method: "POST" }),
  // Public stop/start aliases (preferred in SDK and UI)
  stop: (id: string) =>
    call<{ status: string }>(`/sandboxes/${id}/stop`, { method: "POST" }),
  start: (id: string) =>
    call<{ status: string }>(`/sandboxes/${id}/start`, { method: "POST" }),

  // Managed databases (postgres-16 sandboxes with DB ergonomics)
  databases: async () => {
    const data = await call<{ items: DatabaseInfo[]; count: number } | DatabaseInfo[]>("/databases");
    return Array.isArray(data) ? data : data.items ?? [];
  },
  getDatabase: (id: string) => call<DatabaseInfo>(`/databases/${encodeURIComponent(id)}`),
  createDatabase: (req: CreateDatabaseRequest = {}) =>
    call<DatabaseInfo>("/databases", { method: "POST", body: JSON.stringify(req) }),
  deleteDatabase: (id: string) =>
    call<void>(`/databases/${encodeURIComponent(id)}`, { method: "DELETE" }),
  databaseStats: (id: string) =>
    call<DatabaseStats>(`/databases/${encodeURIComponent(id)}/stats`),
  databaseLogs: (id: string, lines = 300) =>
    call<{ logs: string }>(`/databases/${encodeURIComponent(id)}/logs?lines=${lines}`),
  failoverDatabase: (id: string) =>
    call<DatabaseInfo>(`/databases/${encodeURIComponent(id)}/failover`, { method: "POST" }),
  // Resume an idle (auto-suspended) database now instead of waiting for the
  // next connection to wake it.
  wakeDatabase: (id: string) =>
    call<{ id: string; status: string; detail?: string }>(
      `/databases/${encodeURIComponent(id)}/wake`, { method: "POST" }),
  updateDatabase: (id: string, patch: { always_on?: boolean }) =>
    call<DatabaseInfo>(`/databases/${encodeURIComponent(id)}`,
      { method: "PATCH", body: JSON.stringify(patch) }),
  resetDatabaseCredentials: (id: string) =>
    call<DatabaseInfo>(`/databases/${encodeURIComponent(id)}/reset-credentials`,
      { method: "POST" }),
  // Historical CPU + memory + net/disk I/O for one database. Bucketed on the
  // server side; the client just plots the points. Range picks a default
  // bucket unless one is passed explicitly.
  databaseMetrics: (id: string, range: "1h" | "24h" | "7d" | "30d" = "24h", bucket?: string) => {
    const q = new URLSearchParams({ range });
    if (bucket) q.set("bucket", bucket);
    return call<DatabaseMetrics>(`/databases/${encodeURIComponent(id)}/metrics?${q}`);
  },

  streamSandboxLogs: async (
    id: string,
    onLine: (line: string) => void,
    signal?: AbortSignal,
  ): Promise<void> => {
    const r = await fetch(`${API_BASE}/v1/sandboxes/${encodeURIComponent(id)}/logs?follow=1`, {
      headers: await getAuthHeaders(),
      signal,
    });
    if (!r.ok || !r.body) throw new Error(`log stream failed: HTTP ${r.status}`);
    const reader = r.body.getReader();
    const dec = new TextDecoder();
    let buf = "";
    for (;;) {
      const { done, value } = await reader.read();
      if (done) return;
      buf += dec.decode(value, { stream: true });
      const events = buf.split("\n\n");
      buf = events.pop() ?? "";
      for (const ev of events) {
        const m = ev.match(/data: ([\s\S]+)/);
        if (!m) continue;
        try {
          const d = JSON.parse(m[1]) as { line?: string };
          if (typeof d.line === "string") onLine(d.line);
        } catch {
          // Non-JSON data lines: surface verbatim.
          const raw = m[1].trim();
          if (raw && raw !== "end") onLine(raw);
        }
      }
    }
  },

  // Phase 4: volumes
  volumes: () => call<Volume[]>("/volumes"),
  createVolume: (name: string, size_mb: number) =>
    call<Volume>("/volumes", {
      method: "POST",
      body: JSON.stringify({ name, size_mb }),
    }),
  deleteVolume: (name: string) =>
    call<void>(`/volumes/${encodeURIComponent(name)}`, { method: "DELETE" }),

  // Phase 4: template build (multipart, not JSON)
  buildTemplate: async (
    name: string,
    size_mb: number,
    rootfs: File,
    cpu = 8, // server pins every template to 8 burstable vCPUs — RAM is the only knob
    memory_mb = 512
  ): Promise<TemplateBuild> => {
    const fd = new FormData();
    fd.append("name", name);
    fd.append("size_mb", String(size_mb));
    fd.append("cpu", String(cpu));
    fd.append("memory_mb", String(memory_mb));
    fd.append("rootfs", rootfs);
    const r = await fetch(`${API_BASE}/v1/templates/build`, {
      method: "POST",
      headers: await getAuthHeaders(),
      body: fd,
    });
    if (!r.ok) throw new Error(await r.text());
    return r.json();
  },
  templateBuilds: () => call<TemplateBuild[]>("/templates/builds"),
  templateBuild: (id: string) =>
    call<TemplateBuild>(`/templates/builds/${id}`),
  deleteTemplate: (name: string) =>
    call<void>(`/templates/${encodeURIComponent(name)}`, { method: "DELETE" }),

  ports: (id: string) => call<Port[]>(`/sandboxes/${id}/ports`),
  registerPort: (id: string, port: number, label?: string) =>
    call<Port>(`/sandboxes/${id}/ports`, {
      method: "POST",
      body: JSON.stringify({ port, label }),
    }),
  deletePort: (id: string, port: number) =>
    call<void>(`/sandboxes/${id}/ports/${port}`, { method: "DELETE" }),
  proxyURL: (id: string, port: number, path = "/") =>
    `${API_BASE}/v1/sandboxes/${id}/proxy/${port}${path.startsWith("/") ? path : "/" + path}`,
  previewURL: (id: string, port: number, scheme: "https" | "http" = "https") => {
    if (!Number.isInteger(port) || port < 1 || port > 65535) {
      throw new Error(`port out of range: ${port}`);
    }
    const suffix = previewHostSuffix();
    return `${scheme}://${port}-${id}.${suffix}`;
  },
};

// Override via NEXT_PUBLIC_PANDASTACK_PREVIEW_HOST; else derived from API_BASE
// by stripping a leading "api." label (api.example.com -> example.com).
// Self-hosted deployments should set the env var explicitly.
export function previewHostSuffix(): string {
  const override = process.env.NEXT_PUBLIC_PANDASTACK_PREVIEW_HOST;
  if (override) return override.replace(/^https?:\/\//, "").replace(/\/.*$/, "");
  try {
    const u = new URL(API_BASE);
    const host = u.hostname;
    if (host.startsWith("api.") && host.length > 4) return host.slice(4);
    return host;
  } catch {
    return "localhost";
  }
}

export type Port = {
  port: number;
  label?: string;
  listening: boolean;
  source: "user" | "detected";
  proxy_url: string;
};

export type Volume = {
  name: string;
  size_mb: number;
  size_bytes: number;
};

export type TemplateBuild = {
  id: string;
  name: string;
  status: "queued" | "running" | "done" | "failed";
  error?: string;
  started_at: string;
  ended_at?: string;
  size_mb: number;
  bytes?: number;
};

export type ForkResult = {
  parent_id: string;
  snapshot_id: string;
  children: string[];
  at: string;
};


