// SPDX-License-Identifier: Apache-2.0
"use client";

import Link from "next/link";
import { useEffect, useMemo, useState, useTransition } from "react";
import { Code2, Play, Plus, RefreshCw, Trash2 } from "lucide-react";
import { toast } from "sonner";
import { api, functionEndpoint, type FunctionInfo } from "@/lib/api";
import { Badge, Btn, Card, Input, PageHeader, Select, Table, Td, Th, useConfirm } from "@/components/ui";
import { compareValue, ErrorState, LoadingTable, PaginationBar, RelativeTime, RowAction, RowActions, rowNavProps, SearchInput, SortHeader, type SortDir, useDebouncedValue, usePagedRows } from "@/components/list-quality";
import { Quickstart } from "@/components/Quickstart";

type SortKey = "name" | "runtime" | "template" | "created_at";

function msg(error: unknown) {
  return error instanceof Error ? error.message : String(error);
}

function parseEnv(raw: string): Record<string, string> | undefined {
  const entries = raw
    .split(/[\n,]/)
    .map((part) => part.trim())
    .filter(Boolean)
    .map((entry) => {
      const [key, ...rest] = entry.split("=");
      return [key?.trim(), rest.join("=").trim()] as const;
    })
    .filter(([key]) => !!key);
  if (entries.length === 0) return undefined;
  return Object.fromEntries(entries as Array<[string, string]>);
}

export default function FunctionsPage() {
  const [items, setItems] = useState<FunctionInfo[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [pending, start] = useTransition();
  const [showDeploy, setShowDeploy] = useState(false);
  const [search, setSearch] = useState("");
  const debouncedSearch = useDebouncedValue(search);
  const [sort, setSort] = useState<{ key: SortKey; dir: SortDir }>({ key: "created_at", dir: "desc" });
  const [form, setForm] = useState({
    name: "",
    runtime: "python" as "python" | "nodejs",
    template: "code-interpreter",
    public: false,
    envText: "",
    file: null as File | null,
  });
  const confirm = useConfirm();

  const refresh = async () => {
    setError(null);
    try {
      setItems(await api.functions());
    } catch (error) {
      const message = msg(error);
      setError(message);
      toast.error(`Failed to fetch functions: ${message}`);
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    void refresh();
  }, []);

  const deploy = (event: React.FormEvent) => {
    event.preventDefault();
    if (!form.file) {
      toast.error("Choose a source file to deploy");
      return;
    }
    const file = form.file;
    start(async () => {
      const id = toast.loading("Deploying function…");
      try {
        await api.deployFunction({
          name: form.name || file.name.replace(/\.[^.]+$/, ""),
          runtime: form.runtime,
          file,
          template: form.template,
          public: form.public,
          env: parseEnv(form.envText),
        });
        setShowDeploy(false);
        setForm({ name: "", runtime: "python", template: "code-interpreter", public: false, envText: "", file: null });
        toast.success("Function deployed", { id });
        await refresh();
      } catch (error) {
        toast.error(`Deploy failed: ${msg(error)}`, { id });
      }
    });
  };

  const act = (label: string, fn: () => Promise<unknown>) =>
    start(async () => {
      const id = toast.loading(`${label}…`);
      try {
        await fn();
        toast.success(`${label} complete`, { id });
        await refresh();
      } catch (error) {
        toast.error(`${label} failed: ${msg(error)}`, { id });
      }
    });

  const filtered = useMemo(() => {
    const query = debouncedSearch.trim().toLowerCase();
    return items
      .filter((item) => !query || [item.name, item.runtime, item.template ?? "", functionEndpoint(item) ?? ""].some((value) => value.toLowerCase().includes(query)))
      .sort((a, b) => {
        const left = sort.key === "template" ? a.template ?? "" : a[sort.key];
        const right = sort.key === "template" ? b.template ?? "" : b[sort.key];
        const comparison = compareValue(left, right);
        return sort.dir === "asc" ? comparison : -comparison;
      });
  }, [items, debouncedSearch, sort]);

  const { page, setPage, pageSize, pageRows } = usePagedRows(filtered);
  const toggleSort = (key: SortKey) => setSort((current) => current.key === key ? { key, dir: current.dir === "asc" ? "desc" : "asc" } : { key, dir: key === "created_at" ? "desc" : "asc" });

  return (
    <>
      <PageHeader
        title="Functions"
        description="Deploy Python or Node.js functions into isolated microVMs and trigger them on demand."
        badge={<span className="rounded-full px-2 py-0.5 text-[11px] font-medium" style={{ background: "var(--bg-elevated)", color: "var(--text-muted)", border: "1px solid var(--border-default)" }}>{items.length}</span>}
        actions={<Btn variant="primary" size="sm" icon={<Plus size={13} />} onClick={() => setShowDeploy((value) => !value)}>{items.length === 0 ? "New Function" : "New Function"}</Btn>}
      />

      {showDeploy && (
        <Card className="mb-4 p-4">
          <div className="mb-3 text-[12px] font-semibold" style={{ color: "var(--text-secondary)" }}>Deployment</div>
          <form onSubmit={deploy} className="grid gap-3 lg:grid-cols-2">
            <Input label="Name" value={form.name} onChange={(event) => setForm((current) => ({ ...current, name: event.target.value }))} placeholder="my-fn" />
            <Select label="Runtime" value={form.runtime} onChange={(event) => setForm((current) => ({ ...current, runtime: event.target.value as "python" | "nodejs" }))}>
              <option value="python">python</option>
              <option value="nodejs">nodejs</option>
            </Select>
            <Input label="Template" value={form.template} onChange={(event) => setForm((current) => ({ ...current, template: event.target.value }))} placeholder="code-interpreter" />
            <label className="flex flex-col gap-1">
              <span className="text-[11px] font-medium uppercase tracking-wider" style={{ color: "var(--text-secondary)" }}>Source file</span>
              <input type="file" onChange={(event) => setForm((current) => ({ ...current, file: event.target.files?.[0] ?? null }))} className="rounded-md border px-3 py-1.5 text-[13px]" style={{ background: "var(--bg-elevated)", border: "1px solid var(--border-default)", color: "var(--text-primary)" }} />
            </label>
            <label className="flex items-center gap-2 pt-6 text-[13px]" style={{ color: "var(--text-secondary)" }}>
              <input type="checkbox" checked={form.public} onChange={(event) => setForm((current) => ({ ...current, public: event.target.checked }))} className="rounded accent-emerald-500" />
              Public HTTP endpoint
            </label>
            <label className="lg:col-span-2 flex flex-col gap-1">
              <span className="text-[11px] font-medium uppercase tracking-wider" style={{ color: "var(--text-secondary)" }}>Env vars</span>
              <textarea value={form.envText} onChange={(event) => setForm((current) => ({ ...current, envText: event.target.value }))} placeholder="KEY=VALUE" rows={4} className="rounded-md border px-3 py-2 text-[13px] focus:outline-none focus:border-emerald-500/60" style={{ background: "var(--bg-elevated)", border: "1px solid var(--border-default)", color: "var(--text-primary)" }} />
            </label>
            <div className="lg:col-span-2 flex items-center gap-2">
              <Btn variant="primary" type="submit" disabled={pending}>{pending ? "Deploying…" : "Deploy"}</Btn>
              <Btn variant="ghost" onClick={() => setShowDeploy(false)}>Cancel</Btn>
            </div>
          </form>
        </Card>
      )}

      {error && <div className="mb-3"><ErrorState error={error} onRetry={() => void refresh()} /></div>}

      <div className="mb-3 flex flex-col gap-2 lg:flex-row lg:items-center">
        <SearchInput value={search} onChange={setSearch} placeholder="Filter functions…" />
        <div className="lg:ml-auto flex items-center gap-2">
          <Btn size="sm" variant="ghost" icon={<RefreshCw size={12} />} onClick={() => void refresh()} disabled={pending}>Refresh</Btn>
        </div>
      </div>

      <Card>
        {loading ? <LoadingTable cols={6} /> : (
          <Table>
            <thead>
              <tr>
                <SortHeader label="Name" sortKey="name" current={sort} onSort={toggleSort} />
                <SortHeader label="Runtime" sortKey="runtime" current={sort} onSort={toggleSort} />
                <SortHeader label="Template" sortKey="template" current={sort} onSort={toggleSort} className="hidden md:table-cell" />
                <Th>Public URL</Th>
                <SortHeader label="Created" sortKey="created_at" current={sort} onSort={toggleSort} className="hidden lg:table-cell" />
                <Th right>Actions</Th>
              </tr>
            </thead>
            <tbody>
              {pageRows.map((item, index) => {
                const endpoint = functionEndpoint(item);
                return (
                  <tr key={item.id} className="group transition-colors" onMouseEnter={(event) => { event.currentTarget.style.background = "var(--bg-elevated)"; }} onMouseLeave={(event) => { event.currentTarget.style.background = ""; }} {...rowNavProps(index, () => { window.location.href = `/functions/${item.id}`; })}>
                    <Td>
                      <Link href={`/functions/${item.id}`} className="font-medium transition-colors hover:text-emerald-400" style={{ color: "var(--text-primary)" }}>{item.name}</Link>
                      <div className="mt-0.5 text-[10px] font-mono" style={{ color: "var(--text-muted)" }}>{item.id.slice(0, 12)}…</div>
                    </Td>
                    <Td muted>{item.runtime}</Td>
                    <Td muted className="hidden md:table-cell">{item.template ?? "—"}</Td>
                    <Td className="max-w-[320px]">
                      {endpoint ? (
                        <div className="space-y-1">
                          <Badge variant={item.public ? "success" : "default"}>{item.public ? "Public" : "Private"}</Badge>
                          <a href={endpoint} target="_blank" rel="noreferrer" className="block truncate text-[12px] hover:underline" style={{ color: "var(--text-secondary)" }}>{endpoint}</a>
                        </div>
                      ) : <span style={{ color: "var(--text-muted)" }}>—</span>}
                    </Td>
                    <Td muted className="hidden lg:table-cell"><RelativeTime value={item.created_at} /></Td>
                    <Td right>
                      <RowActions>
                        <RowAction onClick={() => { window.location.href = `/functions/${item.id}`; }}>View</RowAction>
                        <RowAction onClick={() => act("Invoke", () => api.triggerFunction(item.id))}><Play size={12} />Invoke</RowAction>
                        <RowAction destructive onClick={async () => {
                          const ok = await confirm({ title: `Delete function ${item.name}?`, description: "This removes the function and all future invocations. This cannot be undone.", confirmLabel: "Delete", destructive: true });
                          if (ok) act("Delete", () => api.deleteFunction(item.id));
                        }}><Trash2 size={12} />Delete</RowAction>
                      </RowActions>
                    </Td>
                  </tr>
                );
              })}
            </tbody>
          </Table>
        )}
      </Card>

      {!loading && filtered.length === 0 && !search && <Quickstart resource="function" />}
      {!loading && filtered.length > 0 && <PaginationBar total={filtered.length} page={page} pageSize={pageSize} onPage={setPage} label="functions" />}
    </>
  );
}
