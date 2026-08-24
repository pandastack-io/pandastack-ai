// SPDX-License-Identifier: Apache-2.0
// Resource presentation is RAM-first everywhere: memory is the only per-template
// knob a user turns, while every sandbox — first-party or custom template — runs
// the same 8 burstable vCPUs. Showing a varying "8C" next to memory implied CPU
// was a spec you shop for; it isn't. Centralised here so the sandboxes list,
// sandbox detail and templates table can never drift apart again.

export const BURST_VCPUS = 8;

/** "4 GB" · "1.5 GB" · "512 MB" — whole numbers when the size is a clean GiB. */
export function formatMemory(memMB: number | undefined | null): string {
  if (!memMB || memMB <= 0) return "—";
  if (memMB < 1024) return `${memMB} MB`;
  const gb = memMB / 1024;
  return `${gb.toFixed(memMB % 1024 === 0 ? 0 : 1)} GB`;
}

/** The RAM-first resource string, e.g. "4 GB RAM · 8 vCPU burst". */
export function formatResources(memMB: number | undefined | null): string {
  return `${formatMemory(memMB)} RAM · ${BURST_VCPUS} vCPU burst`;
}

/** Hover copy explaining that the numbers are a ceiling, not a reservation. */
export const RESOURCE_TITLE =
  "Burst ceiling, not a reservation — a sandbox bursts up to 8 vCPUs when it has work to do. RAM is baked into the template.";
