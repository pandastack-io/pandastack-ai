// SPDX-License-Identifier: Apache-2.0
//
// github_webhook.go — GitHub App webhook receiver.
//
// GitHub delivers events (push, installation, …) to POST /v1/webhooks/github,
// HMAC-signed with the App-level webhook secret (GITHUB_APP_WEBHOOK_SECRET) in
// the X-Hub-Signature-256 header. We verify the signature in constant time,
// then dispatch on the X-GitHub-Event header:
//
//	push         — auto-deploy every app wired to that repo+branch whose
//	               auto_deploy flag is on.
//	installation — when an installation is "deleted", drop our record of it
//	               (apps fall back to unauthenticated clones / the env
//	               installation thereafter).
//
// The route is auth-skipped via the /v1/webhooks/ prefix (see main.go); the
// HMAC signature is the authentication.
package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
)

const githubWebhookMaxBody = 5 << 20 // 5 MiB — push payloads can be large

// githubWebhookSecret is the App-level secret used to verify deliveries.
func githubWebhookSecret() string { return strings.TrimSpace(os.Getenv("GITHUB_APP_WEBHOOK_SECRET")) }

// verifyGitHubSignature reports whether sigHeader (an "sha256=<hex>" value from
// X-Hub-Signature-256) is a valid HMAC-SHA256 of body under secret. Constant-
// time comparison avoids leaking timing information.
func verifyGitHubSignature(secret string, body []byte, sigHeader string) bool {
	const prefix = "sha256="
	if secret == "" || !strings.HasPrefix(sigHeader, prefix) {
		return false
	}
	want := strings.TrimPrefix(sigHeader, prefix)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	got := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(got), []byte(want))
}

// --- Event payload subsets -------------------------------------------------

type githubPushEvent struct {
	Ref        string `json:"ref"` // refs/heads/<branch>
	After      string `json:"after"`
	Deleted    bool   `json:"deleted"`
	Repository struct {
		ID       int64  `json:"id"`
		FullName string `json:"full_name"`
	} `json:"repository"`
}

type githubInstallationEvent struct {
	Action       string `json:"action"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
}

// githubWebhook is the HTTP entrypoint for all GitHub App deliveries.
func (a *appsAPI) githubWebhook(w http.ResponseWriter, r *http.Request) {
	secret := githubWebhookSecret()
	if secret == "" {
		// Misconfigured: refuse rather than silently accept unverifiable data.
		writeErrOrg(w, http.StatusServiceUnavailable, "github webhook not configured")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, githubWebhookMaxBody))
	if err != nil {
		writeErrOrg(w, http.StatusBadRequest, "could not read body")
		return
	}
	if !verifyGitHubSignature(secret, body, r.Header.Get("X-Hub-Signature-256")) {
		writeErrOrg(w, http.StatusUnauthorized, "invalid signature")
		return
	}

	event := r.Header.Get("X-GitHub-Event")
	switch event {
	case "push":
		a.handleGitHubPush(r.Context(), body)
	case "installation":
		a.handleGitHubInstallation(r.Context(), body)
	default:
		// ping and other events: acknowledge so GitHub marks delivery healthy.
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"ok":true}`))
}

// handleGitHubPush auto-deploys apps wired to the pushed repo + branch.
func (a *appsAPI) handleGitHubPush(ctx context.Context, body []byte) {
	var ev githubPushEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		a.log.Warn("github push: bad payload", "err", err)
		return
	}
	if ev.Deleted {
		return // branch deletion — nothing to deploy
	}
	branch := strings.TrimPrefix(ev.Ref, "refs/heads/")
	if branch == ev.Ref || branch == "" {
		return // tag push or non-branch ref
	}

	// Find apps wired to this repo (by stable repo ID, falling back to
	// full_name) with auto_deploy on and a matching tracked branch.
	rows, err := a.db.QueryContext(ctx, `
		SELECT `+appColumns+`
		FROM apps
		WHERE auto_deploy = true
		  AND git_branch = $1
		  AND (github_repo_id = $2 OR github_repo_full_name = $3)`,
		branch, ev.Repository.ID, ev.Repository.FullName)
	if err != nil {
		a.log.Error("github push: query apps failed", "err", err)
		return
	}
	defer rows.Close()

	var apps []AppInfo
	for rows.Next() {
		app, scanErr := scanAppInfo(rows)
		if scanErr != nil {
			a.log.Error("github push: scan app failed", "err", scanErr)
			continue
		}
		apps = append(apps, app)
	}
	for _, app := range apps {
		ref := branch
		if ev.After != "" {
			ref = ev.After // pin to the exact pushed commit
		}
		if _, derr := a.enqueueDeploy(ctx, app, ref); derr != nil {
			a.log.Error("github push: enqueue deploy failed", "app_id", app.ID, "err", derr)
			continue
		}
		a.log.Info("github push: auto-deploy triggered",
			"app_id", app.ID, "repo", ev.Repository.FullName, "branch", branch, "commit", ev.After)
	}
}

// handleGitHubInstallation reacts to installation lifecycle events. We only act
// on "deleted": drop our record so stale installations don't linger.
func (a *appsAPI) handleGitHubInstallation(ctx context.Context, body []byte) {
	var ev githubInstallationEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		a.log.Warn("github installation: bad payload", "err", err)
		return
	}
	if ev.Action != "deleted" || ev.Installation.ID == 0 {
		return
	}
	if _, err := a.db.ExecContext(ctx,
		`DELETE FROM github_installations WHERE installation_id = $1`, ev.Installation.ID); err != nil {
		a.log.Error("github installation: delete failed", "installation_id", ev.Installation.ID, "err", err)
		return
	}
	a.log.Info("github installation removed", "installation_id", ev.Installation.ID)
}
