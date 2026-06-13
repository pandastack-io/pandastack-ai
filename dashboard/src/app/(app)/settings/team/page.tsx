// SPDX-License-Identifier: Apache-2.0
"use client";

import { useEffect, useMemo, useState, useTransition } from "react";
import { toast } from "sonner";
import { Check, Copy, MailPlus, Trash2, Users, X } from "lucide-react";
import { isStubAuth } from "@/lib/auth-mode";
import {
  type Org,
  type OrgMember,
  getMe,
  getOrgMembers,
  inviteMember,
  listOrgs,
  removeMember,
} from "@/lib/api";
import { Alert, Badge, Btn, Card, Input, PageHeader, Select, Table, Td, useConfirm } from "@/components/ui";
import { compareValue, ErrorState, LoadingTable, PaginationBar, RelativeTime, RowAction, RowActions, rowNavProps, SearchInput, SortHeader, type SortDir, useDebouncedValue, usePagedRows } from "@/components/list-quality";

function errorMessage(error: unknown) {
  return error instanceof Error ? error.message : String(error);
}

function roleBadge(role: OrgMember["role"]) {
  const variant = role === "owner" ? "info" : role === "admin" ? "violet" : "default";
  return <Badge variant={variant}>{role}</Badge>;
}

async function resolveCurrentOrg(): Promise<Org | null> {
  try {
    const me = await getMe();
    const stored = typeof window !== "undefined" ? window.localStorage.getItem("pandastack_org_id") : null;
    return me.orgs.find((org) => org.id === (me.current_org_id ?? stored)) ?? me.orgs[0] ?? null;
  } catch {
    const orgs = await listOrgs();
    const stored = typeof window !== "undefined" ? window.localStorage.getItem("pandastack_org_id") : null;
    return orgs.find((org) => org.id === stored) ?? orgs[0] ?? null;
  }
}

export default function TeamPage() {
  const [org, setOrg] = useState<Org | null>(null);
  const [members, setMembers] = useState<OrgMember[]>([]);
  const [loading, setLoading] = useState(true);
  const [email, setEmail] = useState("");
  const [role, setRole] = useState<"member" | "admin">("member");
  const [inviteUrl, setInviteUrl] = useState<string | null>(null);
  const [copied, setCopied] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [pending, start] = useTransition();
  const [query, setQuery] = useState("");
  const debouncedQuery = useDebouncedValue(query);
  const [sort, setSort] = useState<{ key: "email" | "user_id" | "role" | "joined_at"; dir: SortDir }>({ key: "joined_at", dir: "desc" });
  const confirm = useConfirm();

  const refresh = async () => {
    setLoading(true);
    setError(null);
    try {
      const current = await resolveCurrentOrg();
      setOrg(current);
      setMembers(current ? await getOrgMembers(current.id) : []);
    } catch (e) {
      const msg = errorMessage(e);
      setError(msg);
      toast.error(`Failed to load team: ${msg}`);
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    if (isStubAuth()) {
      setLoading(false);
      return;
    }
    refresh();
  }, []);

  const submitInvite = (e: React.FormEvent) => {
    e.preventDefault();
    if (!org) return;
    const trimmed = email.trim();
    if (!trimmed) return;
    start(async () => {
      try {
        const invite = await inviteMember(org.id, trimmed, role);
        setInviteUrl(invite.invite_url);
        setCopied(false);
        setEmail("");
        toast.success("Invite created");
        // Send invite email best-effort
        fetch("/api/send-invite", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ email: trimmed, invite_url: invite.invite_url, org_name: org.name }),
        }).catch(() => {/* best-effort */});
        await refresh();
      } catch (err) {
        toast.error(`Invite failed: ${errorMessage(err)}`);
      }
    });
  };

  const remove = async (member: OrgMember) => {
    if (!org) return;
    const ok = await confirm({
      title: `Remove ${member.email}?`,
      description: `They'll lose access to ${org.name} immediately. You can re-invite them later.`,
      confirmLabel: "Remove",
      destructive: true,
    });
    if (!ok) return;
    start(async () => {
      try {
        await removeMember(org.id, member.user_id);
        setMembers((current) => current.filter((item) => item.user_id !== member.user_id));
        toast.success("Member removed");
      } catch (err) {
        toast.error(`Remove failed: ${errorMessage(err)}`);
      }
    });
  };


  const filtered = useMemo(() => {
    const q = debouncedQuery.trim().toLowerCase();
    return members
      .filter((m) => !q || m.email.toLowerCase().includes(q) || m.user_id.toLowerCase().includes(q) || m.role.toLowerCase().includes(q))
      .sort((a, b) => { const cmp = compareValue(a[sort.key], b[sort.key]); return sort.dir === "asc" ? cmp : -cmp; });
  }, [members, debouncedQuery, sort]);
  const { page, setPage, pageSize, pageRows } = usePagedRows(filtered);
  const toggleSort = (key: "email" | "user_id" | "role" | "joined_at") => setSort((s) => s.key === key ? { key, dir: s.dir === "asc" ? "desc" : "asc" } : { key, dir: key === "joined_at" ? "desc" : "asc" });

  const copyInvite = async () => {
    if (!inviteUrl) return;
    try {
      await navigator.clipboard.writeText(inviteUrl);
      setCopied(true);
      toast.success("Copied invite URL");
    } catch {
      toast.error("Copy failed");
    }
  };

  if (isStubAuth()) {
    return (
      <>
        <PageHeader title="Team & Members" description="Team management is disabled in local stub auth mode." />
        <Card padding>
          <p className="text-sm" style={{ color: "var(--text-secondary)" }}>Local dev mode uses one hardcoded user. Invitations and member management are hidden because auth is disabled.</p>
        </Card>
      </>
    );
  }

  return (
    <>
      <PageHeader
        title="Team & Members"
        description={org ? `Manage access for ${org.name}.` : "Invite teammates and manage organization roles."}
        badge={<Badge variant="default">{members.length} members</Badge>}
      />

      {error && <div className="mb-4"><ErrorState error={error} onRetry={() => void refresh()} /></div>}
      <div className="mb-3 flex flex-col gap-2 sm:flex-row sm:items-center sm:justify-between"><SearchInput value={query} onChange={setQuery} placeholder="Filter members…" /><div className="text-[12px]" style={{ color: "var(--text-muted)" }}>{filtered.length} of {members.length} members</div></div>

      <div className="grid gap-5 lg:grid-cols-[1fr_320px]">
        <Card>
          {loading ? (
            <LoadingTable cols={4} rows={4} />
          ) : (
            <Table>
              <thead><tr><SortHeader label="Email" sortKey="email" current={sort} onSort={toggleSort} /><SortHeader label="Role" sortKey="role" current={sort} onSort={toggleSort} /><SortHeader label="Joined" sortKey="joined_at" current={sort} onSort={toggleSort} /><th className="px-4 py-2.5 text-right text-[11px] font-medium uppercase tracking-wider" style={{ color: "var(--text-muted)", borderBottom: "1px solid var(--border-subtle)" }}>Actions</th></tr></thead>
              <tbody>
                {pageRows.map((member, i) => (
                  <tr key={member.user_id} className="focus:outline-none focus:ring-1 focus:ring-emerald-500/40" {...rowNavProps(i)}>
                    <Td>{member.email}</Td>
                    <Td>{roleBadge(member.role)}</Td>
                    <Td muted><RelativeTime value={member.joined_at} /></Td>
                    <Td right>
                      <RowActions><RowAction onClick={() => navigator.clipboard.writeText(member.user_id).then(() => toast.success("Copied"))}><Copy size={12} />Copy user ID</RowAction><RowAction destructive disabled={pending || member.role === "owner"} onClick={() => remove(member)}><Trash2 size={12} />Remove</RowAction></RowActions>
                    </Td>
                  </tr>
                ))}
              </tbody>
            </Table>
          )}
        </Card>

        <Card padding>
          <form onSubmit={submitInvite} className="space-y-4">
            <div>
              <h2 className="text-[13px] font-semibold" style={{ color: "var(--text-primary)" }}>Invite member</h2>
              <p className="mt-1 text-[12px]" style={{ color: "var(--text-muted)" }}>Send an invite link for this organization.</p>
            </div>
            <Input id="team-invite-email" label="Email" type="email" required value={email} onChange={(e) => setEmail(e.target.value)} placeholder="teammate@example.com" />
            <Select label="Role" value={role} onChange={(e) => setRole(e.target.value as "member" | "admin")}>
              <option value="member">Member</option>
              <option value="admin">Admin</option>
            </Select>
            <Btn variant="primary" type="submit" disabled={pending || !org} icon={<MailPlus size={13} />}>
              {pending ? "Inviting…" : "Invite member"}
            </Btn>
          </form>
        </Card>
      </div>
      {!loading && filtered.length > 0 && <PaginationBar total={filtered.length} page={page} pageSize={pageSize} onPage={setPage} label="members" />}

      {inviteUrl && (
        <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/50 px-4" onClick={() => setInviteUrl(null)}>
          <div className="w-full max-w-lg rounded-xl shadow-2xl" style={{ background: "var(--bg-elevated)", border: "1px solid var(--border-strong)" }} onClick={(e) => e.stopPropagation()}>
            <div className="flex items-center justify-between px-4 py-3" style={{ borderBottom: "1px solid var(--border-subtle)" }}>
              <div className="text-[13px] font-semibold" style={{ color: "var(--text-primary)" }}>Invite URL</div>
              <button onClick={() => setInviteUrl(null)} className="rounded p-1" style={{ color: "var(--text-muted)" }}><X size={14} /></button>
            </div>
            <div className="space-y-4 p-4">
              <Alert type="success">Share this URL with the invited member.</Alert>
              <div className="flex items-start gap-2">
                <code className="flex-1 select-all break-all rounded-md px-3 py-2.5 font-mono text-[12px]" style={{ background: "var(--bg-base)", border: "1px solid var(--border-subtle)", color: "var(--text-primary)" }}>{inviteUrl}</code>
                <Btn variant="secondary" size="sm" icon={copied ? <Check size={12} /> : <Copy size={12} />} onClick={copyInvite}>{copied ? "Copied" : "Copy"}</Btn>
              </div>
            </div>
          </div>
        </div>
      )}
    </>
  );
}
