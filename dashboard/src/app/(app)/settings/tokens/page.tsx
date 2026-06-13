// SPDX-License-Identifier: Apache-2.0
"use client";

import { useEffect, useMemo, useState, useTransition } from "react";
import { toast } from "sonner";
import { Check, Copy, Key, Plus, Trash2, X } from "lucide-react";
import {
  type ApiToken,
  type NewApiToken,
  createApiToken,
  listApiTokens,
  revokeApiToken,
} from "@/lib/api";
import { Alert, Btn, Card, Input, PageHeader, Table, Tabs, Td, useConfirm } from "@/components/ui";
import { compareValue, ErrorState, LoadingTable, PaginationBar, RelativeTime, RowAction, RowActions, rowNavProps, SearchInput, SortHeader, type SortDir, useDebouncedValue, usePagedRows } from "@/components/list-quality";

type ExampleTab = "curl" | "python" | "cli";
type SortKey = "label" | "prefix" | "created_at";

const EXAMPLE_TOKEN = "pds_xxxxxxxx";

function formatRelative(value: string) {
  const time = new Date(value).getTime();
  if (Number.isNaN(time)) return value;

  const seconds = Math.max(0, Math.floor((Date.now() - time) / 1000));
  if (seconds < 60) return "just now";
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m ago`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours}h ago`;
  const days = Math.floor(hours / 24);
  if (days < 30) return `${days}d ago`;
  return new Date(value).toLocaleDateString();
}

function errorMessage(error: unknown) {
  return error instanceof Error ? error.message : String(error);
}

export default function TokensPage() {
  const [items, setItems] = useState<ApiToken[]>([]);
  const [loading, setLoading] = useState(true);
  const [listError, setListError] = useState<string | null>(null);
  const [pending, start] = useTransition();
  const [showCreate, setShowCreate] = useState(false);
  const [label, setLabel] = useState("");
  const [modalError, setModalError] = useState("");
  const [createdToken, setCreatedToken] = useState<NewApiToken | null>(null);
  const [copiedCreated, setCopiedCreated] = useState(false);
  const [closeDelayElapsed, setCloseDelayElapsed] = useState(false);
  const [exampleTab, setExampleTab] = useState<ExampleTab>("curl");
  const [copiedExample, setCopiedExample] = useState(false);
  const [query, setQuery] = useState("");
  const debouncedQuery = useDebouncedValue(query);
  const [sort, setSort] = useState<{ key: SortKey; dir: SortDir }>({ key: "created_at", dir: "desc" });
  const confirm = useConfirm();

  const exampleBase = process.env.NEXT_PUBLIC_PANDASTACK_API || "https://api.pandastack.ai";
  const examples = useMemo(
    () => ({
      curl: `curl -H "Authorization: Bearer ${EXAMPLE_TOKEN}" \\\n  ${exampleBase}/v1/sandboxes`,
      python: `from pandastack import Sandbox

sb = Sandbox.create(template="ubuntu-24.04")
result = sb.exec("echo hello")
print(result.stdout)
sb.kill()`,
      cli: `export PANDASTACK_TOKEN=${EXAMPLE_TOKEN}
pandastack sandbox create --template code-interpreter
pandastack sandbox list
pandastack sandbox exec <id> "echo hello"`,
    }),
    [exampleBase]
  );

  const refresh = async () => {
    setListError(null);
    try {
      const r = await listApiTokens();
      setItems(r.items ?? []);
    } catch (e) {
      const msg = errorMessage(e);
      setListError(msg);
      toast.error(msg.includes("401") ? "Session expired" : `Failed to load API tokens: ${msg}`);
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    refresh();
  }, []);

  useEffect(() => {
    if (!createdToken) return;
    setCloseDelayElapsed(false);
    const t = window.setTimeout(() => setCloseDelayElapsed(true), 3000);
    return () => window.clearTimeout(t);
  }, [createdToken]);

  const openCreate = () => {
    setShowCreate(true);
    setLabel("");
    setModalError("");
    setCreatedToken(null);
    setCopiedCreated(false);
    setCloseDelayElapsed(false);
  };

  const closeCreate = () => {
    if (createdToken && !copiedCreated && !closeDelayElapsed) return;
    setShowCreate(false);
  };

  const submitCreate = (e: React.FormEvent) => {
    e.preventDefault();
    const trimmed = label.trim();
    if (trimmed.length < 1 || trimmed.length > 64) {
      setModalError("Label must be 1–64 characters.");
      return;
    }

    start(async () => {
      setModalError("");
      try {
        const token = await createApiToken(trimmed);
        setCreatedToken(token);
        setCopiedCreated(false);
        setItems((current) => [token, ...current.filter((item) => item.prefix !== token.prefix)]);
        toast.success("Token created");
      } catch (e) {
        setModalError(errorMessage(e));
      }
    });
  };

  const copyText = async (text: string, onCopied?: () => void) => {
    try {
      await navigator.clipboard.writeText(text);
      onCopied?.();
      toast.success("Copied");
    } catch {
      toast.error("Copy failed");
    }
  };


  const filtered = useMemo(() => {
    const q = debouncedQuery.trim().toLowerCase();
    return items
      .filter((token) => !q || token.label.toLowerCase().includes(q) || token.prefix.toLowerCase().includes(q) || token.created_at.toLowerCase().includes(q))
      .sort((a, b) => {
        const cmp = compareValue(a[sort.key], b[sort.key]);
        return sort.dir === "asc" ? cmp : -cmp;
      });
  }, [items, debouncedQuery, sort]);
  const { page, setPage, pageSize, pageRows } = usePagedRows(filtered);
  const toggleSort = (key: SortKey) => setSort((s) => s.key === key ? { key, dir: s.dir === "asc" ? "desc" : "asc" } : { key, dir: key === "created_at" ? "desc" : "asc" });

  const revoke = async (token: ApiToken) => {
    const ok = await confirm({
      title: `Revoke token ${token.prefix}…?`,
      description: "Anything still using this token will start getting 401s immediately. This cannot be undone.",
      confirmLabel: "Revoke",
      destructive: true,
    });
    if (!ok) return;

    start(async () => {
      const id = toast.loading("Revoking token…");
      try {
        await revokeApiToken(token.prefix);
        setItems((current) => current.filter((item) => item.prefix !== token.prefix));
        toast.success("Token revoked", { id });
      } catch (e) {
        toast.error(`Revoke failed: ${errorMessage(e)}`, { id });
      }
    });
  };

  return (
    <>
      <PageHeader
        title="API Tokens"
        description="Use these long-lived tokens to authenticate the PandaStack CLI, Python/TypeScript SDKs, or your own scripts. Treat them like passwords."
        badge={
          <span
            className="rounded-full px-2 py-0.5 text-[11px] font-medium"
            style={{ background: "var(--bg-elevated)", color: "var(--text-muted)", border: "1px solid var(--border-default)" }}
          >
            {items.length}
          </span>
        }
        actions={
          <Btn variant="primary" size="sm" icon={<Plus size={13} />} onClick={openCreate}>
            {items.length === 0 ? "Create your first token" : "Create token"}
          </Btn>
        }
      />

      {listError && <div className="mb-4"><ErrorState error={listError} onRetry={() => void refresh()} /></div>}

      <div className="mb-3 flex flex-col gap-2 sm:flex-row sm:items-center sm:justify-between">
        <SearchInput value={query} onChange={setQuery} placeholder="Filter tokens…" />
        <div className="text-[12px]" style={{ color: "var(--text-muted)" }}>{filtered.length} of {items.length} tokens</div>
      </div>

      <Card>
        {loading ? (
          <LoadingTable cols={4} rows={4} />
        ) : (
          <Table>
            <thead>
              <tr>
                <SortHeader label="Label" sortKey="label" current={sort} onSort={toggleSort} />
                <SortHeader label="Prefix" sortKey="prefix" current={sort} onSort={toggleSort} />
                <SortHeader label="Created" sortKey="created_at" current={sort} onSort={toggleSort} />
                <th className="px-4 py-2.5 text-right text-[11px] font-medium uppercase tracking-wider" style={{ color: "var(--text-muted)", borderBottom: "1px solid var(--border-subtle)" }}>Actions</th>
              </tr>
            </thead>
            <tbody>
              {pageRows.map((token, i) => (
                <tr
                  key={token.prefix}
                  className="group transition-colors focus:outline-none focus:ring-1 focus:ring-emerald-500/40"
                  {...rowNavProps(i)}
                  onMouseEnter={(e) => { e.currentTarget.style.background = "var(--bg-elevated)"; }}
                  onMouseLeave={(e) => { e.currentTarget.style.background = ""; }}
                >
                  <Td>{token.label}</Td>
                  <Td mono>{token.prefix}…</Td>
                  <Td muted><RelativeTime value={token.created_at} /></Td>
                  <Td right>
                    <RowActions><RowAction onClick={() => copyText(token.prefix)}><Copy size={12} />Copy prefix</RowAction><RowAction destructive disabled={pending} onClick={() => revoke(token)}><Trash2 size={12} />Revoke</RowAction></RowActions>
                  </Td>
                </tr>
              ))}
            </tbody>
          </Table>
        )}
      </Card>

      {!loading && filtered.length > 0 && <PaginationBar total={filtered.length} page={page} pageSize={pageSize} onPage={setPage} label="tokens" />}

      <Card className="mt-5">
        <div className="flex items-center justify-between px-4 py-3" style={{ borderBottom: "1px solid var(--border-subtle)" }}>
          <div>
            <div className="text-[13px] font-semibold" style={{ color: "var(--text-primary)" }}>How to use</div>
            <div className="mt-0.5 text-[12px]" style={{ color: "var(--text-muted)" }}>
              Replace {EXAMPLE_TOKEN} with a token you created.
            </div>
          </div>
          {exampleTab === "cli" && (
            <span className="rounded px-1.5 py-0.5 text-[11px] font-medium" style={{ background: "rgba(22,163,74,0.1)", color: "var(--status-running)", border: "1px solid rgba(22,163,74,0.25)" }}>
              available
            </span>
          )}
        </div>
        <Tabs<ExampleTab>
          value={exampleTab}
          onChange={(tab) => {
            setExampleTab(tab);
            setCopiedExample(false);
          }}
          tabs={[
            { value: "curl", label: "curl" },
            { value: "python", label: "Python" },
            { value: "cli", label: "CLI" },
          ]}
        />
        <div className="p-4">
          <div className="flex items-start gap-2">
            <pre
              className="min-h-24 flex-1 overflow-x-auto rounded-md px-3 py-2.5 font-mono text-[12px] leading-5"
              style={{ background: "var(--bg-elevated)", border: "1px solid var(--border-subtle)", color: "var(--text-primary)" }}
            >
              <code>{examples[exampleTab]}</code>
            </pre>
            <Btn
              size="sm"
              variant="secondary"
              icon={copiedExample ? <Check size={12} /> : <Copy size={12} />}
              onClick={() => copyText(examples[exampleTab], () => {
                setCopiedExample(true);
                window.setTimeout(() => setCopiedExample(false), 1500);
              })}
            >
              {copiedExample ? "Copied" : "Copy"}
            </Btn>
          </div>
        </div>
      </Card>

      {showCreate && (
        <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/50 px-4" onClick={closeCreate}>
          <div
            className="w-full max-w-lg rounded-xl shadow-2xl"
            style={{ background: "var(--bg-elevated)", border: "1px solid var(--border-strong)" }}
            onClick={(e) => e.stopPropagation()}
          >
            <div className="flex items-center justify-between px-4 py-3" style={{ borderBottom: "1px solid var(--border-subtle)" }}>
              <div className="text-[13px] font-semibold" style={{ color: "var(--text-primary)" }}>
                {createdToken ? "Token created" : "Create API token"}
              </div>
              <button
                onClick={closeCreate}
                disabled={Boolean(createdToken && !copiedCreated && !closeDelayElapsed)}
                className="rounded p-1 disabled:cursor-not-allowed disabled:opacity-30"
                style={{ color: "var(--text-muted)" }}
              >
                <X size={14} />
              </button>
            </div>

            {createdToken ? (
              <div className="space-y-4 p-4">
                <Alert type="info">Copy this token now. You won&apos;t be able to see it again.</Alert>
                <div className="flex items-start gap-2">
                  <code
                    className="flex-1 select-all break-all rounded-md px-3 py-2.5 font-mono text-[12px] text-emerald-300"
                    style={{ background: "var(--bg-base)", border: "1px solid var(--border-subtle)" }}
                  >
                    {createdToken.token}
                  </code>
                  <Btn
                    size="sm"
                    variant="secondary"
                    icon={copiedCreated ? <Check size={12} /> : <Copy size={12} />}
                    onClick={() => copyText(createdToken.token, () => setCopiedCreated(true))}
                  >
                    {copiedCreated ? "Copied" : "Copy"}
                  </Btn>
                </div>
                <div className="flex justify-end">
                  <Btn variant="primary" onClick={closeCreate} disabled={!copiedCreated && !closeDelayElapsed}>
                    Close
                  </Btn>
                </div>
              </div>
            ) : (
              <form onSubmit={submitCreate} className="space-y-4 p-4">
                <Input
                  label="Label"
                  required
                  minLength={1}
                  maxLength={64}
                  placeholder="e.g. my laptop, ci-pipeline"
                  value={label}
                  onChange={(e) => setLabel(e.target.value)}
                  autoFocus
                  error={modalError || undefined}
                />
                <div className="flex justify-end gap-2">
                  <Btn variant="ghost" onClick={() => setShowCreate(false)}>Cancel</Btn>
                  <Btn variant="primary" type="submit" disabled={pending} icon={<Plus size={13} />}>
                    {pending ? "Creating…" : "Create token"}
                  </Btn>
                </div>
              </form>
            )}
          </div>
        </div>
      )}
    </>
  );
}
