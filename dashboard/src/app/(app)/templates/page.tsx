// SPDX-License-Identifier: Apache-2.0
"use client";

import { useEffect, useMemo, useRef, useState, useTransition } from "react";
import { toast } from "sonner";
import Link from "next/link";
import {
  Plus,
  Trash2,
  Rocket,
  Copy,
  HelpCircle,
} from "lucide-react";
import { api, type Template } from "@/lib/api";
import { formatMemory, RESOURCE_TITLE } from "@/lib/resources";
import {
  Badge,
  Btn,
  Card,
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

const CATEGORY_ORDER: CategoryFilter[] = ["all", "agents", "coding", "web", "data", "base", "custom"];


// The API's is_global flag is authoritative for first-party vs custom;
// TEMPLATE_INFO only supplies labels/icons/descriptions.
function templateCategory(t: Template): TemplateInfo["category"] {
  if (t.is_global === false) return "custom";
  const cat = getTemplateInfo(t.name).category;
  if (cat !== "custom" || !t.is_global) return cat;
  const apiCat = t.category;
  return apiCat && apiCat in CATEGORY_LABEL ? (apiCat as TemplateInfo["category"]) : "base";
}

export default function TemplatesPage() {
  const [items, setItems] = useState<Template[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [pending, start] = useTransition();
  const [filter, setFilter] = useState<CategoryFilter>("all");
  const [query, setQuery] = useState("");
  const debouncedQuery = useDebouncedValue(query);
  const [sort, setSort] = useState<{ key: SortKey; dir: SortDir }>({ key: "name", dir: "asc" });
  const confirm = useConfirm();

  const refresh = async () => {
    setError(null);
    try {
      const t = await api.templates();
      setItems((t ?? []).slice().sort((a, b) => a.name.localeCompare(b.name)));
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
      if (filter !== "all" && templateCategory(t) !== filter) return false;
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

  const counts = useMemo(() => {
    const m: Record<string, number> = { all: items.length };
    for (const t of items) {
      const c = templateCategory(t);
      m[c] = (m[c] ?? 0) + 1;
    }
    return m;
  }, [items]);

  const copy = async (txt: string) => {
    try { await navigator.clipboard.writeText(txt); toast.success("Copied"); } catch { toast.error("Copy failed"); }
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
          <Link href="/templates/custom">
            <Btn variant="primary" size="sm" icon={<Plus size={13} />}>
              Custom builds
            </Btn>
          </Link>
        }
      />


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
          <LoadingTable cols={7} rows={6} />
        ) : (
          <Table>
            <thead>
              <tr>
                <Th>Template</Th>
                <Th>Category</Th>
                <Th>Base</Th>
                <Th>Tools</Th>
                <Th right>
                  <span className="inline-flex items-center gap-1" title={RESOURCE_TITLE}>
                    Resources
                    <HelpCircle size={11} />
                  </span>
                </Th>
                <Th right>Disk</Th>
                <Th right>Actions</Th>
              </tr>
            </thead>
            <tbody>
              {pageRows.map((t, i) => {
                const info = getTemplateInfo(t.name);
                const sizeGB = t.size_bytes / 1024 / 1024 / 1024;
                const memMB = t.memory_mb ?? 512;
                const memGB = formatMemory(memMB);
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
                      <Badge variant="default">{CATEGORY_LABEL[templateCategory(t)]}</Badge>
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
                      <span className="font-mono text-[12px]" title={RESOURCE_TITLE}>
                        {memGB} RAM<span style={{ color: "var(--text-muted)" }}> · 8 vCPU burst</span>
                      </span>
                    </Td>
                    <Td right muted>
                      <span className="font-mono text-[12px]">
                        {sizeGB >= 1 ? `${sizeGB.toFixed(1)} GB` : `${(t.size_bytes / 1024 / 1024).toFixed(0)} MB`}
                      </span>
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

    </>
  );
}

