// SPDX-License-Identifier: Apache-2.0
"use client";

// The app shell: a rail of grouped links on the left, a top bar that is mostly
// a command line, and the page. Styles: components/shell/shell.module.css.

import Link from "next/link";
import { usePathname, useRouter } from "next/navigation";
import { useEffect, useRef, useState } from "react";
import { toast } from "sonner";
import type { User as SupabaseUser } from "@supabase/supabase-js";
import { isStubAuth, STUB_USER_EMAIL } from "@/lib/auth-mode";
import { API_BASE, getMe, listOrgs, setCurrentOrg, type Org } from "@/lib/api";
import { createClient } from "@/lib/supabase/client";
import { clearListCache } from "@/lib/list-cache";
import { ConfirmProvider } from "@/components/ui";
import { CommandBar, PageCode, hasPageCode } from "@/components/shell/command";
import { useShellData, type ShellData } from "@/components/shell/shell-data";
import s from "@/components/shell/shell.module.css";

type Count = keyof NonNullable<ShellData["counts"]>;
type NavLink = { href: string; label: string; count?: Count };

// The pages, grouped by what you are doing: running things, keeping things,
// building things, watching things, and the account.
const NAV: { group?: string; links: NavLink[] }[] = [
  { links: [{ href: "/now", label: "Now", count: "live" }] },
  {
    group: "run",
    links: [{ href: "/sandboxes", label: "Sandboxes", count: "sandboxes" }],
  },
  {
    group: "store",
    links: [
      { href: "/databases", label: "Databases", count: "databases" },
      { href: "/volumes", label: "Volumes" },
      { href: "/snapshots", label: "Snapshots" },
    ],
  },
  { group: "build", links: [{ href: "/templates", label: "Templates" }] },
  {
    group: "watch",
    links: [
      { href: "/stats", label: "Performance" },
      { href: "/observability", label: "Observability" },
      { href: "/audit", label: "Audit log" },
    ],
  },
  {
    group: "account",
    links: [
      { href: "/settings/tokens", label: "API keys" },
      { href: "/settings/team", label: "Team" },
      { href: "/settings/orgs", label: "Organizations" },
    ],
  },
];

const SHORTCUTS: { keys: string[]; or?: string[]; label: string }[] = [
  { keys: ["⌘", "K"], or: ["/"], label: "Open the command bar" },
  { keys: ["G", "N"], label: "Go to Now" },
  { keys: ["G", "S"], label: "Go to Sandboxes" },
  { keys: ["G", "T"], label: "Go to Templates" },
  { keys: ["?"], label: "Show these shortcuts" },
  { keys: ["Esc"], label: "Close a dialog" },
];

/** A dropdown closes when you press Esc or press anywhere outside it. */
function useDismiss<T extends HTMLElement>(open: boolean, setOpen: (open: boolean) => void) {
  const ref = useRef<T | null>(null);
  useEffect(() => {
    if (!open) return;
    const onDown = (e: PointerEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false);
    };
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") setOpen(false);
    };
    document.addEventListener("pointerdown", onDown);
    document.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("pointerdown", onDown);
      document.removeEventListener("keydown", onKey);
    };
  }, [open, setOpen]);
  return ref;
}

export default function AppLayout({ children }: { children: React.ReactNode }) {
  const pathname = usePathname();
  const router = useRouter();
  const shell = useShellData();
  const [showCommand, setShowCommand] = useState(false);
  const [showShortcuts, setShowShortcuts] = useState(false);
  const [showCode, setShowCode] = useState(false);
  const [railOpen, setRailOpen] = useState(false);

  // A link in the mobile drawer has done its job once the page changes.
  useEffect(() => {
    setRailOpen(false);
  }, [pathname]);

  useEffect(() => {
    const seq: string[] = [];
    const handler = (e: KeyboardEvent) => {
      const el = e.target as HTMLElement | null;
      const typing = el instanceof HTMLInputElement || el instanceof HTMLTextAreaElement || Boolean(el?.isContentEditable);
      // ⌘K works from anywhere; the single-key shortcuts must not fire while typing.
      if (e.key === "k" && (e.metaKey || e.ctrlKey) && !e.shiftKey) {
        e.preventDefault();
        setShowCommand((v) => !v);
        return;
      }
      if (typing) return;
      if (e.key === "?" && !e.metaKey) {
        setShowShortcuts((v) => !v);
        return;
      }
      if (e.key === "/" && !e.metaKey && !e.ctrlKey) {
        e.preventDefault();
        setShowCommand(true);
        return;
      }
      if (e.key === "Escape") {
        setShowCommand(false);
        setShowShortcuts(false);
        setShowCode(false);
        setRailOpen(false);
        return;
      }
      seq.push(e.key.toLowerCase());
      if (seq.length > 2) seq.shift();
      const combo = seq.join(" ");
      const to = combo === "g n" ? "/now" : combo === "g s" ? "/sandboxes" : combo === "g t" ? "/templates" : null;
      if (to) {
        seq.length = 0;
        router.push(to);
      }
    };
    window.addEventListener("keydown", handler);
    return () => window.removeEventListener("keydown", handler);
  }, [router]);

  // /now is one ruled sheet that runs edge to edge; every other page keeps its
  // padded column.
  const bleed = pathname === "/now";

  return (
    <ConfirmProvider>
      <div className={s.shell}>
        {railOpen && <div className={s.scrim} onClick={() => setRailOpen(false)} aria-hidden="true" />}
        <Rail pathname={pathname} shell={shell} open={railOpen} />
        <div className={s.main}>
          <TopBar
            onCommand={() => setShowCommand(true)}
            onShortcuts={() => setShowShortcuts(true)}
            onCode={hasPageCode(pathname) ? () => setShowCode(true) : undefined}
            onMenu={() => setRailOpen(true)}
          />
          <main className={s.content}>{bleed ? children : <div className={s.padded}>{children}</div>}</main>
        </div>

        {showCommand && <CommandBar things={shell.things} onClose={() => setShowCommand(false)} onRan={shell.refresh} />}
        {showCode && <PageCode pathname={pathname} onClose={() => setShowCode(false)} />}
        {showShortcuts && <ShortcutsSheet onClose={() => setShowShortcuts(false)} />}
      </div>
    </ConfirmProvider>
  );
}

// ------------------------------------------------------------------ rail ---

function Rail({ pathname, shell, open }: { pathname: string; shell: ShellData; open: boolean }) {
  const stub = isStubAuth();
  return (
    <aside className={`${s.rail}${open ? ` ${s.open}` : ""}`} aria-label="Main">
      <Link href="/now" className={s.brand}>
        <img src="/logo.svg" alt="" />
        pandastack
      </Link>
      <OrgSwitcher />
      <nav className={s.nav}>
        {NAV.map((section, i) => (
          <div key={section.group ?? i}>
            {section.group && <div className={s.group}>{section.group}</div>}
            {section.links.map((l) => {
              // Team has nothing to show without real accounts.
              if (stub && l.href === "/settings/team") return null;
              const active = pathname === l.href || pathname.startsWith(`${l.href}/`);
              const n = l.count && shell.counts ? shell.counts[l.count] : undefined;
              const live = l.count === "live";
              return (
                <Link key={l.href} href={l.href} className={s.link} aria-current={active ? "page" : undefined}>
                  {l.label}
                  {n !== undefined && (live ? n > 0 : true) && (
                    <span className={`${s.n}${live ? ` ${s.live}` : ""}`}>
                      {live && <span className={`${s.dot} live-pulse`} />}
                      {live ? `${n} live` : n}
                    </span>
                  )}
                </Link>
              );
            })}
          </div>
        ))}
      </nav>
      <div className={s.foot}>
        <div className={s.links}>
          <a href="https://docs.pandastack.ai" target="_blank" rel="noopener noreferrer">
            docs
          </a>
          <a href="https://discord.gg/GzxpktHpHD" target="_blank" rel="noopener noreferrer">
            discord
          </a>
          <a href="https://pandastack.ai" target="_blank" rel="noopener noreferrer">
            website
          </a>
        </div>
      </div>
    </aside>
  );
}

function OrgSwitcher() {
  const router = useRouter();
  const [orgs, setOrgs] = useState<Org[]>([]);
  const [currentOrgId, setCurrentOrgId] = useState<string | null>(null);
  const [open, setOpen] = useState(false);
  const [loading, setLoading] = useState(false);
  const box = useDismiss<HTMLDivElement>(open, setOpen);

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
          const {
            data: { session },
          } = await createClient().auth.getSession();
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
    // loadOrgs is recreated every render and only reads state setters.
    // eslint-disable-next-line react-hooks/exhaustive-deps
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
    <div className={s.org} ref={box}>
      <button type="button" className={s.orgBtn} onClick={() => setOpen((v) => !v)} aria-expanded={open} title="Switch organization">
        <span>{current?.name ?? (loading ? "Loading…" : "No organization")}</span>
        {current?.role && <span className={s.chip}>{current.role}</span>}
        <span className={s.caret} aria-hidden="true">
          ▾
        </span>
      </button>
      {open && (
        <div className={s.menu}>
          <div className={s.menuHead}>organizations</div>
          {orgs.length === 0 ? (
            <div className={s.menuHead} style={{ borderBottom: 0 }}>
              none found
            </div>
          ) : (
            orgs.map((org) => (
              <button key={org.id} type="button" className={s.menuItem} aria-current={org.id === currentOrgId} onClick={() => switchOrg(org)}>
                <span>{org.name}</span>
                <small>{org.role}</small>
              </button>
            ))
          )}
          <div className={s.menuFoot}>
            <Link href="/settings/orgs" className={s.menuItem} onClick={() => setOpen(false)}>
              <span>Manage organizations →</span>
            </Link>
          </div>
        </div>
      )}
    </div>
  );
}

// --------------------------------------------------------------- top bar ---

function TopBar({
  onCommand,
  onShortcuts,
  onCode,
  onMenu,
}: {
  onCommand: () => void;
  onShortcuts: () => void;
  onCode?: () => void;
  onMenu: () => void;
}) {
  const router = useRouter();
  const [health, setHealth] = useState<"ok" | "down" | "degraded" | "checking">("checking");
  const [degraded, setDegraded] = useState<string[]>([]);
  const [user, setUser] = useState<SupabaseUser | null>(null);
  const [menuOpen, setMenuOpen] = useState(false);
  const meBox = useDismiss<HTMLDivElement>(menuOpen, setMenuOpen);

  useEffect(() => {
    let alive = true;
    let timer: ReturnType<typeof setTimeout> | null = null;
    const poll = async () => {
      let nextDelay = 30000;
      try {
        // /readyz, not /healthz: healthz only reports "the process is up and
        // configured" and stayed 200 through a full control-plane outage where
        // every create failed. readyz pings the database and checks for a live
        // compute node, so this pill tells the truth.
        const h = await fetch(`${API_BASE}/readyz`, { cache: "no-store" });
        if (!alive) return;
        if (h.ok) {
          setHealth("ok");
        } else {
          // 503 with a JSON body = reachable but degraded (we can name what);
          // anything else = not reachable at all.
          let subsystems: string[] = [];
          try {
            const body = await h.json();
            if (Array.isArray(body?.degraded)) subsystems = body.degraded;
          } catch {
            /* non-JSON error page — treat as down */
          }
          setHealth(subsystems.length > 0 ? "degraded" : "down");
          setDegraded(subsystems);
          nextDelay = 3000;
        }
      } catch {
        if (alive) setHealth("down");
        nextDelay = 3000;
      }
      if (alive) timer = setTimeout(poll, nextDelay);
    };
    poll();
    const onVis = () => {
      if (!document.hidden) poll();
    };
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

  const email = isStubAuth() ? STUB_USER_EMAIL : (user?.email ?? "Signed in");
  const initials = email.slice(0, 2).toUpperCase();

  const signOut = async () => {
    await createClient().auth.signOut();
    clearListCache(); // drop cached list rows so a later account never sees them
    setMenuOpen(false);
    router.push(isStubAuth() ? "/now" : "/login");
    router.refresh();
  };

  const healthText =
    health === "ok"
      ? "all systems normal"
      : health === "down"
        ? "API unreachable"
        : health === "degraded"
          ? `degraded${degraded.length ? `: ${degraded.join(", ")}` : ""}`
          : "checking…";

  return (
    <header className={s.top}>
      <button type="button" className={s.burger} onClick={onMenu} aria-label="Open the menu">
        <svg width="16" height="12" viewBox="0 0 16 12" fill="none" aria-hidden="true">
          <path d="M0 1h16M0 6h16M0 11h16" stroke="currentColor" strokeWidth="1.5" />
        </svg>
      </button>
      <button type="button" className={s.cmd} onClick={onCommand} aria-label="Open the command bar">
        <span className={s.prompt} aria-hidden="true">
          &gt;
        </span>
        <span className={s.ghost}>sandbox create --template claude</span>
        <span className={s.hint}>type what you would type in the CLI, or search by name or id</span>
        <span className={s.kbd}>⌘K</span>
      </button>
      <div className={s.cell} title={healthText}>
        <span className={`${s.health}${health === "ok" ? "" : ` ${s[health]}`}`} />
        {healthText}
      </div>
      {onCode && (
        <button type="button" className={s.iconBtn} onClick={onCode} title="This page, as code" aria-label="This page, as code">
          {"</>"}
        </button>
      )}
      <button type="button" className={s.iconBtn} onClick={onShortcuts} title="Keyboard shortcuts (?)" aria-label="Keyboard shortcuts">
        ?
      </button>
      <div className={s.me} ref={meBox}>
        <button type="button" className={s.meBtn} onClick={() => setMenuOpen((v) => !v)} aria-expanded={menuOpen} aria-label="Account menu">
          <span className={s.avatar}>{initials}</span>
        </button>
        {menuOpen && (
          <div className={`${s.menu} ${s.right}`}>
            <div className={s.menuHead}>{email}</div>
            <Link href="/settings/tokens" className={s.menuItem} onClick={() => setMenuOpen(false)}>
              <span>API keys</span>
            </Link>
            <Link href="/settings/orgs" className={s.menuItem} onClick={() => setMenuOpen(false)}>
              <span>Organizations</span>
            </Link>
            <div className={s.menuFoot}>
              <button type="button" className={s.menuItem} onClick={signOut}>
                <span>Sign out</span>
              </button>
            </div>
          </div>
        )}
      </div>
    </header>
  );
}

function ShortcutsSheet({ onClose }: { onClose: () => void }) {
  return (
    <div className={s.veil} onClick={onClose}>
      <div className={s.sheet} role="dialog" aria-modal="true" aria-label="Keyboard shortcuts" onClick={(e) => e.stopPropagation()}>
        <div className={s.sheetHead}>
          <span>keyboard shortcuts</span>
          <button type="button" onClick={onClose}>
            close
          </button>
        </div>
        <div className={s.sheetBody}>
          {SHORTCUTS.map((k, i) => (
            <div key={i} className={s.keyRow}>
              <span>{k.label}</span>
              <span>
                {k.keys.map((key) => (
                  <kbd key={key}>{key}</kbd>
                ))}
                {k.or && (
                  <>
                    <em>or</em>
                    {k.or.map((key) => (
                      <kbd key={key}>{key}</kbd>
                    ))}
                  </>
                )}
              </span>
            </div>
          ))}
        </div>
      </div>
    </div>
  );
}
