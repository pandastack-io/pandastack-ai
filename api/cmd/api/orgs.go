// SPDX-License-Identifier: Apache-2.0
// Orgs / tenancy control plane.
//
// Endpoints (all under /v1/):
//
//	GET    /v1/orgs                          list orgs current user is a member of
//	POST   /v1/orgs                          create new org (current user becomes owner)
//	GET    /v1/orgs/{id}                     org details
//	GET    /v1/orgs/{id}/members             list members
//	POST   /v1/orgs/{id}/members             invite by email
//	DELETE /v1/orgs/{id}/members/{user_id}   remove member
//	POST   /v1/orgs/invites/{token}/accept   accept invite (called by invitee on first login)
//	GET    /v1/me                            current user + orgs + active org
//	POST   /v1/me/current-org                {org_id} switch active org
//
// Auth model: all endpoints require a JWT auth context (rejected for pds_*
// API tokens, since orgs are user-scoped). The unifiedAuth middleware sets
// X-Pandastack-User-Id; that header drives membership lookups.
package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

type orgsAPI struct {
	db  *sql.DB
	log *slog.Logger
}

func newOrgsAPI(db *sql.DB, log *slog.Logger) *orgsAPI {
	return &orgsAPI{db: db, log: log}
}

// SetupSchema ensures the orgs / multi-tenancy tables exist. Idempotent.
func (a *orgsAPI) SetupSchema(ctx context.Context) error {
	if a.db == nil {
		return errors.New("orgs: nil db")
	}
	if _, err := a.db.ExecContext(ctx, orgsSchema); err != nil {
		return fmt.Errorf("orgs schema: %w", err)
	}
	return nil
}

// Org / multi-tenancy DDL, inlined so the api binary can ensure the schema
// exists at startup (idempotent CREATEs).
const orgsSchema = `
CREATE TABLE IF NOT EXISTS orgs (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    slug                  TEXT NOT NULL UNIQUE,
    name                  TEXT NOT NULL,
    owner_user_id         TEXT NOT NULL,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS orgs_owner_idx ON orgs (owner_user_id);

CREATE TABLE IF NOT EXISTS org_members (
    org_id     UUID NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    user_id    TEXT NOT NULL,
    email      TEXT NOT NULL DEFAULT '',
    role       TEXT NOT NULL CHECK (role IN ('owner','admin','member')),
    joined_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, user_id)
);
CREATE INDEX IF NOT EXISTS org_members_user_idx ON org_members (user_id);

CREATE TABLE IF NOT EXISTS org_invites (
    token        TEXT PRIMARY KEY,
    org_id       UUID NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    email        TEXT NOT NULL,
    role         TEXT NOT NULL CHECK (role IN ('admin','member')),
    invited_by   TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ NOT NULL,
    accepted_at  TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS org_invites_email_idx ON org_invites (LOWER(email));
CREATE INDEX IF NOT EXISTS org_invites_org_idx   ON org_invites (org_id);

CREATE TABLE IF NOT EXISTS user_current_org (
    user_id    TEXT PRIMARY KEY,
    org_id     UUID NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
`

// --- types -----------------------------------------------------------------

type Org struct {
	ID          string    `json:"id"`
	Slug        string    `json:"slug"`
	Name        string    `json:"name"`
	OwnerUserID string    `json:"owner_user_id"`
	Role        string    `json:"role,omitempty"` // populated when listing my orgs
	CreatedAt   time.Time `json:"created_at"`
}

type OrgMember struct {
	UserID   string    `json:"user_id"`
	Email    string    `json:"email"`
	Role     string    `json:"role"`
	JoinedAt time.Time `json:"joined_at"`
}

type OrgInvite struct {
	Token     string    `json:"token"`
	OrgID     string    `json:"org_id"`
	Email     string    `json:"email"`
	Role      string    `json:"role"`
	InvitedBy string    `json:"invited_by"`
	ExpiresAt time.Time `json:"expires_at"`
	InviteURL string    `json:"invite_url"`
}

// --- helpers ---------------------------------------------------------------

// userIDFromReq returns the JWT-authenticated user id. Rejected (empty) for
// token-auth callers — those should use admin endpoints, not org management.
func userIDFromReq(r *http.Request) string {
	method := r.Header.Get("X-Pandastack-Auth-Method")
	if method != "jwt" && method != "stub" {
		return ""
	}
	return strings.TrimSpace(r.Header.Get("X-Pandastack-User-Id"))
}

func writeJSONOrg(w http.ResponseWriter, code int, v any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErrOrg(w http.ResponseWriter, code int, msg string) {
	writeJSONOrg(w, code, map[string]string{"error": msg})
}

// slugFromName makes a URL-safe org slug from a display name. Keeps a-z/0-9/-
// only, collapses runs of '-'. If empty after sanitizing, falls back to a
// short random suffix.
func slugFromName(name string) string {
	var b strings.Builder
	last := byte('-')
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'A' && c <= 'Z':
			c = c - 'A' + 'a'
			fallthrough
		case (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9'):
			b.WriteByte(c)
			last = c
		case c == ' ' || c == '_' || c == '-':
			if last != '-' {
				b.WriteByte('-')
				last = '-'
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		var buf [4]byte
		_, _ = rand.Read(buf[:])
		out = "org-" + hex.EncodeToString(buf[:])
	}
	return out
}

func randomToken(n int) string {
	buf := make([]byte, n)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

// --- route registration ----------------------------------------------------

func (a *orgsAPI) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/orgs", a.listOrgs)
	mux.HandleFunc("POST /v1/orgs", a.createOrg)
	mux.HandleFunc("GET /v1/orgs/{id}", a.getOrg)
	mux.HandleFunc("GET /v1/orgs/{id}/members", a.listMembers)
	mux.HandleFunc("POST /v1/orgs/{id}/members", a.inviteMember)
	mux.HandleFunc("DELETE /v1/orgs/{id}/members/{user_id}", a.removeMember)
	mux.HandleFunc("POST /v1/orgs/invites/{token}/accept", a.acceptInvite)
	mux.HandleFunc("GET /v1/me", a.getMe)
	mux.HandleFunc("POST /v1/me/current-org", a.setCurrentOrg)
}

func registerOrgsUnavailableRoutes(mux *http.ServeMux) {
	unavailable := func(w http.ResponseWriter, _ *http.Request) {
		writeErrOrg(w, http.StatusServiceUnavailable, "org control plane unavailable")
	}
	mux.HandleFunc("GET /v1/orgs", unavailable)
	mux.HandleFunc("POST /v1/orgs", unavailable)
	mux.HandleFunc("GET /v1/orgs/{id}", unavailable)
	mux.HandleFunc("GET /v1/orgs/{id}/members", unavailable)
	mux.HandleFunc("POST /v1/orgs/{id}/members", unavailable)
	mux.HandleFunc("DELETE /v1/orgs/{id}/members/{user_id}", unavailable)
	mux.HandleFunc("POST /v1/orgs/invites/{token}/accept", unavailable)
	mux.HandleFunc("GET /v1/me", func(w http.ResponseWriter, r *http.Request) {
		writeJSONOrg(w, http.StatusOK, map[string]any{
			"user_id":     strings.TrimSpace(r.Header.Get("X-Pandastack-User-Id")),
			"email":       strings.TrimSpace(r.Header.Get("X-Pandastack-User-Email")),
			"current_org": nil,
			"orgs":        []Org{},
			"auth_method": r.Header.Get("X-Pandastack-Auth-Method"),
		})
	})
	mux.HandleFunc("POST /v1/me/current-org", unavailable)
}

// --- handlers --------------------------------------------------------------

func (a *orgsAPI) listOrgs(w http.ResponseWriter, r *http.Request) {
	uid := userIDFromReq(r)
	if uid == "" {
		writeErrOrg(w, 401, "jwt auth required for org endpoints")
		return
	}
	rows, err := a.db.QueryContext(r.Context(), `
		SELECT o.id, o.slug, o.name, o.owner_user_id,
		       o.created_at, m.role
		FROM orgs o
		JOIN org_members m ON m.org_id = o.id AND m.user_id = $1
		ORDER BY o.created_at ASC`, uid)
	if err != nil {
		writeErrOrg(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []Org{}
	for rows.Next() {
		var o Org
		if err := rows.Scan(&o.ID, &o.Slug, &o.Name, &o.OwnerUserID,
			&o.CreatedAt, &o.Role); err != nil {
			writeErrOrg(w, 500, err.Error())
			return
		}
		out = append(out, o)
	}
	writeJSONOrg(w, 200, out)
}

func (a *orgsAPI) createOrg(w http.ResponseWriter, r *http.Request) {
	uid := userIDFromReq(r)
	if uid == "" {
		writeErrOrg(w, 401, "jwt auth required")
		return
	}
	var body struct {
		Name string `json:"name"`
		Slug string `json:"slug,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErrOrg(w, 400, "bad json")
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if body.Name == "" {
		writeErrOrg(w, 400, "name required")
		return
	}
	slug := strings.TrimSpace(body.Slug)
	if slug == "" {
		slug = slugFromName(body.Name)
	}
	// Pull email from auth header if available — useful for org_members row.
	email := strings.TrimSpace(r.Header.Get("X-Pandastack-User-Email"))

	// Insert org + owner membership in a transaction.
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeErrOrg(w, 500, err.Error())
		return
	}
	defer func() { _ = tx.Rollback() }()

	var orgID string
	err = tx.QueryRowContext(r.Context(),
		`INSERT INTO orgs (slug, name, owner_user_id) VALUES ($1,$2,$3) RETURNING id`,
		slug, body.Name, uid).Scan(&orgID)
	if err != nil {
		if isUniqueViolation(err) {
			writeErrOrg(w, 409, "slug already taken")
			return
		}
		writeErrOrg(w, 500, err.Error())
		return
	}
	_, err = tx.ExecContext(r.Context(),
		`INSERT INTO org_members (org_id, user_id, email, role) VALUES ($1,$2,$3,'owner')`,
		orgID, uid, email)
	if err != nil {
		writeErrOrg(w, 500, err.Error())
		return
	}
	// If this is the user's first org, make it the current one.
	_, _ = tx.ExecContext(r.Context(),
		`INSERT INTO user_current_org (user_id, org_id) VALUES ($1,$2)
		 ON CONFLICT (user_id) DO NOTHING`,
		uid, orgID)
	if err := tx.Commit(); err != nil {
		writeErrOrg(w, 500, err.Error())
		return
	}
	writeJSONOrg(w, 201, Org{
		ID: orgID, Slug: slug, Name: body.Name,
		OwnerUserID: uid, Role: "owner", CreatedAt: time.Now().UTC(),
	})
}

func (a *orgsAPI) getOrg(w http.ResponseWriter, r *http.Request) {
	uid := userIDFromReq(r)
	if uid == "" {
		writeErrOrg(w, 401, "jwt auth required")
		return
	}
	id := r.PathValue("id")
	var o Org
	var role string
	err := a.db.QueryRowContext(r.Context(), `
		SELECT o.id, o.slug, o.name, o.owner_user_id,
		       o.created_at, m.role
		FROM orgs o
		JOIN org_members m ON m.org_id = o.id AND m.user_id = $1
		WHERE o.id = $2`, uid, id).
		Scan(&o.ID, &o.Slug, &o.Name, &o.OwnerUserID,
			&o.CreatedAt, &role)
	if errors.Is(err, sql.ErrNoRows) {
		writeErrOrg(w, 404, "not found or not a member")
		return
	}
	if err != nil {
		writeErrOrg(w, 500, err.Error())
		return
	}
	o.Role = role
	writeJSONOrg(w, 200, o)
}

// requireRole returns ("", true) if the user is allowed (role >= min);
// otherwise writes an error response and returns (_, false). The caller
// should `return` immediately on false.
func (a *orgsAPI) requireRole(w http.ResponseWriter, r *http.Request, orgID, minRole string) (string, bool) {
	uid := userIDFromReq(r)
	if uid == "" {
		writeErrOrg(w, 401, "jwt auth required")
		return "", false
	}
	var role string
	err := a.db.QueryRowContext(r.Context(),
		`SELECT role FROM org_members WHERE org_id = $1 AND user_id = $2`, orgID, uid).
		Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		writeErrOrg(w, 404, "not a member")
		return "", false
	}
	if err != nil {
		writeErrOrg(w, 500, err.Error())
		return "", false
	}
	if !roleAllows(role, minRole) {
		writeErrOrg(w, 403, "insufficient role: need "+minRole+", have "+role)
		return "", false
	}
	return uid, true
}

// roleAllows: owner > admin > member.
func roleAllows(have, min string) bool {
	rank := map[string]int{"owner": 3, "admin": 2, "member": 1}
	return rank[have] >= rank[min]
}

func (a *orgsAPI) listMembers(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("id")
	if _, ok := a.requireRole(w, r, orgID, "member"); !ok {
		return
	}
	rows, err := a.db.QueryContext(r.Context(),
		`SELECT user_id, email, role, joined_at FROM org_members WHERE org_id = $1 ORDER BY joined_at ASC`, orgID)
	if err != nil {
		writeErrOrg(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []OrgMember{}
	for rows.Next() {
		var m OrgMember
		if err := rows.Scan(&m.UserID, &m.Email, &m.Role, &m.JoinedAt); err != nil {
			writeErrOrg(w, 500, err.Error())
			return
		}
		out = append(out, m)
	}
	writeJSONOrg(w, 200, out)
}

func (a *orgsAPI) inviteMember(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("id")
	uid, ok := a.requireRole(w, r, orgID, "admin")
	if !ok {
		return
	}
	var body struct {
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErrOrg(w, 400, "bad json")
		return
	}
	email := strings.ToLower(strings.TrimSpace(body.Email))
	if email == "" {
		writeErrOrg(w, 400, "email required")
		return
	}
	if body.Role == "" {
		body.Role = "member"
	}
	if body.Role != "member" && body.Role != "admin" {
		writeErrOrg(w, 400, "role must be member or admin")
		return
	}

	token := randomToken(24)
	expires := time.Now().Add(7 * 24 * time.Hour)
	_, err := a.db.ExecContext(r.Context(),
		`INSERT INTO org_invites (token, org_id, email, role, invited_by, expires_at)
		 VALUES ($1,$2,$3,$4,$5,$6)`,
		token, orgID, email, body.Role, uid, expires)
	if err != nil {
		writeErrOrg(w, 500, err.Error())
		return
	}
	base := strings.TrimRight(getenv("PANDASTACK_DASHBOARD_URL"), "/")
	if base == "" {
		base = "https://app.pandastack.ai"
	}
	writeJSONOrg(w, 201, OrgInvite{
		Token:     token,
		OrgID:     orgID,
		Email:     email,
		Role:      body.Role,
		InvitedBy: uid,
		ExpiresAt: expires,
		InviteURL: base + "/invite/" + token,
	})
}

func (a *orgsAPI) removeMember(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("id")
	uid, ok := a.requireRole(w, r, orgID, "admin")
	if !ok {
		return
	}
	target := r.PathValue("user_id")
	if target == "" {
		writeErrOrg(w, 400, "user_id required")
		return
	}
	// Don't allow removing the org owner.
	var ownerID string
	if err := a.db.QueryRowContext(r.Context(),
		`SELECT owner_user_id FROM orgs WHERE id = $1`, orgID).Scan(&ownerID); err == nil {
		if target == ownerID {
			writeErrOrg(w, 400, "cannot remove org owner")
			return
		}
	}
	// Admins can't remove other admins or owners. Owners can remove anyone except themselves.
	var targetRole string
	_ = a.db.QueryRowContext(r.Context(),
		`SELECT role FROM org_members WHERE org_id = $1 AND user_id = $2`, orgID, target).Scan(&targetRole)
	var actorRole string
	_ = a.db.QueryRowContext(r.Context(),
		`SELECT role FROM org_members WHERE org_id = $1 AND user_id = $2`, orgID, uid).Scan(&actorRole)
	if actorRole == "admin" && (targetRole == "admin" || targetRole == "owner") {
		writeErrOrg(w, 403, "admins cannot remove admins/owners")
		return
	}
	_, err := a.db.ExecContext(r.Context(),
		`DELETE FROM org_members WHERE org_id = $1 AND user_id = $2`, orgID, target)
	if err != nil {
		writeErrOrg(w, 500, err.Error())
		return
	}
	w.WriteHeader(204)
}

func (a *orgsAPI) acceptInvite(w http.ResponseWriter, r *http.Request) {
	uid := userIDFromReq(r)
	if uid == "" {
		writeErrOrg(w, 401, "jwt auth required")
		return
	}
	token := r.PathValue("token")
	var (
		orgID, email, role string
		expires            time.Time
		accepted           sql.NullTime
	)
	err := a.db.QueryRowContext(r.Context(),
		`SELECT org_id, email, role, expires_at, accepted_at FROM org_invites WHERE token = $1`, token).
		Scan(&orgID, &email, &role, &expires, &accepted)
	if errors.Is(err, sql.ErrNoRows) {
		writeErrOrg(w, 404, "invite not found")
		return
	}
	if err != nil {
		writeErrOrg(w, 500, err.Error())
		return
	}
	if accepted.Valid {
		writeErrOrg(w, 410, "invite already used")
		return
	}
	if time.Now().After(expires) {
		writeErrOrg(w, 410, "invite expired")
		return
	}
	callerEmail := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Pandastack-User-Email")))
	if callerEmail != "" && callerEmail != email {
		writeErrOrg(w, 403, "invite was for a different email")
		return
	}

	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeErrOrg(w, 500, err.Error())
		return
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(r.Context(),
		`INSERT INTO org_members (org_id, user_id, email, role) VALUES ($1,$2,$3,$4)
		 ON CONFLICT (org_id, user_id) DO UPDATE SET role = EXCLUDED.role, email = EXCLUDED.email`,
		orgID, uid, email, role)
	if err != nil {
		writeErrOrg(w, 500, err.Error())
		return
	}
	_, _ = tx.ExecContext(r.Context(),
		`UPDATE org_invites SET accepted_at = now() WHERE token = $1`, token)
	if err := tx.Commit(); err != nil {
		writeErrOrg(w, 500, err.Error())
		return
	}
	writeJSONOrg(w, 200, map[string]any{"org_id": orgID, "role": role})
}

// --- /v1/me + current-org switching ---------------------------------------

func (a *orgsAPI) getMe(w http.ResponseWriter, r *http.Request) {
	uid := userIDFromReq(r)
	if uid == "" {
		// Token-auth: return a minimal user identity sourced from header.
		writeJSONOrg(w, 200, map[string]any{
			"user_id":     strings.TrimSpace(r.Header.Get("X-Pandastack-User-Id")),
			"email":       "",
			"current_org": nil,
			"orgs":        []Org{},
			"auth_method": r.Header.Get("X-Pandastack-Auth-Method"),
		})
		return
	}
	email := strings.TrimSpace(r.Header.Get("X-Pandastack-User-Email"))

	rows, err := a.db.QueryContext(r.Context(), `
		SELECT o.id, o.slug, o.name, o.owner_user_id,
		       o.created_at, m.role
		FROM orgs o JOIN org_members m ON m.org_id = o.id
		WHERE m.user_id = $1
		ORDER BY o.created_at ASC`, uid)
	if err != nil {
		writeErrOrg(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []Org{}
	for rows.Next() {
		var o Org
		if err := rows.Scan(&o.ID, &o.Slug, &o.Name, &o.OwnerUserID,
			&o.CreatedAt, &o.Role); err != nil {
			writeErrOrg(w, 500, err.Error())
			return
		}
		out = append(out, o)
	}

	// Resolve / auto-create current org. If user has none, create a personal
	// one named after their email-local part so first-login works seamlessly.
	if len(out) == 0 {
		name := "Personal"
		if at := strings.Index(email, "@"); at > 0 {
			name = email[:at]
		}
		// Use a deterministic slug derived from user id so re-runs don't
		// keep creating new orgs if the insert below races on retries.
		slug := slugFromName(name) + "-" + uid[:8]
		var orgID string
		err = a.db.QueryRowContext(r.Context(),
			`INSERT INTO orgs (slug, name, owner_user_id) VALUES ($1,$2,$3)
			 ON CONFLICT (slug) DO UPDATE SET name = EXCLUDED.name
			 RETURNING id`,
			slug, name, uid).Scan(&orgID)
		if err == nil {
			_, _ = a.db.ExecContext(r.Context(),
				`INSERT INTO org_members (org_id, user_id, email, role) VALUES ($1,$2,$3,'owner')
				 ON CONFLICT DO NOTHING`,
				orgID, uid, email)
			_, _ = a.db.ExecContext(r.Context(),
				`INSERT INTO user_current_org (user_id, org_id) VALUES ($1,$2)
				 ON CONFLICT (user_id) DO NOTHING`,
				uid, orgID)
			out = append(out, Org{
				ID: orgID, Slug: slug, Name: name,
				OwnerUserID: uid, Role: "owner", CreatedAt: time.Now().UTC(),
			})
		}
	}

	// Pick current org: explicit row in user_current_org, else first.
	var currentOrgID string
	_ = a.db.QueryRowContext(r.Context(),
		`SELECT org_id FROM user_current_org WHERE user_id = $1`, uid).Scan(&currentOrgID)
	if currentOrgID == "" && len(out) > 0 {
		currentOrgID = out[0].ID
		_, _ = a.db.ExecContext(r.Context(),
			`INSERT INTO user_current_org (user_id, org_id) VALUES ($1,$2)
			 ON CONFLICT (user_id) DO NOTHING`,
			uid, currentOrgID)
	}
	var current *Org
	for i := range out {
		if out[i].ID == currentOrgID {
			current = &out[i]
			break
		}
	}
	authMethod := r.Header.Get("X-Pandastack-Auth-Method")
	if authMethod == "" {
		authMethod = "jwt"
	}
	writeJSONOrg(w, 200, map[string]any{
		"user_id":     uid,
		"email":       email,
		"current_org": current,
		"orgs":        out,
		"auth_method": authMethod,
	})
}

func (a *orgsAPI) setCurrentOrg(w http.ResponseWriter, r *http.Request) {
	uid := userIDFromReq(r)
	if uid == "" {
		writeErrOrg(w, 401, "jwt auth required")
		return
	}
	var body struct {
		OrgID string `json:"org_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErrOrg(w, 400, "bad json")
		return
	}
	// Verify user is a member.
	var n int
	_ = a.db.QueryRowContext(r.Context(),
		`SELECT COUNT(*) FROM org_members WHERE org_id = $1 AND user_id = $2`, body.OrgID, uid).Scan(&n)
	if n == 0 {
		writeErrOrg(w, 403, "not a member of that org")
		return
	}
	_, err := a.db.ExecContext(r.Context(),
		`INSERT INTO user_current_org (user_id, org_id, updated_at) VALUES ($1,$2,now())
		 ON CONFLICT (user_id) DO UPDATE SET org_id = EXCLUDED.org_id, updated_at = now()`,
		uid, body.OrgID)
	if err != nil {
		writeErrOrg(w, 500, err.Error())
		return
	}
	writeJSONOrg(w, 200, map[string]string{"org_id": body.OrgID})
}

func nullTimeJSON(t sql.NullTime) any {
	if !t.Valid {
		return nil
	}
	return t.Time
}

// --- helpers ---------------------------------------------------------------

// isUniqueViolation matches the pgx unique_violation SQLSTATE.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "23505") || strings.Contains(s, "duplicate key")
}
