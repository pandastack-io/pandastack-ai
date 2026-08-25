'use client';

import { useState, ReactNode } from 'react';
import { Copy, Check } from 'lucide-react';
import clsx from 'clsx';

interface QuickstartProps {
  resource: 'sandbox' | 'database' | 'volume';
}

const SNIPPETS = {
  sandbox: {
    python: `# Install SDK
pip install pandastack

# Create a sandbox
from pandastack import Sandbox

sandbox = Sandbox.create(
    template="code-interpreter",
    ttl_seconds=3600
)

# Execute a command
result = sandbox.exec(
    "python -c 'print(2 + 2)'",
    timeout_seconds=30
)
print(result.stdout)  # "4"

# Cleanup
sandbox.kill()`,
    typescript: `// Install SDK
npm install @pandastack/sdk

// Create a sandbox
import { Sandbox } from "@pandastack/sdk";

const sandbox = await Sandbox.create({
  template: "code-interpreter",
  ttlSeconds: 3600
});

// Execute a command
const result = await sandbox.exec(
  "python -c 'print(2 + 2)'",
  { timeoutSeconds: 30 }
);
console.log(result.stdout);  // "4"

// Cleanup
await sandbox.kill();`,
    cli: `# Install CLI
npm install -g @pandastack/sdk

# Set your token
export PANDASTACK_API_KEY=pds_...

# Create a sandbox
pandastack sandbox create \\
  --template code-interpreter \\
  --ttl 3600

# Execute a command
pandastack sandbox exec <sandbox-id> -- \\
  python -c 'print(2 + 2)'

# Delete sandbox
pandastack sandbox delete <sandbox-id>`,
  },
  database: {
    python: `# Install SDK
pip install pandastack

# Create a database
from pandastack import Client

client = Client()
db = client.databases.create(label="my-db")

# Get connection URL
print(db["connection_url"])
# postgres://...@<id>.db.<your-domain>:5432/pandastack

# Connect with psycopg2 or any PostgreSQL client
import psycopg2
conn = psycopg2.connect(db["connection_url"])`,
    typescript: `// Install SDK
npm install @pandastack/sdk

// Create a database
import { Client } from "@pandastack/sdk";

const client = new Client();
const db = await client.databases.create({
  label: "my-db"
});

// Get connection URL
console.log(db.connection_url);
// postgres://...@<id>.db.<your-domain>:5432/pandastack

// Connect with pg or any PostgreSQL client
import { Client as PgClient } from "pg";
const pgClient = new PgClient(db.connection_url);
await pgClient.connect();`,
    cli: `# Install CLI
npm install -g @pandastack/sdk

# Set your token
export PANDASTACK_API_KEY=pds_...

# Create a database
pandastack database create --label my-db

# Get connection details
pandastack database list

# Connect via psql
psql "<connection-url>"`,
  },
  volume: {
    python: `# Install SDK
pip install pandastack

# Create a persistent volume
from pandastack import Client, Sandbox

client = Client()
client.volumes.create(name="my-data", size_mb=1024)

# Attach it to a sandbox at create time
sandbox = Sandbox.create(
    template="code-interpreter",
    volumes=[{"name": "my-data"}]
)

# Mount the attached block device in the guest
sandbox.exec("mkdir -p /mnt/my-data && mount /dev/vdb /mnt/my-data")

# Data on the volume survives sandbox deletion
sandbox.kill()`,
    typescript: `// Install SDK
npm install @pandastack/sdk

// Create a persistent volume
import { Client, Sandbox } from "@pandastack/sdk";

const client = new Client();
await client.volumes.create({ name: "my-data", size_mb: 1024 });

// Attach it to a sandbox at create time
const sandbox = await Sandbox.create({
  template: "code-interpreter",
  volumes: [{ name: "my-data" }]
});

// Mount the attached block device in the guest
await sandbox.exec("mkdir -p /mnt/my-data && mount /dev/vdb /mnt/my-data");

// Data on the volume survives sandbox deletion
await sandbox.kill();`,
    cli: `# Install CLI
npm install -g @pandastack/sdk

# Set your token
export PANDASTACK_API_KEY=pds_...

# Volumes are created via the SDK (client.volumes.create);
# attach an existing volume at sandbox create time:
pandastack sandbox create \\
  --template code-interpreter \\
  --volumes my-data

# Read-only attach
pandastack sandbox create --volumes my-data:ro`,
  },
};

type Tab = 'python' | 'typescript' | 'cli';

// Simple syntax highlighter
function syntaxHighlight(code: string, language: string): ReactNode[] {
  const tokens: ReactNode[] = [];
  let id = 0;

  if (language === 'python') {
    // Python keywords
    const pythonKeywords = ['def', 'class', 'import', 'from', 'as', 'if', 'else', 'elif', 'for', 'while', 'return', 'True', 'False', 'None', 'and', 'or', 'not', 'in', 'is'];
    const pythonBuiltins = ['print', 'len', 'range', 'str', 'int', 'float', 'list', 'dict', 'set', 'tuple', 'open', 'zip', 'enumerate', 'map', 'filter'];

    let lastIndex = 0;
    const regex = /(\b(?:pip|pip install|python)\b|#.*?$|['"`][^'"`]*['"`]|\b[a-zA-Z_]\w*\b|\d+|\W)/gm;
    let match;

    while ((match = regex.exec(code)) !== null) {
      if (match.index > lastIndex) {
        tokens.push(<span key={id++}>{code.slice(lastIndex, match.index)}</span>);
      }
      const token = match[0];
      if (token.startsWith('#')) {
        tokens.push(<span key={id++} style={{ color: '#6a8759' }}>{token}</span>);
      } else if (pythonKeywords.includes(token)) {
        tokens.push(<span key={id++} style={{ color: '#d19a94' }}>{token}</span>);
      } else if (pythonBuiltins.includes(token)) {
        tokens.push(<span key={id++} style={{ color: '#6897bb' }}>{token}</span>);
      } else if (/^['"`]/.test(token)) {
        tokens.push(<span key={id++} style={{ color: '#6a8759' }}>{token}</span>);
      } else if (/^\d+$/.test(token)) {
        tokens.push(<span key={id++} style={{ color: '#d19a94' }}>{token}</span>);
      } else {
        tokens.push(<span key={id++}>{token}</span>);
      }
      lastIndex = regex.lastIndex;
    }
    if (lastIndex < code.length) {
      tokens.push(<span key={id++}>{code.slice(lastIndex)}</span>);
    }
  } else if (language === 'typescript') {
    const tsKeywords = ['import', 'export', 'from', 'const', 'let', 'var', 'function', 'async', 'await', 'new', 'class', 'interface', 'type', 'return', 'if', 'else', 'for', 'while', 'true', 'false', 'null', 'undefined'];
    const tsBuiltins = ['console', 'Array', 'Object', 'String', 'Number', 'Boolean', 'Date', 'Math', 'JSON', 'Promise', 'Map', 'Set'];

    let lastIndex = 0;
    const regex = /(\b(?:npm|npm install)\b|\/\/.*?$|['"`][^'"`]*['"`]|\b[a-zA-Z_]\w*\b|\d+|\W)/gm;
    let match;

    while ((match = regex.exec(code)) !== null) {
      if (match.index > lastIndex) {
        tokens.push(<span key={id++}>{code.slice(lastIndex, match.index)}</span>);
      }
      const token = match[0];
      if (token.startsWith('//')) {
        tokens.push(<span key={id++} style={{ color: '#6a8759' }}>{token}</span>);
      } else if (tsKeywords.includes(token)) {
        tokens.push(<span key={id++} style={{ color: '#d19a94' }}>{token}</span>);
      } else if (tsBuiltins.includes(token)) {
        tokens.push(<span key={id++} style={{ color: '#6897bb' }}>{token}</span>);
      } else if (/^['"`]/.test(token)) {
        tokens.push(<span key={id++} style={{ color: '#6a8759' }}>{token}</span>);
      } else if (/^\d+$/.test(token)) {
        tokens.push(<span key={id++} style={{ color: '#d19a94' }}>{token}</span>);
      } else {
        tokens.push(<span key={id++}>{token}</span>);
      }
      lastIndex = regex.lastIndex;
    }
    if (lastIndex < code.length) {
      tokens.push(<span key={id++}>{code.slice(lastIndex)}</span>);
    }
  } else if (language === 'bash') {
    // Bash/shell syntax
    const bashKeywords = ['npm', 'pip', 'export', 'install', 'create', 'list', 'deploy', '-g', '--'];
    let lastIndex = 0;
    const regex = /(#.*?$|['"`][^'"`]*['"`]|\$\w+|\b(?:npm|pip|export|install|create|list|deploy|pandastack|psql)\b|\W)/gm;
    let match;

    while ((match = regex.exec(code)) !== null) {
      if (match.index > lastIndex) {
        tokens.push(<span key={id++}>{code.slice(lastIndex, match.index)}</span>);
      }
      const token = match[0];
      if (token.startsWith('#')) {
        tokens.push(<span key={id++} style={{ color: '#6a8759' }}>{token}</span>);
      } else if (bashKeywords.some(kw => token.includes(kw))) {
        tokens.push(<span key={id++} style={{ color: '#d19a94' }}>{token}</span>);
      } else if (/^['"`]/.test(token) || token.startsWith('$')) {
        tokens.push(<span key={id++} style={{ color: '#6a8759' }}>{token}</span>);
      } else {
        tokens.push(<span key={id++}>{token}</span>);
      }
      lastIndex = regex.lastIndex;
    }
    if (lastIndex < code.length) {
      tokens.push(<span key={id++}>{code.slice(lastIndex)}</span>);
    }
  }

  return tokens.length > 0 ? tokens : [code];
}

export function Quickstart({ resource }: QuickstartProps) {
  const [tab, setTab] = useState<Tab>('python');
  const [copied, setCopied] = useState(false);

  const snippet = SNIPPETS[resource][tab];
  const highlighted = syntaxHighlight(snippet, tab);

  const copy = () => {
    navigator.clipboard.writeText(snippet);
    setCopied(true);
    setTimeout(() => setCopied(false), 2000);
  };

  return (
    <div className="mt-6 rounded-xl overflow-hidden" style={{ border: '1px solid var(--border-subtle)' }}>
      {/* Tabs */}
      <div
        className="flex items-center gap-1 px-4 py-2"
        style={{ background: 'var(--bg-surface)', borderBottom: '1px solid var(--border-subtle)' }}
      >
        {(['python', 'typescript', 'cli'] as const).map((t) => (
          <button
            key={t}
            onClick={() => setTab(t)}
            className={clsx(
              'px-3 py-1.5 text-xs font-medium rounded-md transition-all',
              tab === t && 'shadow-sm'
            )}
            style={{
              background: tab === t ? 'var(--bg-base)' : 'transparent',
              color: tab === t ? 'var(--brand)' : 'var(--text-muted)',
              borderBottom: tab === t ? '2px solid var(--brand)' : '2px solid transparent',
            }}
          >
            {t === 'python' ? 'Python' : t === 'typescript' ? 'TypeScript' : 'CLI'}
          </button>
        ))}
      </div>

      {/* Code block */}
      <div className="relative" style={{ background: 'var(--bg-elevated)' }}>
        <button
          onClick={copy}
          className="absolute top-3 right-3 p-2 rounded-lg transition-all z-10"
          style={{
            background: 'var(--bg-surface)',
            color: 'var(--text-muted)',
            border: '1px solid var(--border-subtle)',
          }}
          title="Copy code"
        >
          {copied ? <Check size={14} /> : <Copy size={14} />}
        </button>
        <pre className="p-4 pr-12 overflow-x-auto text-xs leading-relaxed" style={{ color: 'var(--text-secondary)' }}>
          <code>{highlighted}</code>
        </pre>
      </div>
    </div>
  );
}
