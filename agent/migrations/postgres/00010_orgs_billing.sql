-- +goose Up
-- 00010_orgs_billing.sql
--
-- Organizations / tenancy tables. Lives in the SHARED Postgres (same DB the
-- control-plane api + multi-node agents use). Orgs are the top-level tenancy
-- unit; users belong to one or more orgs via org_members. Workspaces become
-- bound to an org via metadata['workspace'] := org.slug.
--
-- These tables are also created idempotently by the control-plane api at
-- startup (orgs.SetupSchema); the migration keeps a fresh agent-side DB valid.

CREATE TABLE IF NOT EXISTS orgs (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    slug                  TEXT NOT NULL UNIQUE,           -- becomes X-Fcs-Workspace
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

-- Track which org a user's session currently has "active".
CREATE TABLE IF NOT EXISTS user_current_org (
    user_id    TEXT PRIMARY KEY,
    org_id     UUID NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE IF EXISTS user_current_org;
DROP TABLE IF EXISTS org_invites;
DROP TABLE IF EXISTS org_members;
DROP TABLE IF EXISTS orgs;
