// SPDX-License-Identifier: Apache-2.0
"use client";

import { useEffect, useMemo, useState, useTransition } from "react";
import Link from "next/link";
import { toast } from "sonner";
import { AppWindow, ChevronDown, ChevronRight, Copy, ExternalLink, GitBranch, Plus, RefreshCw, Rocket, RotateCcw, ScrollText, Settings, Trash2, Unplug } from "lucide-react";
import { api, type AppInfo, type CreateAppRequest, type GitHubInstallation, type GitHubRepo } from "@/lib/api";
import { Badge, Btn, Card, Input, PageHeader, Select, Table, Td, Th, useConfirm } from "@/components/ui";
import { ErrorState, LoadingTable, RelativeTime, RowAction, RowActions, StatusBadge } from "@/components/list-quality";
import { DeployLogsDrawer } from "./_components/DeployLogsDrawer";

function msg(e: unknown) { return e instanceof Error ? e.message : String(e); }

// Surface the resolved runtime for the list view. Prefer an explicit runtime
// pin, fall back to the auto-detected framework, else show "auto".
function runtimeLabel(a: AppInfo): string {
  const rt = (a.runtime ?? "").trim();
  if (rt) return a.runtime_version?.trim() ? `${rt} ${a.runtime_version.trim()}` : rt;
  if (a.framework?.trim()) return a.framework.trim();
  return "auto";
}

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

type AppForm = {
  name: string;
  git_url: string;
  git_branch: string;
  runtime: string;
  runtime_version: string;
  install_command: string;
  build_command: string;
  start_command: string;
  root_directory: string;
  port: string;
  // Set when a repo is picked from a connected GitHub installation. These let
  // the backend clone via an installation token (no PAT) and link the app to
  // the repo so push webhooks can auto-deploy it.
  github_installation_id?: number;
  github_repo_id?: number;
  github_repo_full_name?: string;
  auto_deploy: boolean;
};

const EMPTY_FORM: AppForm = {
  name: "", git_url: "", git_branch: "", runtime: "", runtime_version: "",
  install_command: "", build_command: "", start_command: "", root_directory: "", port: "",
  github_installation_id: undefined, github_repo_id: undefined, github_repo_full_name: undefined,
  auto_deploy: true,
};

export default function AppsPage() {
  const [items, setItems] = useState<AppInfo[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [pending, start] = useTransition();
  const [showCreate, setShowCreate] = useState(false);
  const [showAdvanced, setShowAdvanced] = useState(false);
  const [form, setForm] = useState<AppForm>(EMPTY_FORM);
  const [search, setSearch] = useState("");
  const [logsTarget, setLogsTarget] = useState<{ appId: string; appName: string; deploymentId?: string } | null>(null);
  const [installations, setInstallations] = useState<GitHubInstallation[]>([]);
  const [ghLoading, setGhLoading] = useState(false);
  const [selectedInstall, setSelectedInstall] = useState<number | null>(null);
  const [repos, setRepos] = useState<GitHubRepo[]>([]);
  const [reposLoading, setReposLoading] = useState(false);
  const [repoFilter, setRepoFilter] = useState("");
  const confirm = useConfirm();
  const set = (k: keyof AppForm) => (e: React.ChangeEvent<HTMLInputElement | HTMLSelectElement>) =>
    setForm((f) => ({ ...f, [k]: e.target.value }));

  const refresh = async () => {
    setError(null);
    try { setItems((await api.apps()) ?? []); }
    catch (e) { const m = msg(e); setError(m); toast.error("Failed to fetch apps: " + m); }
    finally { setLoading(false); }
  };

  // Load connected GitHub installations (org-scoped). Non-fatal if it fails —
  // the page still works with plain public Git URLs.
  const loadInstallations = async () => {
    setGhLoading(true);
    try { setInstallations((await api.githubInstallations()) ?? []); }
    catch { /* GitHub App may be unconfigured locally; ignore */ }
    finally { setGhLoading(false); }
  };

  useEffect(() => {
    refresh();
    void loadInstallations();
    const t = setInterval(refresh, 4000);
    return () => clearInterval(t);
  }, []);

  // Surface the callback redirect result (?github=connected | error) once on mount,
  // then strip the query so a refresh doesn't re-toast. When the callback tells
  // us which installation was connected/updated, auto-select it and load its
  // repos so the picker is immediately usable (no extra click on the chip).
  useEffect(() => {
    const params = new URLSearchParams(window.location.search);
    const status = params.get("github");
    if (!status) return;
    if (status === "connected") {
      toast.success("GitHub connected — pick a repository below");
      setShowCreate(true);
      void loadInstallations();
      const instId = Number(params.get("installation_id"));
      if (Number.isInteger(instId) && instId > 0) pickInstallation(instId);
    } else if (status === "error") {
      toast.error("GitHub connection failed: " + (params.get("reason") || "unknown error"));
    }
    const url = new URL(window.location.href);
    ["github", "reason", "installation_id"].forEach((k) => url.searchParams.delete(k));
    window.history.replaceState({}, "", url.pathname + url.search);
  }, []);

  // Kick off the GitHub App install + OAuth flow. The backend returns the
  // github.com/apps/<slug>/installations/new URL carrying a one-time state.
  const connectGitHub = () => start(async () => {
    const id = toast.loading("Opening GitHub…");
    try {
      const url = await api.githubConnectUrl();
      toast.dismiss(id);
      window.location.href = url;
    } catch (e) { toast.error("Could not start GitHub connect: " + msg(e), { id }); }
  });

  const disconnectGitHub = (installationId: number) => start(async () => {
    const id = toast.loading("Disconnecting…");
    try {
      await api.githubDisconnect(installationId);
      toast.success("GitHub account disconnected", { id });
      if (selectedInstall === installationId) { setSelectedInstall(null); setRepos([]); }
      await loadInstallations();
    } catch (e) { toast.error("Disconnect failed: " + msg(e), { id }); }
  });

  // Load repos for an installation and remember the selection so the repo
  // picker can render.
  const pickInstallation = (installationId: number) => {
    setSelectedInstall(installationId);
    setRepos([]);
    setRepoFilter("");
    setReposLoading(true);
    void (async () => {
      try { setRepos((await api.githubRepos(installationId)) ?? []); }
      catch (e) { toast.error("Failed to list repos: " + msg(e)); }
      finally { setReposLoading(false); }
    })();
  };

  // Fill the create form from a picked repo: clone URL + branch, and the link
  // fields the backend uses for installation-token clone + webhook auto-deploy.
  const pickRepo = (repo: GitHubRepo) => {
    setForm((f) => ({
      ...f,
      name: f.name.trim() || repo.full_name.split("/").pop() || f.name,
      git_url: repo.clone_url,
      git_branch: repo.default_branch || f.git_branch,
      github_installation_id: selectedInstall ?? undefined,
      github_repo_id: repo.id,
      github_repo_full_name: repo.full_name,
    }));
    toast.success(`Selected ${repo.full_name}`);
  };

  const create = (e: React.FormEvent) => {
    e.preventDefault();
    if (!form.name.trim() || !form.git_url.trim()) { toast.error("Name and Git URL are required"); return; }
    const port = form.port.trim() ? Number(form.port.trim()) : undefined;
    if (port !== undefined && (!Number.isInteger(port) || port < 1 || port > 65535)) {
      toast.error("Port must be a number between 1 and 65535"); return;
    }
    const req: CreateAppRequest = { name: form.name.trim(), git_url: form.git_url.trim() };
    const opt: Partial<CreateAppRequest> = {
      git_branch: form.git_branch.trim() || undefined,
      runtime: form.runtime.trim() || undefined,
      runtime_version: form.runtime_version.trim() || undefined,
      install_command: form.install_command.trim() || undefined,
      build_command: form.build_command.trim() || undefined,
      start_command: form.start_command.trim() || undefined,
      root_directory: form.root_directory.trim() || undefined,
      port,
      github_installation_id: form.github_installation_id,
      github_repo_id: form.github_repo_id,
      github_repo_full_name: form.github_repo_full_name,
      // Only meaningful when linked to a repo; harmless otherwise.
      auto_deploy: form.github_repo_id ? form.auto_deploy : undefined,
    };
    for (const [k, v] of Object.entries(opt)) if (v !== undefined) (req as Record<string, unknown>)[k] = v;
    start(async () => {
      const id = toast.loading("Creating app…");
      try {
        const app = await api.createApp(req);
        setShowCreate(false);
        setShowAdvanced(false);
        setForm(EMPTY_FORM);
        toast.success("App created — deploy it to go live", { id });
        await refresh();
        deploy(app.id, app.name);
      } catch (e) { toast.error("Create failed: " + msg(e), { id }); }
    });
  };

  const deploy = (appId: string, appName: string) => start(async () => {
    const id = toast.loading("Triggering deployment…");
    try {
      const dep = await api.deployApp(appId);
      toast.success("Deployment queued — streaming build logs", { id });
      setLogsTarget({ appId, appName, deploymentId: dep?.id });
      await refresh();
    }
    catch (e) { toast.error("Deploy failed: " + msg(e), { id }); }
  });

  const rollback = (appId: string) => start(async () => {
    const id = toast.loading("Rolling back…");
    try { await api.rollbackApp(appId); toast.success("Rollback queued", { id }); await refresh(); }
    catch (e) { toast.error("Rollback failed: " + msg(e), { id }); }
  });

  const remove = (appId: string) => start(async () => {
    const id = toast.loading("Deleting app…");
    try { await api.deleteApp(appId); toast.success("App deleted", { id }); await refresh(); }
    catch (e) { toast.error("Delete failed: " + msg(e), { id }); }
  });

  const filtered = useMemo(() => {
    const q = search.toLowerCase().trim();
    return items.filter((a) => !q || a.name.toLowerCase().includes(q) || a.id.toLowerCase().includes(q) || (a.git_url ?? "").toLowerCase().includes(q) || (a.status ?? "").toLowerCase().includes(q));
  }, [items, search]);

  return <>
    <PageHeader
      title="Apps"
      description="Git-driven app hosting — push a repo, get a live URL."
      badge={<Badge variant="warning">Beta</Badge>}
      actions={<Btn variant="primary" size="sm" icon={<Plus size={13} />} onClick={() => setShowCreate((v) => !v)}>{items.length === 0 ? "Deploy your first app" : "New app"}</Btn>}
    />

    {showCreate && (
      <Card className="mb-4 p-4">
        {/* GitHub App connect + repo picker. Lets private repos clone via an
            installation token and links the app for push auto-deploys. */}
        <div className="mb-3 flex items-center justify-between">
          <div className="text-[12px] font-semibold" style={{ color: "var(--text-secondary)" }}>Connect GitHub</div>
          <Btn size="sm" variant="ghost" icon={<GitBranch size={13} />} onClick={connectGitHub} disabled={pending}>
            {installations.length === 0 ? "Connect GitHub" : "Add another account"}
          </Btn>
        </div>

        {ghLoading && installations.length === 0 ? (
          <p className="mb-3 text-[11px]" style={{ color: "var(--text-muted)" }}>Loading connected accounts…</p>
        ) : installations.length === 0 ? (
          <p className="mb-3 text-[11px]" style={{ color: "var(--text-muted)" }}>
            No GitHub accounts connected. Connect to deploy private repositories — or paste a public Git URL below.
          </p>
        ) : (
          <div className="mb-4 space-y-2">
            <div className="flex flex-wrap items-center gap-2">
              {installations.map((inst) => (
                <div
                  key={inst.installation_id}
                  className="inline-flex items-center gap-1.5 rounded-md px-2 py-1 text-[12px]"
                  style={{
                    background: selectedInstall === inst.installation_id ? "var(--accent-subtle, var(--bg-elevated))" : "var(--bg-elevated)",
                    border: `1px solid ${selectedInstall === inst.installation_id ? "var(--accent, var(--border-default))" : "var(--border-default)"}`,
                  }}
                >
                  <button type="button" onClick={() => pickInstallation(inst.installation_id)} className="inline-flex items-center gap-1.5 font-medium" style={{ color: "var(--text-primary)" }}>
                    <GitBranch size={12} />{inst.account_login}
                    <span className="text-[10px]" style={{ color: "var(--text-muted)" }}>{inst.account_type}</span>
                  </button>
                  <button
                    type="button"
                    title="Disconnect"
                    onClick={async () => {
                      const ok = await confirm({ title: `Disconnect ${inst.account_login}?`, description: "Apps linked to this account will stop auto-deploying on push.", confirmLabel: "Disconnect", destructive: true });
                      if (ok) disconnectGitHub(inst.installation_id);
                    }}
                    style={{ color: "var(--text-muted)" }}
                  >
                    <Unplug size={12} />
                  </button>
                </div>
              ))}
            </div>

            {selectedInstall !== null && (
              <div className="space-y-2">
                <div className="flex items-center gap-2">
                  <input
                    value={repoFilter}
                    onChange={(e) => setRepoFilter(e.target.value)}
                    placeholder="Search repositories…"
                    className="w-full max-w-xs rounded-md px-3 py-1.5 text-[12px] focus:outline-none"
                    style={{ background: "var(--bg-elevated)", border: "1px solid var(--border-default)", color: "var(--text-primary)" }}
                  />
                  {/* Re-list repositories, e.g. after changing the App's repo access on GitHub. */}
                  <Btn
                    size="sm"
                    variant="ghost"
                    icon={<RefreshCw size={12} />}
                    onClick={() => pickInstallation(selectedInstall)}
                    disabled={reposLoading}
                  >
                    Refresh
                  </Btn>
                </div>
                {reposLoading ? (
                  <p className="text-[11px]" style={{ color: "var(--text-muted)" }}>Loading repositories…</p>
                ) : repos.length === 0 ? (
                  <p className="text-[11px]" style={{ color: "var(--text-muted)" }}>No repositories accessible. Grant the GitHub App access to repos on GitHub, then hit Refresh.</p>
                ) : (
                  (() => {
                    const q = repoFilter.toLowerCase().trim();
                    const visible = repos.filter((r) => !q || r.full_name.toLowerCase().includes(q));
                    return visible.length === 0 ? (
                      <p className="text-[11px]" style={{ color: "var(--text-muted)" }}>No repositories match “{repoFilter}”.</p>
                    ) : (
                      <div className="max-h-48 overflow-y-auto rounded-md" style={{ border: "1px solid var(--border-subtle)" }}>
                        {visible.map((repo) => {
                          const active = form.github_repo_id === repo.id;
                          return (
                            <button
                              key={repo.id}
                              type="button"
                              onClick={() => pickRepo(repo)}
                              className="flex w-full items-center justify-between px-3 py-1.5 text-left text-[12px]"
                              style={{
                                background: active ? "var(--bg-elevated)" : "transparent",
                                borderBottom: "1px solid var(--border-subtle)",
                                color: "var(--text-primary)",
                              }}
                            >
                              <span className="font-mono">{repo.full_name}</span>
                              <span className="inline-flex items-center gap-2">
                                {repo.private && <Badge variant="default">private</Badge>}
                                <span className="text-[10px]" style={{ color: "var(--text-muted)" }}>{repo.default_branch}</span>
                                {active && <span style={{ color: "var(--accent, var(--text-primary))" }}>✓</span>}
                              </span>
                            </button>
                          );
                        })}
                      </div>
                    );
                  })()
                )}
              </div>
            )}
          </div>
        )}

        <div className="mb-3 text-[12px] font-semibold" style={{ color: "var(--text-secondary)" }}>Deploy from Git</div>
        <form onSubmit={create} className="space-y-3">
          <div className="flex flex-wrap items-end gap-3">
            <Input label="Name" placeholder="my-app" value={form.name} onChange={set("name")} className="w-44" />
            <Input label="Git URL" placeholder="https://github.com/me/private-repo" value={form.git_url} onChange={set("git_url")} className="w-72" />
            <Input label="Branch" placeholder="main" value={form.git_branch} onChange={set("git_branch")} className="w-32" />
            <Select label="Runtime" value={form.runtime} onChange={set("runtime")} className="w-36">
              {RUNTIMES.map((r) => <option key={r.value} value={r.value}>{r.label}</option>)}
            </Select>
            <Input label="Version" placeholder="auto" value={form.runtime_version} onChange={set("runtime_version")} className="w-24" />
          </div>

          <button
            type="button"
            onClick={() => setShowAdvanced((v) => !v)}
            className="inline-flex items-center gap-1 text-[11px] font-medium"
            style={{ color: "var(--text-secondary)" }}
          >
            {showAdvanced ? <ChevronDown size={13} /> : <ChevronRight size={13} />}
            Build &amp; run commands (optional)
          </button>

          {showAdvanced && (
            <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
              <Input label="Install command" placeholder="npm install · pip install -r requirements.txt" value={form.install_command} onChange={set("install_command")} />
              <Input label="Build command" placeholder="npm run build · (blank if none)" value={form.build_command} onChange={set("build_command")} />
              <Input label="Start command" placeholder="npm start · uvicorn app.main:app --host 0.0.0.0 --port $PORT" value={form.start_command} onChange={set("start_command")} />
              <Input label="Root directory" placeholder="(repo root)" value={form.root_directory} onChange={set("root_directory")} />
              <Input label="Port" type="number" inputMode="numeric" placeholder="auto ($PORT)" value={form.port} onChange={set("port")} className="w-32" />
            </div>
          )}

          {form.github_repo_id ? (
            <label className="flex items-center gap-2 pt-1 text-[12px]" style={{ color: "var(--text-secondary)" }}>
              <input
                type="checkbox"
                checked={form.auto_deploy}
                onChange={(e) => setForm((f) => ({ ...f, auto_deploy: e.target.checked }))}
              />
              Auto-deploy on push to <code>{form.git_branch || "default branch"}</code>
              <span className="text-[11px]" style={{ color: "var(--text-muted)" }}>· linked to {form.github_repo_full_name}</span>
            </label>
          ) : null}

          <div className="flex items-center gap-3 pt-1">
            <Btn variant="primary" type="submit" disabled={pending}>{pending ? "Creating…" : "Create & deploy"}</Btn>
            <Btn variant="ghost" onClick={() => setShowCreate(false)}>Cancel</Btn>
          </div>
        </form>
        <p className="mt-3 text-[11px]" style={{ color: "var(--text-muted)" }}>Language-agnostic — Node, Python, Go, Ruby, Rust and more run on the universal <code>base</code> runtime. Leave fields blank to auto-detect the runtime and commands from your repo; override any of them here or via <code>pandastack.json</code>. First deploy clones, installs, builds, and starts the app on a persistent sandbox. Public repos clone without auth; <strong>private GitHub repos</strong> clone automatically once the PandaStack GitHub App is installed on the repo.</p>
      </Card>
    )}

    {error && <div className="mb-3"><ErrorState error={error} onRetry={() => void refresh()} /></div>}

    <div className="mb-3 flex items-center gap-2">
      <input
        value={search}
        onChange={(e) => setSearch(e.target.value)}
        placeholder="Filter apps…"
        className="w-full max-w-xs rounded-md px-3 py-1.5 text-[13px] focus:outline-none"
        style={{ background: "var(--bg-elevated)", border: "1px solid var(--border-default)", color: "var(--text-primary)" }}
      />
      <Btn size="sm" variant="ghost" icon={<RefreshCw size={12} />} onClick={refresh} disabled={pending} className="ml-auto">Refresh</Btn>
    </div>

    <Card>
      {loading ? <LoadingTable cols={6} /> : (
        <Table>
          <thead><tr>
            <Th>Name</Th>
            <Th>Repository</Th>
            <Th>Runtime</Th>
            <Th>Status</Th>
            <th className="hidden px-4 py-2.5 text-left text-[11px] font-medium uppercase tracking-wider lg:table-cell" style={{ color: "var(--text-muted)", borderBottom: "1px solid var(--border-subtle)" }}>Created</th>
            <Th right>Actions</Th>
          </tr></thead>
          <tbody>{filtered.map((a) => (
            <tr key={a.id} className="group">
              <Td>
                <Link href={`/apps/${a.id}`} className="font-medium hover:underline" style={{ color: "var(--text-primary)" }}>{a.name}</Link>
                {a.url && (
                  <a href={a.url} target="_blank" rel="noreferrer" className="ml-2 inline-flex items-center gap-1 text-[11px]" style={{ color: "var(--text-muted)" }}>
                    <ExternalLink size={11} />Open
                  </a>
                )}
              </Td>
              <Td muted><span className="font-mono text-[12px]">{a.github_repo_full_name ?? a.git_url}</span>{a.git_branch ? <span className="ml-1 text-[11px]" style={{ color: "var(--text-muted)" }}>@{a.git_branch}</span> : null}{a.auto_deploy ? <span className="ml-1.5 inline-flex items-center gap-0.5 text-[10px]" style={{ color: "var(--text-muted)" }} title="Auto-deploys on push"><GitBranch size={10} />auto</span> : null}</Td>
              <Td muted>{runtimeLabel(a)}</Td>
              <Td><StatusBadge value={a.status} /></Td>
              <Td muted className="hidden lg:table-cell">{a.created_at ? <RelativeTime value={a.created_at} /> : "—"}</Td>
              <Td right>
                <RowActions>
                  <RowAction onClick={() => { window.location.href = `/apps/${a.id}`; }}><Settings size={12} />Manage</RowAction>
                  <RowAction onClick={() => deploy(a.id, a.name)}><Rocket size={12} />Deploy</RowAction>
                  <RowAction onClick={() => setLogsTarget({ appId: a.id, appName: a.name })}><ScrollText size={12} />Logs</RowAction>
                  <RowAction onClick={() => rollback(a.id)}><RotateCcw size={12} />Rollback</RowAction>
                  {a.url && <RowAction onClick={() => void navigator.clipboard.writeText(a.url!).then(() => toast.success("Copied URL"))}><Copy size={12} />Copy URL</RowAction>}
                  <RowAction onClick={() => void navigator.clipboard.writeText(a.id).then(() => toast.success("Copied"))}><Copy size={12} />Copy ID</RowAction>
                  <RowAction destructive onClick={async () => {
                    const ok = await confirm({ title: `Delete app ${a.name}?`, description: "This tears down the app and its runtime sandbox. This cannot be undone.", confirmLabel: "Delete", destructive: true });
                    if (ok) remove(a.id);
                  }}><Trash2 size={12} />Delete</RowAction>
                </RowActions>
              </Td>
            </tr>
          ))}</tbody>
        </Table>
      )}
    </Card>

    {logsTarget && (
      <DeployLogsDrawer
        appId={logsTarget.appId}
        appName={logsTarget.appName}
        deploymentId={logsTarget.deploymentId}
        onClose={() => setLogsTarget(null)}
      />
    )}
  </>;
}
