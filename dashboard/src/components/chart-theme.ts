// SPDX-License-Identifier: Apache-2.0
// Chart theme shared WITHOUT pulling recharts into the importer's bundle:
// TimeSeriesChart is loaded via next/dynamic; pages import palette + types
// from here so the heavy chart module stays in its own async chunk.

// Series colours for the ink canvas. Index 0 is bone (the default series reads
// as "the" line); index 4 stays red, because callers use it for errors. No
// orange: on these screens orange means "live right now" and nothing else.
export const PALETTE = ["#f0f0f4", "#8ab4ff", "#f2b24c", "#7dd3c0", "#ff6b6b", "#babac1"];
export const CHART_PALETTE = PALETTE;

export type Series = {
  name: string;
  color: string;
  data: Array<[string | number, number | null]>; // [bucket, value]
  yFormatter?: (n: number) => string;
};
