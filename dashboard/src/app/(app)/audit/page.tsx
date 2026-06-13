// SPDX-License-Identifier: Apache-2.0
"use client";

import { useCallback, useEffect, useMemo, useState } from "react";
import { Copy, RefreshCw } from "lucide-react";
import { API_BASE, getAuthHeaders } from "@/lib/api";
import { Badge, Btn, Card, PageHeader, Table, Td, Th } from "@/components/ui";
import { compareValue, ErrorState, LoadingTable, PaginationBar, RelativeTime, RowAction, RowActions, rowNavProps, SearchInput, SortHeader, type SortDir, statusTone, useDebouncedValue, usePagedRows } from "@/components/list-quality";
import { toast } from "sonner";

type AuditEntry = { id: number; ts: string; workspace: string; request_id: string; method: string; path: string; status: number; actor?: string; meta?: Record<string, string> };
type AuditResponse = { entries: AuditEntry[] | null; since: string };
type SortKey = "ts" | "id" | "path" | "method" | "status" | "actor";

const WINDOWS = [{ label: "1h", hours: 1 }, { label: "6h", hours: 6 }, { label: "24h", hours: 24 }, { label: "7d", hours: 168 }];

function methodColor(m: string): string { return m === "DELETE" ? "var(--status-failed)" : m === "POST" ? "var(--status-running)" : m === "PUT" || m === "PATCH" ? "var(--status-creating)" : "var(--text-muted)"; }

export default function AuditPage() {
  const [data, setData] = useState<AuditResponse | null>(null);
  const [hours, setHours] = useState(24);
  const [filter, setFilter] = useState("");
  const debouncedFilter = useDebouncedValue(filter);
  const [loading, setLoading] = useState(true);
  const [err, setErr] = useState<string | null>(null);
  const [sort, setSort] = useState<{ key: SortKey; dir: SortDir }>({ key: "ts", dir: "desc" });

  const refresh = useCallback(async () => {
    setLoading(true); setErr(null);
    try {
      const since = new Date(Date.now() - hours * 3600 * 1000).toISOString();
      const r = await fetch(`${API_BASE}/v1/audit?since=${encodeURIComponent(since)}&limit=500`, { cache: "no-store", headers: await getAuthHeaders() });
      if (!r.ok) throw new Error(`audit fetch failed: ${r.status}`);
      setData(await r.json());
    } catch (e: unknown) { setErr(e instanceof Error ? e.message : String(e)); }
    finally { setLoading(false); }
  }, [hours]);

  useEffect(() => { void refresh(); }, [refresh]);

  const rows = useMemo(() => {
    const f = debouncedFilter.toLowerCase().trim();
    return (data?.entries ?? [])
      .filter((e) => !f || e.path.toLowerCase().includes(f) || e.method.toLowerCase().includes(f) || String(e.status).includes(f) || (e.actor || "").toLowerCase().includes(f) || e.request_id?.toLowerCase().includes(f))
      .sort((a, b) => { const cmp = compareValue(sort.key === "actor" ? a.actor : a[sort.key], sort.key === "actor" ? b.actor : b[sort.key]); return sort.dir === "asc" ? cmp : -cmp; });
  }, [data, debouncedFilter, sort]);
  const { page, setPage, pageSize, pageRows } = usePagedRows(rows);
  const toggleSort = (key: SortKey) => setSort((s) => s.key === key ? { key, dir: s.dir === "asc" ? "desc" : "asc" } : { key, dir: key === "ts" ? "desc" : "asc" });

  return (
    <div className="space-y-5">
      <PageHeader title="Audit Log" description="Every mutating API call against your workspace. Reads and streaming endpoints are skipped." actions={<Btn variant="secondary" size="sm" onClick={() => void refresh()} disabled={loading}><RefreshCw size={13} className={loading ? "animate-spin" : ""} /><span className="ml-1">Refresh</span></Btn>} />
      <div className="flex flex-col gap-2 lg:flex-row lg:items-center">
        <div className="flex flex-wrap gap-2">{WINDOWS.map((w) => <button key={w.label} onClick={() => setHours(w.hours)} className="rounded px-3 py-1 text-xs font-medium" style={{ background: hours === w.hours ? "var(--brand-dim)" : "var(--bg-overlay)", color: hours === w.hours ? "var(--brand)" : "var(--text-secondary)", border: `1px solid ${hours === w.hours ? "var(--brand-border)" : "var(--border-subtle)"}` }}>Last {w.label}</button>)}</div>
        <div className="lg:ml-auto"><SearchInput value={filter} onChange={setFilter} placeholder="Filter path, method, status…" /></div>
      </div>
      {err && <ErrorState error={err} onRetry={() => void refresh()} />}
      <Card>
        {loading ? <LoadingTable cols={7} rows={6} /> : (
          <Table>
            <thead><tr><SortHeader label="Time" sortKey="ts" current={sort} onSort={toggleSort} /><SortHeader label="Method" sortKey="method" current={sort} onSort={toggleSort} /><SortHeader label="Path" sortKey="path" current={sort} onSort={toggleSort} /><SortHeader label="Status" sortKey="status" current={sort} onSort={toggleSort} /><SortHeader label="Actor" sortKey="actor" current={sort} onSort={toggleSort} /><SortHeader label="Request ID" sortKey="id" current={sort} onSort={toggleSort} /><Th right>Actions</Th></tr></thead>
            <tbody>{pageRows.map((e, i) => <tr key={e.id} className="transition-colors focus:outline-none focus:ring-1 focus:ring-emerald-500/40" onMouseEnter={(ev) => { ev.currentTarget.style.background = "var(--bg-elevated)"; }} onMouseLeave={(ev) => { ev.currentTarget.style.background = ""; }} {...rowNavProps(i)}>
              <Td muted><RelativeTime value={e.ts} /></Td><Td><span className="font-mono font-semibold" style={{ color: methodColor(e.method) }}>{e.method}</span></Td><Td mono className="max-w-[340px] truncate">{e.path}</Td><Td><Badge variant={statusTone(e.status)}>{e.status || "—"}</Badge></Td><Td muted>{e.actor || "—"}</Td><Td mono muted>{e.request_id?.slice(0, 12) || "—"}</Td><Td right><RowActions><RowAction onClick={() => void navigator.clipboard.writeText(e.request_id || String(e.id)).then(() => toast.success("Copied"))}><Copy size={12} />Copy request ID</RowAction></RowActions></Td>
            </tr>)}</tbody>
          </Table>
        )}
      </Card>
      {!loading && rows.length > 0 && <PaginationBar total={rows.length} page={page} pageSize={pageSize} onPage={setPage} label="audit events" />}
    </div>
  );
}
