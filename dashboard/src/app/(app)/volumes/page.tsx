// SPDX-License-Identifier: Apache-2.0
"use client";

import { useEffect, useMemo, useState, useTransition } from "react";
import { toast } from "sonner";
import { Copy, HardDrive, Plus, Trash2 } from "lucide-react";
import { api, type Volume } from "@/lib/api";
import { Btn, Card, Input, PageHeader, Table, Td, Th, useConfirm } from "@/components/ui";
import { compareValue, CopyButton, ErrorState, LoadingTable, PaginationBar, RowAction, RowActions, rowNavProps, SearchInput, SortHeader, type SortDir, useDebouncedValue, usePagedRows } from "@/components/list-quality";
import { Quickstart } from "@/components/Quickstart";

type SortKey = "name" | "size_mb" | "size_bytes";

function errorMessage(error: unknown) {
  return error instanceof Error ? error.message : String(error);
}

export default function VolumesPage() {
  const [items, setItems] = useState<Volume[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [pending, start] = useTransition();
  const [form, setForm] = useState({ name: "", size_mb: 256 });
  const [show, setShow] = useState(false);
  const [query, setQuery] = useState("");
  const debouncedQuery = useDebouncedValue(query);
  const [sort, setSort] = useState<{ key: SortKey; dir: SortDir }>({ key: "name", dir: "asc" });
  const confirm = useConfirm();

  const refresh = async () => {
    setError(null);
    try {
      const r = await api.volumes();
      setItems(r ?? []);
    } catch (e) {
      const msg = errorMessage(e);
      setError(msg);
      toast.error(`Failed to load volumes: ${msg}`);
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    refresh();
    const t = setInterval(refresh, 5000);
    return () => clearInterval(t);
  }, []);

  const create = (e: React.FormEvent) => {
    e.preventDefault();
    start(async () => {
      const id = toast.loading("Creating volume…");
      try {
        await api.createVolume(form.name, form.size_mb);
        setForm({ name: "", size_mb: 256 });
        setShow(false);
        toast.success("Volume created", { id });
        await refresh();
      } catch (e) {
        toast.error(`Create failed: ${errorMessage(e)}`, { id });
      }
    });
  };

  const filtered = useMemo(() => {
    const q = debouncedQuery.trim().toLowerCase();
    return items
      .filter((v) => !q || v.name.toLowerCase().includes(q) || String(v.size_mb).includes(q) || String(v.size_bytes).includes(q))
      .sort((a, b) => {
        const cmp = compareValue(a[sort.key], b[sort.key]);
        return sort.dir === "asc" ? cmp : -cmp;
      });
  }, [items, debouncedQuery, sort]);
  const { page, setPage, pageSize, pageRows } = usePagedRows(filtered);
  const toggleSort = (key: SortKey) => setSort((s) => s.key === key ? { key, dir: s.dir === "asc" ? "desc" : "asc" } : { key, dir: "asc" });

  return (
    <>
      <PageHeader
        title="Volumes"
        description="Persistent ext4 disks. Attach to sandboxes at create-time; data survives across runs."
        badge={<span className="rounded-full px-2 py-0.5 text-[11px] font-medium" style={{ background: "var(--bg-elevated)", color: "var(--text-muted)", border: "1px solid var(--border-default)" }}>{items.length}</span>}
        actions={<Btn variant="primary" size="sm" icon={<Plus size={13} />} onClick={() => setShow((v) => !v)}>{items.length === 0 ? "Create your first volume" : "Create volume"}</Btn>}
      />

      {show && (
        <Card className="mb-5 p-4">
          <div className="mb-3 text-[12px] font-semibold" style={{ color: "var(--text-secondary)" }}>Create volume</div>
          <form onSubmit={create} className="flex flex-wrap items-end gap-3">
            <Input label="Name" required placeholder="my-data" value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })} className="w-48" />
            <Input label="Size (MiB)" type="number" min={16} step={64} value={form.size_mb} onChange={(e) => setForm({ ...form, size_mb: Number(e.target.value) })} className="w-28" />
            <div className="flex gap-2"><Btn variant="primary" type="submit" disabled={pending}>{pending ? "Creating…" : "Create"}</Btn><Btn variant="ghost" onClick={() => setShow(false)}>Cancel</Btn></div>
          </form>
          <div className="mt-2 text-[12px]" style={{ color: "var(--text-muted)" }}>Attach via <code className="font-mono rounded px-1" style={{ background: "var(--bg-elevated)", color: "var(--text-secondary)" }}>volumes:[{"{"}name{"}"}]</code> on create</div>
        </Card>
      )}

      {error && <div className="mb-4"><ErrorState error={error} onRetry={() => void refresh()} /></div>}

      <div className="mb-3 flex flex-col gap-2 sm:flex-row sm:items-center sm:justify-between">
        <SearchInput value={query} onChange={setQuery} placeholder="Filter volumes…" />
        <div className="text-[12px]" style={{ color: "var(--text-muted)" }}>{filtered.length} of {items.length} volumes</div>
      </div>

      <Card>
        {loading ? <LoadingTable cols={4} /> : (
          <Table>
            <thead><tr><SortHeader label="Name" sortKey="name" current={sort} onSort={toggleSort} /><SortHeader label="Size" sortKey="size_mb" current={sort} onSort={toggleSort} /><SortHeader label="Bytes on disk" sortKey="size_bytes" current={sort} onSort={toggleSort} /><Th right>Actions</Th></tr></thead>
            <tbody>{pageRows.map((v, i) => (
              <tr key={v.name} className="group transition-colors focus:outline-none focus:ring-1 focus:ring-emerald-500/40" onMouseEnter={(e) => { e.currentTarget.style.background = "var(--bg-elevated)"; }} onMouseLeave={(e) => { e.currentTarget.style.background = ""; }} {...rowNavProps(i)}>
                <Td mono>{v.name}</Td><Td>{v.size_mb} MiB</Td><Td muted>{v.size_bytes.toLocaleString()}</Td>
                <Td right><RowActions><RowAction onClick={() => void navigator.clipboard.writeText(v.name).then(() => toast.success("Copied"))}><Copy size={12} />Copy name</RowAction><RowAction destructive onClick={async () => { const ok = await confirm({ title: `Delete volume ${v.name}?`, description: "Any data on this volume will be permanently erased.", confirmLabel: "Delete", destructive: true }); if (!ok) return; start(async () => { const id = toast.loading("Deleting…"); try { await api.deleteVolume(v.name); toast.success("Deleted", { id }); await refresh(); } catch (e) { toast.error(`Delete failed: ${errorMessage(e)}`, { id }); } }); }}><Trash2 size={12} />Delete</RowAction></RowActions></Td>
              </tr>
            ))}</tbody>
          </Table>
        )}
      </Card>
      {!loading && items.length === 0 && <Quickstart resource="volume" />}
      {!loading && filtered.length > 0 && <PaginationBar total={filtered.length} page={page} pageSize={pageSize} onPage={setPage} label="volumes" />}
    </>
  );
}
