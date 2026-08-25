// SPDX-License-Identifier: Apache-2.0
"use client";

import { useEffect, useState } from "react";
import { toast } from "sonner";
import Link from "next/link";
import { ArrowLeft, CheckCircle, Clock, Loader2, Terminal, XCircle } from "lucide-react";
import { api, type TemplateBuild } from "@/lib/api";
import { Badge, Card, PageHeader, Table, Td, Th } from "@/components/ui";

// Custom template builds: read-only build history. Builds are a developer
// workflow pushed via the CLI/API (Ubuntu/Debian base images only — the guest
// agent and sshd need glibc); the dashboard deliberately has no upload form.
export default function CustomTemplatesPage() {
  const [builds, setBuilds] = useState<TemplateBuild[]>([]);
  const [loading, setLoading] = useState(true);

  const refresh = async () => {
    try {
      const b = await api.templateBuilds();
      setBuilds((b ?? []).sort((a, x) => (a.started_at < x.started_at ? 1 : -1)));
    } catch (e) {
      toast.error("Failed to load builds: " + (e instanceof Error ? e.message : String(e)));
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    refresh();
    // Visibility-gated poll: build status only moves while the tab is watched.
    const t = setInterval(() => { if (!document.hidden) refresh(); }, 5000);
    return () => clearInterval(t);
  }, []);

  return (
    <>
      <PageHeader
        title="Custom templates"
        description="Build history for templates pushed with the CLI or API."
        actions={
          <Link href="/templates" className="flex items-center gap-1.5 text-[12px] font-medium transition-colors hover:text-emerald-400" style={{ color: "var(--text-muted)" }}>
            <ArrowLeft size={13} /> All templates
          </Link>
        }
      />

      <Card className="mb-5 p-4">
        <div className="flex items-start gap-3">
          <Terminal size={16} className="mt-0.5 shrink-0" style={{ color: "var(--text-muted)" }} />
          <div className="text-[12px] leading-5" style={{ color: "var(--text-muted)" }}>
            <div className="mb-1 font-semibold" style={{ color: "var(--text-secondary)" }}>
              Custom templates are built from the CLI
            </div>
            <code className="font-mono" style={{ color: "var(--text-secondary)" }}>
              pandastack template build -f Dockerfile -n my-template
            </code>
            <div className="mt-2">
              Base images must be <span className="font-medium" style={{ color: "var(--text-secondary)" }}>Ubuntu or Debian</span> — the
              PandaStack guest agent and SSH bridge require glibc, so Alpine/musl images are not supported. See the{" "}
              <a href="https://docs.pandastack.ai/docs/concepts/templates" target="_blank" rel="noreferrer" className="underline hover:text-emerald-400">
                templates docs
              </a>.
            </div>
          </div>
        </div>
      </Card>

      <div className="mb-3 text-[11px] font-medium uppercase tracking-wider" style={{ color: "var(--text-muted)" }}>
        Build history
      </div>
      <Card className="p-0 overflow-hidden">
        {loading ? (
          <div className="px-4 py-6 text-[12px]" style={{ color: "var(--text-muted)" }}>Loading…</div>
        ) : builds.length === 0 ? (
          <div className="px-4 py-6 text-[12px]" style={{ color: "var(--text-muted)" }}>
            No builds yet. Upload a rootfs above or push one with the CLI.
          </div>
        ) : (
          <Table>
            <thead>
              <tr>
                <Th>Name</Th>
                <Th>Status</Th>
                <Th>Size</Th>
                <Th>Started</Th>
                <Th>Error</Th>
              </tr>
            </thead>
            <tbody>
              {builds.slice(0, 20).map((b) => (
                <tr key={b.id}>
                  <Td mono>{b.name}</Td>
                  <Td><BuildBadge status={b.status} /></Td>
                  <Td muted>{b.size_mb} MiB</Td>
                  <Td muted>{new Date(b.started_at).toLocaleTimeString()}</Td>
                  <Td>
                    {b.error && (
                      <span className="text-red-400 text-[12px] truncate max-w-[260px] block">{b.error}</span>
                    )}
                  </Td>
                </tr>
              ))}
            </tbody>
          </Table>
        )}
      </Card>
    </>
  );
}

function BuildBadge({ status }: { status: TemplateBuild["status"] }) {
  const map: Record<
    TemplateBuild["status"],
    { variant: "success" | "warning" | "error" | "info"; icon: React.ReactNode }
  > = {
    done: { variant: "success", icon: <CheckCircle size={11} /> },
    running: { variant: "warning", icon: <Loader2 size={11} className="animate-spin" /> },
    queued: { variant: "info", icon: <Clock size={11} /> },
    failed: { variant: "error", icon: <XCircle size={11} /> },
  };
  const { variant, icon } = map[status] ?? map.queued;
  return (
    <Badge variant={variant} className="gap-1">
      {icon}
      {status}
    </Badge>
  );
}
