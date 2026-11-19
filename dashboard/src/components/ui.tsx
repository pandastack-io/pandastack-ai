// SPDX-License-Identifier: Apache-2.0
import { ReactNode, createContext, useCallback, useContext, useEffect, useRef, useState } from "react";
import { clsx } from "clsx";
import { Copy, Eye, EyeOff } from "lucide-react";
import { toast } from "sonner";

// The system's label voice: small, mono, lowercase, smoke. One constant so a
// table head, a form label and a key all say it the same way.
export const LABEL = "font-mono text-[11px] lowercase tracking-normal";

// ─── Page Header ───────────────────────────────────────────────────────────────
export function PageHeader({
  title,
  description,
  actions,
  badge,
}: {
  title: string;
  description?: string;
  actions?: ReactNode;
  badge?: ReactNode;
}) {
  return (
    <div className="mb-6 flex items-start justify-between gap-4 pb-5 border-b" style={{ borderColor: "var(--border-subtle)" }}>
      <div className="flex items-center gap-3">
        <div>
          <div className="flex items-center gap-2.5">
            <h1 className="text-[22px] font-semibold leading-none tracking-[-0.035em]" style={{ color: "var(--text-primary)" }}>
              {title}
            </h1>
            {badge}
          </div>
          {description && (
            <p className="mt-2 text-[13px]" style={{ color: "var(--text-muted)" }}>{description}</p>
          )}
        </div>
      </div>
      {actions && <div className="flex items-center gap-2 shrink-0">{actions}</div>}
    </div>
  );
}

// ─── Card ──────────────────────────────────────────────────────────────────────
export function Card({
  children,
  className = "",
  padding = false,
  style,
}: {
  children: ReactNode;
  className?: string;
  padding?: boolean;
  style?: React.CSSProperties;
}) {
  return (
    <div
      className={clsx(
        "rounded-lg overflow-hidden",
        padding && "p-4",
        className
      )}
      style={{
        background: "var(--bg-surface)",
        border: "1px solid var(--border-subtle)",
        ...style,
      }}
    >
      {children}
    </div>
  );
}

// ─── Button ────────────────────────────────────────────────────────────────────
type BtnVariant = "primary" | "secondary" | "ghost" | "danger" | "outline";
type BtnSize = "xs" | "sm" | "md";

export function Btn({
  children,
  variant = "ghost",
  size = "md",
  disabled,
  onClick,
  type = "button",
  className = "",
  icon,
}: {
  children?: ReactNode;
  variant?: BtnVariant;
  size?: BtnSize;
  disabled?: boolean;
  onClick?: () => void;
  type?: "button" | "submit";
  className?: string;
  icon?: ReactNode;
}) {
  const base =
    "inline-flex items-center justify-center gap-1.5 rounded-md font-mono font-medium whitespace-nowrap transition-colors disabled:cursor-not-allowed disabled:opacity-40 select-none";

  const sizes: Record<BtnSize, string> = {
    xs: "px-2 py-0.5 text-[11px] h-6",
    sm: "px-2.5 py-1 text-[11.5px] h-7",
    md: "px-3.5 py-1.5 text-[12.5px] h-8",
  };

  const variants: Record<BtnVariant, string> = {
    primary:
      "bg-bone text-ink hover:bg-white active:bg-zinc-200",
    secondary:
      "bg-coal-2 text-bone border border-line-2 hover:border-smoke active:bg-coal",
    ghost:
      "text-zinc-300 hover:bg-zinc-800 hover:text-zinc-100 active:bg-zinc-900",
    danger:
      "text-red-400 hover:bg-red-500/10 hover:text-red-300 active:bg-red-500/20 border border-red-500/30 hover:border-red-500/60",
    outline:
      "border text-zinc-300 hover:text-zinc-100 hover:border-smoke",
  };

  return (
    <button
      type={type}
      disabled={disabled}
      onClick={onClick}
      className={clsx(base, sizes[size], variants[variant], className)}
      style={variant === "outline" ? { borderColor: "var(--border-strong)" } : undefined}
    >
      {icon && <span className="shrink-0">{icon}</span>}
      {children}
    </button>
  );
}

// ─── Input ─────────────────────────────────────────────────────────────────────
export function Input(
  props: React.InputHTMLAttributes<HTMLInputElement> & {
    label?: string;
    hint?: string;
    error?: string;
    prefixEl?: ReactNode;
  }
) {
  const { label, hint, error, prefixEl, className = "", ...rest } = props;
  return (
    <label className="flex flex-col gap-1">
      {label && (
        <span className={LABEL} style={{ color: "var(--text-muted)" }}>
          {label}
        </span>
      )}
      <div className="relative flex items-center">
        {prefixEl && (
          <div className="absolute left-2.5 flex items-center" style={{ color: "var(--text-muted)" }}>
            {prefixEl}
          </div>
        )}
        <input
          {...rest}
          className={clsx(
            "rounded-md border px-3 py-1.5 text-[13px] transition-colors w-full",
            "placeholder:text-zinc-600 focus:outline-none",
            prefixEl && "pl-8",
            error ? "border-red-500/50 focus:border-red-400" : "focus:border-zinc-400",
            className
          )}
          style={{
            background: "var(--bg-elevated)",
            border: `1px solid var(--border-default)`,
            color: "var(--text-primary)",
          }}
        />
      </div>
      {hint && !error && <span className="text-[11px]" style={{ color: "var(--text-muted)" }}>{hint}</span>}
      {error && <span className="text-[11px] text-red-400">{error}</span>}
    </label>
  );
}

// ─── Select ────────────────────────────────────────────────────────────────────
export function Select(
  props: React.SelectHTMLAttributes<HTMLSelectElement> & { label?: string }
) {
  const { label, className = "", ...rest } = props;
  return (
    <label className="flex flex-col gap-1">
      {label && (
        <span className={LABEL} style={{ color: "var(--text-muted)" }}>
          {label}
        </span>
      )}
      <select
        {...rest}
        className={clsx(
          "rounded-md border px-3 py-1.5 text-[13px] transition-colors focus:outline-none focus:border-zinc-400 cursor-pointer",
          className
        )}
        style={{
          background: "var(--bg-elevated)",
          border: "1px solid var(--border-default)",
          color: "var(--text-primary)",
        }}
      />
    </label>
  );
}

// ─── Status Pill ───────────────────────────────────────────────────────────────
type SandboxStatus = "creating" | "running" | "paused" | "stopping" | "deleted" | "failed" | "hibernated";

// One orange thing: the dot beside "running". Everything else is the ramp;
// asleep is a dashed outline (the same dash the timeline draws for sleep) and
// only failure gets a hue.
const STATUS_MAP: Record<SandboxStatus, { text: string; border: string; dot: string; dashed?: boolean; live?: boolean }> = {
  running:    { text: "var(--c-bone)",  border: "var(--c-line-2)", dot: "var(--c-signal)", live: true },
  creating:   { text: "var(--c-ash)",   border: "var(--c-line-2)", dot: "var(--c-ash)", live: true },
  paused:     { text: "var(--c-ash)",   border: "var(--c-line-2)", dot: "var(--c-warn)" },
  stopping:   { text: "var(--c-ash)",   border: "var(--c-line-2)", dot: "var(--c-warn)", live: true },
  hibernated: { text: "var(--c-smoke)", border: "var(--c-line-2)", dot: "var(--c-mute)", dashed: true },
  failed:     { text: "var(--c-fail)",  border: "rgba(255,107,107,0.45)", dot: "var(--c-fail)" },
  deleted:    { text: "var(--c-mute)",  border: "var(--c-line)", dot: "var(--c-mute)" },
};

export function StatusPill({ status }: { status: SandboxStatus }) {
  const s = STATUS_MAP[status] ?? STATUS_MAP.deleted;
  return (
    <span
      className="inline-flex h-5 items-center gap-1.5 rounded-sm px-1.5 font-mono text-[11px]"
      style={{ color: s.text, border: `1px ${s.dashed ? "dashed" : "solid"} ${s.border}` }}
    >
      <span
        className={clsx("size-1.5 rounded-full shrink-0", s.live && "live-pulse")}
        style={{ background: s.dot }}
      />
      {status === "hibernated" ? "asleep" : status}
    </span>
  );
}

// ─── Empty State ───────────────────────────────────────────────────────────────
export function Empty({
  title,
  hint,
  cta,
  icon,
}: {
  title: string;
  hint?: string;
  cta?: ReactNode;
  icon?: ReactNode;
}) {
  return (
    <div className="flex flex-col items-center justify-center gap-4 rounded-lg border-dashed py-20 text-center"
      style={{ border: "1px dashed var(--border-default)" }}>
      {icon ? (
        <div className="flex size-12 items-center justify-center rounded-xl" style={{ background: "var(--bg-elevated)", border: "1px solid var(--border-subtle)" }}>
          {icon}
        </div>
      ) : (
        <div className="flex size-12 items-center justify-center rounded-xl text-zinc-600" style={{ background: "var(--bg-elevated)", border: "1px solid var(--border-subtle)" }}>
          <svg width="20" height="20" viewBox="0 0 20 20" fill="none" xmlns="http://www.w3.org/2000/svg">
            <rect x="2" y="2" width="7" height="7" rx="1.5" stroke="currentColor" strokeWidth="1.5"/>
            <rect x="11" y="2" width="7" height="7" rx="1.5" stroke="currentColor" strokeWidth="1.5"/>
            <rect x="2" y="11" width="7" height="7" rx="1.5" stroke="currentColor" strokeWidth="1.5"/>
            <rect x="11" y="11" width="7" height="7" rx="1.5" stroke="currentColor" strokeWidth="1.5"/>
          </svg>
        </div>
      )}
      <div>
        <div className="text-[14px] font-medium" style={{ color: "var(--text-primary)" }}>{title}</div>
        {hint && <div className="mt-1 max-w-sm text-[13px]" style={{ color: "var(--text-secondary)" }}>{hint}</div>}
      </div>
      {cta && <div className="mt-1">{cta}</div>}
    </div>
  );
}

// ─── Skeleton ──────────────────────────────────────────────────────────────────
export function Skeleton({ className = "", h = "h-4" }: { className?: string; h?: string }) {
  return <div className={clsx("skeleton rounded", h, className)} />;
}

export function SkeletonRow({ cols = 5 }: { cols?: number }) {
  return (
    <tr>
      {Array.from({ length: cols }).map((_, i) => (
        <td key={i} className="px-4 py-3">
          <Skeleton h="h-3.5" className="w-full max-w-[120px]" />
        </td>
      ))}
    </tr>
  );
}

// ─── Badge ─────────────────────────────────────────────────────────────────────
export function Badge({
  children,
  variant = "default",
  className = "",
}: {
  children: ReactNode;
  variant?: "default" | "success" | "warning" | "error" | "info" | "violet";
  className?: string;
}) {
  const styles: Record<string, { bg: string; text: string; border: string }> = {
    default: { bg: "transparent", text: "var(--c-ash)",   border: "var(--c-line-2)" },
    success: { bg: "transparent", text: "var(--c-bone)",  border: "var(--c-line-2)" },
    warning: { bg: "transparent", text: "var(--c-warn)",  border: "rgba(242,178,76,0.4)" },
    error:   { bg: "transparent", text: "var(--c-fail)",  border: "rgba(255,107,107,0.45)" },
    info:    { bg: "transparent", text: "var(--c-ash)",   border: "var(--c-line-2)" },
    violet:  { bg: "transparent", text: "var(--c-smoke)", border: "var(--c-line-2)" },
  };
  const s = styles[variant] ?? styles.default;
  return (
    <span
      className={clsx("inline-flex h-[18px] items-center rounded-sm px-1.5 font-mono text-[10.5px]", className)}
      style={{ background: s.bg, color: s.text, border: `1px solid ${s.border}` }}
    >
      {children}
    </span>
  );
}

// ─── Divider ───────────────────────────────────────────────────────────────────
export function Divider() {
  return <div className="my-5" style={{ borderTop: "1px solid var(--border-subtle)" }} />;
}

// ─── Kv (key-value) ────────────────────────────────────────────────────────────
export function Kv({ k, v }: { k: string; v: string }) {
  return (
    <div>
      <div className={LABEL} style={{ color: "var(--text-muted)" }}>{k}</div>
      <div className="mt-0.5 font-mono text-[13px]" style={{ color: "var(--text-primary)" }}>{v || "—"}</div>
    </div>
  );
}

// ─── Copy Row ────────────────────────────────────────────────────────────────
// A labelled value with a one-click copy button. When `secret` is set the value
// is masked by default with a Show/Hide toggle (used for DB passwords + broker
// tokens, so a credentials card shown right after create — peak screenshare
// moment — never exposes the secret). `masked` overrides the hidden-state text
// (e.g. a connection URL that blanks only its embedded password); it defaults to
// a full dot-mask. Copy always writes the real `value`. Shared by the database
// list + detail pages so they can't drift (the list used to print cleartext).
export function CopyRow({ label, value, secret = false, masked }: { label: string; value: string; secret?: boolean; masked?: string }) {
  const [revealed, setRevealed] = useState(false);
  const isHidden = secret && !revealed;
  const shown = isHidden ? (masked ?? "••••••••••••") : value;
  return (
    <div className="flex items-center gap-2">
      <span className={clsx("w-28 shrink-0", LABEL)} style={{ color: "var(--text-muted)" }}>{label}</span>
      <code className="flex-1 truncate rounded px-2 py-1 font-mono text-[12px]" style={{ background: "var(--bg-elevated)", color: "var(--text-primary)", border: "1px solid var(--border-subtle)" }}>{shown}</code>
      {secret && (
        <Btn size="sm" variant="ghost" icon={revealed ? <EyeOff size={12} /> : <Eye size={12} />} onClick={() => setRevealed((v) => !v)}>
          {revealed ? "Hide" : "Show"}
        </Btn>
      )}
      <Btn size="sm" variant="ghost" icon={<Copy size={12} />} onClick={() => void navigator.clipboard?.writeText(value).then(() => toast.success("Copied")).catch(() => toast.error("Copy failed"))}>Copy</Btn>
    </div>
  );
}

// ─── Data Table ────────────────────────────────────────────────────────────────
export function Table({ children }: { children: ReactNode }) {
  return (
    <div className="overflow-x-auto">
      <table className="w-full border-collapse text-[13px]">
        {children}
      </table>
    </div>
  );
}

export function Th({ children, right }: { children?: ReactNode; right?: boolean }) {
  return (
    <th
      className={clsx("px-4 py-2.5 font-normal whitespace-nowrap", LABEL, right && "text-right")}
      style={{ color: "var(--text-muted)", borderBottom: "1px solid var(--border-subtle)" }}
    >
      {children}
    </th>
  );
}

export function Td({ children, right, mono, muted, className = "" }: {
  children?: ReactNode; right?: boolean; mono?: boolean; muted?: boolean; className?: string;
}) {
  return (
    <td
      className={clsx("px-4 py-2.5", right && "text-right", mono && "font-mono", className)}
      style={{
        color: muted ? "var(--text-secondary)" : "var(--text-primary)",
        borderBottom: "1px solid var(--border-subtle)",
      }}
    >
      {children}
    </td>
  );
}

// ─── Alert ─────────────────────────────────────────────────────────────────────
export function Alert({ type = "error", children }: { type?: "error" | "success" | "info"; children: ReactNode }) {
  const styles = {
    error:   { bg: "rgba(255,107,107,0.06)", border: "rgba(255,107,107,0.35)", text: "var(--red-300)" },
    success: { bg: "var(--c-coal)", border: "var(--c-line-2)", text: "var(--c-bone)" },
    info:    { bg: "var(--c-coal)", border: "var(--c-line-2)", text: "var(--c-ash)" },
  }[type];
  return (
    <div
      className="rounded-lg px-4 py-3 text-[13px]"
      style={{ background: styles.bg, border: `1px solid ${styles.border}`, color: styles.text }}
    >
      {children}
    </div>
  );
}

// ─── Confirm Dialog ────────────────────────────────────────────────────────────
type ConfirmOptions = {
  title: string;
  description?: ReactNode;
  confirmLabel?: string;
  cancelLabel?: string;
  destructive?: boolean;
};

type ConfirmContextValue = (opts: ConfirmOptions) => Promise<boolean>;

const ConfirmContext = createContext<ConfirmContextValue | null>(null);

export function ConfirmProvider({ children }: { children: ReactNode }) {
  const [opts, setOpts] = useState<ConfirmOptions | null>(null);
  const resolverRef = useRef<((v: boolean) => void) | null>(null);

  const confirm = useCallback<ConfirmContextValue>((options) => {
    setOpts(options);
    return new Promise<boolean>((resolve) => {
      resolverRef.current = resolve;
    });
  }, []);

  const close = useCallback((v: boolean) => {
    resolverRef.current?.(v);
    resolverRef.current = null;
    setOpts(null);
  }, []);

  useEffect(() => {
    if (!opts) return;
    // The key press that opened this dialog may still be on its way up to
    // window when this effect runs (the command bar asks on Enter), and a held
    // key repeats. Neither may answer the question, least of all "delete?":
    // listen from the next task on, and only to a fresh press.
    const onKey = (e: KeyboardEvent) => {
      if (e.repeat) return;
      if (e.key === "Escape") close(false);
      if (e.key === "Enter") close(true);
    };
    const armed = setTimeout(() => window.addEventListener("keydown", onKey), 0);
    return () => {
      clearTimeout(armed);
      window.removeEventListener("keydown", onKey);
    };
  }, [opts, close]);

  return (
    <ConfirmContext.Provider value={confirm}>
      {children}
      {opts && (
        <div
          // Above everything else, the command bar (z 80) included: a question
          // that is hidden behind the thing that asked it cannot be answered.
          className="fixed inset-0 z-[90] flex items-center justify-center bg-black/60 px-4"
          onClick={() => close(false)}
        >
          <div
            className="w-full max-w-md rounded-xl shadow-2xl"
            style={{ background: "var(--bg-elevated)", border: "1px solid var(--border-strong)" }}
            onClick={(e) => e.stopPropagation()}
          >
            <div className="px-5 pt-4 pb-3">
              <div className="text-[14px] font-semibold" style={{ color: "var(--text-primary)" }}>
                {opts.title}
              </div>
              {opts.description && (
                <div className="mt-1.5 text-[13px] leading-5" style={{ color: "var(--text-secondary)" }}>
                  {opts.description}
                </div>
              )}
            </div>
            <div
              className="flex items-center justify-end gap-2 px-5 py-3"
              style={{ borderTop: "1px solid var(--border-subtle)" }}
            >
              <Btn variant="ghost" size="sm" onClick={() => close(false)}>
                {opts.cancelLabel ?? "Cancel"}
              </Btn>
              <Btn
                variant={opts.destructive ? "danger" : "primary"}
                size="sm"
                onClick={() => close(true)}
              >
                {opts.confirmLabel ?? "Confirm"}
              </Btn>
            </div>
          </div>
        </div>
      )}
    </ConfirmContext.Provider>
  );
}

export function useConfirm() {
  const ctx = useContext(ConfirmContext);
  if (!ctx) {
    // Fallback to native confirm so callers still work if provider isn't mounted (e.g. tests).
    return async (opts: ConfirmOptions) =>
      typeof window !== "undefined" ? window.confirm(opts.title) : false;
  }
  return ctx;
}

// ─── Tabs ──────────────────────────────────────────────────────────────────────
export function Tabs<T extends string>({
  value,
  onChange,
  tabs,
}: {
  value: T;
  onChange: (v: T) => void;
  tabs: { value: T; label: string; icon?: ReactNode }[];
}) {
  return (
    <div className="flex items-center gap-0.5 overflow-x-auto" style={{ borderBottom: "1px solid var(--border-subtle)" }}>
      {tabs.map((tab) => {
        const active = tab.value === value;
        return (
          <button
            key={tab.value}
            onClick={() => onChange(tab.value)}
            className={clsx(
              "flex items-center gap-1.5 whitespace-nowrap px-4 py-2.5 text-[13px] font-medium transition-colors border-b-2 -mb-px",
              active
                ? "border-zinc-100 text-zinc-100"
                : "border-transparent hover:text-zinc-200"
            )}
            style={{ color: active ? undefined : "var(--text-muted)" }}
          >
            {tab.icon}
            {tab.label}
          </button>
        );
      })}
    </div>
  );
}
