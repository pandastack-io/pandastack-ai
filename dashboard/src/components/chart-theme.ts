// SPDX-License-Identifier: Apache-2.0
// Chart theme shared WITHOUT pulling recharts into the importer's bundle:
// TimeSeriesChart is loaded via next/dynamic; pages import palette + types
// from here so the heavy chart module stays in its own async chunk.

export const PALETTE = ["#a78bfa", "#34d399", "#fbbf24", "#60a5fa", "#f87171", "#22d3ee"];
export const CHART_PALETTE = PALETTE;

export type Series = {
  name: string;
  color: string;
  data: Array<[string | number, number | null]>; // [bucket, value]
  yFormatter?: (n: number) => string;
};
