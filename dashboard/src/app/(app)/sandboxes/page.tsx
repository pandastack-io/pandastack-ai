// SPDX-License-Identifier: Apache-2.0
"use client";

import Link from "next/link";
import { useEffect, useMemo, useState, useTransition } from "react";
import { toast } from "sonner";
import { Box, Camera, Copy, Lock, Pause, Play, Plus, RefreshCw, Square, Trash2 } from "lucide-react";
import { api, type Sandbox, type Template } from "@/lib/api";
import { Btn, Card, PageHeader, Select, Table, Td, Th, useConfirm } from "@/components/ui";
import { compareValue, ErrorState, LoadingTable, PaginationBar, RelativeTime, RowAction, RowActions, rowNavProps, SearchInput, SortHeader, StatusBadge, type SortDir, useDebouncedValue, usePagedRows } from "@/components/list-quality";
import { Quickstart } from "@/components/Quickstart";

type SortKey = "id" | "template" | "status" | "cpu" | "created_at";
function msg(e: unknown) { return e instanceof Error ? e.message : String(e); }

// A sandbox that backs a managed feature (a database or a hosted app) owns
// durable state a stray delete would destroy, so it can only be torn down from
// the feature that owns it. The agent enforces this server-side (409); here we
// surface it in the UI by disabling Delete and pointing to the right page.
type Managed = { kind: "database" | "app"; href: string; label: string } | null;
function managedBy(s: Sandbox): Managed {
  if (s.template === "postgres-16") return { kind: "database", href: "/databases", label: "Databases" };
  if (s.metadata?.kind === "app" || s.metadata?.["app.id"]) return { kind: "app", href: "/apps", label: "Apps" };
  return null;
}

export default function SandboxesPage() {
  const [items, setItems] = useState<Sandbox[]>([]);
  const [templates, setTemplates] = useState<Template[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [pending, start] = useTransition();
  const [form, setForm] = useState({ template: "ubuntu-24.04" });
  const [showCreate, setShowCreate] = useState(false);
  const [search, setSearch] = useState("");
  const debouncedSearch = useDebouncedValue(search);
  const [statusFilter, setStatusFilter] = useState("all");
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [sort, setSort] = useState<{ key: SortKey; dir: SortDir }>({ key: "created_at", dir: "desc" });
  const confirm = useConfirm();

  const refresh = async () => {
    setError(null);
    try { setItems((await api.list()) ?? []); }
    catch (e) { const m = msg(e); setError(m); toast.error("Failed to fetch sandboxes: " + m); }
    finally { setLoading(false); }
  };

  useEffect(() => {
    refresh();
    api.templates().then((t) => { setTemplates(t ?? []); if (t?.length) setForm((f) => ({ ...f, template: t[0].name })); }).catch(() => {});
    const t = setInterval(refresh, 3000);
    return () => clearInterval(t);
  }, []);

  const create = (e: React.FormEvent) => { e.preventDefault(); start(async () => { const id = toast.loading("Launching sandbox…"); try { await api.create(form); setShowCreate(false); toast.success("Sandbox launched", { id }); await refresh(); } catch (e) { toast.error("Launch failed: " + msg(e), { id }); } }); };
  const act = (label: string, fn: () => Promise<unknown>) => start(async () => { const id = toast.loading(label + "…"); try { await fn(); toast.success(label + " complete", { id }); await refresh(); } catch (e) { toast.error(label + " failed: " + msg(e), { id }); } });
  const bulkDelete = async () => { const ok = await confirm({ title: `Delete ${selected.size} sandbox${selected.size === 1 ? "" : "es"}?`, description: "These sandboxes will be killed and their disks released. This cannot be undone.", confirmLabel: "Delete", destructive: true }); if (!ok) return; start(async () => { const id = toast.loading(`Deleting ${selected.size}…`); await Promise.allSettled([...selected].map((sid) => api.remove(sid))); setSelected(new Set()); toast.success("Deleted", { id }); await refresh(); }); };

  const filtered = useMemo(() => {
    const q = debouncedSearch.toLowerCase().trim();
    return items.filter((s) => (statusFilter === "all" || s.status === statusFilter) && (!q || s.id.toLowerCase().includes(q) || s.template.toLowerCase().includes(q) || s.status.toLowerCase().includes(q) || s.guest_ip?.includes(q)))
      .sort((a, b) => { const cmp = compareValue(sort.key === "cpu" ? a.cpu : a[sort.key], sort.key === "cpu" ? b.cpu : b[sort.key]); return sort.dir === "asc" ? cmp : -cmp; });
  }, [items, debouncedSearch, statusFilter, sort]);
  const { page, setPage, pageSize, pageRows } = usePagedRows(filtered);
  const toggleSort = (key: SortKey) => setSort((s) => s.key === key ? { key, dir: s.dir === "asc" ? "desc" : "asc" } : { key, dir: key === "created_at" ? "desc" : "asc" });
  // Select-all only covers deletable (non-managed) sandboxes — managed ones
  // can't be bulk-deleted here.
  const selectable = useMemo(() => filtered.filter((s) => !managedBy(s)), [filtered]);
  const toggleAll = () => setSelected(selected.size === selectable.length && selectable.length > 0 ? new Set() : new Set(selectable.map((s) => s.id)));

  return <>
    <PageHeader title="Sandboxes" description="Agent platform for anything" badge={<span className="rounded-full px-2 py-0.5 text-[11px] font-medium" style={{ background: "var(--bg-elevated)", color: "var(--text-muted)", border: "1px solid var(--border-default)" }}>{items.length}</span>} actions={<Btn variant="primary" size="sm" icon={<Plus size={13} />} onClick={() => setShowCreate((v) => !v)}>{items.length === 0 ? "Create your first sandbox" : "Create sandbox"}</Btn>} />
    {showCreate && <Card className="mb-4 p-4"><div className="mb-3 text-[12px] font-semibold" style={{ color: "var(--text-secondary)" }}>Launch configuration</div><form onSubmit={create} className="flex flex-wrap items-end gap-3"><Select label="Template" value={form.template} onChange={(e) => setForm({ ...form, template: e.target.value })} className="w-56">{(templates.length ? templates : [{ name: "ubuntu-24.04", cpu: 1, memory_mb: 512 } as Template]).map((t) => <option key={t.name} value={t.name}>{t.name} — {t.cpu ?? 1} vCPU · {t.memory_mb ?? 512} MiB</option>)}</Select><Btn variant="primary" type="submit" disabled={pending}>{pending ? "Launching…" : "Launch"}</Btn><Btn variant="ghost" onClick={() => setShowCreate(false)}>Cancel</Btn></form></Card>}
    {error && <div className="mb-3"><ErrorState error={error} onRetry={() => void refresh()} /></div>}
    <div className="mb-3 flex flex-col gap-2 lg:flex-row lg:items-center"><SearchInput value={search} onChange={setSearch} placeholder="Filter sandboxes…" /><div className="flex flex-wrap gap-1 rounded-md p-0.5" style={{ background: "var(--bg-elevated)", border: "1px solid var(--border-default)" }}>{["all", "running", "paused", "hibernated", "failed", "creating"].map((s) => <button key={s} onClick={() => setStatusFilter(s)} className="rounded px-2.5 py-1 text-[12px] font-medium capitalize" style={statusFilter === s ? { background: "var(--bg-overlay)", color: "var(--text-primary)" } : { color: "var(--text-muted)" }}>{s}</button>)}</div><div className="lg:ml-auto flex items-center gap-2">{selected.size > 0 && <><span className="text-[12px]" style={{ color: "var(--text-secondary)" }}>{selected.size} selected</span><Btn size="sm" variant="danger" icon={<Trash2 size={12} />} onClick={bulkDelete}>Delete</Btn></>}<Btn size="sm" variant="ghost" icon={<RefreshCw size={12} />} onClick={refresh} disabled={pending}>Refresh</Btn></div></div>
    <Card>{loading ? <LoadingTable cols={8} /> : <Table><thead><tr><Th><input type="checkbox" checked={selected.size === selectable.length && selectable.length > 0} onChange={toggleAll} className="rounded accent-emerald-500" /></Th><SortHeader label="ID" sortKey="id" current={sort} onSort={toggleSort} /><SortHeader label="Template" sortKey="template" current={sort} onSort={toggleSort} /><SortHeader label="Status" sortKey="status" current={sort} onSort={toggleSort} /><th className="hidden px-4 py-2.5 text-left text-[11px] font-medium uppercase tracking-wider md:table-cell" style={{ color: "var(--text-muted)", borderBottom: "1px solid var(--border-subtle)" }}>Network</th><SortHeader label="Resources" sortKey="cpu" current={sort} onSort={toggleSort} className="hidden sm:table-cell" /><SortHeader label="Created" sortKey="created_at" current={sort} onSort={toggleSort} className="hidden lg:table-cell" /><Th right>Actions</Th></tr></thead><tbody>{pageRows.map((s, i) => <tr key={s.id} className="group transition-colors focus:outline-none focus:ring-1 focus:ring-emerald-500/40" style={{ background: selected.has(s.id) ? "rgba(16,185,129,0.03)" : undefined }} onMouseEnter={(e) => { if (!selected.has(s.id)) e.currentTarget.style.background = "var(--bg-elevated)"; }} onMouseLeave={(e) => { e.currentTarget.style.background = selected.has(s.id) ? "rgba(16,185,129,0.03)" : ""; }} {...rowNavProps(i, () => { window.location.href = `/sandboxes/${s.id}`; })}><Td><input type="checkbox" checked={selected.has(s.id)} disabled={!!managedBy(s)} onChange={() => { const n = new Set(selected); n.has(s.id) ? n.delete(s.id) : n.add(s.id); setSelected(n); }} className="rounded accent-emerald-500 disabled:opacity-30 disabled:cursor-not-allowed" title={managedBy(s) ? `Managed by ${managedBy(s)!.label} — delete it there` : undefined} /></Td><Td><Link href={`/sandboxes/${s.id}`} className="font-mono text-[12px] font-medium transition-colors hover:text-emerald-400" style={{ color: "var(--text-primary)" }}>{s.id.slice(0, 12)}…</Link>{s.from_snapshot && <div className="mt-0.5 text-[10px]" style={{ color: "var(--text-muted)" }}>from snapshot</div>}</Td><Td muted>{s.template}</Td><Td><StatusBadge value={s.status} /></Td><Td mono muted className="hidden md:table-cell">{s.guest_ip || "—"}</Td><Td muted className="hidden sm:table-cell">{s.cpu}C / {s.memory_mb}MiB</Td><Td muted className="hidden lg:table-cell"><RelativeTime value={s.created_at} /></Td><Td right><RowActions><RowAction onClick={() => { window.location.href = `/sandboxes/${s.id}`; }}>View</RowAction><RowAction onClick={() => void navigator.clipboard.writeText(s.id).then(() => toast.success("Copied"))}><Copy size={12} />Copy ID</RowAction>{s.status === "running" && <RowAction onClick={() => act("Stop", () => api.stop(s.id))}><Square size={12} />Stop</RowAction>}{s.status === "running" && <RowAction onClick={() => act("Pause", () => api.pause(s.id))}><Pause size={12} />Pause</RowAction>}{s.status === "paused" && <RowAction onClick={() => act("Resume", () => api.resume(s.id))}><Play size={12} />Resume</RowAction>}{s.status === "hibernated" && <RowAction onClick={() => act("Start", () => api.start(s.id))}><Play size={12} />Start</RowAction>}<RowAction onClick={() => act("Snapshot", () => api.snapshot(s.id))}><Camera size={12} />Snapshot</RowAction>{managedBy(s) ? <RowAction onClick={() => { window.location.href = managedBy(s)!.href; }}><Lock size={12} />Managed by {managedBy(s)!.label}</RowAction> : <RowAction destructive onClick={async () => { const ok = await confirm({ title: `Delete sandbox ${s.id.slice(0, 8)}?`, description: "The sandbox will be killed and its disk released. This cannot be undone.", confirmLabel: "Delete", destructive: true }); if (ok) act("Delete", () => api.remove(s.id)); }}><Trash2 size={12} />Delete</RowAction>}</RowActions></Td></tr>)}</tbody></Table>}</Card>
    {!loading && filtered.length === 0 && !search && statusFilter === "all" && <Quickstart resource="sandbox" />}
    {!loading && filtered.length > 0 && <PaginationBar total={filtered.length} page={page} pageSize={pageSize} onPage={setPage} label="sandboxes" />}
  </>;
}
