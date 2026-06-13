import { createMDX } from "fumadocs-mdx/next";

const withMDX = createMDX();

const isStatic = process.env.DOCS_TARGET === "static";

export default withMDX({
  reactStrictMode: true,
  output: isStatic ? "export" : undefined,
  images: { unoptimized: true },
  trailingSlash: true,
  // A stray lockfile in $HOME makes Next infer the wrong workspace root —
  // pin it to this app. (Note: building also requires Node >= 20.19 / 22.12
  // for require(esm), which the fumadocs-mdx Turbopack loader relies on.)
  turbopack: { root: process.cwd() },
});
