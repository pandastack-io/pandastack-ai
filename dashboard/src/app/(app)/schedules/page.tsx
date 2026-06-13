// SPDX-License-Identifier: Apache-2.0
"use client";

import Link from "next/link";
import { useEffect, useMemo, useState, useTransition } from "react";
import { Clock, Pause, Play, Plus, RefreshCw, Trash2 } from "lucide-react";
import { toast } from "sonner";
import { api, type FunctionInfo, type ScheduleInfo } from "@/lib/api";
import { Badge, Btn, Card, Input, PageHeader, Select, Table, Td, Th, useConfirm } from "@/components/ui";
import { compareValue, ErrorState, LoadingTable, PaginationBar, RelativeTime, RowAction, RowActions, rowNavProps, SearchInput, SortHeader, type SortDir, useDebouncedValue, usePagedRows } from "@/components/list-quality";
import { Quickstart } from "@/components/Quickstart";

function msg(error: unknown) {
  return error instanceof Error ? error.message : String(error);
}

type SortKey = "name" | "function" | "cron" | "paused" | "last_run_at" | "created_at";

export default function SchedulesPage() {
  const [items, setItems] = useState<ScheduleInfo[]>([]);
  const [functions, setFunctions] = useState<FunctionInfo[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [pending, start] = useTransition();
  const [showCreate, setShowCreate] = useState(false);
  const [search, setSearch] = useState("");
  const debouncedSearch = useDebouncedValue(search);
  const [sort, setSort] = useState<{ key: SortKey; dir: SortDir }>({ key: "created_at", dir: "desc" });
  const [form, setForm] = useState({ name: "", function_id: "", cron: "0 9 * * *" });
  const confirm = useConfirm();

  const refresh = async () => {
    setError(null);
    try {
      const [scheduleItems, functionItems] = await Promise.all([api.schedules(), api.functions()]);
      setItems(scheduleItems);
      setFunctions(functionItems);
      setForm((current) => ({ ...current, function_id: current.function_id || functionItems[0]?.id || "" }));
    } catch (error) {
      const message = msg(error);
      setError(message);
      toast.error(`Failed to fetch schedules: ${message}`);
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    void refresh();
  }, []);

  const functionNames = useMemo(() => Object.fromEntries(functions.map((item) => [item.id, item.name])), [functions]);

  const create = (event: React.FormEvent) => {
    event.preventDefault();
    start(async () => {
      const id = toast.loading("Creating schedule…");
      try {
        await api.createSchedule({ ...form, paused: false });
        setShowCreate(false);
        setForm((current) => ({ ...current, name: "", cron: "0 9 * * *" }));
        toast.success("Schedule created", { id });
        await refresh();
      } catch (error) {
        toast.error(`Create failed: ${msg(error)}`, { id });
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
      .filter((item) => !query || [item.name, item.cron, functionNames[item.function_id] ?? item.function_id].some((value) => value.toLowerCase().includes(query)))
      .sort((a, b) => {
        const left = sort.key === "function"
          ? functionNames[a.function_id] ?? a.function_id
          : sort.key === "paused"
            ? (a.paused ? 1 : 0)
            : a[sort.key];
        const right = sort.key === "function"
          ? functionNames[b.function_id] ?? b.function_id
          : sort.key === "paused"
            ? (b.paused ? 1 : 0)
            : b[sort.key];
        const comparison = compareValue(left, right);
        return sort.dir === "asc" ? comparison : -comparison;
      });
  }, [items, debouncedSearch, functionNames, sort]);

  const { page, setPage, pageSize, pageRows } = usePagedRows(filtered);
  const toggleSort = (key: SortKey) => setSort((current) => current.key === key ? { key, dir: current.dir === "asc" ? "desc" : "asc" } : { key, dir: key === "created_at" || key === "last_run_at" ? "desc" : "asc" });

  return (
    <>
      <PageHeader
        title="Schedules"
        description="Run functions on cron schedules. Every invocation gets a fresh isolated microVM."
        badge={<span className="rounded-full px-2 py-0.5 text-[11px] font-medium" style={{ background: "var(--bg-elevated)", color: "var(--text-muted)", border: "1px solid var(--border-default)" }}>{items.length}</span>}
        actions={<Btn variant="primary" size="sm" icon={<Plus size={13} />} onClick={() => setShowCreate((value) => !value)}>New Schedule</Btn>}
      />

      {showCreate && (
        <Card className="mb-4 p-4">
          <div className="mb-3 text-[12px] font-semibold" style={{ color: "var(--text-secondary)" }}>Create schedule</div>
          <form onSubmit={create} className="grid gap-3 lg:grid-cols-3">
            <Input label="Name" value={form.name} onChange={(event) => setForm((current) => ({ ...current, name: event.target.value }))} placeholder="daily-report" />
            <Select label="Function" value={form.function_id} onChange={(event) => setForm((current) => ({ ...current, function_id: event.target.value }))}>
              {functions.map((item) => <option key={item.id} value={item.id}>{item.name}</option>)}
            </Select>
            <Input label="Cron" value={form.cron} onChange={(event) => setForm((current) => ({ ...current, cron: event.target.value }))} placeholder="0 9 * * *" />
            <div className="lg:col-span-3 flex items-center gap-2">
              <Btn variant="primary" type="submit" disabled={pending || !form.function_id}>{pending ? "Creating…" : "Create"}</Btn>
              <Btn variant="ghost" onClick={() => setShowCreate(false)}>Cancel</Btn>
            </div>
          </form>
        </Card>
      )}

      {error && <div className="mb-3"><ErrorState error={error} onRetry={() => void refresh()} /></div>}

      <div className="mb-3 flex flex-col gap-2 lg:flex-row lg:items-center">
        <SearchInput value={search} onChange={setSearch} placeholder="Filter schedules…" />
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
                <SortHeader label="Function" sortKey="function" current={sort} onSort={toggleSort} />
                <SortHeader label="Cron" sortKey="cron" current={sort} onSort={toggleSort} />
                <SortHeader label="Status" sortKey="paused" current={sort} onSort={toggleSort} />
                <SortHeader label="Last Run" sortKey="last_run_at" current={sort} onSort={toggleSort} />
                <SortHeader label="Created" sortKey="created_at" current={sort} onSort={toggleSort} className="hidden lg:table-cell" />
                <Th right>Actions</Th>
              </tr>
            </thead>
            <tbody>
              {pageRows.map((item, index) => (
                <tr key={item.id} className="group transition-colors" onMouseEnter={(event) => { event.currentTarget.style.background = "var(--bg-elevated)"; }} onMouseLeave={(event) => { event.currentTarget.style.background = ""; }} {...rowNavProps(index, () => { window.location.href = `/schedules/${item.id}`; })}>
                  <Td>
                    <Link href={`/schedules/${item.id}`} className="font-medium transition-colors hover:text-emerald-400" style={{ color: "var(--text-primary)" }}>{item.name}</Link>
                    <div className="mt-0.5 text-[10px] font-mono" style={{ color: "var(--text-muted)" }}>{item.id.slice(0, 12)}…</div>
                  </Td>
                  <Td>
                    <Link href={`/functions/${item.function_id}`} className="hover:text-emerald-400" style={{ color: "var(--text-secondary)" }}>{functionNames[item.function_id] ?? item.function_id}</Link>
                  </Td>
                  <Td mono>{item.cron}</Td>
                  <Td><Badge variant={item.paused ? "warning" : "success"}>{item.paused ? "Paused" : "Active"}</Badge></Td>
                  <Td muted><RelativeTime value={item.last_run_at} /></Td>
                  <Td muted className="hidden lg:table-cell"><RelativeTime value={item.created_at} /></Td>
                  <Td right>
                    <RowActions>
                      <RowAction onClick={() => { window.location.href = `/schedules/${item.id}`; }}>View</RowAction>
                      <RowAction onClick={() => act("Trigger", () => api.triggerSchedule(item.id))}><Play size={12} />Trigger now</RowAction>
                      {item.paused ? <RowAction onClick={() => act("Resume", () => api.updateSchedule(item.id, { paused: false }))}><Play size={12} />Resume</RowAction> : <RowAction onClick={() => act("Pause", () => api.updateSchedule(item.id, { paused: true }))}><Pause size={12} />Pause</RowAction>}
                      <RowAction destructive onClick={async () => {
                        const ok = await confirm({ title: `Delete schedule ${item.name}?`, description: "This removes the cron schedule. Existing function runs are not affected.", confirmLabel: "Delete", destructive: true });
                        if (ok) act("Delete", () => api.deleteSchedule(item.id));
                      }}><Trash2 size={12} />Delete</RowAction>
                    </RowActions>
                  </Td>
                </tr>
              ))}
            </tbody>
          </Table>
        )}
      </Card>

      {!loading && filtered.length === 0 && !search && <Quickstart resource="schedule" />}
      {!loading && filtered.length > 0 && <PaginationBar total={filtered.length} page={page} pageSize={pageSize} onPage={setPage} label="schedules" />}
    </>
  );
}
