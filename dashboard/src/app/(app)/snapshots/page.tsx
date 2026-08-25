// SPDX-License-Identifier: Apache-2.0
"use client";

import Link from "next/link";
import { useMemo, useState } from "react";
import { toast } from "sonner";
import { Camera, Copy, RefreshCw, Trash2 } from "lucide-react";
import { api, type SnapshotInfo } from "@/lib/api";
import { Badge, Btn, Card, Empty, PageHeader, Table, Td, Th, useConfirm } from "@/components/ui";
import { compareValue, ErrorState, LoadingTable, PaginationBar, RelativeTime, RowAction, RowActions, SearchInput, SortHeader, type SortDir, useAsyncList, usePagedRows } from "@/components/list-quality";

function msg(e: unknown) { return e instanceof Error ? e.message : String(e); }
type SortKey = "id" | "sandbox_id" | "template" | "created_at";

function fmtBytes(n?: number): string {
  if (!n || n <= 0) return "—";
  const u = ["B", "KB", "MB", "GB"];
  let i = 0, v = n;
  while (v >= 1024 && i < u.length - 1) { v /= 1024; i++; }
  return `${v.toFixed(v < 10 && i > 0 ? 1 : 0)} ${u[i]}`;
}

export default function SnapshotsPage() {
  const { items, loading, error, refresh } = useAsyncList<SnapshotInfo>(
    () => api.snapshots(),
    { pollMs: 8000, cacheKey: "snapshots", onError: (m) => toast.error("Failed to fetch snapshots: " + m) },
  );
  const [search, setSearch] = useState("");
  const [sort, setSort] = useState<{ key: SortKey; dir: SortDir }>({ key: "created_at", dir: "desc" });
  const [busy, setBusy] = useState(false);
  const confirm = useConfirm();

  const filtered = useMemo(() => {
    const q = search.toLowerCase().trim();
    const rows = items.filter((s) => !q || s.id.toLowerCase().includes(q) || s.sandbox_id.toLowerCase().includes(q) || (s.template ?? "").toLowerCase().includes(q));
    return rows.sort((a, b) => {
      const cmp = compareValue(a[sort.key], b[sort.key]);
      return sort.dir === "asc" ? cmp : -cmp;
    });
  }, [items, search, sort]);
  const { page, setPage, pageSize, pageRows } = usePagedRows(filtered);
  const toggleSort = (key: SortKey) => setSort((s) => s.key === key ? { key, dir: s.dir === "asc" ? "desc" : "asc" } : { key, dir: key === "created_at" ? "desc" : "asc" });

  const remove = async (id: string) => {
    const ok = await confirm({ title: `Delete snapshot ${id.slice(0, 8)}?`, description: "This permanently removes the snapshot and its stored copy. This cannot be undone.", confirmLabel: "Delete", destructive: true });
    if (!ok) return;
    setBusy(true);
    const t = toast.loading("Deleting snapshot…");
    try { await api.deleteSnapshot(id); toast.success("Snapshot deleted", { id: t }); await refresh(); }
    catch (e) { toast.error("Delete failed: " + msg(e), { id: t }); }
    finally { setBusy(false); }
  };

  return <>
    <PageHeader
      title="Snapshots"
      description="Frozen sandbox memory + disk you can restore from."
      badge={<span className="rounded-full px-2 py-0.5 text-[11px] font-medium" style={{ background: "var(--bg-elevated)", color: "var(--text-muted)", border: "1px solid var(--border-default)" }}>{items.length}</span>}
      actions={<Btn size="sm" variant="ghost" icon={<RefreshCw size={12} />} onClick={refresh} disabled={busy}>Refresh</Btn>}
    />

    {error && <div className="mb-3"><ErrorState error={error} onRetry={() => void refresh()} /></div>}

    <div className="mb-3 flex items-center gap-2">
      <SearchInput value={search} onChange={setSearch} placeholder="Filter snapshots…" />
    </div>

    <Card>
      {loading ? <LoadingTable cols={5} /> : filtered.length === 0 ? (
        items.length === 0 ? (
          <Empty
            icon={<Camera size={20} />}
            title="No snapshots yet"
            hint="Open a sandbox and choose “Snapshot” to freeze its memory + disk. You can then create a new sandbox from a snapshot."
            cta={<Link href="/sandboxes" className="text-[13px] hover:underline" style={{ color: "var(--text-secondary)" }}>Go to sandboxes →</Link>}
          />
        ) : (
          <Empty icon={<Camera size={20} />} title="No matching snapshots" hint={search ? `No snapshots match “${search}”.` : undefined} />
        )
      ) : (
        <Table>
          <thead><tr>
            <SortHeader label="ID" sortKey="id" current={sort} onSort={toggleSort} />
            <SortHeader label="From sandbox" sortKey="sandbox_id" current={sort} onSort={toggleSort} />
            <SortHeader label="Template" sortKey="template" current={sort} onSort={toggleSort} className="hidden md:table-cell" />
            <th className="hidden px-4 py-2.5 text-left text-[11px] font-medium uppercase tracking-wider sm:table-cell" style={{ color: "var(--text-muted)", borderBottom: "1px solid var(--border-subtle)" }}>Size</th>
            <SortHeader label="Created" sortKey="created_at" current={sort} onSort={toggleSort} className="hidden lg:table-cell" />
            <Th right>Actions</Th>
          </tr></thead>
          <tbody>{pageRows.map((s) => (
            <tr key={s.id} className="group">
              <Td><span className="font-mono text-[12px]" style={{ color: "var(--text-primary)" }}>{s.id.slice(0, 12)}…</span></Td>
              <Td muted>
                <Link href={`/sandboxes/${s.sandbox_id}`} className="font-mono text-[12px] hover:underline">{s.sandbox_id.slice(0, 12)}…</Link>
              </Td>
              <Td muted className="hidden md:table-cell">{s.template ? <Badge>{s.template}</Badge> : "—"}</Td>
              <Td muted className="hidden sm:table-cell">{fmtBytes(s.size_bytes)}</Td>
              <Td muted className="hidden lg:table-cell"><RelativeTime value={s.created_at} /></Td>
              <Td right>
                <RowActions>
                  <RowAction onClick={() => void navigator.clipboard.writeText(s.id).then(() => toast.success("Copied")).catch(() => toast.error("Copy failed"))}><Copy size={12} />Copy ID</RowAction>
                  <RowAction destructive onClick={() => void remove(s.id)}><Trash2 size={12} />Delete</RowAction>
                </RowActions>
              </Td>
            </tr>
          ))}</tbody>
        </Table>
      )}
    </Card>
    {!loading && filtered.length > 0 && <PaginationBar total={filtered.length} page={page} pageSize={pageSize} onPage={setPage} label="snapshots" />}

    {!loading && items.length > 0 && (
      <p className="mt-3 text-[12px]" style={{ color: "var(--text-muted)" }}>
        Create a new sandbox from a snapshot with the SDK/CLI:{" "}
        <code className="font-mono">Sandbox.create(&#123; from_snapshot: &quot;&lt;id&gt;&quot; &#125;)</code> or{" "}
        <code className="font-mono">pandastack sandbox create --from-snapshot &lt;id&gt;</code>.
      </p>
    )}
  </>;
}
