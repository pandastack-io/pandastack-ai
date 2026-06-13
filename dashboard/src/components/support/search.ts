/**
 * search.ts
 *
 * Lexical search scorer with synonyms, error-code fast path, and fallback.
 */

export interface SearchEntry {
  title: string;
  keywords: string[];
  heading: string;
  body: string;
  url: string;
  type: 'doc' | 'troubleshooting';
}

export interface SearchResult extends SearchEntry {
  score: number;
}

// Synonym map: user term → canonical
const SYNONYMS: Record<string, string> = {
  'vm': 'sandbox',
  'microvm': 'sandbox',
  'container': 'sandbox',
  'psql': 'database',
  'postgresql': 'database',
  'postgres': 'database',
  'db': 'database',
  'repo': 'app',
  'git': 'app',
  'deploy': 'deployment',
  'lambda': 'function',
  'serverless': 'function',
  'cron': 'schedule',
  'job': 'schedule',
  'tier': 'plan',
  'subscription': 'plan',
  'billing': 'pricing',
  'cost': 'pricing',
  'auth': 'authentication',
  'token': 'authentication',
  'api key': 'authentication',
  'error': 'troubleshooting',
  'bug': 'troubleshooting',
  'issue': 'troubleshooting',
  'problem': 'troubleshooting',
  'failing': 'troubleshooting',
  'broken': 'troubleshooting',
};

// Error code fast path: query contains HTTP status or keyword → pin those entries
const ERROR_CODES = [
  '401', '402', '403', '404', '409', '422', '429', '500', '503', '507',
  'unauthorized', 'payment required', 'forbidden', 'not found', 'conflict',
  'rate limit', 'unavailable', 'insufficient storage'
];

function tokenize(text: string): string[] {
  return text
    .toLowerCase()
    .replace(/[^\w\s-]/g, ' ')
    .split(/\s+/)
    .filter(Boolean)
    .map(t => SYNONYMS[t] || t);
}

function score(entry: SearchEntry, query: string): number {
  const queryTokens = tokenize(query);
  if (queryTokens.length === 0) return 0;

  let total = 0;

  // Error-code fast path: if query contains an error code and entry has it, huge boost
  const hasErrorCode = ERROR_CODES.some(code => query.toLowerCase().includes(code));
  if (hasErrorCode) {
    const entryText = `${entry.title} ${entry.keywords.join(' ')} ${entry.body}`.toLowerCase();
    if (ERROR_CODES.some(code => entryText.includes(code))) {
      total += 100; // Pin to top
    }
  }

  // Weights: title 5, keywords 4, heading 3, body 1
  const titleTokens = tokenize(entry.title);
  const keywordTokens = entry.keywords.flatMap(k => tokenize(k));
  const headingTokens = tokenize(entry.heading);
  const bodyTokens = tokenize(entry.body);

  for (const qToken of queryTokens) {
    if (titleTokens.includes(qToken)) total += 5;
    if (keywordTokens.includes(qToken)) total += 4;
    if (headingTokens.includes(qToken)) total += 3;
    if (bodyTokens.includes(qToken)) total += 1;
  }

  // Prefix match on last query token (partial typing)
  const lastToken = queryTokens[queryTokens.length - 1];
  if (lastToken.length >= 2) {
    if (titleTokens.some(t => t.startsWith(lastToken))) total += 2;
    if (keywordTokens.some(t => t.startsWith(lastToken))) total += 1;
  }

  // Troubleshooting entries get a small boost
  if (entry.type === 'troubleshooting') total += 2;

  return total;
}

export function search(index: SearchEntry[], query: string, limit = 10): SearchResult[] {
  if (!query.trim()) return [];

  const results = index
    .map(entry => ({ ...entry, score: score(entry, query) }))
    .filter(r => r.score > 0)
    .sort((a, b) => b.score - a.score)
    .slice(0, limit);

  return results;
}

// Threshold: if top score < 5, query is too vague — return fallback guides
export const SCORE_THRESHOLD = 5;

export const FALLBACK_GUIDES = [
  { title: 'Quickstart', url: 'https://docs.pandastack.ai/docs/getting-started/quickstart' },
  { title: 'Sandboxes', url: 'https://docs.pandastack.ai/docs/concepts/sandbox-lifecycle' },
  { title: 'Databases', url: 'https://docs.pandastack.ai/docs/concepts/databases' },
  { title: 'API Reference', url: 'https://docs.pandastack.ai/docs/reference/rest-api' },
];
