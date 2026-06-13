import { createMDX } from "fumadocs-mdx/next";

const withMDX = createMDX();

const isStatic = process.env.DOCS_TARGET === "static";

export default withMDX({
  reactStrictMode: true,
  output: isStatic ? "export" : undefined,
  images: { unoptimized: true },
  trailingSlash: true,
});
