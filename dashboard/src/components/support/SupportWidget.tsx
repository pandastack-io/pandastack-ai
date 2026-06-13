'use client';

import { useState, useEffect, useRef } from 'react';
import { usePathname } from 'next/navigation';
import { HelpCircle, X, Search, ExternalLink, Mail } from 'lucide-react';
import { search, SearchEntry, SearchResult, SCORE_THRESHOLD, FALLBACK_GUIDES } from './search';

// Contextual suggestions keyed on pathname
const CONTEXTUAL: Record<string, { title: string; url: string }[]> = {
  '/sandboxes': [
    { title: 'Creating your first sandbox', url: '/docs/quickstart#create-sandbox' },
    { title: 'Sandbox templates', url: '/docs/concepts/sandboxes#templates' },
    { title: 'Executing commands', url: '/docs/guides/exec' },
  ],
  '/databases': [
    { title: 'Creating a PostgreSQL database', url: '/docs/quickstart#create-database' },
    { title: 'Connection strings', url: '/docs/concepts/databases#connection' },
    { title: 'Backup and restore', url: '/docs/concepts/databases#backup' },
  ],
  '/apps': [
    { title: 'Deploying your first app', url: '/docs/guides/apps#deploy' },
    { title: 'Framework detection', url: '/docs/guides/apps#framework-detection' },
    { title: 'Custom build commands', url: '/docs/guides/apps#build-commands' },
  ],
  '/functions': [
    { title: 'Creating a function', url: '/docs/guides/functions#create' },
    { title: 'Runtime selection', url: '/docs/guides/functions#runtime' },
    { title: 'Invoking functions', url: '/docs/guides/functions#invoke' },
  ],
  '/schedules': [
    { title: 'Creating a schedule', url: '/docs/guides/schedules#create' },
    { title: 'Cron syntax', url: '/docs/guides/schedules#cron' },
    { title: 'Monitoring schedules', url: '/docs/guides/schedules#monitor' },
  ],
};

const QUICK_LINKS = [
  { title: 'Documentation', url: 'https://docs.pandastack.ai' },
  { title: 'API Reference', url: '/docs/api' },
  { title: 'Discord Community', url: 'https://discord.gg/C7Du7XbG' },
];

export function SupportWidget() {
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState('');
  const [results, setResults] = useState<SearchResult[]>([]);
  const [index, setIndex] = useState<SearchEntry[]>([]);
  const [loading, setLoading] = useState(true);
  const pathname = usePathname();
  const inputRef = useRef<HTMLInputElement>(null);

  // Load index on mount
  useEffect(() => {
    fetch('/support-index.json')
      .then(r => r.json())
      .then(data => {
        setIndex(data);
        setLoading(false);
      })
      .catch(err => {
        console.warn('Failed to load support index:', err);
        setLoading(false);
      });
  }, []);

  // Focus input when opening
  useEffect(() => {
    if (open && inputRef.current) {
      inputRef.current.focus();
    }
  }, [open]);

  // Search on query change
  useEffect(() => {
    if (!query.trim()) {
      setResults([]);
      return;
    }
    const hits = search(index, query, 8);
    setResults(hits);
  }, [query, index]);

  // Escape to close
  useEffect(() => {
    const handler = (e: KeyboardEvent) => {
      if (e.key === 'Escape' && open) {
        setOpen(false);
      }
    };
    document.addEventListener('keydown', handler);
    return () => document.removeEventListener('keydown', handler);
  }, [open]);

  const contextual = CONTEXTUAL[pathname] || [];
  const showFallback = query.trim() && results.length > 0 && results[0].score < SCORE_THRESHOLD;

  return (
    <>
      {/* Floating help button */}
      <button
        onClick={() => setOpen(true)}
        className="fixed bottom-6 right-6 z-40 flex items-center justify-center w-12 h-12 rounded-full shadow-lg transition-all hover:scale-105 active:scale-95"
        style={{
          background: 'var(--brand)',
          color: '#fff',
        }}
        aria-label="Help"
      >
        <HelpCircle size={22} />
      </button>

      {/* Slide-over panel */}
      {open && (
        <div className="fixed inset-0 z-50 flex justify-end">
          {/* Backdrop */}
          <div
            className="absolute inset-0"
            style={{ background: 'rgba(0,0,0,0.55)' }}
            onClick={() => setOpen(false)}
          />

          {/* Panel */}
          <div
            className="relative w-full max-w-md h-full flex flex-col"
            style={{
              background: 'var(--bg-base)',
              borderLeft: '1px solid var(--border-default)',
            }}
          >
            {/* Header */}
            <div
              className="flex items-center justify-between px-6 py-4"
              style={{ borderBottom: '1px solid var(--border-subtle)' }}
            >
              <h2 className="text-lg font-semibold" style={{ color: 'var(--text-primary)' }}>
                Help & Support
              </h2>
              <button
                onClick={() => setOpen(false)}
                className="p-1.5 rounded-lg transition-colors"
                style={{ color: 'var(--text-muted)' }}
                onMouseEnter={(e) => (e.currentTarget.style.background = 'var(--bg-surface)')}
                onMouseLeave={(e) => (e.currentTarget.style.background = 'transparent')}
              >
                <X size={18} />
              </button>
            </div>

            {/* Content */}
            <div className="flex-1 overflow-y-auto px-6 py-4 space-y-6">
              {/* Search */}
              <div>
                <div className="relative">
                  <Search
                    size={16}
                    className="absolute left-3 top-1/2 -translate-y-1/2"
                    style={{ color: 'var(--text-muted)' }}
                  />
                  <input
                    ref={inputRef}
                    type="text"
                    placeholder="Search documentation..."
                    value={query}
                    onChange={(e) => setQuery(e.target.value)}
                    className="w-full pl-10 pr-4 py-2.5 rounded-lg outline-none transition-all"
                    style={{
                      background: 'var(--bg-surface)',
                      border: '1px solid var(--border-subtle)',
                      color: 'var(--text-primary)',
                    }}
                    onFocus={(e) => (e.target.style.borderColor = 'var(--brand-border)')}
                    onBlur={(e) => (e.target.style.borderColor = 'var(--border-subtle)')}
                  />
                </div>

                {/* Results */}
                {query.trim() && (
                  <div className="mt-3 space-y-2">
                    {loading && (
                      <p className="text-sm" style={{ color: 'var(--text-secondary)' }}>
                        Loading...
                      </p>
                    )}
                    {!loading && results.length === 0 && (
                      <p className="text-sm" style={{ color: 'var(--text-secondary)' }}>
                        No results found
                      </p>
                    )}
                    {!loading && showFallback && (
                      <div className="p-3 rounded-lg" style={{ background: 'var(--bg-surface)' }}>
                        <p className="text-xs mb-2" style={{ color: 'var(--text-secondary)' }}>
                          No strong matches. Try these guides:
                        </p>
                        {FALLBACK_GUIDES.map((g) => (
                          <a
                            key={g.url}
                            href={g.url}
                            target="_blank"
                            rel="noopener noreferrer"
                            className="block py-1.5 text-sm transition-colors"
                            style={{ color: 'var(--brand)' }}
                          >
                            {g.title}
                          </a>
                        ))}
                      </div>
                    )}
                    {!loading && !showFallback && results.map((r) => (
                      <a
                        key={r.url + r.heading}
                        href={r.url}
                        target="_blank"
                        rel="noopener noreferrer"
                        className="block p-3 rounded-lg transition-all"
                        style={{
                          background: 'var(--bg-surface)',
                          border: '1px solid transparent',
                        }}
                        onMouseEnter={(e) => {
                          e.currentTarget.style.borderColor = 'var(--brand-border)';
                        }}
                        onMouseLeave={(e) => {
                          e.currentTarget.style.borderColor = 'transparent';
                        }}
                      >
                        <div className="flex items-start justify-between gap-2">
                          <div className="flex-1 min-w-0">
                            <p className="text-sm font-medium truncate" style={{ color: 'var(--text-primary)' }}>
                              {r.title}
                            </p>
                            {r.heading && (
                              <p className="text-xs mt-0.5" style={{ color: 'var(--text-secondary)' }}>
                                {r.heading}
                              </p>
                            )}
                            <p className="text-xs mt-1 line-clamp-2" style={{ color: 'var(--text-muted)' }}>
                              {r.body.slice(0, 120)}...
                            </p>
                          </div>
                          <ExternalLink size={14} style={{ color: 'var(--text-muted)', flexShrink: 0 }} />
                        </div>
                      </a>
                    ))}
                  </div>
                )}
              </div>

              {/* Contextual suggestions */}
              {!query.trim() && contextual.length > 0 && (
                <div>
                  <h3 className="text-xs font-semibold uppercase mb-2" style={{ color: 'var(--text-muted)' }}>
                    For this page
                  </h3>
                  <div className="space-y-1">
                    {contextual.map((s) => (
                      <a
                        key={s.url}
                        href={s.url}
                        target="_blank"
                        rel="noopener noreferrer"
                        className="block px-3 py-2 rounded-lg text-sm transition-colors"
                        style={{ color: 'var(--text-primary)' }}
                        onMouseEnter={(e) => (e.currentTarget.style.background = 'var(--bg-surface)')}
                        onMouseLeave={(e) => (e.currentTarget.style.background = 'transparent')}
                      >
                        {s.title}
                      </a>
                    ))}
                  </div>
                </div>
              )}

              {/* Quick links */}
              {!query.trim() && (
                <div>
                  <h3 className="text-xs font-semibold uppercase mb-2" style={{ color: 'var(--text-muted)' }}>
                    Quick Links
                  </h3>
                  <div className="space-y-1">
                    {QUICK_LINKS.map((l) => (
                      <a
                        key={l.url}
                        href={l.url}
                        target="_blank"
                        rel="noopener noreferrer"
                        className="flex items-center gap-2 px-3 py-2 rounded-lg text-sm transition-colors"
                        style={{ color: 'var(--text-primary)' }}
                        onMouseEnter={(e) => (e.currentTarget.style.background = 'var(--bg-surface)')}
                        onMouseLeave={(e) => (e.currentTarget.style.background = 'transparent')}
                      >
                        {l.title}
                        <ExternalLink size={12} style={{ color: 'var(--text-muted)' }} />
                      </a>
                    ))}
                  </div>
                </div>
              )}
            </div>

            {/* Footer — mailto escalation */}
            <div
              className="px-6 py-4"
              style={{
                borderTop: '1px solid var(--border-subtle)',
                background: 'var(--bg-surface)',
              }}
            >
              <p className="text-xs mb-2" style={{ color: 'var(--text-secondary)' }}>
                Can't find what you need?
              </p>
              <a
                href={`mailto:support@pandastack.ai?subject=Support request from ${pathname}&body=Page: ${pathname}%0AWorkspace: (please include your workspace ID)`}
                className="flex items-center gap-2 px-4 py-2.5 rounded-lg text-sm font-medium transition-all"
                style={{
                  background: 'var(--brand)',
                  color: '#fff',
                }}
              >
                <Mail size={16} />
                Contact Support
              </a>
            </div>
          </div>
        </div>
      )}
    </>
  );
}
