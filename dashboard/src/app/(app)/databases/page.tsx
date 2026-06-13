// SPDX-License-Identifier: Apache-2.0
"use client";

import { useEffect, useMemo, useState, useTransition } from "react";
import Link from "next/link";
import { toast } from "sonner";
import { Copy, Database, Eye, Plus, RefreshCw, Trash2, X } from "lucide-react";
import { api, type DatabaseInfo } from "@/lib/api";
import { Badge, Btn, Card, Input, PageHeader, Table, Td, Th, useConfirm } from "@/components/ui";
import { ErrorState, LoadingTable, RelativeTime, RowAction, RowActions, StatusBadge } from "@/components/list-quality";
import { Quickstart } from "@/components/Quickstart";

function msg(e: unknown) { return e instanceof Error ? e.message : String(e); }

export default function DatabasesPage() {
  const [items, setItems] = useState<DatabaseInfo[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [pending, start] = useTransition();
  const [showCreate, setShowCreate] = useState(false);
  const [form, setForm] = useState<{ label: string }>({ label: "" });
  const [search, setSearch] = useState("");
  const [conn, setConn] = useState<DatabaseInfo | null>(null);
  const confirm = useConfirm();

  const refresh = async () => {
    setError(null);
    try { setItems((await api.databases()) ?? []); }
    catch (e) { const m = msg(e); setError(m); toast.error("Failed to fetch databases: " + m); }
    finally { setLoading(false); }
  };

  useEffect(() => {
    refresh();
    const t = setInterval(refresh, 4000);
    return () => clearInterval(t);
  }, []);

  const create = (e: React.FormEvent) => {
    e.preventDefault();
    start(async () => {
      const id = toast.loading("Provisioning database… (cold boot can take ~60s)");
      try {
        const db = await api.createDatabase(form.label ? { label: form.label } : {});
        setShowCreate(false);
        setForm({ label: "" });
        toast.success("Database ready", { id });
        setConn(db);
        await refresh();
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

  const filtered = useMemo(() => {
    const q = search.toLowerCase().trim();
    return items.filter((d) => !q || d.id.toLowerCase().includes(q) || (d.label ?? "").toLowerCase().includes(q) || (d.status ?? "").toLowerCase().includes(q));
  }, [items, search]);

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
          <Input label="Label (optional)" placeholder="my-app-db" value={form.label} onChange={(e) => setForm({ label: e.target.value })} className="w-64" />
          <Btn variant="primary" type="submit" disabled={pending}>{pending ? "Provisioning…" : "Create database"}</Btn>
          <Btn variant="ghost" onClick={() => setShowCreate(false)}>Cancel</Btn>
        </form>
        <p className="mt-3 text-[11px]" style={{ color: "var(--text-muted)" }}>2 vCPU · 1 GiB · durable storage. Connection credentials are shown once on creation — store them securely.</p>
      </Card>
    )}

    {conn && <ConnectionCard db={conn} onClose={() => setConn(null)} />}

    {error && <div className="mb-3"><ErrorState error={error} onRetry={() => void refresh()} /></div>}

    <div className="mb-3 flex items-center gap-2">
      <input
        value={search}
        onChange={(e) => setSearch(e.target.value)}
        placeholder="Filter databases…"
        className="w-full max-w-xs rounded-md px-3 py-1.5 text-[13px] focus:outline-none"
        style={{ background: "var(--bg-elevated)", border: "1px solid var(--border-default)", color: "var(--text-primary)" }}
      />
      <Btn size="sm" variant="ghost" icon={<RefreshCw size={12} />} onClick={refresh} disabled={pending} className="ml-auto">Refresh</Btn>
    </div>

    <Card>
      {loading ? <LoadingTable cols={5} /> : (
        <Table>
          <thead><tr>
            <Th>ID</Th>
            <Th>Label</Th>
            <Th>Status</Th>
            <th className="hidden px-4 py-2.5 text-left text-[11px] font-medium uppercase tracking-wider lg:table-cell" style={{ color: "var(--text-muted)", borderBottom: "1px solid var(--border-subtle)" }}>Created</th>
            <Th right>Actions</Th>
          </tr></thead>
          <tbody>{filtered.map((d) => (
            <tr key={d.id} className="group">
              <Td><Link href={`/databases/${d.id}`} className="font-mono text-[12px] font-medium hover:underline" style={{ color: "var(--text-primary)" }}>{d.id.slice(0, 12)}…</Link></Td>
              <Td muted><Link href={`/databases/${d.id}`} className="hover:underline">{d.label || "—"}</Link></Td>
              <Td><StatusBadge value={d.status} /></Td>
              <Td muted className="hidden lg:table-cell">{d.created_at ? <RelativeTime value={new Date(d.created_at * 1000).toISOString()} /> : "—"}</Td>
              <Td right>
                <RowActions>
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
  </>;
}

function CopyRow({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex items-center gap-2">
      <span className="w-28 shrink-0 text-[11px] uppercase tracking-wider" style={{ color: "var(--text-muted)" }}>{label}</span>
      <code className="flex-1 truncate rounded px-2 py-1 font-mono text-[12px]" style={{ background: "var(--bg-elevated)", color: "var(--text-primary)", border: "1px solid var(--border-subtle)" }}>{value}</code>
      <Btn size="sm" variant="ghost" icon={<Copy size={12} />} onClick={() => void navigator.clipboard.writeText(value).then(() => toast.success("Copied"))}>Copy</Btn>
    </div>
  );
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
          {db.password && <CopyRow label="Password" value={db.password} />}
          {db.broker_url && <CopyRow label="REST query API" value={`${db.broker_url}/v1/query`} />}
          {db.broker_token && <CopyRow label="Broker token" value={db.broker_token} />}
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
