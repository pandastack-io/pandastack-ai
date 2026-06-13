// SPDX-License-Identifier: Apache-2.0
"use client";

import { useEffect, useMemo, useRef, useState, useTransition } from "react";
import { toast } from "sonner";
import {
  Plus,
  Trash2,
  Upload,
  Clock,
  CheckCircle,
  XCircle,
  Loader2,
  Rocket,
  Copy,
} from "lucide-react";
import { api, type Template, type TemplateBuild } from "@/lib/api";
import {
  Badge,
  Btn,
  Card,
  Input,
  PageHeader,
  Table,
  Td,
  Th,
  useConfirm,
} from "@/components/ui";
import { compareValue, ErrorState, LoadingTable, PaginationBar, RelativeTime, RowAction, RowActions, rowNavProps, SearchInput, SortHeader, type SortDir, useDebouncedValue, usePagedRows } from "@/components/list-quality";
import { CATEGORY_LABEL, getTemplateInfo, type TemplateInfo } from "@/lib/template-meta";

type CategoryFilter = "all" | TemplateInfo["category"];
type SortKey = "name" | "cpu" | "memory_mb" | "size_bytes";
type BuildSortKey = "name" | "status" | "started_at" | "size_mb";

const CATEGORY_ORDER: CategoryFilter[] = ["all", "agents", "coding", "web", "data", "base", "custom"];

export default function TemplatesPage() {
  const [items, setItems] = useState<Template[]>([]);
  const [builds, setBuilds] = useState<TemplateBuild[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [pending, start] = useTransition();
  const [show, setShow] = useState(false);
  const [form, setForm] = useState({ name: "", size_mb: 1024 });
  const fileRef = useRef<HTMLInputElement | null>(null);
  const [filter, setFilter] = useState<CategoryFilter>("all");
  const [query, setQuery] = useState("");
  const debouncedQuery = useDebouncedValue(query);
  const [sort, setSort] = useState<{ key: SortKey; dir: SortDir }>({ key: "name", dir: "asc" });
  const [buildSort, setBuildSort] = useState<{ key: BuildSortKey; dir: SortDir }>({ key: "started_at", dir: "desc" });
  const confirm = useConfirm();

  const refresh = async () => {
    setError(null);
    try {
      const [t, b] = await Promise.all([api.templates(), api.templateBuilds()]);
      setItems((t ?? []).slice().sort((a, b) => a.name.localeCompare(b.name)));
      setBuilds((b ?? []).sort((a, b) => (a.started_at < b.started_at ? 1 : -1)));
    } catch (e) {
      const msg = e instanceof Error ? e.message : String(e);
      setError(msg);
      toast.error("Failed to load templates: " + msg);
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    refresh();
    const t = setInterval(refresh, 5000);
    return () => clearInterval(t);
  }, []);

  const filtered = useMemo(() => {
    const q = debouncedQuery.trim().toLowerCase();
    return items.filter((t) => {
      const info = getTemplateInfo(t.name);
      if (filter !== "all" && info.category !== filter) return false;
      if (!q) return true;
      return (
        t.name.toLowerCase().includes(q) ||
        info.label.toLowerCase().includes(q) ||
        info.tools.some((x) => x.toLowerCase().includes(q))
      );
    }).sort((a, b) => { const cmp = compareValue(a[sort.key], b[sort.key]); return sort.dir === "asc" ? cmp : -cmp; });
  }, [items, filter, debouncedQuery, sort]);
  const { page, setPage, pageSize, pageRows } = usePagedRows(filtered);
  const toggleSort = (key: SortKey) => setSort((x) => x.key === key ? { key, dir: x.dir === "asc" ? "desc" : "asc" } : { key, dir: "asc" });
  const sortedBuilds = useMemo(() => builds.slice().sort((a, b) => { const cmp = compareValue(a[buildSort.key], b[buildSort.key]); return buildSort.dir === "asc" ? cmp : -cmp; }), [builds, buildSort]);
  const buildPage = usePagedRows(sortedBuilds);
  const toggleBuildSort = (key: BuildSortKey) => setBuildSort((x) => x.key === key ? { key, dir: x.dir === "asc" ? "desc" : "asc" } : { key, dir: key === "started_at" ? "desc" : "asc" });

  const counts = useMemo(() => {
    const m: Record<string, number> = { all: items.length };
    for (const t of items) {
      const c = getTemplateInfo(t.name).category;
      m[c] = (m[c] ?? 0) + 1;
    }
    return m;
  }, [items]);

  const copy = async (txt: string) => {
    try { await navigator.clipboard.writeText(txt); toast.success("Copied"); } catch { toast.error("Copy failed"); }
  };

  const upload = (e: React.FormEvent) => {
    e.preventDefault();
    const f = fileRef.current?.files?.[0];
    if (!f) {
      toast.error("rootfs file required");
      return;
    }
    start(async () => {
      const id = toast.loading("Building template…");
      try {
        await api.buildTemplate(form.name, form.size_mb, f);
        setShow(false);
        setForm({ name: "", size_mb: 1024 });
        if (fileRef.current) fileRef.current.value = "";
        toast.success("Build queued", { id });
        await refresh();
      } catch (e) {
        toast.error("Build failed: " + String(e), { id });
      }
    });
  };

  return (
    <>
      <PageHeader
        title="Templates"
        description="Pre-baked Firecracker rootfs snapshots. Launch a sandbox from any of them."
        badge={
          <span
            className="rounded-full px-2 py-0.5 text-[11px] font-medium"
            style={{
              background: "var(--bg-elevated)",
              color: "var(--text-muted)",
              border: "1px solid var(--border-default)",
            }}
          >
            {items.length}
          </span>
        }
        actions={
          <Btn
            variant="primary"
            size="sm"
            icon={<Plus size={13} />}
            onClick={() => setShow((v) => !v)}
          >
            {items.length === 0 ? "Create your first template" : "Create template"}
          </Btn>
        }
      />

      {show && (
        <Card className="mb-5 p-4">
          <div
            className="mb-3 text-[12px] font-semibold"
            style={{ color: "var(--text-secondary)" }}
          >
            Build from rootfs
          </div>
          <form onSubmit={upload} className="flex flex-wrap items-end gap-3">
            <Input
              label="Name"
              required
              placeholder="alpine-3.20"
              value={form.name}
              onChange={(e) => setForm({ ...form, name: e.target.value })}
              className="w-44"
            />
            <Input
              label="Size (MiB)"
              type="number"
              min={128}
              step={128}
              value={form.size_mb}
              onChange={(e) => setForm({ ...form, size_mb: Number(e.target.value) })}
              className="w-28"
            />
            <label className="flex flex-col gap-1">
              <span
                className="text-[11px] font-medium uppercase tracking-wider"
                style={{ color: "var(--text-secondary)" }}
              >
                rootfs.tar(.gz)
              </span>
              <input
                ref={fileRef}
                type="file"
                accept=".tar,.gz,.tgz,application/x-tar,application/gzip"
                className="block text-[12px] file:mr-3 file:rounded-md file:border-0 file:px-3 file:py-1.5 file:text-[12px] file:font-medium file:cursor-pointer transition-colors"
                style={{ color: "var(--text-secondary)" } as React.CSSProperties}
              />
            </label>
            <div className="flex gap-2">
              <Btn variant="primary" type="submit" disabled={pending} icon={<Upload size={13} />}>
                {pending ? "Uploading…" : "Build"}
              </Btn>
              <Btn variant="ghost" onClick={() => setShow(false)}>
                Cancel
              </Btn>
            </div>
          </form>
          <div
            className="mt-3 rounded-md px-3 py-2 text-[12px]"
            style={{ background: "var(--bg-elevated)", color: "var(--text-muted)" }}
          >
            <span className="font-medium" style={{ color: "var(--text-secondary)" }}>
              Tip:
            </span>{" "}
            <code className="font-mono" style={{ color: "var(--text-secondary)" }}>
              pandastack template build -f Dockerfile -n my-template
            </code>{" "}
            (or upload a tarball here directly)
          </div>
        </Card>
      )}

      {error && <div className="mb-4"><ErrorState error={error} onRetry={() => void refresh()} /></div>}

      {/* Filter + search bar */}
      <div className="mb-3 flex flex-wrap items-center gap-2">
        <div className="flex flex-wrap gap-1.5">
          {CATEGORY_ORDER.map((c) => {
            const active = filter === c;
            const n = counts[c] ?? 0;
            const label = c === "all" ? "All" : CATEGORY_LABEL[c];
            return (
              <button
                key={c}
                onClick={() => setFilter(c)}
                className="rounded-md px-2.5 py-1 text-[11px] font-medium transition-colors"
                style={{
                  background: active ? "var(--brand-dim)" : "var(--bg-elevated)",
                  color: active ? "var(--brand-primary)" : "var(--text-secondary)",
                  border: `1px solid ${active ? "var(--brand-border)" : "var(--border-default)"}`,
                }}
              >
                {label}
                <span className="ml-1.5 opacity-60">{n}</span>
              </button>
            );
          })}
        </div>
        <div className="ml-auto"><SearchInput value={query} onChange={setQuery} placeholder="Filter by name, tool, base…" /></div>
      </div>

      {/* Templates table */}
      <Card className="mb-6 p-0 overflow-hidden">
        {loading ? (
          <LoadingTable cols={9} rows={6} />
        ) : (
          <Table>
            <thead>
              <tr>
                <Th>Template</Th>
                <Th>Category</Th>
                <Th>Base</Th>
                <Th>Tools</Th>
                <Th right>vCPU</Th>
                <Th right>Memory</Th>
                <Th right>Size</Th>
                <Th right>Arch</Th>
                <Th right>Actions</Th>
              </tr>
            </thead>
            <tbody>
              {pageRows.map((t, i) => {
                const info = getTemplateInfo(t.name);
                const arch = (t.meta?.arch as string | undefined) ?? "x86_64";
                const sizeGB = t.size_bytes / 1024 / 1024 / 1024;
                const snippet = `curl -X POST $API/v1/sandboxes -H "authorization: Bearer $TOKEN" -H "content-type: application/json" -d '{"template":"${t.name}"}'`;
                return (
                  <tr key={t.name} className="focus:outline-none focus:ring-1 focus:ring-emerald-500/40" {...rowNavProps(i)}>
                    <Td>
                      <div className="flex items-center gap-2.5">
                        <span
                          className="flex size-7 items-center justify-center rounded-md"
                          style={{
                            background: "var(--bg-elevated)",
                            border: "1px solid var(--border-default)",
                          }}
                        >
                          {info.icon}
                        </span>
                        <div className="min-w-0">
                          <div
                            className="text-[13px] font-medium leading-tight"
                            style={{ color: "var(--text-primary)" }}
                          >
                            {info.label}
                          </div>
                          <div
                            className="font-mono text-[11px] leading-tight"
                            style={{ color: "var(--text-muted)" }}
                          >
                            {t.name}
                          </div>
                        </div>
                      </div>
                    </Td>
                    <Td>
                      <Badge variant="default">{CATEGORY_LABEL[info.category]}</Badge>
                    </Td>
                    <Td muted className="hidden md:table-cell">
                      <span className="font-mono text-[11.5px]">{info.base}</span>
                    </Td>
                    <Td className="hidden lg:table-cell">
                      <div className="flex flex-wrap gap-1">
                        {info.tools.slice(0, 4).map((tool) => (
                          <span
                            key={tool}
                            className="rounded px-1.5 py-0.5 font-mono text-[10.5px]"
                            style={{
                              background: "var(--bg-elevated)",
                              color: "var(--text-secondary)",
                              border: "1px solid var(--border-default)",
                            }}
                          >
                            {tool}
                          </span>
                        ))}
                        {info.tools.length > 4 && (
                          <span
                            className="text-[10.5px]"
                            style={{ color: "var(--text-muted)" }}
                          >
                            +{info.tools.length - 4}
                          </span>
                        )}
                      </div>
                    </Td>
                    <Td right muted className="hidden sm:table-cell">
                      <span className="font-mono text-[12px]">{t.cpu ?? 1}</span>
                    </Td>
                    <Td right muted className="hidden sm:table-cell">
                      <span className="font-mono text-[12px]">{t.memory_mb ?? 512} MiB</span>
                    </Td>
                    <Td right muted>
                      <span className="font-mono text-[12px]">
                        {sizeGB >= 1 ? `${sizeGB.toFixed(1)} GB` : `${(t.size_bytes / 1024 / 1024).toFixed(0)} MB`}
                      </span>
                    </Td>
                    <Td right muted>
                      <span className="font-mono text-[11px]">{arch}</span>
                    </Td>
                    <Td right>
                      <RowActions><RowAction onClick={() => copy(t.name)}><Copy size={12} />Copy name</RowAction><RowAction onClick={() => copy(snippet)}><Copy size={12} />Copy curl</RowAction><RowAction onClick={() => start(async () => { const tid = toast.loading(`Launching ${t.name}…`); try { const sb = await api.create({ template: t.name }); toast.success(`Sandbox ${sb.id.slice(0, 8)} ready`, { id: tid }); } catch (e) { toast.error("Launch failed: " + String(e), { id: tid }); } })}><Rocket size={12} />Launch</RowAction>{!t.is_global && (<RowAction destructive onClick={async () => { const ok = await confirm({ title: `Delete template ${t.name}?`, description: "Existing sandboxes built from this template keep running, but no new sandboxes can be launched from it.", confirmLabel: "Delete template", destructive: true }); if (!ok) return; start(async () => { const tid = toast.loading("Deleting…"); try { await api.deleteTemplate(t.name); toast.success("Deleted", { id: tid }); await refresh(); } catch (e) { toast.error("Delete failed: " + String(e), { id: tid }); } }); }}><Trash2 size={12} />Delete</RowAction>)}</RowActions>
                    </Td>
                  </tr>
                );
              })}
            </tbody>
          </Table>
        )}
      </Card>
      {!loading && filtered.length > 0 && <PaginationBar total={filtered.length} page={page} pageSize={pageSize} onPage={setPage} label="templates" />}

      {/* Build history */}
      {builds.length > 0 && (
        <div>
          <div
            className="mb-3 text-[11px] font-medium uppercase tracking-wider"
            style={{ color: "var(--text-muted)" }}
          >
            Build history
          </div>
          <Card className="p-0 overflow-hidden">
            <Table>
              <thead>
                <tr>
                  <Th>Name</Th>
                  <Th>Status</Th>
                  <Th>Size</Th>
                  <Th>Started</Th>
                  <Th>Error</Th>
                </tr>
              </thead>
              <tbody>
                {builds.slice(0, 10).map((b) => (
                  <tr key={b.id}>
                    <Td mono>{b.name}</Td>
                    <Td>
                      <BuildBadge status={b.status} />
                    </Td>
                    <Td muted>{b.size_mb} MiB</Td>
                    <Td muted>{new Date(b.started_at).toLocaleTimeString()}</Td>
                    <Td>
                      {b.error && (
                        <span className="text-red-400 text-[12px] truncate max-w-[260px] block">
                          {b.error}
                        </span>
                      )}
                    </Td>
                  </tr>
                ))}
              </tbody>
            </Table>
          </Card>
        </div>
      )}
    </>
  );
}

function BuildBadge({ status }: { status: TemplateBuild["status"] }) {
  const map: Record<
    TemplateBuild["status"],
    { variant: "success" | "warning" | "error" | "info"; icon: React.ReactNode }
  > = {
    done: { variant: "success", icon: <CheckCircle size={11} /> },
    running: { variant: "warning", icon: <Loader2 size={11} className="animate-spin" /> },
    queued: { variant: "info", icon: <Clock size={11} /> },
    failed: { variant: "error", icon: <XCircle size={11} /> },
  };
  const { variant, icon } = map[status] ?? map.queued;
  return (
    <Badge variant={variant} className="gap-1">
      {icon}
      {status}
    </Badge>
  );
}
