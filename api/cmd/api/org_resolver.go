// SPDX-License-Identifier: Apache-2.0
// Org resolver middleware.
//
// Runs AFTER unifiedAuth. For JWT-authenticated requests it looks up the
// caller's "current org" (table user_current_org, set via POST /v1/me/current-org
// or auto-initialized on first /v1/me hit) and rewrites X-Fcs-Workspace to
// the org slug. This way the per-agent workspace-isolation logic (which keys
// off X-Fcs-Workspace) becomes per-org isolation without any per-agent
// changes.
//
// Behavior:
//   - skipPrefixes (preview / healthz / metrics): pass through untouched.
//   - pds_ token auth: pass through — the token already encodes a workspace.
//   - JWT auth + explicit X-Pandastack-Org header: use that org (if user is a
//     member); else 403. Lets dashboards request a non-current org.
//   - JWT auth, no explicit org: use user_current_org.org_id; if absent,
//     fall through with X-Fcs-Workspace = user_id (back-compat for users
//     whose first /v1/me hasn't run yet).
//
// All lookups are cached in-process for 30s to keep the hot path cheap.
package main

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

type orgResolver struct {
	db    *sql.DB
	log   *slog.Logger
	cache sync.Map // key: "user:"+uid or "memcheck:"+uid+"|"+orgID -> cacheEntry
	ttl   time.Duration
}

type cacheEntry struct {
	val string
	exp time.Time
	ok  bool
}

func newOrgResolver(db *sql.DB, log *slog.Logger) *orgResolver {
	return &orgResolver{db: db, log: log, ttl: 30 * time.Second}
}

func (o *orgResolver) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if o == nil || o.db == nil {
			next.ServeHTTP(w, r)
			return
		}
		method := r.Header.Get("X-Pandastack-Auth-Method")
		if method != "jwt" && method != "stub" {
			next.ServeHTTP(w, r)
			return
		}
		// Don't rewrite for endpoints that are user-scoped (not org-scoped).
		// /v1/me, /v1/orgs are user-scoped — the handler in orgs.go uses
		// X-Pandastack-User-Id directly, ignoring X-Fcs-Workspace.
		uid := r.Header.Get("X-Pandastack-User-Id")
		if uid == "" {
			next.ServeHTTP(w, r)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		// Explicit X-Pandastack-Org header overrides current org.
		if orgID := strings.TrimSpace(r.Header.Get("X-Pandastack-Org")); orgID != "" {
			slug, ok := o.slugForMember(ctx, uid, orgID)
			if !ok {
				http.Error(w, `{"error":"not a member of requested org"}`, http.StatusForbidden)
				return
			}
			r.Header.Set("X-Fcs-Workspace", slug)
			r.Header.Set("X-Pandastack-Org-Id", orgID)
			next.ServeHTTP(w, r)
			return
		}
		// Otherwise look up current org.
		slug, orgID, ok := o.currentSlug(ctx, uid)
		if ok {
			r.Header.Set("X-Fcs-Workspace", slug)
			r.Header.Set("X-Pandastack-Org-Id", orgID)
		}
		next.ServeHTTP(w, r)
	})
}

// currentSlug returns (slug, orgID, ok) for the user's current org.
// Cached for ttl.
func (o *orgResolver) currentSlug(ctx context.Context, uid string) (string, string, bool) {
	key := "user:" + uid
	if v, found := o.cache.Load(key); found {
		ce := v.(cacheEntry)
		if time.Now().Before(ce.exp) {
			if !ce.ok {
				return "", "", false
			}
			parts := strings.SplitN(ce.val, "|", 2)
			if len(parts) == 2 {
				return parts[0], parts[1], true
			}
		}
	}
	var slug, orgID string
	err := o.db.QueryRowContext(ctx, `
		SELECT o.slug, o.id FROM orgs o
		JOIN user_current_org uco ON uco.org_id = o.id
		WHERE uco.user_id = $1`, uid).Scan(&slug, &orgID)
	if errors.Is(err, sql.ErrNoRows) {
		// No current org — fall back to oldest org membership.
		err = o.db.QueryRowContext(ctx, `
			SELECT o.slug, o.id FROM orgs o
			JOIN org_members m ON m.org_id = o.id
			WHERE m.user_id = $1
			ORDER BY o.created_at ASC LIMIT 1`, uid).Scan(&slug, &orgID)
	}
	if err != nil {
		o.cache.Store(key, cacheEntry{exp: time.Now().Add(o.ttl), ok: false})
		return "", "", false
	}
	o.cache.Store(key, cacheEntry{val: slug + "|" + orgID, exp: time.Now().Add(o.ttl), ok: true})
	return slug, orgID, true
}

// slugForMember returns (slug, ok=true) if uid is a member of orgID.
func (o *orgResolver) slugForMember(ctx context.Context, uid, orgID string) (string, bool) {
	key := "memcheck:" + uid + "|" + orgID
	if v, found := o.cache.Load(key); found {
		ce := v.(cacheEntry)
		if time.Now().Before(ce.exp) {
			return ce.val, ce.ok
		}
	}
	var slug string
	err := o.db.QueryRowContext(ctx, `
		SELECT o.slug FROM orgs o
		JOIN org_members m ON m.org_id = o.id AND m.user_id = $1
		WHERE o.id = $2`, uid, orgID).Scan(&slug)
	if err != nil {
		o.cache.Store(key, cacheEntry{exp: time.Now().Add(o.ttl), ok: false})
		return "", false
	}
	o.cache.Store(key, cacheEntry{val: slug, exp: time.Now().Add(o.ttl), ok: true})
	return slug, true
}

// InvalidateUser drops cached entries for a user (used after they switch
// current org or accept an invite).
func (o *orgResolver) InvalidateUser(uid string) {
	if o == nil {
		return
	}
	o.cache.Delete("user:" + uid)
	// memcheck entries expire on their own; not worth scanning.
}
