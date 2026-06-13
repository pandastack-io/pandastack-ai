// SPDX-License-Identifier: Apache-2.0
"use client";

import { use, useEffect, useMemo, useRef, useState, useTransition } from "react";
import Link from "next/link";
import { toast } from "sonner";
import { AlertTriangle, ArrowLeft, Copy, Eye, EyeOff, ExternalLink, History, Plus, Radio, RefreshCw, Rocket, RotateCcw, ScrollText, Terminal, Trash2 } from "lucide-react";
import { api, type AppInfo, type DeploymentInfo, type UpdateAppRequest } from "@/lib/api";
import { Badge, Btn, Card, Input, Kv, PageHeader, Select, Skeleton, Table, Td, Th, useConfirm } from "@/components/ui";
import { ErrorState, RelativeTime, StatusBadge } from "@/components/list-quality";
import { DeployLogsDrawer } from "../_components/DeployLogsDrawer";

function msg(e: unknown) { return e instanceof Error ? e.message : String(e); }

// Compact human duration between two ISO timestamps, e.g. "1m 4s" / "820ms".
function fmtDuration(start?: string, end?: string): string {
  if (!start || !end) return "—";
  const ms = new Date(end).getTime() - new Date(start).getTime();
  if (!Number.isFinite(ms) || ms < 0) return "—";
  if (ms < 1000) return `${ms}ms`;
  const s = Math.round(ms / 1000);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  return `${m}m ${s % 60}s`;
}

// Terminal deploy states never resolve to "still running".
const TERMINAL = new Set(["live", "failed", "superseded", "rolled_back", "cancelled"]);

// Runtimes pre-warmed in the universal `base` template (mise-managed). Empty =
// auto-detect from the repo (framework heuristics + any mise/.tool-versions pin).
const RUNTIMES = [
  { value: "", label: "Auto-detect" },
  { value: "node", label: "Node.js" },
  { value: "python", label: "Python" },
  { value: "go", label: "Go" },
  { value: "ruby", label: "Ruby" },
  { value: "rust", label: "Rust" },
  { value: "bun", label: "Bun" },
  { value: "deno", label: "Deno" },
];

// Editable config mirror of AppInfo (everything PATCH /v1/apps/{id} accepts that
// makes sense to edit here). Held as strings for the form; coerced on save.
type SettingsForm = {
  git_branch: string;
  runtime: string;
  runtime_version: string;
  install_command: string;
  build_command: string;
  start_command: string;
  root_directory: string;
  port: string;
};

function formFromApp(a: AppInfo): SettingsForm {
  return {
    git_branch: a.git_branch ?? "",
    runtime: a.runtime ?? "",
    runtime_version: a.runtime_version ?? "",
    install_command: a.install_command ?? "",
    build_command: a.build_command ?? "",
    start_command: a.start_command ?? "",
    root_directory: a.root_directory ?? "",
    port: a.port ? String(a.port) : "",
  };
}

type EnvRow = { key: string; value: string; reveal?: boolean };

function envRowsFromApp(a: AppInfo): EnvRow[] {
  const e = a.env ?? {};
  const rows = Object.entries(e).map(([key, value]) => ({ key, value, reveal: false }));
  return rows.length ? rows : [{ key: "", value: "", reveal: false }];
}

export default function AppDetail({ params }: { params: Promise<{ id: string }> }) {
  const { id } = use(params);
  const [app, setApp] = useState<AppInfo | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [pending, start] = useTransition();
  const [logsTarget, setLogsTarget] = useState<{ deploymentId?: string } | null>(null);
  const confirm = useConfirm();

  const [form, setForm] = useState<SettingsForm>({
    git_branch: "", runtime: "", runtime_version: "", install_command: "",
    build_command: "", start_command: "", root_directory: "", port: "",
  });
  const [envRows, setEnvRows] = useState<EnvRow[]>([{ key: "", value: "", reveal: false }]);

  // Deployment history + a per-app "deploy a specific ref" override.
  const [deploys, setDeploys] = useState<DeploymentInfo[]>([]);
  const [deployRef, setDeployRef] = useState("");

  // On-demand runtime (sandbox console) logs — snapshot fetch or live SSE follow.
  const [runtimeOpen, setRuntimeOpen] = useState(false);
  const [runtimeLines, setRuntimeLines] = useState<string[]>([]);
  const [runtimeLoading, setRuntimeLoading] = useState(false);
  const [runtimeFollow, setRuntimeFollow] = useState(false);
  const runtimeAbort = useRef<AbortController | null>(null);

  const refreshDeploys = async () => {
    try { setDeploys(await api.appDeploys(id)); }
    catch { /* non-fatal — history is supplementary */ }
  };

  const refresh = async () => {
    setError(null);
    try {
      const a = await api.getApp(id);
      setApp(a);
      setForm(formFromApp(a));
      setEnvRows(envRowsFromApp(a));
      await refreshDeploys();
    } catch (e) {
      const m = msg(e); setError(m); toast.error("Failed to load app: " + m);
    } finally {
      setLoading(false);
    }
  };

  const loadRuntimeLogs = async () => {
    if (!app?.sandbox_id) return;
    setRuntimeLoading(true);
    try { const t = await api.appRuntimeLogs(app.id); setRuntimeLines((typeof t === "string" ? t : "").split("\n")); }
    catch (e) { setRuntimeLines(["Failed to load runtime logs: " + msg(e)]); }
    finally { setRuntimeLoading(false); }
  };

  // Initial load only — avoid clobbering in-flight edits with a poll. The user
  // re-pulls server state explicitly via Refresh or after a save.
  useEffect(() => { void refresh(); /* eslint-disable-next-line react-hooks/exhaustive-deps */ }, [id]);

  const set = (k: keyof SettingsForm) => (e: React.ChangeEvent<HTMLInputElement | HTMLSelectElement>) =>
    setForm((f) => ({ ...f, [k]: e.target.value }));

  // Build the env map from rows: trim keys, drop empties, last-write-wins on dups.
  const buildEnv = (): Record<string, string> | null => {
    const out: Record<string, string> = {};
    for (const { key, value } of envRows) {
      const k = key.trim();
      if (!k) continue;
      out[k] = value;
    }
    return out;
  };

  // Compose a minimal PATCH: only fields that actually changed vs the loaded app.
  const buildPatch = (includeEnv: boolean): { body: UpdateAppRequest; portError?: string } => {
    if (!app) return { body: {} };
    const body: UpdateAppRequest = {};
    const trimEq = (a: string, b?: string) => a.trim() === (b ?? "").trim();
    if (!trimEq(form.git_branch, app.git_branch)) body.git_branch = form.git_branch.trim();
    if (!trimEq(form.runtime, app.runtime)) body.runtime = form.runtime.trim();
    if (!trimEq(form.runtime_version, app.runtime_version)) body.runtime_version = form.runtime_version.trim();
    if (!trimEq(form.install_command, app.install_command)) body.install_command = form.install_command.trim();
    if (!trimEq(form.build_command, app.build_command)) body.build_command = form.build_command.trim();
    if (!trimEq(form.start_command, app.start_command)) body.start_command = form.start_command.trim();
    if (!trimEq(form.root_directory, app.root_directory)) body.root_directory = form.root_directory.trim();

    const portStr = form.port.trim();
    const currentPort = app.port ? String(app.port) : "";
    if (portStr !== currentPort) {
      if (portStr === "") {
        body.port = 0; // clear → let the platform auto-detect ($PORT)
      } else {
        const p = Number(portStr);
        if (!Number.isInteger(p) || p < 1 || p > 65535) {
          return { body, portError: "Port must be a number between 1 and 65535" };
        }
        body.port = p;
      }
    }

    if (includeEnv) {
      const next = buildEnv();
      if (next) {
        const cur = app.env ?? {};
        const same = JSON.stringify(next) === JSON.stringify(cur)
          // key order shouldn't matter — compare normalized
          || JSON.stringify(Object.fromEntries(Object.entries(next).sort())) === JSON.stringify(Object.fromEntries(Object.entries(cur).sort()));
        if (!same) body.env = next;
      }
    }
    return { body };
  };

  const save = (opts: { redeploy?: boolean } = {}) => {
    const { body, portError } = buildPatch(true);
    if (portError) { toast.error(portError); return; }

    // Dup-key guard for env (surface before we silently collapse).
    const keys = envRows.map((r) => r.key.trim()).filter(Boolean);
    if (new Set(keys).size !== keys.length) { toast.error("Duplicate environment variable keys"); return; }

    if (Object.keys(body).length === 0 && !opts.redeploy) {
      toast.message("No changes to save"); return;
    }

    start(async () => {
      const tid = toast.loading(opts.redeploy ? "Saving & redeploying…" : "Saving changes…");
      try {
        if (Object.keys(body).length > 0) await api.updateApp(id, body);
        if (opts.redeploy) {
          const dep = await api.deployApp(id);
          toast.success("Saved — deployment queued", { id: tid });
          setLogsTarget({ deploymentId: dep?.id });
        } else {
          toast.success("Settings saved — applies on next deploy", { id: tid });
        }
        await refresh();
      } catch (e) {
        toast.error("Save failed: " + msg(e), { id: tid });
      }
    });
  };

  // Deploy the configured branch, or an explicit ref (branch/tag/commit) when
  // provided. Opens the live build-log drawer for the new deployment.
  const deploy = (ref?: string) => start(async () => {
    const r = ref?.trim() || undefined;
    const tid = toast.loading(r ? `Deploying ${r}…` : "Triggering deployment…");
    try {
      const dep = await api.deployApp(id, r);
      toast.success("Deployment queued — streaming build logs", { id: tid });
      setLogsTarget({ deploymentId: dep?.id });
      setDeployRef("");
      await refresh();
    } catch (e) { toast.error("Deploy failed: " + msg(e), { id: tid }); }
  });

  // Roll back to the previous live deploy (no arg) or a specific deployment.
  // Opens the live build-log drawer for the resulting rollback deployment.
  const rollbackTo = (deploymentId?: string) => start(async () => {
    const tid = toast.loading("Rolling back…");
    try {
      const dep = await api.rollbackApp(id, deploymentId);
      toast.success("Rollback queued — streaming build logs", { id: tid });
      if (dep?.id) setLogsTarget({ deploymentId: dep.id });
      await refresh();
    } catch (e) { toast.error("Rollback failed: " + msg(e), { id: tid }); }
  });
  const rollback = () => rollbackTo();

  const remove = () => start(async () => {
    const tid = toast.loading("Deleting app…");
    try {
      await api.deleteApp(id);
      toast.success("App deleted", { id: tid });
      window.location.href = "/apps";
    } catch (e) { toast.error("Delete failed: " + msg(e), { id: tid }); }
  });

  const dirty = useMemo(() => Object.keys(buildPatch(true).body).length > 0, [form, envRows, app]); // eslint-disable-line react-hooks/exhaustive-deps

  // The deploys list is newest-first; surface a failure banner when the most
  // recent deployment failed (older failures stay visible in the history table).
  const latestDeploy = deploys[0];
  const failBanner = latestDeploy && latestDeploy.status === "failed"
    ? (latestDeploy.error?.trim() || "The most recent deployment failed.")
    : null;

  // Auto-refresh the deploy history every 3s while a deployment is in flight,
  // then pull a fresh app record once it settles — but only if the user has no
  // unsaved edits, so polling never clobbers in-progress form changes.
  const inFlight = !!latestDeploy && !TERMINAL.has(latestDeploy.status);
  const wasInFlight = useRef(false);
  useEffect(() => {
    if (!inFlight) {
      if (wasInFlight.current && !dirty) void refresh();
      wasInFlight.current = false;
      return;
    }
    wasInFlight.current = true;
    const t = setInterval(() => { void refreshDeploys(); }, 3000);
    return () => clearInterval(t);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [inFlight, id]);

  // Live-follow runtime logs over SSE when the panel is open and Follow is on.
  // Aborts the stream on toggle-off, sandbox change, or unmount.
  useEffect(() => {
    if (!runtimeOpen || !runtimeFollow || !app?.sandbox_id) return;
    const ac = new AbortController();
    runtimeAbort.current = ac;
    setRuntimeLines([]);
    api.streamAppRuntimeLogs(app.id, (line) => setRuntimeLines((l) => [...l.slice(-500), line]), ac.signal)
      .catch((e) => { if ((e as Error).name !== "AbortError") setRuntimeLines((l) => [...l, "stream error: " + msg(e)]); });
    return () => ac.abort();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [runtimeOpen, runtimeFollow, app?.sandbox_id]);

  const back = (
    <Link href="/apps" className="inline-flex items-center gap-1 text-[12px]" style={{ color: "var(--text-muted)" }}>
      <ArrowLeft size={13} /> Apps
    </Link>
  );

  if (loading) {
    return <>
      <div className="mb-3">{back}</div>
      <Card className="p-4 space-y-3">
        <Skeleton h="h-6" className="w-48" />
        <Skeleton className="w-72" />
        <Skeleton className="w-full" />
      </Card>
    </>;
  }

  if (error || !app) {
    return <>
      <div className="mb-3">{back}</div>
      <ErrorState title="Couldn't load app" error={error ?? "Not found"} onRetry={() => void refresh()} />
    </>;
  }

  return <>
    <div className="mb-3">{back}</div>

    <PageHeader
      title={app.name}
      description={app.git_url}
      badge={<StatusBadge value={app.status} />}
      actions={
        <div className="flex items-center gap-2">
          {app.url && (
            <Btn variant="secondary" size="sm" icon={<ExternalLink size={13} />} onClick={() => window.open(app.url!, "_blank", "noopener")}>Open</Btn>
          )}
          <Btn variant="ghost" size="sm" icon={<ScrollText size={13} />} onClick={() => setLogsTarget({})}>Logs</Btn>
          <Btn variant="ghost" size="sm" icon={<RotateCcw size={13} />} onClick={rollback} disabled={pending}>Rollback</Btn>
          <Btn variant="primary" size="sm" icon={<Rocket size={13} />} onClick={() => deploy()} disabled={pending}>Deploy</Btn>
        </div>
      }
    />

    {failBanner && (
      <div className="mb-4 flex items-start gap-2 rounded-md p-3 text-[12px]" style={{ background: "color-mix(in srgb, var(--danger, #ef4444) 12%, transparent)", border: "1px solid color-mix(in srgb, var(--danger, #ef4444) 35%, transparent)", color: "var(--text-primary)" }}>
        <AlertTriangle size={14} className="mt-0.5 shrink-0" style={{ color: "var(--danger, #ef4444)" }} />
        <div className="min-w-0">
          <div className="font-semibold">Last deployment failed</div>
          <div className="mt-0.5 whitespace-pre-wrap break-words font-mono text-[11px]" style={{ color: "var(--text-secondary)" }}>{failBanner}</div>
          {latestDeploy && (
            <button onClick={() => setLogsTarget({ deploymentId: latestDeploy.id })} className="mt-1 inline-flex items-center gap-1 text-[11px] underline" style={{ color: "var(--text-secondary)" }}>
              <ScrollText size={11} />View build logs
            </button>
          )}
        </div>
      </div>
    )}

    {/* Overview — read-only snapshot of the live record. */}
    <Card className="mb-4 p-4">
      <div className="mb-3 flex items-center justify-between">
        <div className="text-[12px] font-semibold" style={{ color: "var(--text-secondary)" }}>Overview</div>
        <Btn size="xs" variant="ghost" icon={<RefreshCw size={11} />} onClick={() => void refresh()} disabled={pending}>Refresh</Btn>
      </div>
      <div className="grid grid-cols-2 gap-4 sm:grid-cols-3 lg:grid-cols-4">
        <Kv k="Repository" v={app.git_url} />
        <Kv k="Branch" v={app.git_branch || "—"} />
        <Kv k="Runtime" v={app.runtime ? (app.runtime_version ? `${app.runtime} ${app.runtime_version}` : app.runtime) : (app.framework || "auto")} />
        <Kv k="Port" v={app.port ? String(app.port) : "auto"} />
        <Kv k="Resources" v={`${app.cpu} vCPU · ${app.memory_mb} MiB`} />
        <Kv k="Template" v={app.template || "—"} />
        <div>
          <div className="text-[11px] uppercase tracking-wider" style={{ color: "var(--text-muted)" }}>Created</div>
          <div className="text-[13px]" style={{ color: "var(--text-primary)" }}>{app.created_at ? <RelativeTime value={app.created_at} /> : "—"}</div>
        </div>
        <div>
          <div className="text-[11px] uppercase tracking-wider" style={{ color: "var(--text-muted)" }}>Updated</div>
          <div className="text-[13px]" style={{ color: "var(--text-primary)" }}>{app.updated_at ? <RelativeTime value={app.updated_at} /> : "—"}</div>
        </div>
      </div>
      {app.url && (
        <div className="mt-3 flex items-center gap-2 text-[12px]">
          <a href={app.url} target="_blank" rel="noreferrer" className="inline-flex items-center gap-1 font-mono" style={{ color: "var(--text-secondary)" }}>
            <ExternalLink size={11} />{app.url}
          </a>
          <button onClick={() => void navigator.clipboard.writeText(app.url!).then(() => toast.success("Copied URL"))} className="inline-flex items-center gap-1" style={{ color: "var(--text-muted)" }}>
            <Copy size={11} />Copy
          </button>
        </div>
      )}
    </Card>

    {/* Build & runtime settings — editable. */}
    <Card className="mb-4 p-4">
      <div className="mb-1 text-[12px] font-semibold" style={{ color: "var(--text-secondary)" }}>Build &amp; runtime</div>
      <p className="mb-3 text-[11px]" style={{ color: "var(--text-muted)" }}>Changes are saved to the app record and take effect on the <strong>next deploy</strong>.</p>
      <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
        <Input label="Branch" placeholder="main" value={form.git_branch} onChange={set("git_branch")} />
        <div className="flex items-end gap-2">
          <Select label="Runtime" value={form.runtime} onChange={set("runtime")} className="flex-1">
            {RUNTIMES.map((r) => <option key={r.value} value={r.value}>{r.label}</option>)}
          </Select>
          <Input label="Version" placeholder="auto" value={form.runtime_version} onChange={set("runtime_version")} className="w-24" />
        </div>
        <Input label="Install command" placeholder="npm install · pip install -r requirements.txt" value={form.install_command} onChange={set("install_command")} />
        <Input label="Build command" placeholder="npm run build · (blank if none)" value={form.build_command} onChange={set("build_command")} />
        <Input label="Start command" placeholder="npm start · uvicorn app.main:app --host 0.0.0.0 --port $PORT" value={form.start_command} onChange={set("start_command")} />
        <Input label="Root directory" placeholder="(repo root)" value={form.root_directory} onChange={set("root_directory")} />
        <Input label="Port" type="number" inputMode="numeric" placeholder="auto ($PORT)" value={form.port} onChange={set("port")} className="w-40" />
      </div>
    </Card>

    {/* Environment variables — full-map replace on save. */}
    <Card className="mb-4 p-4">
      <div className="mb-1 text-[12px] font-semibold" style={{ color: "var(--text-secondary)" }}>Environment variables</div>
      <p className="mb-3 text-[11px]" style={{ color: "var(--text-muted)" }}>Injected into the build and runtime. Saving replaces the full set; values apply on the next deploy.</p>
      <div className="space-y-2">
        {envRows.map((row, i) => (
          <div key={i} className="flex items-center gap-2">
            <input
              value={row.key}
              onChange={(e) => setEnvRows((rows) => rows.map((r, j) => j === i ? { ...r, key: e.target.value } : r))}
              placeholder="KEY"
              className="w-56 rounded-md px-3 py-1.5 font-mono text-[12px] focus:outline-none"
              style={{ background: "var(--bg-elevated)", border: "1px solid var(--border-default)", color: "var(--text-primary)" }}
            />
            <div className="relative flex-1">
              <input
                value={row.value}
                onChange={(e) => setEnvRows((rows) => rows.map((r, j) => j === i ? { ...r, value: e.target.value } : r))}
                placeholder="value"
                type={row.reveal ? "text" : "password"}
                autoComplete="off"
                spellCheck={false}
                className="w-full rounded-md px-3 py-1.5 pr-9 font-mono text-[12px] focus:outline-none"
                style={{ background: "var(--bg-elevated)", border: "1px solid var(--border-default)", color: "var(--text-primary)" }}
              />
              <button
                type="button"
                onClick={() => setEnvRows((rows) => rows.map((r, j) => j === i ? { ...r, reveal: !r.reveal } : r))}
                className="absolute right-1.5 top-1/2 -translate-y-1/2 rounded p-1 transition-colors hover:bg-white/5"
                style={{ color: "var(--text-muted)" }}
                aria-label={row.reveal ? "Hide value" : "Reveal value"}
              >
                {row.reveal ? <EyeOff size={13} /> : <Eye size={13} />}
              </button>
            </div>
            <button
              onClick={() => setEnvRows((rows) => { const next = rows.filter((_, j) => j !== i); return next.length ? next : [{ key: "", value: "", reveal: false }]; })}
              className="rounded p-1.5 transition-colors hover:bg-white/5"
              style={{ color: "var(--text-muted)" }}
              aria-label="Remove variable"
            >
              <Trash2 size={13} />
            </button>
          </div>
        ))}
      </div>
      <div className="mt-3">
        <Btn size="xs" variant="ghost" icon={<Plus size={11} />} onClick={() => setEnvRows((rows) => [...rows, { key: "", value: "", reveal: true }])}>Add variable</Btn>
      </div>
    </Card>

    {/* Deployment history — newest first. */}
    <Card className="mb-4 p-4">
      <div className="mb-3 flex flex-wrap items-center gap-2">
        <div className="flex items-center gap-1.5 text-[12px] font-semibold" style={{ color: "var(--text-secondary)" }}>
          <History size={13} /> Deployments
        </div>
        <div className="ml-auto flex items-center gap-2">
          <input
            value={deployRef}
            onChange={(e) => setDeployRef(e.target.value)}
            onKeyDown={(e) => { if (e.key === "Enter" && deployRef.trim()) deploy(deployRef); }}
            placeholder="branch / tag / commit"
            className="w-44 rounded-md px-3 py-1.5 font-mono text-[12px] focus:outline-none"
            style={{ background: "var(--bg-elevated)", border: "1px solid var(--border-default)", color: "var(--text-primary)" }}
          />
          <Btn size="xs" variant="secondary" icon={<Rocket size={11} />} disabled={pending || !deployRef.trim()} onClick={() => deploy(deployRef)}>Deploy ref</Btn>
          <Btn size="xs" variant="ghost" icon={<RefreshCw size={11} />} onClick={() => void refreshDeploys()} disabled={pending}>Refresh</Btn>
        </div>
      </div>
      {deploys.length === 0 ? (
        <p className="text-[12px]" style={{ color: "var(--text-muted)" }}>No deployments yet — trigger a deploy to populate history.</p>
      ) : (
        <Table>
          <thead><tr>
            <Th>Status</Th>
            <Th>Commit</Th>
            <Th>Ref</Th>
            <Th>Started</Th>
            <Th>Duration</Th>
            <Th right>Actions</Th>
          </tr></thead>
          <tbody>{deploys.map((d) => (
            <tr key={d.id} className="group align-top">
              <Td>
                <div className="flex items-center gap-2">
                  <StatusBadge value={d.status} />
                  {app.active_deployment_id === d.id && <Badge variant="success">active</Badge>}
                </div>
                {d.status === "failed" && d.error && (
                  <div className="mt-1 max-w-md whitespace-pre-wrap break-words font-mono text-[10px]" style={{ color: "var(--danger, #ef4444)" }}>{d.error}</div>
                )}
              </Td>
              <Td muted><span className="font-mono text-[12px]">{d.git_commit ? d.git_commit.slice(0, 8) : "—"}</span></Td>
              <Td muted><span className="font-mono text-[12px]">{d.git_ref || "—"}</span></Td>
              <Td muted>{d.created_at ? <RelativeTime value={d.created_at} /> : "—"}</Td>
              <Td muted>{TERMINAL.has(d.status) ? fmtDuration(d.created_at, d.finished_at || d.updated_at) : <span style={{ color: "var(--text-muted)" }}>running…</span>}</Td>
              <Td right>
                <div className="flex items-center justify-end gap-1">
                  {app.active_deployment_id !== d.id && !!d.git_commit && d.status !== "failed" && (
                    <Btn size="xs" variant="ghost" icon={<RotateCcw size={11} />} disabled={pending} onClick={async () => {
                      const ok = await confirm({ title: "Roll back to this deployment?", description: `Redeploys commit ${d.git_commit!.slice(0, 8)} (${d.git_ref || "—"}) on a fresh sandbox and flips traffic to it.`, confirmLabel: "Roll back" });
                      if (ok) rollbackTo(d.id);
                    }}>Rollback</Btn>
                  )}
                  <Btn size="xs" variant="ghost" icon={<ScrollText size={11} />} onClick={() => setLogsTarget({ deploymentId: d.id })}>Logs</Btn>
                </div>
              </Td>
            </tr>
          ))}</tbody>
        </Table>
      )}
    </Card>

    {/* Runtime logs — on-demand sandbox console output for the running app. */}
    <Card className="mb-4 p-4">
      <div className="mb-3 flex items-center gap-2">
        <div className="flex items-center gap-1.5 text-[12px] font-semibold" style={{ color: "var(--text-secondary)" }}>
          <Terminal size={13} /> Runtime logs
        </div>
        <div className="ml-auto flex items-center gap-2">
          {runtimeOpen && (
            <Btn size="xs" variant={runtimeFollow ? "primary" : "ghost"} icon={<Radio size={11} />} onClick={() => setRuntimeFollow((f) => !f)} disabled={!app.sandbox_id}>{runtimeFollow ? "Following" : "Follow"}</Btn>
          )}
          {runtimeOpen && !runtimeFollow && (
            <Btn size="xs" variant="ghost" icon={<RefreshCw size={11} />} onClick={() => void loadRuntimeLogs()} disabled={runtimeLoading || !app.sandbox_id}>Refresh</Btn>
          )}
          <Btn size="xs" variant="secondary" onClick={() => { const next = !runtimeOpen; setRuntimeOpen(next); if (next) { if (!runtimeFollow && runtimeLines.length === 0) void loadRuntimeLogs(); } else { setRuntimeFollow(false); } }} disabled={!app.sandbox_id}>
            {runtimeOpen ? "Hide" : "Show logs"}
          </Btn>
        </div>
      </div>
      {!app.sandbox_id ? (
        <p className="text-[12px]" style={{ color: "var(--text-muted)" }}>No runtime sandbox yet — deploy the app to start it.</p>
      ) : runtimeOpen ? (
        <div className="max-h-96 overflow-auto rounded-md px-3 py-2 font-mono text-[11px] leading-relaxed" style={{ background: "var(--bg-elevated)", color: "var(--text-secondary)", border: "1px solid var(--border-subtle)" }}>
          {runtimeLoading && runtimeLines.length === 0 ? <span style={{ color: "var(--text-muted)" }}>Loading…</span>
            : runtimeLines.length > 0 ? <pre className="whitespace-pre-wrap break-words">{runtimeLines.join("\n")}</pre>
            : <span style={{ color: "var(--text-muted)" }}>{runtimeFollow ? "Waiting for output…" : "No console output captured."}</span>}
        </div>
      ) : (
        <p className="text-[12px]" style={{ color: "var(--text-muted)" }}>Console output from the app&apos;s runtime sandbox (<span className="font-mono">{app.sandbox_id}</span>).</p>
      )}
    </Card>

    {/* Save bar. */}
    <Card className="mb-4 flex flex-wrap items-center gap-3 p-4">
      <Btn variant="primary" onClick={() => save()} disabled={pending || !dirty}>{pending ? "Saving…" : "Save changes"}</Btn>
      <Btn variant="secondary" icon={<Rocket size={13} />} onClick={() => save({ redeploy: true })} disabled={pending}>Save &amp; redeploy</Btn>
      <Btn variant="ghost" onClick={() => { setForm(formFromApp(app)); setEnvRows(envRowsFromApp(app)); }} disabled={pending || !dirty}>Reset</Btn>
      <span className="ml-auto text-[11px]" style={{ color: "var(--text-muted)" }}>{dirty ? "Unsaved changes" : "All changes saved"}</span>
    </Card>

    {/* Danger zone. */}
    <Card className="p-4" style={{ borderColor: "var(--border-default)" }}>
      <div className="mb-1 text-[12px] font-semibold" style={{ color: "var(--text-secondary)" }}>Danger zone</div>
      <div className="flex items-center justify-between gap-3">
        <p className="text-[11px]" style={{ color: "var(--text-muted)" }}>Deleting tears down the app and its runtime sandbox. This cannot be undone.</p>
        <Btn variant="danger" size="sm" icon={<Trash2 size={13} />} disabled={pending} onClick={async () => {
          const ok = await confirm({ title: `Delete app ${app.name}?`, description: "This tears down the app and its runtime sandbox. This cannot be undone.", confirmLabel: "Delete", destructive: true });
          if (ok) remove();
        }}>Delete app</Btn>
      </div>
    </Card>

    {logsTarget && (
      <DeployLogsDrawer
        appId={id}
        appName={app.name}
        deploymentId={logsTarget.deploymentId}
        onClose={() => setLogsTarget(null)}
      />
    )}
  </>;
}
