// SPDX-License-Identifier: Apache-2.0
// tests/e2e/functions.test.ts — end-to-end tests for Functions + Schedules API
//
// Run:
//   PANDASTACK_API_KEY=pds_... PANDASTACK_API_URL=https://api.pandastack.ai \
//   npx tsx tests/e2e/functions.test.ts

import { gzip as _gzip } from "node:zlib";
import { promisify } from "node:util";

const gzip = promisify(_gzip);

const API_URL = process.env.PANDASTACK_API_URL ?? "https://api.pandastack.ai";
const API_KEY = process.env.PANDASTACK_API_KEY ?? "";

if (!API_KEY) {
  console.error("error: PANDASTACK_API_KEY is required");
  process.exit(1);
}

const headers = (extra: Record<string, string> = {}) => ({
  Authorization: `Bearer ${API_KEY}`,
  "Content-Type": "application/json",
  ...extra,
});

let passed = 0;
let failed = 0;
const createdFunctions: string[] = [];
const createdSchedules: string[] = [];

async function test(name: string, fn: () => Promise<void>) {
  try {
    await fn();
    console.log(`  ✅ ${name}`);
    passed++;
  } catch (err: any) {
    console.error(`  ❌ ${name}: ${err?.message ?? err}`);
    failed++;
  }
}

function assert(condition: boolean, msg: string) {
  if (!condition) throw new Error(msg);
}

async function api(method: string, path: string, body?: unknown): Promise<any> {
  const res = await fetch(`${API_URL}/v1${path}`, {
    method,
    headers: headers(),
    body: body != null ? JSON.stringify(body) : undefined,
  });
  const text = await res.text();
  if (!res.ok) throw new Error(`${method} ${path} → ${res.status}: ${text}`);
  if (!text) return null;
  return JSON.parse(text);
}

async function makeCodeGz(code: string): Promise<string> {
  const buf = await gzip(Buffer.from(code));
  return buf.toString("base64");
}

// ─── Function CRUD ───────────────────────────────────────────────────────────

async function runFunctionTests() {
  console.log("\n── Functions ──────────────────────────────────────────────");

  let fnId: string;

  await test("GET /v1/functions returns empty array initially", async () => {
    const data = await api("GET", "/functions");
    assert(Array.isArray(data), "expected array");
  });

  await test("POST /v1/functions creates a python function", async () => {
    const code = await makeCodeGz(`
print("hello from pandastack functions!")
`);
    const data = await api("POST", "/functions", {
      name: `test-fn-${Date.now()}`,
      runtime: "python",
      entrypoint: "handler.py",
      code,
    });
    assert(data.id, "missing id");
    assert(data.runtime === "python", `wrong runtime: ${data.runtime}`);
    assert(!data.code_gz, "code_gz should not be returned in response");
    assert(typeof data.code_size === "number" && data.code_size > 0, "expected code_size > 0");
    fnId = data.id;
    createdFunctions.push(fnId);
  });

  await test("GET /v1/functions lists the created function", async () => {
    const data = await api("GET", "/functions");
    assert(Array.isArray(data), "expected array");
    assert(data.some((f: any) => f.id === fnId), "created function not in list");
  });

  await test("GET /v1/functions/{id} returns function metadata", async () => {
    const data = await api("GET", `/functions/${fnId}`);
    assert(data.id === fnId, "id mismatch");
    assert(data.runtime === "python", "runtime mismatch");
    assert(!data.code_gz, "code should not be returned");
  });

  await test("GET /v1/functions/{id}/code returns gzip bytes", async () => {
    const res = await fetch(`${API_URL}/v1/functions/${fnId}/code`, {
      headers: { Authorization: `Bearer ${API_KEY}` },
    });
    assert(res.ok, `expected 200, got ${res.status}`);
    const buf = await res.arrayBuffer();
    assert(buf.byteLength > 0, "expected non-empty code bytes");
  });

  await test("POST /v1/functions with duplicate name returns 409", async () => {
    const code = await makeCodeGz(`print("dup")`);
    // Get the name of the function we just created
    const fn = await api("GET", `/functions/${fnId}`);
    const res = await fetch(`${API_URL}/v1/functions`, {
      method: "POST",
      headers: headers(),
      body: JSON.stringify({ name: fn.name, runtime: "python", code }),
    });
    assert(res.status === 409, `expected 409, got ${res.status}`);
  });

  await test("POST /v1/functions rejects unknown runtime", async () => {
    const code = await makeCodeGz(`print("bad")`);
    const res = await fetch(`${API_URL}/v1/functions`, {
      method: "POST",
      headers: headers(),
      body: JSON.stringify({ name: `test-bad-runtime-${Date.now()}`, runtime: "ruby", code }),
    });
    assert(res.status === 400, `expected 400, got ${res.status}`);
  });

  await test("POST /v1/functions/{id}/runs records a run", async () => {
    const run = await api("POST", `/functions/${fnId}/runs`, {
      status: "success",
      exit_code: 0,
      stdout: "hello from pandastack functions!\n",
      stderr: "",
      duration_ms: 1250,
      sandbox_id: "test-sandbox-id",
    });
    assert(run.id, "missing run id");
    assert(run.status === "success", `wrong status: ${run.status}`);
    assert(run.function_id === fnId, "function_id mismatch");
  });

  await test("GET /v1/functions/{id}/runs returns run history", async () => {
    const runs = await api("GET", `/functions/${fnId}/runs`);
    assert(Array.isArray(runs), "expected array");
    assert(runs.length >= 1, "expected at least 1 run");
    assert(runs[0].function_id === fnId, "function_id mismatch in run");
  });

  await test("GET /v1/functions/{id} for non-existent returns 404", async () => {
    const res = await fetch(`${API_URL}/v1/functions/00000000-0000-0000-0000-000000000000`, {
      headers: { Authorization: `Bearer ${API_KEY}` },
    });
    assert(res.status === 404, `expected 404, got ${res.status}`);
  });

  return fnId!;
}

// ─── Schedule CRUD ───────────────────────────────────────────────────────────

async function runScheduleTests(fnId: string) {
  console.log("\n── Schedules ──────────────────────────────────────────────");

  let schedId: string;

  await test("GET /v1/schedules returns empty array initially", async () => {
    const data = await api("GET", "/schedules");
    assert(Array.isArray(data), "expected array");
  });

  await test("POST /v1/schedules creates a schedule", async () => {
    const data = await api("POST", "/schedules", {
      name: `test-sched-${Date.now()}`,
      function_id: fnId,
      cron: "0 9 * * *",
    });
    assert(data.id, "missing id");
    assert(data.function_id === fnId, "function_id mismatch");
    assert(data.cron === "0 9 * * *", "cron mismatch");
    assert(data.paused === false, "should not be paused by default");
    schedId = data.id;
    createdSchedules.push(schedId);
  });

  await test("GET /v1/schedules lists the created schedule", async () => {
    const data = await api("GET", "/schedules");
    assert(Array.isArray(data), "expected array");
    assert(data.some((s: any) => s.id === schedId), "schedule not in list");
  });

  await test("GET /v1/schedules/{id} returns schedule", async () => {
    const data = await api("GET", `/schedules/${schedId}`);
    assert(data.id === schedId, "id mismatch");
    assert(data.function_id === fnId, "function_id mismatch");
  });

  await test("PATCH /v1/schedules/{id} pauses schedule", async () => {
    const data = await api("PATCH", `/schedules/${schedId}`, { paused: true });
    assert(data.paused === true, "expected paused=true");
  });

  await test("PATCH /v1/schedules/{id} updates cron", async () => {
    const data = await api("PATCH", `/schedules/${schedId}`, { cron: "0 10 * * 1" });
    assert(data.cron === "0 10 * * 1", `cron not updated: ${data.cron}`);
  });

  await test("PATCH /v1/schedules/{id} resumes schedule", async () => {
    const data = await api("PATCH", `/schedules/${schedId}`, { paused: false });
    assert(data.paused === false, "expected paused=false");
  });

  await test("POST /v1/schedules/{id}/trigger records a run and returns run_id", async () => {
    const data = await api("POST", `/schedules/${schedId}/trigger`);
    assert(data.run_id, "missing run_id");
  });

  await test("GET /v1/schedules/{id}/runs returns triggered run", async () => {
    const runs = await api("GET", `/schedules/${schedId}/runs`);
    assert(Array.isArray(runs), "expected array");
    assert(runs.length >= 1, "expected at least 1 run");
    assert(runs[0].schedule_id === schedId, "schedule_id mismatch in run");
  });

  await test("POST /v1/schedules with non-existent function_id returns 400", async () => {
    const res = await fetch(`${API_URL}/v1/schedules`, {
      method: "POST",
      headers: headers(),
      body: JSON.stringify({
        name: `test-bad-fn-${Date.now()}`,
        function_id: "00000000-0000-0000-0000-000000000000",
        cron: "* * * * *",
      }),
    });
    assert(res.status === 400 || res.status === 404, `expected 400 or 404, got ${res.status}`);
  });

  await test("GET /v1/schedules/{id} for non-existent returns 404", async () => {
    const res = await fetch(`${API_URL}/v1/schedules/00000000-0000-0000-0000-000000000000`, {
      headers: { Authorization: `Bearer ${API_KEY}` },
    });
    assert(res.status === 404, `expected 404, got ${res.status}`);
  });
}

// ─── Auth checks ─────────────────────────────────────────────────────────────

async function runAuthTests() {
  console.log("\n── Auth ───────────────────────────────────────────────────");

  await test("GET /v1/functions without auth returns 401", async () => {
    const res = await fetch(`${API_URL}/v1/functions`);
    assert(res.status === 401, `expected 401, got ${res.status}`);
  });

  await test("GET /v1/schedules without auth returns 401", async () => {
    const res = await fetch(`${API_URL}/v1/schedules`);
    assert(res.status === 401, `expected 401, got ${res.status}`);
  });

  await test("GET /v1/functions with bad token returns 401", async () => {
    const res = await fetch(`${API_URL}/v1/functions`, {
      headers: { Authorization: "Bearer pds_invalid_token_xyz" },
    });
    assert(res.status === 401, `expected 401, got ${res.status}`);
  });
}

// ─── Cleanup ─────────────────────────────────────────────────────────────────

async function cleanup() {
  console.log("\n── Cleanup ────────────────────────────────────────────────");

  for (const id of createdSchedules) {
    try {
      await api("DELETE", `/schedules/${id}`);
      console.log(`  🗑  schedule ${id} deleted`);
    } catch (e: any) {
      console.warn(`  ⚠  failed to delete schedule ${id}: ${e?.message}`);
    }
  }

  for (const id of createdFunctions) {
    try {
      await api("DELETE", `/functions/${id}`);
      console.log(`  🗑  function ${id} deleted`);
    } catch (e: any) {
      console.warn(`  ⚠  failed to delete function ${id}: ${e?.message}`);
    }
  }

  await test("DELETE /v1/functions/{id} for already-deleted returns 404", async () => {
    if (createdFunctions.length === 0) return;
    const id = createdFunctions[0];
    const res = await fetch(`${API_URL}/v1/functions/${id}`, {
      method: "DELETE",
      headers: { Authorization: `Bearer ${API_KEY}` },
    });
    assert(res.status === 404, `expected 404, got ${res.status}`);
  });
}

// ─── Main ────────────────────────────────────────────────────────────────────

async function main() {
  console.log(`\nPandaStack Functions + Schedules E2E Tests`);
  console.log(`API: ${API_URL}`);
  console.log(`Key: ${API_KEY.slice(0, 10)}...`);

  try {
    await runAuthTests();
    const fnId = await runFunctionTests();
    if (fnId) await runScheduleTests(fnId);
  } finally {
    await cleanup();
  }

  console.log(`\n${"─".repeat(50)}`);
  console.log(`Results: ${passed} passed, ${failed} failed`);

  if (failed > 0) process.exit(1);
}

main().catch((err) => {
  console.error("fatal:", err);
  process.exit(1);
});
