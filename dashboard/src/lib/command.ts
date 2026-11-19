// SPDX-License-Identifier: Apache-2.0
// The command bar's grammar: what you would type after `pandastack` in a
// terminal. It reads a line and says what it means; running it is the caller's
// job (components/shell/command.tsx), so this file has no fetch and no React.
//
// Only commands the real CLI has are understood (docs: sdk/cli), and only the
// ones the dashboard can carry out. Anything else comes back as "unknown" and
// the bar offers to search for it instead of guessing.

export type Noun = "sandbox" | "database" | "snapshot" | "template" | "volume";

export type Command =
  | { kind: "list"; noun: Noun }
  | { kind: "sandbox.create"; template: string; ttl?: number }
  | { kind: "open"; noun: "sandbox" | "database"; ref: string }
  | { kind: "sandbox.act"; verb: "delete" | "hibernate" | "wake" | "pause" | "resume" | "snapshot" | "fork"; ref: string }
  | { kind: "database.wake"; ref: string };

export type Parsed =
  | { ok: true; command: Command; /** the line as the CLI would take it */ text: string }
  | { ok: false; /** a known noun with something we cannot run: say so */ reason?: string };

const NOUNS: Record<string, Noun> = {
  sandbox: "sandbox",
  sandboxes: "sandbox",
  sbx: "sandbox",
  database: "database",
  databases: "database",
  db: "database",
  snapshot: "snapshot",
  snapshots: "snapshot",
  template: "template",
  templates: "template",
  volume: "volume",
  volumes: "volume",
};

export const LIST_ROUTE: Record<Noun, string> = {
  sandbox: "/sandboxes",
  database: "/databases",
  snapshot: "/snapshots",
  template: "/templates",
  volume: "/volumes",
};

/** Splits on spaces, keeping "quoted strings" together. */
function words(line: string): string[] {
  const out: string[] = [];
  const re = /"([^"]*)"|'([^']*)'|(\S+)/g;
  let m: RegExpExecArray | null;
  while ((m = re.exec(line))) out.push(m[1] ?? m[2] ?? m[3]);
  return out;
}

/** `--flag value` and `--flag=value`; everything else is positional. */
function split(args: string[]): { flags: Record<string, string>; rest: string[] } {
  const flags: Record<string, string> = {};
  const rest: string[] = [];
  for (let i = 0; i < args.length; i++) {
    const a = args[i];
    if (a.startsWith("--")) {
      const eq = a.indexOf("=");
      if (eq > 0) flags[a.slice(2, eq)] = a.slice(eq + 1);
      else if (i + 1 < args.length && !args[i + 1].startsWith("--")) flags[a.slice(2)] = args[++i];
      else flags[a.slice(2)] = "";
    } else if (a === "-t" && i + 1 < args.length) {
      flags.template = args[++i];
    } else {
      rest.push(a);
    }
  }
  return { flags, rest };
}

export function parseCommand(input: string): Parsed {
  const all = words(input.trim());
  if (all[0] === "pandastack") all.shift();
  if (all.length === 0) return { ok: false };
  const noun = NOUNS[all[0].toLowerCase()];
  if (!noun) return { ok: false };
  const verb = (all[1] ?? "list").toLowerCase();
  const { flags, rest } = split(all.slice(2));
  const ref = rest[0];
  const cli = noun;
  const done = (command: Command, text: string): Parsed => ({ ok: true, command, text: `pandastack ${text}` });
  const needRef = (what: string): Parsed => ({ ok: false, reason: `${cli} ${verb} needs ${what}` });

  if (verb === "list" || verb === "ls") return done({ kind: "list", noun }, `${cli} list`);

  if (noun === "sandbox") {
    if (verb === "create" || verb === "new") {
      const template = flags.template || "base";
      const ttlRaw = flags.ttl;
      let ttl: number | undefined;
      if (ttlRaw !== undefined) {
        ttl = Number(ttlRaw);
        if (!Number.isInteger(ttl) || ttl <= 0) return { ok: false, reason: "--ttl takes a whole number of seconds" };
      }
      if ("from-snapshot" in flags) return { ok: false, reason: "creating from a snapshot is not run from here yet: use the CLI" };
      return done({ kind: "sandbox.create", template, ttl }, `sandbox create --template ${template}${ttl ? ` --ttl ${ttl}` : ""}`);
    }
    if (verb === "ssh" || verb === "get" || verb === "open") {
      if (!ref) return needRef("a sandbox id");
      return done({ kind: "open", noun: "sandbox", ref }, `sandbox ${verb === "open" ? "get" : verb} ${ref}`);
    }
    const act = ({ delete: "delete", rm: "delete", hibernate: "hibernate", sleep: "hibernate", wake: "wake", pause: "pause", resume: "resume", snapshot: "snapshot", fork: "fork" } as const)[verb];
    if (act) {
      if (!ref) return needRef("a sandbox id");
      return done({ kind: "sandbox.act", verb: act, ref }, `sandbox ${act} ${ref}`);
    }
    return { ok: false, reason: `"sandbox ${verb}" is not run from here: use the CLI` };
  }

  if (noun === "database") {
    if (verb === "get" || verb === "open") return ref ? done({ kind: "open", noun: "database", ref }, `database get ${ref}`) : needRef("a database id or label");
    if (verb === "wake") return ref ? done({ kind: "database.wake", ref }, `database wake ${ref}`) : needRef("a database id or label");
    return { ok: false, reason: `"database ${verb}" is not run from here: use the CLI` };
  }
  return { ok: false, reason: `"${cli} ${verb}" is not run from here: use the CLI` };
}

/** A thing the bar can find by id or name. */
export type Thing = { noun: "sandbox" | "database"; id: string; name: string; sub: string; href: string };

/**
 * Finds the one thing `ref` names: a full id, an id prefix, or an exact name.
 * Two matches is "ambiguous", never a guess, because the caller may be about
 * to delete it.
 */
export function resolve(things: Thing[], noun: Thing["noun"], ref: string): { thing?: Thing; error?: string } {
  const pool = things.filter((t) => t.noun === noun);
  const exact = pool.filter((t) => t.id === ref || t.name === ref);
  if (exact.length === 1) return { thing: exact[0] };
  const prefix = pool.filter((t) => t.id.startsWith(ref));
  if (prefix.length === 1) return { thing: prefix[0] };
  if (exact.length > 1 || prefix.length > 1) return { error: `"${ref}" matches more than one ${noun}: give more of the id` };
  return { error: `no ${noun} "${ref}" in this workspace` };
}

/** Plain search: every word must appear in the id, the name or the detail. */
export function search(things: Thing[], query: string, limit = 8): Thing[] {
  const terms = query.toLowerCase().split(/\s+/).filter(Boolean);
  if (terms.length === 0) return [];
  return things
    .filter((t) => {
      const hay = `${t.id} ${t.name} ${t.sub} ${t.noun}`.toLowerCase();
      return terms.every((w) => hay.includes(w));
    })
    .slice(0, limit);
}
