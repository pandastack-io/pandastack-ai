// SPDX-License-Identifier: Apache-2.0
"use client";

import { useMemo, useState, useTransition } from "react";
import Link from "next/link";
import { useRouter } from "next/navigation";
import { toast } from "sonner";
import { Copy, Eye, Plus, RefreshCw, Trash2, X, Zap } from "lucide-react";
import { api, type DatabaseInfo } from "@/lib/api";
import { Badge, Btn, Card, CopyRow, Input, PageHeader, Table, Td, Th, useConfirm } from "@/components/ui";
import { compareValue, DbStatusBadge, ErrorState, LoadingTable, PaginationBar, RelativeTime, RowAction, RowActions, SearchInput, SortHeader, type SortDir, useAsyncList, usePagedRows } from "@/components/list-quality";
import dynamic from "next/dynamic";
// Only rendered in the first-run empty state — keep it out of the main chunk.
const Quickstart = dynamic(() => import("@/components/Quickstart").then(m => m.Quickstart), { ssr: false });

function msg(e: unknown) { return e instanceof Error ? e.message : String(e); }

type SortKey = "id" | "label" | "status" | "created_at";

export default function DatabasesPage() {
  const { items, loading, error, refresh } = useAsyncList<DatabaseInfo>(
    () => api.databases(),
    { pollMs: 4000, cacheKey: "databases", onError: (m) => toast.error("Failed to fetch databases: " + m) },
  );
  const [pending, start] = useTransition();
  const [showCreate, setShowCreate] = useState(false);
  const [form, setForm] = useState<{ label: string }>({ label: "" });
  const [search, setSearch] = useState("");
  const [sort, setSort] = useState<{ key: SortKey; dir: SortDir }>({ key: "created_at", dir: "desc" });
  const [conn, setConn] = useState<DatabaseInfo | null>(null);
  const confirm = useConfirm();
  const router = useRouter();

  const create = (e: React.FormEvent) => {
    e.preventDefault();
    start(async () => {
      const id = toast.loading("Creating database…");
      try {
        // The API returns 202 immediately ({status:"provisioning"}, no creds yet).
        // Rather than block the list on the 30–90s cold boot, send the user
        // straight to the database's own page, which polls and reveals the
        // connection details the moment postgres is ready.
        const created = await api.createDatabase(form.label ? { label: form.label } : {});
        setShowCreate(false);
        setForm({ label: "" });
        toast.success("Database created — provisioning…", { id });
        router.push(`/databases/${created.id}`);
      } catch (e) { toast.error("Create failed: " + msg(e), { id }); }
    });
  };

  const reveal = (dbId: string) => start(async () => {
    const id = toast.loading("Fetching connection…");
    try { const db = await api.getDatabase(dbId); setConn(db); toast.dismiss(id); }
    catch (e) { toast.error("Could not fetch connection: " + msg(e), { id }); }
  });

  const remove = (dbId: string) => start(async () => {
    const id = toast.loading("Deleting database…");
    try { await api.deleteDatabase(dbId); toast.success("Database deleted", { id }); await refresh(); }
    catch (e) { toast.error("Delete failed: " + msg(e), { id }); }
  });

  const wake = (dbId: string) => start(async () => {
    const id = toast.loading("Waking database…");
    try {
      await api.wakeDatabase(dbId);
      toast.success("Database is waking — it will report running shortly", { id });
      await refresh();
    } catch (e) { toast.error("Wake failed: " + msg(e), { id }); }
  });

  const filtered = useMemo(() => {
    const q = search.toLowerCase().trim();
    const rows = items.filter((d) => !q || d.id.toLowerCase().includes(q) || (d.label ?? "").toLowerCase().includes(q) || (d.status ?? "").toLowerCase().includes(q));
    return rows.sort((a, b) => {
      const cmp = compareValue(a[sort.key], b[sort.key]);
      return sort.dir === "asc" ? cmp : -cmp;
    });
  }, [items, search, sort]);
  const { page, setPage, pageSize, pageRows } = usePagedRows(filtered);
  const toggleSort = (key: SortKey) => setSort((s) => s.key === key ? { key, dir: s.dir === "asc" ? "desc" : "asc" } : { key, dir: key === "created_at" ? "desc" : "asc" });

  return <>
    <PageHeader
      title="Databases"
      description="Managed PostgreSQL 16 — a real database in seconds."
      badge={<Badge variant="warning">Beta</Badge>}
      actions={<Btn variant="primary" size="sm" icon={<Plus size={13} />} onClick={() => setShowCreate((v) => !v)}>{items.length === 0 ? "Create your first database" : "Create database"}</Btn>}
    />

    {showCreate && (
      <Card className="mb-4 p-4">
        <div className="mb-3 text-[12px] font-semibold" style={{ color: "var(--text-secondary)" }}>New PostgreSQL 16 database</div>
        <form onSubmit={create} className="flex flex-wrap items-end gap-3">
          <Input label="Label (optional)" placeholder="my-app-db" value={form.label} onChange={(e) => setForm((f) => ({ ...f, label: e.target.value }))} className="w-64" />
          <Btn variant="primary" type="submit" disabled={pending}>{pending ? "Provisioning…" : "Create database"}</Btn>
          <Btn variant="ghost" onClick={() => setShowCreate(false)}>Cancel</Btn>
        </form>
        <p className="mt-3 text-[11px]" style={{ color: "var(--text-muted)" }}>Durable storage with continuous WAL archiving. Connection credentials are shown once on creation — store them securely.</p>
      </Card>
    )}

    {conn && <ConnectionCard db={conn} onClose={() => setConn(null)} />}

    {error && <div className="mb-3"><ErrorState error={error} onRetry={() => void refresh()} /></div>}

    <div className="mb-3 flex items-center gap-2">
      <SearchInput value={search} onChange={setSearch} placeholder="Filter databases…" />
      <Btn size="sm" variant="ghost" icon={<RefreshCw size={12} />} onClick={refresh} disabled={pending} className="ml-auto">Refresh</Btn>
    </div>

    <Card>
      {loading ? <LoadingTable cols={5} /> : (
        <Table>
          <thead><tr>
            <SortHeader label="ID" sortKey="id" current={sort} onSort={toggleSort} />
            <SortHeader label="Label" sortKey="label" current={sort} onSort={toggleSort} />
            <SortHeader label="Status" sortKey="status" current={sort} onSort={toggleSort} />
            <SortHeader label="Created" sortKey="created_at" current={sort} onSort={toggleSort} className="hidden lg:table-cell" />
            <Th right>Actions</Th>
          </tr></thead>
          <tbody>{pageRows.map((d) => (
            <tr key={d.id} className="group">
              <Td><Link href={`/databases/${d.id}`} className="font-mono text-[12px] font-medium hover:underline" style={{ color: "var(--text-primary)" }}>{d.id.slice(0, 12)}…</Link></Td>
              <Td muted><Link href={`/databases/${d.id}`} className="hover:underline">{d.label || "—"}</Link></Td>
              <Td><DbStatusBadge status={d.status} /></Td>
              <Td muted className="hidden lg:table-cell">{d.created_at ? <RelativeTime value={new Date(d.created_at * 1000).toISOString()} /> : "—"}</Td>
              <Td right>
                <RowActions>
                  {d.status === "hibernated" && (
                    <RowAction onClick={() => wake(d.id)}><Zap size={12} />Resume now</RowAction>
                  )}
                  <RowAction onClick={() => reveal(d.id)}><Eye size={12} />Connection</RowAction>
                  <RowAction onClick={() => void navigator.clipboard.writeText(d.id).then(() => toast.success("Copied"))}><Copy size={12} />Copy ID</RowAction>
                  <RowAction destructive onClick={async () => {
                    const ok = await confirm({ title: `Delete database ${d.id.slice(0, 8)}?`, description: "This permanently destroys the database and all its data. This cannot be undone.", confirmLabel: "Delete", destructive: true });
                    if (ok) remove(d.id);
                  }}><Trash2 size={12} />Delete</RowAction>
                </RowActions>
              </Td>
            </tr>
          ))}</tbody>
        </Table>
      )}
    </Card>
    {!loading && filtered.length === 0 && !search && <Quickstart resource="database" />}
    {!loading && filtered.length > 0 && <PaginationBar total={filtered.length} page={page} pageSize={pageSize} onPage={setPage} label="databases" />}
  </>;
}

function ConnectionCard({ db, onClose }: { db: DatabaseInfo; onClose: () => void }) {
  const ready = db.connection_url || db.host;
  return (
    <Card className="mb-4 p-4">
      <div className="mb-3 flex items-center justify-between">
        <div className="text-[12px] font-semibold" style={{ color: "var(--text-secondary)" }}>Connection — {db.id.slice(0, 12)}…</div>
        <button onClick={onClose} style={{ color: "var(--text-muted)" }} aria-label="Close"><X size={14} /></button>
      </div>
      {!ready ? (
        <p className="text-[12px]" style={{ color: "var(--text-muted)" }}>Connection info is not available yet — the database may still be starting. Try again in a moment.</p>
      ) : (
        <div className="flex flex-col gap-2">
          {db.connection_url && <CopyRow label="Connection URL" value={db.connection_url} />}
          {db.host && <CopyRow label="Host" value={db.host} />}
          {db.database && <CopyRow label="Database" value={db.database} />}
          {db.username && <CopyRow label="Username" value={db.username} />}
          {db.password && <CopyRow label="Password" value={db.password} secret />}
          {db.broker_url && <CopyRow label="REST query API" value={`${db.broker_url}/v1/query`} />}
          {db.broker_token && <CopyRow label="Broker token" value={db.broker_token} secret />}
          {db.broker_url && (
            <p className="text-[11px]" style={{ color: "var(--text-muted)" }}>
              POST <code className="font-mono">{`{"database":"${db.database || "pandastack"}","sql":"select 1"}`}</code> with <code className="font-mono">Authorization: Bearer &lt;broker token&gt;</code>. Both <code className="font-mono">database</code> and <code className="font-mono">sql</code> are required.{" "}
              <a href="https://docs.pandastack.ai/docs/concepts/databases/" target="_blank" rel="noreferrer" className="hover:underline" style={{ color: "var(--text-secondary)" }}>How to connect →</a>
            </p>
          )}
          <p className="mt-1 text-[11px]" style={{ color: "var(--text-muted)" }}>Store these credentials securely — the password is only retrievable while the database is running.</p>
        </div>
      )}
    </Card>
  );
}
