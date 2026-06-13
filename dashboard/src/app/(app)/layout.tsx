// SPDX-License-Identifier: Apache-2.0
"use client";

import Link from "next/link";
import { usePathname, useRouter } from "next/navigation";
import { useEffect, useRef, useState } from "react";
import { toast } from "sonner";
import type { User as SupabaseUser } from "@supabase/supabase-js";
import { isStubAuth, STUB_USER_EMAIL } from "@/lib/auth-mode";
import { API_BASE, getAuthHeaders, getMe, listOrgs, setCurrentOrg, type Org } from "@/lib/api";
import { createClient } from "@/lib/supabase/client";
import { ConfirmProvider } from "@/components/ui";
import { ThemeToggle } from "@/components/theme";
import {
  AppWindow,
  Box,
  Cpu,
  Database,
  FileBox,
  HardDrive,
  Key,
  LayoutGrid,
  Search,
  Settings,
  X,
  ChevronLeft,
  ChevronRight,
  Zap,
  Activity,
  Clock,
  LineChart,
  Bell,
  User,
  Command,
  ChevronDown,
  Building2,
  Users,
  BookOpen,
  MessageCircle,
  Globe,
  ExternalLink,
} from "lucide-react";

const NAV = [
  { href: "/sandboxes",       label: "Sandboxes",    icon: Box,        shortcut: "G S" },
  { href: "/databases",       label: "Databases",    icon: Database,   shortcut: "" },
  { href: "/apps",            label: "Apps",         icon: AppWindow,  shortcut: "" },
  { href: "/templates",       label: "Templates",    icon: LayoutGrid, shortcut: "G T" },
  { href: "/functions",       label: "Functions",    icon: Zap,        shortcut: "" },
  { href: "/schedules",       label: "Schedules",    icon: Clock,      shortcut: "" },
  { href: "/volumes",         label: "Volumes",      icon: HardDrive,  shortcut: "" },
  { href: "/audit",           label: "Audit Log",    icon: Bell,       shortcut: "" },
  { href: "/stats",           label: "Performance",  icon: Activity,   shortcut: "" },
  { href: "/observability",   label: "Observability",icon: LineChart,  shortcut: "" },
  { href: "/settings/tokens", label: "API Tokens",   icon: Key,        shortcut: "" },
  { href: "/settings/team",   label: "Team",         icon: Users,      shortcut: "" },
  { href: "/settings/orgs",   label: "Organizations",icon: Building2,  shortcut: "" },
];

export default function AppLayout({ children }: { children: React.ReactNode }) {
  const [collapsed, setCollapsed] = useState(false);
  const [showSearch, setShowSearch] = useState(false);
  const [showShortcuts, setShowShortcuts] = useState(false);

  // keyboard shortcuts
  useEffect(() => {
    const seq: string[] = [];
    const handler = (e: KeyboardEvent) => {
      // ignore inputs
      if (e.target instanceof HTMLInputElement || e.target instanceof HTMLTextAreaElement) return;

      // ? key for shortcuts modal
      if (e.key === "?" && !e.metaKey) {
        setShowShortcuts((v) => !v);
        return;
      }
      // / or cmd+k for search
      if ((e.key === "/" || (e.key === "k" && e.metaKey)) && !e.shiftKey) {
        e.preventDefault();
        setShowSearch((v) => !v);
        return;
      }
      // Esc to close modals
      if (e.key === "Escape") {
        setShowSearch(false);
        setShowShortcuts(false);
        return;
      }

      // g-prefixed navigation
      seq.push(e.key.toLowerCase());
      if (seq.length > 2) seq.shift();
      const combo = seq.join(" ");
      if (combo === "g s") { window.location.href = "/sandboxes"; seq.length = 0; }
      if (combo === "g t") { window.location.href = "/templates"; seq.length = 0; }
    };
    window.addEventListener("keydown", handler);
    return () => window.removeEventListener("keydown", handler);
  }, []);

  return (
    <ConfirmProvider>
      <div className="flex h-screen overflow-hidden" style={{ background: "var(--bg-base)" }}>
        <Sidebar collapsed={collapsed} setCollapsed={setCollapsed} />
        <div className="flex flex-1 flex-col overflow-hidden">
          <TopBar
            onSearch={() => setShowSearch(true)}
            onShortcuts={() => setShowShortcuts(true)}
          />
          <main className="flex-1 overflow-y-auto overflow-x-hidden">
            {/* dev-mode banner removed by user request */}
            <div className="mx-auto w-full max-w-7xl px-6 py-6">{children}</div>
          </main>
        </div>

        {/* Search modal */}
        {showSearch && (
          <SearchModal onClose={() => setShowSearch(false)} />
        )}

        {/* Shortcuts modal */}
        {showShortcuts && (
          <ShortcutsModal onClose={() => setShowShortcuts(false)} />
        )}
      </div>
    </ConfirmProvider>
  );
}

function Sidebar({
  collapsed,
  setCollapsed,
}: {
  collapsed: boolean;
  setCollapsed: (v: boolean) => void;
}) {
  const path = usePathname();
  const w = collapsed ? "w-[60px]" : "w-[220px]";
  const navItems = NAV.filter((item) => {
    if (isStubAuth() && item.href === "/settings/team") return false;
    return true;
  });

  return (
    <aside
      className={`hidden md:flex flex-col shrink-0 ${w} transition-[width] duration-200 ease-in-out overflow-hidden`}
      style={{
        background: "var(--bg-base)",
        borderRight: "1px solid var(--border-subtle)",
      }}
    >
      {/* Logo */}
      <div className="flex h-14 items-center gap-3 px-3" style={{ borderBottom: "1px solid var(--border-subtle)" }}>
        <Link href="/sandboxes" className="flex items-center gap-2.5 min-w-0">
          <img
            src="/logo.svg"
            alt="PandaStack"
            className="size-7 shrink-0 rounded-lg object-contain"
          />
          {!collapsed && (
            <div className="flex items-center gap-1.5 min-w-0">
              <span className="text-[14px] font-semibold tracking-tight truncate" style={{ color: "var(--text-primary)" }}>
                PandaStack
              </span>
              <span
                className="rounded px-1.5 py-0.5 text-[10px] font-medium uppercase tracking-wider shrink-0"
                style={{ background: "var(--bg-overlay)", color: "var(--text-muted)" }}
              >
                dev
              </span>
            </div>
          )}
        </Link>
        {!collapsed && (
          <button
            onClick={() => setCollapsed(true)}
            className="ml-auto rounded p-1 transition-colors hover:bg-zinc-800"
            style={{ color: "var(--text-muted)" }}
          >
            <ChevronLeft size={14} />
          </button>
        )}
      </div>

      {/* Nav */}
      <nav className="flex flex-1 flex-col gap-0.5 p-2 overflow-y-auto">
        {navItems.map((n) => {
          const active = path === n.href || (n.href !== "/" && path.startsWith(n.href));
          const Icon = n.icon;
          return (
            <Link
              key={n.href}
              href={n.href}
              title={collapsed ? n.label : undefined}
              className={`group flex items-center gap-2.5 rounded-md px-2.5 py-2 text-[13px] transition-all ${
                active ? "font-medium" : "hover:text-zinc-100"
              }`}
              style={
                active
                  ? { background: "var(--brand-dim)", color: "var(--brand)", border: "1px solid var(--brand-border)" }
                  : { color: "var(--text-secondary)", border: "1px solid transparent" }
              }
            >
              <Icon size={15} className="shrink-0" />
              {!collapsed && <span className="truncate">{n.label}</span>}
            </Link>
          );
        })}
      </nav>

      {/* External links */}
      <div className="px-2 pb-1" style={{ borderTop: "1px solid var(--border-subtle)" }}>
        {collapsed ? (
          <div className="flex flex-col items-center gap-0.5 py-1.5">
            {[
              { href: "https://docs.pandastack.ai", icon: BookOpen, label: "Docs" },
              { href: "https://discord.gg/C7Du7XbG", icon: MessageCircle, label: "Discord" },
              { href: "https://pandastack.ai", icon: Globe, label: "Website" },
            ].map(({ href, icon: Icon, label }) => (
              <a
                key={href}
                href={href}
                target="_blank"
                rel="noopener noreferrer"
                title={label}
                className="flex w-full items-center justify-center rounded-md p-2 transition-colors hover:bg-zinc-800"
                style={{ color: "var(--text-muted)" }}
              >
                <Icon size={13} />
              </a>
            ))}
          </div>
        ) : (
          <div className="flex items-center justify-between px-1 py-2">
            <span className="text-[11px]" style={{ color: "var(--text-muted)" }}>Resources</span>
            <div className="flex items-center gap-0.5">
              {[
                { href: "https://docs.pandastack.ai", icon: BookOpen, label: "Docs" },
                { href: "https://discord.gg/C7Du7XbG", icon: MessageCircle, label: "Discord" },
                { href: "https://pandastack.ai", icon: Globe, label: "Website" },
              ].map(({ href, icon: Icon, label }) => (
                <a
                  key={href}
                  href={href}
                  target="_blank"
                  rel="noopener noreferrer"
                  title={label}
                  className="flex items-center justify-center rounded p-1.5 transition-colors hover:bg-zinc-800"
                  style={{ color: "var(--text-muted)" }}
                >
                  <Icon size={13} />
                </a>
              ))}
            </div>
          </div>
        )}
      </div>

      {/* Footer */}
      <div className="p-2" style={{ borderTop: "1px solid var(--border-subtle)" }}>
        {collapsed ? (
          <button
            onClick={() => setCollapsed(false)}
            className="flex w-full items-center justify-center rounded-md p-2 transition-colors hover:bg-zinc-800"
            style={{ color: "var(--text-muted)" }}
          >
            <ChevronRight size={14} />
          </button>
        ) : (
          <div className="flex items-center gap-2 px-2 py-1.5">
            <div
              className="flex size-6 shrink-0 items-center justify-center rounded-full text-[11px] font-semibold"
              style={{ background: "var(--bg-overlay)", color: "var(--text-secondary)" }}
            >
              A
            </div>
            <div className="min-w-0">
              <div className="truncate text-[12px] font-medium" style={{ color: "var(--text-primary)" }}>Admin</div>
              <div className="truncate text-[11px]" style={{ color: "var(--text-muted)" }}>default workspace</div>
            </div>
            <Settings size={12} className="ml-auto shrink-0" style={{ color: "var(--text-muted)" }} />
          </div>
        )}
      </div>
    </aside>
  );
}

function TopBar({
  onSearch,
  onShortcuts,
}: {
  onSearch: () => void;
  onShortcuts: () => void;
}) {
  const router = useRouter();
  const [health, setHealth] = useState<"ok" | "down" | "checking">("checking");
  const [p50, setP50] = useState<number | null>(null);
  const [_, setTick] = useState(0);
  const [user, setUser] = useState<SupabaseUser | null>(null);
  const [menuOpen, setMenuOpen] = useState(false);

  useEffect(() => {
    let alive = true;
    let timer: ReturnType<typeof setTimeout> | null = null;
    const poll = async () => {
      let nextDelay = 30000;
      try {
        const h = await fetch(`${API_BASE}/healthz`, { cache: "no-store" });
        if (!alive) return;
        setHealth(h.ok ? "ok" : "down");
        if (h.ok) {
          const r = await fetch(`${API_BASE}/v1/stats/boot?limit=20`, { cache: "no-store", headers: await getAuthHeaders() });
          if (r.ok) {
            const j = await r.json();
            if (alive && j?.overall?.p50_ms !== undefined) {
              setP50(j.overall.p50_ms);
            }
          }
        } else {
          nextDelay = 3000;
        }
      } catch {
        if (alive) setHealth("down");
        nextDelay = 3000;
      }
      if (alive) setTick((t) => t + 1);
      if (alive) timer = setTimeout(poll, nextDelay);
    };
    poll();
    const onVis = () => { if (!document.hidden) poll(); };
    document.addEventListener("visibilitychange", onVis);
    return () => {
      alive = false;
      if (timer) clearTimeout(timer);
      document.removeEventListener("visibilitychange", onVis);
    };
  }, []);

  useEffect(() => {
    let alive = true;
    const supabase = createClient();

    supabase.auth.getUser().then(({ data }) => {
      if (alive) setUser(data.user);
    });

    const { data } = supabase.auth.onAuthStateChange((_event, session) => {
      setUser(session?.user ?? null);
    });

    return () => {
      alive = false;
      data.subscription.unsubscribe();
    };
  }, []);

  const email = isStubAuth() ? STUB_USER_EMAIL : user?.email ?? "Signed in";
  const initial = email.slice(0, 1).toUpperCase();

  const signOut = async () => {
    await createClient().auth.signOut();
    setMenuOpen(false);
    router.push(isStubAuth() ? "/sandboxes" : "/login");
    router.refresh();
  };

  return (
    <header
      className="flex h-14 shrink-0 items-center gap-3 px-4"
      style={{
        borderBottom: "1px solid var(--border-subtle)",
        background: "var(--bg-base)",
        backdropFilter: "blur(12px)",
      }}
    >
      {/* Search */}
      <button
        onClick={onSearch}
        className="flex flex-1 max-w-[280px] items-center gap-2 rounded-md px-3 py-1.5 text-[13px] transition-colors hover:border-zinc-700"
        style={{
          background: "var(--bg-elevated)",
          border: "1px solid var(--border-default)",
          color: "var(--text-muted)",
        }}
      >
        <Search size={13} />
        <span className="flex-1 text-left">Search…</span>
        <kbd
          className="ml-auto hidden rounded px-1.5 py-0.5 text-[10px] font-medium sm:block"
          style={{ background: "var(--bg-overlay)", color: "var(--text-muted)" }}
        >
          ⌘K
        </kbd>
      </button>

      <div className="flex-1" />

      <OrgSwitcher />

      {/* Launch latency badge */}
      {p50 !== null && p50 > 0 ? (
        <div
          className="glow-pulse hidden sm:flex items-center gap-1.5 rounded-full px-3 py-1 text-[12px] font-medium"
          style={{
            background: "var(--brand-dim)",
            border: "1px solid var(--brand-border)",
            color: "var(--brand)",
          }}
        >
          <Zap size={11} />
          {p50} ms p50
        </div>
      ) : (
        <div
          className="hidden sm:flex items-center gap-1.5 rounded-full px-3 py-1 text-[12px]"
          style={{ background: "var(--bg-elevated)", border: "1px solid var(--border-default)", color: "var(--text-muted)" }}
        >
          <Activity size={11} />
          no boots yet
        </div>
      )}

      {/* Health */}
      <div className="flex items-center gap-1.5">
        <span
          className={`size-2 rounded-full ${health === "ok" ? "" : health === "down" ? "" : "animate-pulse"}`}
          style={{
            background:
              health === "ok" ? "var(--status-running)"
              : health === "down" ? "var(--status-failed)"
              : "var(--text-muted)",
          }}
        />
        <span className="hidden text-[12px] sm:block" style={{ color: "var(--text-muted)" }}>
          {health === "ok" ? "API online" : health === "down" ? "API down" : "checking…"}
        </span>
      </div>

      {/* Theme toggle */}
      <ThemeToggle />

      {/* Shortcuts hint */}
      <button
        onClick={onShortcuts}
        className="rounded p-1.5 transition-colors hover:bg-zinc-800"
        title="Keyboard shortcuts (?)"
        style={{ color: "var(--text-muted)" }}
      >
        <Command size={14} />
      </button>

      {/* Notifications stub */}
      <button
        className="rounded p-1.5 transition-colors hover:bg-zinc-800"
        style={{ color: "var(--text-muted)" }}
      >
        <Bell size={14} />
      </button>

      {/* User menu */}
      <div className="relative">
        <button
          onClick={() => setMenuOpen((v) => !v)}
          className="flex items-center gap-2 rounded-full py-1 pl-1 pr-2 text-[12px] transition-colors hover:bg-zinc-800"
          style={{ color: "var(--text-secondary)" }}
        >
          <span
            className="flex size-7 items-center justify-center rounded-full text-[12px] font-semibold"
            style={{ background: "var(--bg-overlay)", color: "var(--text-secondary)" }}
          >
            {initial || <User size={14} />}
          </span>
          <span className="hidden max-w-[180px] truncate sm:block">{email}</span>
        </button>
        {menuOpen && (
          <div
            className="absolute right-0 z-50 mt-2 w-56 overflow-hidden rounded-lg shadow-2xl"
            style={{ background: "var(--bg-elevated)", border: "1px solid var(--border-strong)" }}
          >
            <div className="px-3 py-2" style={{ borderBottom: "1px solid var(--border-subtle)" }}>
              <div className="truncate text-[12px] font-medium" style={{ color: "var(--text-primary)" }}>{email}</div>
              <div className="truncate text-[11px]" style={{ color: "var(--text-muted)" }}>{isStubAuth() ? "Local stub workspace" : "Supabase workspace"}</div>
            </div>
            <button
              onClick={signOut}
              className="w-full px-3 py-2 text-left text-[13px] transition-colors hover:bg-zinc-800"
              style={{ color: "var(--text-secondary)" }}
            >
              Sign out
            </button>
          </div>
        )}
      </div>
    </header>
  );
}


function OrgSwitcher() {
  const router = useRouter();
  const [orgs, setOrgs] = useState<Org[]>([]);
  const [currentOrgId, setCurrentOrgId] = useState<string | null>(null);
  const [open, setOpen] = useState(false);
  const [loading, setLoading] = useState(false);

  const loadOrgs = async () => {
    setLoading(true);
    try {
      const stored = typeof window !== "undefined" ? window.localStorage.getItem("pandastack_org_id") : null;
      try {
        const me = await getMe();
        setOrgs(me.orgs);
        const resolvedOrgId = me.current_org_id ?? me.orgs[0]?.id ?? stored ?? null;
        setCurrentOrgId(resolvedOrgId);
        // Stamp localStorage so getAuthHeaders sends the correct org for this user.
        if (resolvedOrgId && typeof window !== "undefined") {
          const { data: { session } } = await createClient().auth.getSession();
          if (session?.user?.id) {
            window.localStorage.setItem("pandastack_org_id", resolvedOrgId);
            window.localStorage.setItem("pandastack_org_user", session.user.id);
          }
        }
      } catch {
        const items = await listOrgs();
        setOrgs(items);
        setCurrentOrgId(stored ?? items[0]?.id ?? null);
      }
    } catch (e) {
      toast.error(e instanceof Error ? e.message : "Failed to load organizations");
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    loadOrgs();
    const handler = () => loadOrgs();
    window.addEventListener("pandastack-org-changed", handler);
    return () => window.removeEventListener("pandastack-org-changed", handler);
  }, []);

  const current = orgs.find((org) => org.id === currentOrgId) ?? orgs[0];

  const switchOrg = async (org: Org) => {
    if (org.id === currentOrgId) {
      setOpen(false);
      return;
    }
    try {
      await setCurrentOrg(org.id);
      setCurrentOrgId(org.id);
      setOpen(false);
      toast.success(`Switched to ${org.name}`);
      window.dispatchEvent(new Event("pandastack-org-changed"));
      router.refresh();
    } catch (e) {
      toast.error(e instanceof Error ? e.message : "Failed to switch organization");
    }
  };

  return (
    <div className="relative hidden sm:block">
      <button
        onClick={() => setOpen((v) => !v)}
        className="flex max-w-[220px] items-center gap-2 rounded-md px-3 py-1.5 text-[12px] transition-colors hover:bg-zinc-800"
        style={{ background: "var(--bg-elevated)", border: "1px solid var(--border-default)", color: "var(--text-secondary)" }}
        title="Switch organization"
      >
        <Building2 size={13} className="shrink-0" />
        <span className="truncate">{current?.name ?? (loading ? "Loading…" : "No organization")}</span>
        <ChevronDown size={12} className="shrink-0" />
      </button>
      {open && (
        <div
          className="absolute right-0 z-50 mt-2 w-64 overflow-hidden rounded-lg shadow-2xl"
          style={{ background: "var(--bg-elevated)", border: "1px solid var(--border-strong)" }}
        >
          <div className="px-3 py-2 text-[11px] uppercase tracking-wider" style={{ color: "var(--text-muted)", borderBottom: "1px solid var(--border-subtle)" }}>
            Organizations
          </div>
          <div className="max-h-64 overflow-y-auto p-1">
            {orgs.length === 0 ? (
              <div className="px-3 py-2 text-[12px]" style={{ color: "var(--text-muted)" }}>No organizations found</div>
            ) : orgs.map((org) => (
              <button
                key={org.id}
                onClick={() => switchOrg(org)}
                className="flex w-full items-center gap-2 rounded-md px-2 py-2 text-left text-[13px] transition-colors hover:bg-zinc-800"
                style={{ color: org.id === currentOrgId ? "var(--brand)" : "var(--text-secondary)" }}
              >
                <span className="min-w-0 flex-1 truncate">{org.name}</span>
                <span className="text-[10px]" style={{ color: "var(--text-muted)" }}>{org.role}</span>
              </button>
            ))}
          </div>
          <Link
            href="/settings/orgs"
            onClick={() => setOpen(false)}
            className="block px-3 py-2 text-[13px] transition-colors hover:bg-zinc-800"
            style={{ color: "var(--text-primary)", borderTop: "1px solid var(--border-subtle)" }}
          >
            Manage organizations →
          </Link>
        </div>
      )}
    </div>
  );
}

function SearchModal({ onClose }: { onClose: () => void }) {
  const inputRef = useRef<HTMLInputElement>(null);
  const router = useRouter();

  useEffect(() => { inputRef.current?.focus(); }, []);

  const actions = [
    { label: "Go to Sandboxes", shortcut: "G S", href: "/sandboxes" },
    { label: "Go to Databases", shortcut: "", href: "/databases" },
    { label: "Go to Apps", shortcut: "", href: "/apps" },
    { label: "Go to Templates", shortcut: "G T", href: "/templates" },
    { label: "Go to Functions", shortcut: "", href: "/functions" },
    { label: "Go to Schedules", shortcut: "", href: "/schedules" },
    { label: "Go to Volumes", shortcut: "", href: "/volumes" },
    { label: "Go to Performance", shortcut: "", href: "/stats" },
    { label: "Go to API Tokens", shortcut: "", href: "/settings/tokens" },
    { label: "Go to Team", shortcut: "", href: "/settings/team" },
    { label: "Go to Organizations", shortcut: "", href: "/settings/orgs" },
  ];

  return (
    <div className="fixed inset-0 z-50 flex items-start justify-center pt-[15vh]" onClick={onClose}>
      <div
        className="w-full max-w-lg rounded-xl shadow-2xl"
        style={{ background: "var(--bg-elevated)", border: "1px solid var(--border-strong)" }}
        onClick={(e) => e.stopPropagation()}
      >
        <div className="flex items-center gap-3 px-4 py-3" style={{ borderBottom: "1px solid var(--border-subtle)" }}>
          <Search size={16} style={{ color: "var(--text-muted)" }} />
          <input
            ref={inputRef}
            placeholder="Search pages, sandboxes…"
            className="flex-1 bg-transparent text-[14px] outline-none"
            style={{ color: "var(--text-primary)" }}
          />
          <button onClick={onClose}>
            <X size={14} style={{ color: "var(--text-muted)" }} />
          </button>
        </div>
        <div className="p-2">
          <div className="px-3 py-1.5 text-[11px] uppercase tracking-wider" style={{ color: "var(--text-muted)" }}>
            Navigate
          </div>
          {actions.map((a) => (
            <button
              key={a.href}
              onClick={() => { router.push(a.href); onClose(); }}
              className="flex w-full items-center gap-3 rounded-md px-3 py-2.5 text-[13px] transition-colors hover:bg-zinc-800"
              style={{ color: "var(--text-primary)" }}
            >
              <span className="flex-1 text-left">{a.label}</span>
              {a.shortcut && (
                <kbd className="rounded px-1.5 py-0.5 text-[10px]" style={{ background: "var(--bg-overlay)", color: "var(--text-muted)" }}>
                  {a.shortcut}
                </kbd>
              )}
            </button>
          ))}
        </div>
      </div>
    </div>
  );
}

function ShortcutsModal({ onClose }: { onClose: () => void }) {
  const shortcuts = [
    { keys: ["G", "S"], label: "Go to Sandboxes" },
    { keys: ["G", "T"], label: "Go to Templates" },
    { keys: ["⌘", "K"], label: "Open search" },
    { keys: ["?"], label: "Toggle shortcuts" },
    { keys: ["Esc"], label: "Close modals" },
  ];

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center" onClick={onClose}>
      <div
        className="w-full max-w-sm rounded-xl shadow-2xl"
        style={{ background: "var(--bg-elevated)", border: "1px solid var(--border-strong)" }}
        onClick={(e) => e.stopPropagation()}
      >
        <div className="flex items-center justify-between px-4 py-3" style={{ borderBottom: "1px solid var(--border-subtle)" }}>
          <div className="text-[13px] font-semibold" style={{ color: "var(--text-primary)" }}>Keyboard Shortcuts</div>
          <button onClick={onClose}><X size={14} style={{ color: "var(--text-muted)" }} /></button>
        </div>
        <div className="p-4 space-y-2">
          {shortcuts.map((s, i) => (
            <div key={i} className="flex items-center justify-between">
              <span className="text-[13px]" style={{ color: "var(--text-secondary)" }}>{s.label}</span>
              <div className="flex items-center gap-1">
                {s.keys.map((k, j) => (
                  <kbd
                    key={j}
                    className="rounded px-2 py-0.5 text-[12px] font-medium"
                    style={{ background: "var(--bg-overlay)", color: "var(--text-primary)", border: "1px solid var(--border-default)" }}
                  >
                    {k}
                  </kbd>
                ))}
              </div>
            </div>
          ))}
        </div>
      </div>
    </div>
  );
}
