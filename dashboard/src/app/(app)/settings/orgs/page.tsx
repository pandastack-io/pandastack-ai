// SPDX-License-Identifier: Apache-2.0
"use client";

import { useEffect, useMemo, useState, useTransition } from "react";
import { useRouter } from "next/navigation";
import { toast } from "sonner";
import { Building2, Copy, Plus } from "lucide-react";
import { type Org, createOrg, getMe, getOrgMembers, listOrgs, setCurrentOrg } from "@/lib/api";
import { Badge, Btn, Card, Input, PageHeader, Table, Td } from "@/components/ui";
import { compareValue, ErrorState, LoadingTable, PaginationBar, RelativeTime, RowAction, RowActions, rowNavProps, SearchInput, SortHeader, type SortDir, useDebouncedValue, usePagedRows } from "@/components/list-quality";

function errorMessage(error: unknown) {
  return error instanceof Error ? error.message : String(error);
}

function roleBadge(role: Org["role"]) {
  const variant = role === "owner" ? "info" : role === "admin" ? "violet" : "default";
  return <Badge variant={variant}>{role}</Badge>;
}

export default function OrgsPage() {
  const router = useRouter();
  const [orgs, setOrgs] = useState<Org[]>([]);
  const [currentOrgId, setCurrentOrgId] = useState<string | null>(null);
  const [memberCounts, setMemberCounts] = useState<Record<string, number>>({});
  const [name, setName] = useState("");
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [pending, start] = useTransition();
  const [query, setQuery] = useState("");
  const debouncedQuery = useDebouncedValue(query);
  const [sort, setSort] = useState<{ key: "name" | "id" | "slug" | "role" | "created_at"; dir: SortDir }>({ key: "created_at", dir: "desc" });

  const refresh = async () => {
    setLoading(true);
    setError(null);
    try {
      let items: Org[];
      let current: string | null = typeof window !== "undefined" ? window.localStorage.getItem("pandastack_org_id") : null;
      try {
        const me = await getMe();
        items = me.orgs;
        current = me.current_org_id ?? current ?? me.orgs[0]?.id ?? null;
      } catch {
        items = await listOrgs();
        current = current ?? items[0]?.id ?? null;
      }
      setOrgs(items);
      setCurrentOrgId(current);
      const counts = await Promise.all(items.map(async (org) => {
        if (typeof org.member_count === "number") return [org.id, org.member_count] as const;
        try {
          const members = await getOrgMembers(org.id);
          return [org.id, members.length] as const;
        } catch {
          return [org.id, -1] as const;
        }
      }));
      setMemberCounts(Object.fromEntries(counts.filter(([, count]) => count >= 0)));
    } catch (e) {
      const msg = errorMessage(e);
      setError(msg);
      toast.error(`Failed to load organizations: ${msg}`);
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    refresh();
  }, []);

  const create = (e: React.FormEvent) => {
    e.preventDefault();
    const trimmed = name.trim();
    if (!trimmed) return;
    start(async () => {
      try {
        const org = await createOrg(trimmed);
        setName("");
        toast.success("Organization created");
        await setCurrentOrg(org.id);
        await refresh();
        router.refresh();
      } catch (err) {
        toast.error(`Create failed: ${errorMessage(err)}`);
      }
    });
  };

  const switchTo = (org: Org) => {
    start(async () => {
      try {
        await setCurrentOrg(org.id);
        setCurrentOrgId(org.id);
        window.dispatchEvent(new Event("pandastack-org-changed"));
        toast.success(`Switched to ${org.name}`);
        router.refresh();
      } catch (err) {
        toast.error(`Switch failed: ${errorMessage(err)}`);
      }
    });
  };


  const filtered = useMemo(() => {
    const q = debouncedQuery.trim().toLowerCase();
    return orgs
      .filter((org) => !q || org.name.toLowerCase().includes(q) || org.slug.toLowerCase().includes(q) || org.id.toLowerCase().includes(q) || org.role.toLowerCase().includes(q))
      .sort((a, b) => { const cmp = compareValue(a[sort.key], b[sort.key]); return sort.dir === "asc" ? cmp : -cmp; });
  }, [orgs, debouncedQuery, sort]);
  const { page, setPage, pageSize, pageRows } = usePagedRows(filtered);
  const toggleSort = (key: "name" | "id" | "slug" | "role" | "created_at") => setSort((s) => s.key === key ? { key, dir: s.dir === "asc" ? "desc" : "asc" } : { key, dir: key === "created_at" ? "desc" : "asc" });

  return (
    <>
      <PageHeader title="Organizations" description="Create, switch, and manage organizations you belong to." badge={<Badge>{orgs.length} orgs</Badge>} />
      {error && <div className="mb-4"><ErrorState error={error} onRetry={() => void refresh()} /></div>}
      <div className="mb-3 flex flex-col gap-2 sm:flex-row sm:items-center sm:justify-between"><SearchInput value={query} onChange={setQuery} placeholder="Filter organizations…" /><div className="text-[12px]" style={{ color: "var(--text-muted)" }}>{filtered.length} of {orgs.length} orgs</div></div>

      <div className="grid gap-5 lg:grid-cols-[1fr_320px]">
        <Card>
          {loading ? (
            <LoadingTable cols={6} rows={4} />
          ) : (
            <Table>
              <thead><tr><SortHeader label="Name" sortKey="name" current={sort} onSort={toggleSort} /><SortHeader label="Slug" sortKey="slug" current={sort} onSort={toggleSort} /><SortHeader label="Role" sortKey="role" current={sort} onSort={toggleSort} /><th className="px-4 py-2.5 text-left text-[11px] font-medium uppercase tracking-wider" style={{ color: "var(--text-muted)", borderBottom: "1px solid var(--border-subtle)" }}>Members</th><SortHeader label="Created" sortKey="created_at" current={sort} onSort={toggleSort} /><th className="px-4 py-2.5 text-right text-[11px] font-medium uppercase tracking-wider" style={{ color: "var(--text-muted)", borderBottom: "1px solid var(--border-subtle)" }}>Actions</th></tr></thead>
              <tbody>
                {pageRows.map((org, i) => (
                  <tr key={org.id} className="focus:outline-none focus:ring-1 focus:ring-emerald-500/40" {...rowNavProps(i)}>
                    <Td>
                      <div className="font-medium">{org.name}</div>
                      <div className="text-[11px]" style={{ color: "var(--text-muted)" }}>{org.id.slice(0, 10)}…</div>
                    </Td>
                    <Td mono>{org.slug}</Td>
                    <Td>{roleBadge(org.role)}</Td>
                    <Td muted>{memberCounts[org.id] ?? "—"}</Td>
                    <Td muted><RelativeTime value={org.created_at} /></Td>
                    <Td right>
                      <RowActions><RowAction onClick={() => navigator.clipboard.writeText(org.id).then(() => toast.success("Copied"))}><Copy size={12} />Copy ID</RowAction><RowAction disabled={pending || currentOrgId === org.id} onClick={() => switchTo(org)}>{currentOrgId === org.id ? "Current" : "Switch to"}</RowAction></RowActions>
                    </Td>
                  </tr>
                ))}
              </tbody>
            </Table>
          )}
        </Card>

        <Card padding>
          <form onSubmit={create} className="space-y-4">
            <div>
              <h2 className="text-[13px] font-semibold" style={{ color: "var(--text-primary)" }}>Create organization</h2>
              <p className="mt-1 text-[12px]" style={{ color: "var(--text-muted)" }}>A URL-safe slug is generated from the name.</p>
            </div>
            <Input id="org-name" label="Name" required value={name} onChange={(e) => setName(e.target.value)} placeholder="Acme Labs" />
            <Btn variant="primary" type="submit" disabled={pending || !name.trim()} icon={<Plus size={13} />}>
              {pending ? "Creating…" : "Create organization"}
            </Btn>
          </form>
        </Card>
      </div>
      {!loading && filtered.length > 0 && <PaginationBar total={filtered.length} page={page} pageSize={pageSize} onPage={setPage} label="organizations" />}
    </>
  );
}
