#!/usr/bin/env node
/**
 * build-support-index.mjs
 *
 * Generates dashboard/public/support-index.json from docs-site MDX files + curated troubleshooting entries.
 * Called by package.json "build" script before `next build`.
 */

import fs from 'fs';
import path from 'path';
import { fileURLToPath } from 'url';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const DOCS_ROOT = path.resolve(__dirname, '../../docs-site/content/docs');
const OUTPUT = path.resolve(__dirname, '../public/support-index.json');

// Curated troubleshooting entries phrased like user complaints/queries
const TROUBLESHOOTING = [
  {
    title: "Getting 401 Unauthorized",
    keywords: ["401", "unauthorized", "auth", "token", "authentication"],
    body: "Check your PANDASTACK_TOKEN is set and valid. Get a new token from dashboard settings.",
    url: "/docs/quickstart/authentication"
  },
  {
    title: "Getting 402 Payment Required",
    keywords: ["402", "payment", "upgrade", "paid", "tier", "plan"],
    body: "This feature requires a paid plan. Upgrade to Pro, Team, or Enterprise.",
    url: "https://pandastack.ai/pricing"
  },
  {
    title: "Getting 429 Too Many Requests",
    keywords: ["429", "rate limit", "quota", "throttle", "too many"],
    body: "You've hit the hourly sandbox creation limit. Wait or upgrade your plan.",
    url: "/docs/concepts/billing#quotas"
  },
  {
    title: "Getting 503 Service Unavailable - no agents",
    keywords: ["503", "unavailable", "no agents", "capacity"],
    body: "No agents available. Contact support if this persists.",
    url: "https://pandastack.ai/contact"
  },
  {
    title: "Getting 507 Insufficient Storage",
    keywords: ["507", "storage", "disk full", "volume"],
    body: "Agent disk full. Volume provisioning failed — contact support or delete unused volumes.",
    url: "/docs/concepts/volumes"
  },
  {
    title: "Sandbox not responding or stuck",
    keywords: ["sandbox", "stuck", "hanging", "timeout", "not responding"],
    body: "Check sandbox logs. Try exec with shorter timeout or restart the sandbox.",
    url: "/docs/guides/debugging"
  },
  {
    title: "Database connection refused",
    keywords: ["database", "postgres", "connection", "refused", "psql"],
    body: "Ensure TLS is enabled. Check connection_url from GET /v1/databases/{id}.",
    url: "/docs/concepts/databases"
  },
  {
    title: "App deploy failing or stuck",
    keywords: ["app", "deploy", "build", "failing", "error"],
    body: "Check deployment logs for errors. Verify framework detection and build commands.",
    url: "/docs/guides/apps"
  },
  {
    title: "Can't create sandbox - quota exceeded",
    keywords: ["quota", "limit", "max sandboxes", "create", "exceeded"],
    body: "You've reached your workspace sandbox limit. Delete old sandboxes or upgrade.",
    url: "/docs/concepts/billing#quotas"
  },
  {
    title: "Using sandbox for machine learning / AI",
    keywords: ["ml", "ai", "python", "jupyter", "notebook", "pytorch", "tensorflow"],
    body: "Use code-interpreter template with Python. Install packages via exec or custom template.",
    url: "/docs/guides/custom-templates"
  }
];

// Strip MDX frontmatter and JSX tags
function stripMDX(content) {
  return content
    .replace(/^---[\s\S]*?^---/m, '') // frontmatter
    .replace(/<[^>]+>/g, ' ')         // JSX tags
    .replace(/```[\s\S]*?```/g, ' ')  // code blocks
    .replace(/`[^`]+`/g, ' ')         // inline code
    .replace(/\[([^\]]+)\]\([^)]+\)/g, '$1') // links → text
    .replace(/[#*_~]/g, ' ')          // markdown syntax
    .replace(/\s+/g, ' ')
    .trim();
}

// Chunk MDX by H2 headings
function chunkByHeadings(content, filePath) {
  const stripped = stripMDX(content);
  const lines = stripped.split('\n');
  const chunks = [];
  let currentHeading = '';
  let currentBody = [];

  for (const line of lines) {
    const h2Match = line.match(/^##\s+(.+)/);
    if (h2Match) {
      if (currentBody.length > 0) {
        chunks.push({ heading: currentHeading, body: currentBody.join(' ').trim() });
      }
      currentHeading = h2Match[1];
      currentBody = [];
    } else {
      currentBody.push(line);
    }
  }
  if (currentBody.length > 0) {
    chunks.push({ heading: currentHeading, body: currentBody.join(' ').trim() });
  }

  return chunks;
}

// Walk docs tree
function* walkDocs(dir, base = DOCS_ROOT) {
  const entries = fs.readdirSync(dir, { withFileTypes: true });
  for (const entry of entries) {
    const fullPath = path.join(dir, entry.name);
    if (entry.isDirectory()) {
      yield* walkDocs(fullPath, base);
    } else if (entry.name.endsWith('.mdx') || entry.name.endsWith('.md')) {
      const rel = path.relative(base, fullPath).replace(/\\/g, '/');
      yield { path: fullPath, url: `/docs/${rel.replace(/\.(mdx?|md)$/, '')}` };
    }
  }
}

// Build index
async function build() {
  const entries = [];

  // Add troubleshooting (high priority)
  for (const t of TROUBLESHOOTING) {
    entries.push({
      title: t.title,
      keywords: t.keywords,
      heading: '',
      body: t.body,
      url: t.url,
      type: 'troubleshooting'
    });
  }

  // Add docs
  if (fs.existsSync(DOCS_ROOT)) {
    for (const { path: filePath, url } of walkDocs(DOCS_ROOT)) {
      try {
        const content = fs.readFileSync(filePath, 'utf-8');
        const titleMatch = content.match(/^title:\s*(.+)$/m);
        const keywordsMatch = content.match(/^keywords:\s*\[([^\]]+)\]$/m);
        const title = titleMatch ? titleMatch[1].replace(/['"]/g, '') : path.basename(filePath, path.extname(filePath));
        const keywords = keywordsMatch ? keywordsMatch[1].split(',').map(k => k.trim().replace(/['"]/g, '')) : [];

        const chunks = chunkByHeadings(content, filePath);
        if (chunks.length === 0) {
          // No headings — single chunk
          entries.push({
            title,
            keywords,
            heading: '',
            body: stripMDX(content).slice(0, 500),
            url,
            type: 'doc'
          });
        } else {
          for (const chunk of chunks) {
            entries.push({
              title,
              keywords,
              heading: chunk.heading,
              body: chunk.body.slice(0, 500),
              url: chunk.heading ? `${url}#${chunk.heading.toLowerCase().replace(/\s+/g, '-')}` : url,
              type: 'doc'
            });
          }
        }
      } catch (err) {
        console.warn(`Skipping ${filePath}: ${err.message}`);
      }
    }
  } else {
    console.warn(`Docs root ${DOCS_ROOT} not found — index will only include troubleshooting entries`);
  }

  fs.mkdirSync(path.dirname(OUTPUT), { recursive: true });
  fs.writeFileSync(OUTPUT, JSON.stringify(entries, null, 2));
  console.log(`✓ Generated ${OUTPUT} (${entries.length} entries)`);
}

build().catch(err => {
  console.error('Build failed:', err);
  process.exit(1);
});
