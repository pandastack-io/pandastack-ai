// SPDX-License-Identifier: Apache-2.0
import { isStubAuth, STUB_USER_EMAIL, STUB_USER_ID } from "@/lib/auth-mode";
import { createClient } from "@/lib/supabase/client";

export const API_BASE =
  process.env.NEXT_PUBLIC_PANDASTACK_API ?? "https://api.pandastack.ai";

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
};

export type CreateDatabaseRequest = {
  cpu?: number;
  memory_mb?: number;
  label?: string;
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

// Git-driven app hosting (Vercel/Render-style) on a persistent sandbox.
export type AppInfo = {
  id: string;
  workspace: string;
  name: string;
  git_url: string;
  git_branch: string;
  framework?: string;
  runtime?: string;
  runtime_version?: string;
  install_command?: string;
  build_command?: string;
  start_command?: string;
  root_directory?: string;
  port: number;
  env: Record<string, string>;
  template: string;
  cpu: number;
  memory_mb: number;
  sandbox_id?: string;
  active_deployment_id?: string;
  status: string;
  url?: string;
  github_installation_id?: number;
  github_repo_id?: number;
  github_repo_full_name?: string;
  auto_deploy: boolean;
  created_at: string;
  updated_at: string;
};

export type DeploymentInfo = {
  id: string;
  app_id: string;
  workspace: string;
  status: string;
  git_commit?: string;
  git_ref?: string;
  sandbox_id?: string;
  build_logs?: string;
  error?: string;
  created_at: string;
  updated_at: string;
  finished_at?: string;
};

export type CreateAppRequest = {
  name: string;
  git_url: string;
  git_branch?: string;
  framework?: string;
  runtime?: string;
  runtime_version?: string;
  install_command?: string;
  build_command?: string;
  start_command?: string;
  root_directory?: string;
  port?: number;
  env?: Record<string, string>;
  template?: string;
  cpu?: number;
  memory_mb?: number;
  // GitHub wiring (set when the repo is picked via the connected GitHub App).
  // github_repo_id is stable across renames; github_repo_full_name is owner/repo.
  // auto_deploy defaults to true on the server when omitted.
  github_installation_id?: number;
  github_repo_id?: number;
  github_repo_full_name?: string;
  auto_deploy?: boolean;
};

// PATCH /v1/apps/{id} — every field optional; only the keys present are
// updated. NOTE: `env` REPLACES the whole map (not a merge), and a PATCH does
// NOT trigger a redeploy — changes take effect on the app's next deploy.
export type UpdateAppRequest = {
  git_branch?: string;
  framework?: string;
  runtime?: string;
  runtime_version?: string;
  install_command?: string;
  build_command?: string;
  start_command?: string;
  root_directory?: string;
  port?: number;
  env?: Record<string, string>;
  template?: string;
  cpu?: number;
  memory_mb?: number;
  auto_deploy?: boolean;
};

// A GitHub App installation connected to the current workspace.
export type GitHubInstallation = {
  installation_id: number;
  account_login: string;
  account_type: string;
  connected_by?: string;
  created_at: string;
};

// A repository accessible to a connected installation.
export type GitHubRepo = {
  id: number;
  full_name: string;
  private: boolean;
  default_branch: string;
  clone_url: string;
  html_url: string;
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

export type FunctionInfo = {
  id: string;
  workspace: string;
  name: string;
  runtime: "python" | "nodejs";
  entrypoint: string;
  code_size: number;
  template?: string;
  env?: Record<string, string>;
  public: boolean;
  endpoint?: string;
  url?: string;
  version: number;
  is_ready: boolean;
  gcs_path?: string;
  created_at: string;
  updated_at: string;
};

export type FunctionMetrics = {
  period: string;
  total: number;
  errors: number;
  error_rate: number;
  p50_ms: number;
  p95_ms: number;
  p99_ms: number;
  cold_start_rate: number;
};

export type ScheduleInfo = {
  id: string;
  workspace: string;
  name: string;
  function_id: string;
  cron: string;
  paused: boolean;
  last_run_at?: string | null;
  next_run_at?: string | null;
  created_at: string;
  updated_at: string;
};

export type ScheduleTriggerResult = {
  status: string;
  exit_code: number;
  duration_ms: number;
  stdout: string;
  stderr: string;
  sandbox_id: string;
};

export type FunctionRun = {
  id: string;
  workspace: string;
  function_id: string;
  schedule_id?: string | null;
  sandbox_id?: string | null;
  status: string;
  exit_code?: number | null;
  stdout?: string;
  stderr?: string;
  duration_ms?: number;
  started_at: string;
  ended_at?: string | null;
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

  const r = await fetch(`${API_BASE}/v1${path}`, {
    ...init,
    headers,
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

function normalizeFunctionInfo(fn: FunctionInfo): FunctionInfo {
  return {
    ...fn,
    endpoint: fn.endpoint || fn.url || undefined,
    version: fn.version ?? 1,
    is_ready: fn.is_ready ?? true,
  };
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

export function functionEndpoint(fn: Pick<FunctionInfo, "endpoint" | "url">): string | null {
  return fn.endpoint ?? fn.url ?? null;
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

  functions: async () => {
    const data = await call<FunctionInfo[] | { items: FunctionInfo[] }>("/functions");
    const items = Array.isArray(data) ? data : data.items ?? [];
    return items.map(normalizeFunctionInfo);
  },
  getFunction: async (id: string) =>
    normalizeFunctionInfo(await call<FunctionInfo>(`/functions/${encodeURIComponent(id)}`)),
  deployFunction: async (req: {
    name: string;
    runtime: "python" | "nodejs";
    file: File;
    entrypoint?: string;
    env?: Record<string, string>;
    template?: string;
    public?: boolean;
  }) => {
    const bytes = new Uint8Array(await req.file.arrayBuffer());
    return normalizeFunctionInfo(await call<FunctionInfo>("/functions", {
      method: "POST",
      body: JSON.stringify({
        name: req.name,
        runtime: req.runtime,
        entrypoint: req.entrypoint ?? req.file.name,
        code: base64EncodeBytes(bytes),
        template: req.template ?? "code-interpreter",
        env: req.env ?? {},
        public: req.public ?? false,
      }),
    }));
  },
  updateFunction: async (id: string, body: { name?: string; env?: Record<string, string>; public?: boolean; entrypoint?: string }) =>
    normalizeFunctionInfo(await call<FunctionInfo>(`/functions/${encodeURIComponent(id)}`, {
      method: "PATCH",
      body: JSON.stringify(body),
    })),
  deleteFunction: (id: string) => call<void>(`/functions/${encodeURIComponent(id)}`, { method: "DELETE" }),
  triggerFunction: (id: string) =>
    call<FunctionRun>(`/functions/${encodeURIComponent(id)}/invoke`, { method: "POST", body: JSON.stringify({}) }),
  functionRuns: async (id: string) => {
    const data = await call<FunctionRun[] | { items: FunctionRun[] }>(`/functions/${encodeURIComponent(id)}/runs`);
    return Array.isArray(data) ? data : data.items ?? [];
  },
  functionMetrics: (id: string, period?: string) =>
    call<FunctionMetrics>(`/functions/${encodeURIComponent(id)}/metrics${period ? `?period=${encodeURIComponent(period)}` : ""}`),
  deployBundle: async (id: string, file: File) => {
    const headers = await getAuthHeaders();
    const formData = new FormData();
    formData.append("bundle", file);
    const base = (process.env.NEXT_PUBLIC_PANDASTACK_API ?? process.env.NEXT_PUBLIC_API_BASE_URL ?? "").replace(/\/$/, "");
    const res = await fetch(`${base}/v1/functions/${encodeURIComponent(id)}/deploy`, {
      method: "POST",
      headers,
      body: formData,
    });
    if (!res.ok) {
      const err = await res.json().catch(() => ({ error: res.statusText })) as { error?: string };
      throw new Error(err.error ?? res.statusText);
    }
    return normalizeFunctionInfo(await res.json() as FunctionInfo);
  },

  schedules: async () => {
    const data = await call<ScheduleInfo[] | { items: ScheduleInfo[] }>("/schedules");
    return Array.isArray(data) ? data : data.items ?? [];
  },
  getSchedule: (id: string) => call<ScheduleInfo>(`/schedules/${encodeURIComponent(id)}`),
  createSchedule: (body: { name: string; function_id: string; cron: string; paused?: boolean }) =>
    call<ScheduleInfo>("/schedules", { method: "POST", body: JSON.stringify({ ...body, paused: body.paused ?? false }) }),
  updateSchedule: (id: string, body: { cron?: string; paused?: boolean }) =>
    call<ScheduleInfo>(`/schedules/${encodeURIComponent(id)}`, { method: "PATCH", body: JSON.stringify(body) }),
  deleteSchedule: (id: string) => call<void>(`/schedules/${encodeURIComponent(id)}`, { method: "DELETE" }),
  triggerSchedule: (id: string) =>
    call<ScheduleTriggerResult>(`/schedules/${encodeURIComponent(id)}/trigger`, { method: "POST", body: JSON.stringify({}) }),
  scheduleRuns: async (id: string) => {
    const data = await call<FunctionRun[] | { items: FunctionRun[] }>(`/schedules/${encodeURIComponent(id)}/runs`);
    return Array.isArray(data) ? data : data.items ?? [];
  },

  // Phase 3: events (snapshot; SSE handled in EventsTab)
  events: (id: string, tail = 100) =>
    call<{ events: SandboxEvent[] }>(`/sandboxes/${id}/events?tail=${tail}`),

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

  // Git-driven apps (Vercel/Render-style hosting on persistent sandboxes)
  apps: async () => {
    const data = await call<{ items: AppInfo[]; count: number } | AppInfo[]>("/apps");
    return Array.isArray(data) ? data : data.items ?? [];
  },
  getApp: (id: string) => call<AppInfo>(`/apps/${encodeURIComponent(id)}`),
  createApp: (req: CreateAppRequest) =>
    call<AppInfo>("/apps", { method: "POST", body: JSON.stringify(req) }),
  updateApp: (id: string, body: UpdateAppRequest) =>
    call<AppInfo>(`/apps/${encodeURIComponent(id)}`, { method: "PATCH", body: JSON.stringify(body) }),
  deleteApp: (id: string) =>
    call<void>(`/apps/${encodeURIComponent(id)}`, { method: "DELETE" }),

  // --- GitHub App connect / repo picker ---------------------------------
  // Returns the GitHub install URL to send the browser to. The backend stores a
  // single-use CSRF state bound to the workspace; GitHub redirects back to
  // /v1/github/callback which records the installation and bounces to /apps.
  githubConnectUrl: async (): Promise<string> => {
    const { url } = await call<{ url: string }>("/github/connect");
    return url;
  },
  githubInstallations: async (): Promise<GitHubInstallation[]> => {
    const data = await call<{ installations: GitHubInstallation[] }>("/github/installations");
    return data.installations ?? [];
  },
  githubDisconnect: (installationId: number) =>
    call<void>(`/github/installations/${encodeURIComponent(String(installationId))}`, {
      method: "DELETE",
    }),
  githubRepos: async (installationId: number): Promise<GitHubRepo[]> => {
    const data = await call<{ repos: GitHubRepo[] }>(
      `/github/repos?installation_id=${encodeURIComponent(String(installationId))}`,
    );
    return data.repos ?? [];
  },

  deployApp: (id: string, gitRef?: string) =>
    call<DeploymentInfo>(`/apps/${encodeURIComponent(id)}/deploys`, {
      method: "POST",
      body: JSON.stringify(gitRef ? { git_ref: gitRef } : {}),
    }),
  appDeploys: async (id: string) => {
    const data = await call<{ items: DeploymentInfo[] } | DeploymentInfo[]>(
      `/apps/${encodeURIComponent(id)}/deploys`
    );
    return Array.isArray(data) ? data : data.items ?? [];
  },
  appDeployment: (appId: string, deploymentId: string) =>
    call<DeploymentInfo>(
      `/apps/${encodeURIComponent(appId)}/deploys/${encodeURIComponent(deploymentId)}`
    ),
  // Roll back an app. Without deploymentId, the backend redeploys the most
  // recent superseded deploy; pass a deploymentId to roll back to a specific one.
  rollbackApp: (id: string, deploymentId?: string) =>
    call<DeploymentInfo>(`/apps/${encodeURIComponent(id)}/rollback`, {
      method: "POST",
      body: JSON.stringify(deploymentId ? { deployment_id: deploymentId } : {}),
    }),
  // Stream a deployment's build/run log over SSE. Calls onLine for every emitted
  // log line and resolves when the deploy reaches a terminal state (the server
  // sends `event: done`) or the stream closes. Pass an AbortSignal to cancel.
  // The backend emits each line as `data: <line>\n\n` and ends with
  // `event: done\ndata: end\n\n`; auth is header-based so we use fetch + a
  // ReadableStream reader (EventSource can't set headers).
  appDeployLogs: async (
    appId: string,
    deployID: string,
    onLine: (line: string) => void,
    signal?: AbortSignal,
  ): Promise<void> => {
    const r = await fetch(
      `${API_BASE}/v1/apps/${encodeURIComponent(appId)}/deploys/${encodeURIComponent(deployID)}/logs`,
      { headers: await getAuthHeaders(), signal },
    );
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
        let terminal = false;
        for (const ln of ev.split("\n")) {
          if (ln.startsWith("event:") && ln.slice(6).trim() === "done") terminal = true;
          else if (ln.startsWith("data:")) {
            const data = ln.slice(5).replace(/^ /, "");
            if (data !== "end") onLine(data);
          }
        }
        if (terminal) return;
      }
    }
  },

  // App runtime logs — the app process's own stdout/stderr captured to the
  // guest file /var/log/pandastack-app.log (NOT the Firecracker console). The
  // server tails into the live current sandbox, so this survives blue-green
  // redeploys. Snapshot fetch returns the last N lines as plain text.
  appRuntimeLogs: (id: string) =>
    call<string>(`/apps/${encodeURIComponent(id)}/runtime-logs`),

  // Live-follow the app's runtime log over SSE. Calls onLine per emitted line;
  // resolves when the stream closes. Pass an AbortSignal to cancel. The backend
  // emits each line as `data: <line>\n\n` (plain text, not JSON).
  streamAppRuntimeLogs: async (
    id: string,
    onLine: (line: string) => void,
    signal?: AbortSignal,
  ): Promise<void> => {
    const r = await fetch(
      `${API_BASE}/v1/apps/${encodeURIComponent(id)}/runtime-logs?follow=1`,
      { headers: await getAuthHeaders(), signal },
    );
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
        for (const ln of ev.split("\n")) {
          if (ln.startsWith("data:")) {
            const data = ln.slice(5).replace(/^ /, "");
            if (data !== "end") onLine(data);
          }
        }
      }
    }
  },

  // Stream a sandbox's runtime console over SSE (firecracker.log, follow mode).
  // Calls onLine for every emitted line; resolves when the stream closes. Pass
  // an AbortSignal to cancel. The backend emits `event: <name>\ndata: <json>\n\n`
  // where the JSON payload carries a `line` field — same shape the sandbox
  // detail logs viewer consumes.
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
    cpu = 1,
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

  // Phase 4: simple REPL (stateless single call)
  replOnce: (id: string, language: string, code: string) =>
    call<{ language: string; stdout: string; stderr: string; exit_code: number }>(
      `/sandboxes/${id}/repl`,
      {
        method: "POST",
        body: JSON.stringify({ language, code }),
      }
    ),

  // Phase 5: persistent REPL sessions
  replSessions: (id: string) =>
    call<ReplSession[]>(`/sandboxes/${id}/repl/sessions`),
  replCreateSession: (id: string, language = "python") =>
    call<ReplSession>(`/sandboxes/${id}/repl/sessions`, {
      method: "POST",
      body: JSON.stringify({ language }),
    }),
  replRun: (id: string, sid: string, code: string, timeout_ms = 30000) =>
    call<ReplRunResult>(`/sandboxes/${id}/repl/sessions/${sid}/run`, {
      method: "POST",
      body: JSON.stringify({ code, timeout_ms }),
    }),
  replDeleteSession: (id: string, sid: string) =>
    call<void>(`/sandboxes/${id}/repl/sessions/${sid}`, { method: "DELETE" }),

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
// by stripping a leading "api." label (api.pandastack.ai -> pandastack.ai).
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
    return "pandastack.ai";
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

export type ReplSession = {
  id: string;
  sandbox_id: string;
  language: string;
  created_at: string;
  cells?: number;
};

export type ReplRunResult = {
  stdout: string;
  stderr: string;
  exit: number;
  duration_ms: number;
};

export type ForkResult = {
  parent_id: string;
  snapshot_id: string;
  children: string[];
  at: string;
};

export type SandboxEvent = {
  time: string;
  sandbox_id: string;
  type: string;
  payload?: Record<string, unknown>;
};

