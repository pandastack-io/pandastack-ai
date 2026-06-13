// SPDX-License-Identifier: Apache-2.0
import type { NextConfig } from "next";
import path from "path";

const nextConfig: NextConfig = {
  // Pin Turbopack workspace root to this directory. Without this, a stray
  // package-lock.json anywhere up the tree (e.g. $HOME/package-lock.json)
  // causes Turbopack to pick that as the root and scan/watch the entire
  // ancestor tree — easily multi-GB RAM on Apple Silicon.
  turbopack: { root: path.resolve(__dirname) },
  // Next 16.2.x auto-generated .next/types/validator.ts imports ResolvingMetadata
  // from "next/types.js" which doesn't re-export it. Skip TS check at build time;
  // we still type-check via `tsc --noEmit` in CI on source files only.
  typescript: { ignoreBuildErrors: true },
};

export default nextConfig;
