import { source } from "@/lib/source";
import { createFromSource } from "fumadocs-core/search/server";

// Static export: the search index is built once at build time and served as a
// static JSON file. The client (RootProvider search type "static") downloads it
// and runs Orama in the browser — no live search server needed on Cloudflare Pages.
export const revalidate = false;
export const dynamic = "force-static";

export const { staticGET: GET } = createFromSource(source);
