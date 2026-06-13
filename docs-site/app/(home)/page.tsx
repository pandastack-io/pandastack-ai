import Link from "next/link";
import { ArrowRight, Box, Layers, Zap, Code2 } from "lucide-react";

export default function Home() {
  return (
    <main className="flex flex-col">
      {/* ---------- hero ---------- */}
      <section className="relative isolate overflow-hidden border-b border-[var(--color-fd-border)]">
        <div className="absolute inset-0 -z-20 bg-grid fade-mask-b" />
        <div
          className="orb -z-10 -top-24 -left-24 size-[420px]"
          style={{ background: "rgba(109,74,255,0.16)" }}
        />
        <div
          className="orb -z-10 top-10 right-[-140px] size-[380px]"
          style={{ background: "rgba(212,75,255,0.10)", animationDelay: "-6s" }}
        />

        <div className="container mx-auto max-w-5xl px-6 py-20 md:py-28">
          <span className="chip">
            <span className="pulse-dot size-1.5 rounded-full bg-[var(--brand)]" />
            open-source · firecracker microVMs
          </span>

          <h1 className="mt-6 text-4xl font-semibold tracking-tight md:text-6xl">
            Sandboxes in a{" "}
            <span className="text-gradient-brand">blink</span>.
          </h1>
          <p className="mt-5 max-w-xl text-lg text-[var(--color-fd-muted-foreground)]">
            Isolated microVMs for AI agents — boot in 179ms, fork
            mid-execution, snapshot in seconds. Your cloud or ours.
          </p>

          <div className="mt-8 flex flex-wrap items-center gap-3">
            <Link
              href="/docs/getting-started/quickstart"
              className="inline-flex items-center gap-2 rounded-md bg-[var(--brand)] px-4 py-2 text-sm font-medium text-white transition-opacity hover:opacity-90"
            >
              Quickstart <ArrowRight className="size-4" />
            </Link>
            <Link
              href="/docs"
              className="inline-flex items-center gap-2 rounded-md border border-[var(--color-fd-border)] bg-[var(--color-fd-card)] px-4 py-2 text-sm font-medium transition-colors hover:bg-[var(--color-fd-accent)]"
            >
              Browse docs
            </Link>
            <a
              href="https://app.pandastack.ai"
              className="inline-flex items-center gap-2 rounded-md px-4 py-2 text-sm font-medium text-[var(--color-fd-muted-foreground)] transition-colors hover:text-[var(--color-fd-foreground)]"
            >
              Dashboard →
            </a>
          </div>

          {/* terminal */}
          <div className="ink-panel mt-12 overflow-hidden">
            <div className="flex items-center gap-1.5 border-b border-[var(--ink-border)] px-4 py-3">
              <span className="size-2.5 rounded-full bg-[#ff5f57]" />
              <span className="size-2.5 rounded-full bg-[#febc2e]" />
              <span className="size-2.5 rounded-full bg-[#28c840]" />
              <span className="ml-3 font-mono text-[11px] text-[#6f6f85]">
                pandastack — zsh
              </span>
            </div>
            <pre className="overflow-x-auto p-5 font-mono text-[13px] leading-relaxed">
              <code>
                <span className="text-[#6f6f85]">$</span>{" "}
                <span className="text-[#e9e9f1]">pip install pandastack</span>
                {"\n"}
                <span className="text-[#6f6f85]">$</span>{" "}
                <span className="text-[#e9e9f1]">python</span>
                {"\n"}
                <span className="text-[#8d6bff]">{">>>"}</span>{" "}
                <span className="text-[#e9e9f1]">
                  sb = Sandbox(template=
                  <span className="text-[#34d399]">
                    &quot;code-interpreter&quot;
                  </span>
                  )
                </span>
                {"\n"}
                <span className="text-[#6f6f85]">
                  ✓ sandbox ready in{" "}
                  <span className="text-[#fbbf24]">179ms</span>
                </span>
                {"\n"}
                <span className="text-[#8d6bff]">{">>>"}</span>{" "}
                <span className="text-[#e9e9f1]">
                  sb.run(
                  <span className="text-[#34d399]">
                    &quot;print(2 + 2)&quot;
                  </span>
                  ).stdout
                </span>
                {"\n"}
                <span className="text-[#e9e9f1]">4</span>
                <span className="caret text-[#8d6bff]">▍</span>
              </code>
            </pre>
          </div>
        </div>
      </section>

      {/* ---------- stats band ---------- */}
      <section className="container mx-auto max-w-5xl px-6 py-14">
        <div className="grid grid-cols-2 gap-8 md:grid-cols-4">
          <Stat value="179ms" label="p50 create" />
          <Stat value="80ms" label="snapshot restore" />
          <Stat value="400ms" label="same-host fork" />
        </div>
      </section>

      <div className="rule" />

      {/* ---------- lifecycle diagram ---------- */}
      <section className="container mx-auto max-w-5xl px-6 py-16">
        <span className="chip">lifecycle</span>
        <h2 className="mt-4 text-2xl font-semibold tracking-tight md:text-3xl">
          Create. Snapshot. <span className="text-gradient-brand">Fork ×N.</span>
        </h2>
        <p className="mt-3 max-w-lg text-sm text-[var(--color-fd-muted-foreground)]">
          Every create restores a baked snapshot — memory and disk are
          copy-on-write, so forks share state until they diverge.
        </p>

        <div className="pcard mt-8 overflow-x-auto p-6">
          <svg
            viewBox="0 0 720 220"
            className="mx-auto h-auto w-full max-w-3xl min-w-[560px]"
            role="img"
            aria-label="Sandbox lifecycle: create restores a snapshot, snapshots fork into parallel sandboxes"
          >
            {/* connectors */}
            <path
              d="M150 110 H 280"
              className="flow-dash"
              stroke="var(--brand)"
              strokeWidth="1.5"
              fill="none"
            />
            <path
              d="M420 110 C 480 110 480 40 540 40"
              className="flow-dash"
              stroke="var(--brand)"
              strokeWidth="1.5"
              fill="none"
            />
            <path
              d="M420 110 H 540"
              className="flow-dash"
              stroke="var(--brand)"
              strokeWidth="1.5"
              fill="none"
            />
            <path
              d="M420 110 C 480 110 480 180 540 180"
              className="flow-dash"
              stroke="var(--brand)"
              strokeWidth="1.5"
              fill="none"
            />

            {/* create node */}
            <rect
              x="30"
              y="80"
              width="120"
              height="60"
              rx="12"
              fill="var(--color-fd-card)"
              stroke="var(--color-fd-border)"
            />
            <text
              x="90"
              y="105"
              textAnchor="middle"
              fontSize="13"
              fontWeight="600"
              fill="var(--color-fd-foreground)"
            >
              create
            </text>
            <text
              x="90"
              y="124"
              textAnchor="middle"
              fontSize="11"
              fontFamily="var(--font-geist-mono), monospace"
              fill="var(--brand)"
            >
              179ms
            </text>

            {/* snapshot node */}
            <rect
              x="280"
              y="80"
              width="140"
              height="60"
              rx="12"
              fill="var(--brand-dim)"
              stroke="var(--brand)"
              strokeOpacity="0.4"
            />
            <text
              x="350"
              y="105"
              textAnchor="middle"
              fontSize="13"
              fontWeight="600"
              fill="var(--color-fd-foreground)"
            >
              snapshot
            </text>
            <text
              x="350"
              y="124"
              textAnchor="middle"
              fontSize="11"
              fontFamily="var(--font-geist-mono), monospace"
              fill="var(--brand)"
            >
              mem + disk CoW
            </text>

            {/* fork nodes */}
            {[
              { y: 12, label: "fork A" },
              { y: 82, label: "fork B" },
              { y: 152, label: "fork C" },
            ].map((f) => (
              <g key={f.label}>
                <rect
                  x="540"
                  y={f.y}
                  width="120"
                  height="56"
                  rx="12"
                  fill="var(--color-fd-card)"
                  stroke="var(--color-fd-border)"
                />
                <circle
                  cx="562"
                  cy={f.y + 28}
                  r="4"
                  className="pulse-dot"
                  fill="var(--brand)"
                />
                <text
                  x="578"
                  y={f.y + 32}
                  fontSize="12"
                  fontWeight="600"
                  fill="var(--color-fd-foreground)"
                >
                  {f.label}
                </text>
              </g>
            ))}
          </svg>
        </div>
      </section>

      <div className="rule" />

      {/* ---------- features ---------- */}
      <section className="container mx-auto max-w-5xl px-6 py-16">
        <span className="chip">why pandastack</span>
        <h2 className="mt-4 text-2xl font-semibold tracking-tight md:text-3xl">
          Built for agents that do real work
        </h2>
        <div className="mt-8 grid gap-4 md:grid-cols-2">
          <Card
            icon={<Zap className="size-5" />}
            title="Fast cold starts"
            desc="Snapshot-restore on every create + reflink CoW rootfs. Real KVM microVMs — hypervisor isolation, not containers."
          />
          <Card
            icon={<Box className="size-5" />}
            title="Fork mid-execution"
            desc="Pause a running VM, fork it 10× from the same memory state. Parallel exploration without re-running setup."
          />
          <Card
            icon={<Layers className="size-5" />}
            title="5 first-party templates"
            desc="base, code-interpreter, agent, browser, postgres-16 — all pre-baked. Build your own from any Dockerfile."
          />
          <Card
            icon={<Code2 className="size-5" />}
            title="Python · TypeScript · CLI"
            desc="First-class SDKs. SSE streaming exec, PTY over WebSocket, filesystem API, managed Postgres, app hosting."
          />
        </div>
      </section>

      {/* ---------- start here ---------- */}
      <section className="border-t border-[var(--color-fd-border)]">
        <div className="container mx-auto max-w-5xl px-6 py-16">
          <h2 className="text-2xl font-semibold tracking-tight md:text-3xl">
            Start here
          </h2>
          <div className="mt-6 grid gap-3 md:grid-cols-3">
            <PathCard
              href="/docs/getting-started/quickstart"
              title="Quickstart"
              desc="curl, Python, and TypeScript — first sandbox in 30 seconds."
            />
            <PathCard
              href="/docs/concepts/sandbox-lifecycle"
              title="Concepts"
              desc="Lifecycle, snapshots, forks, NATID networking."
            />
            <PathCard
              href="/docs/getting-started/self-host"
              title="Self-host"
              desc="Run PandaStack on AWS, GCP, Mac, or bare metal."
            />
          </div>
        </div>
      </section>
    </main>
  );
}

function Stat({ value, label }: { value: string; label: string }) {
  return (
    <div>
      <div className="glow-brand font-mono text-3xl font-semibold tracking-tight text-[var(--brand)] md:text-4xl">
        {value}
      </div>
      <div className="mt-1 text-xs uppercase tracking-wide text-[var(--color-fd-muted-foreground)]">
        {label}
      </div>
    </div>
  );
}

function Card({
  icon,
  title,
  desc,
}: {
  icon: React.ReactNode;
  title: string;
  desc: string;
}) {
  return (
    <div className="pcard p-5">
      <div
        className="flex size-9 items-center justify-center rounded-md"
        style={{ background: "var(--brand-dim)", color: "var(--brand)" }}
      >
        {icon}
      </div>
      <h3 className="mt-4 text-base font-medium">{title}</h3>
      <p className="mt-1 text-sm text-[var(--color-fd-muted-foreground)]">
        {desc}
      </p>
    </div>
  );
}

function PathCard({
  href,
  title,
  desc,
}: {
  href: string;
  title: string;
  desc: string;
}) {
  return (
    <Link href={href} className="pcard group block p-5">
      <h3 className="text-base font-medium group-hover:text-[var(--brand)]">
        {title}
      </h3>
      <p className="mt-1 text-sm text-[var(--color-fd-muted-foreground)]">
        {desc}
      </p>
      <span className="mt-3 inline-flex items-center gap-1 text-xs text-[var(--brand)]">
        Read <ArrowRight className="size-3" />
      </span>
    </Link>
  );
}
