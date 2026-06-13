// SPDX-License-Identifier: Apache-2.0
//
// github_oauth.go — GitHub App "Connect" flow (App install + user OAuth).
//
// Production GitHub integration. A workspace owner connects their GitHub
// account/org by installing the PandaStack GitHub App; the same redirect also
// carries an OAuth `code` (when "Request user authorization (OAuth) during
// installation" is enabled on the App) which we use solely to identify the
// connecting user. We DO NOT persist any long-lived GitHub tokens: repo
// listing and cloning mint short-lived installation tokens on demand from the
// App private key (see apps_github.go). We persist only the durable
// installation identifiers + display metadata in github_installations.
//
// Routes (registered in Register, below):
//
//	GET  /v1/github/connect            — authed: returns {url} to send the
//	                                     browser to GitHub's install screen
//	GET  /v1/github/callback           — UNAUTHED (browser redirect): records
//	                                     the installation, redirects to the
//	                                     dashboard
//	GET  /v1/github/installations      — authed: list connected installations
//	GET  /v1/github/repos              — authed: list repos for an installation
//	DELETE /v1/github/installations/{id} — authed: disconnect an installation
//
// Config:
//
//	GITHUB_APP_ID, GITHUB_APP_PRIVATE_KEY — required (mint tokens / App JWT)
//	GITHUB_APP_SLUG                       — App slug for the install URL
//	GITHUB_APP_CLIENT_ID, GITHUB_APP_CLIENT_SECRET — optional, enables the
//	                                     OAuth code exchange to identify the
//	                                     connecting user (connected_by)
//	PANDASTACK_DASHBOARD_URL              — where the callback redirects back
//	                                     (default http://localhost:3000)
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/bradleyfalzon/ghinstallation/v2"
)

const githubAPIBase = "https://api.github.com"

// ---------------------------------------------------------------------------
// Config helpers
// ---------------------------------------------------------------------------

func githubAppSlug() string      { return getenv("GITHUB_APP_SLUG") }
func githubClientID() string     { return getenv("GITHUB_APP_CLIENT_ID") }
func githubClientSecret() string { return getenv("GITHUB_APP_CLIENT_SECRET") }

// dashboardURL is where the GitHub callback redirects the browser back to.
func dashboardURL() string {
	if u := getenv("PANDASTACK_DASHBOARD_URL"); u != "" {
		return strings.TrimRight(u, "/")
	}
	return "http://localhost:3000"
}

// githubConnectURL builds the GitHub App installation URL carrying our state.
// After install GitHub redirects to the App's configured Callback URL with
// installation_id + setup_action + state (+ code when OAuth-during-install is
// enabled).
func githubConnectURL(state string) string {
	slug := githubAppSlug()
	return "https://github.com/apps/" + url.PathEscape(slug) +
		"/installations/new?state=" + url.QueryEscape(state)
}

// ---------------------------------------------------------------------------
// GitHub API plumbing
// ---------------------------------------------------------------------------

// githubAppClient returns an http.Client whose transport attaches a freshly
// signed App JWT to every request (for /app/* endpoints).
func (a *appsAPI) githubAppClient() (*http.Client, error) {
	appID, err := githubAppID()
	if err != nil {
		return nil, fmt.Errorf("GITHUB_APP_ID: %w", err)
	}
	tr, err := ghinstallation.NewAppsTransport(http.DefaultTransport, appID, githubAppPrivateKey())
	if err != nil {
		return nil, fmt.Errorf("apps transport: %w", err)
	}
	return &http.Client{Transport: tr, Timeout: 15 * time.Second}, nil
}

// githubAPIGet issues an authenticated GET to the GitHub REST API and decodes
// the JSON body into out. token is attached as a Bearer credential when set
// (use "" together with an App-JWT client). Returns the HTTP status code.
func githubAPIGet(ctx context.Context, client *http.Client, token, path string, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, githubAPIBase+path, nil)
	if err != nil {
		return 0, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return resp.StatusCode, fmt.Errorf("github GET %s: status %d", path, resp.StatusCode)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return resp.StatusCode, err
		}
	}
	return resp.StatusCode, nil
}

// githubInstallationMeta is the subset of GET /app/installations/{id} we keep.
type githubInstallationMeta struct {
	ID      int64 `json:"id"`
	Account struct {
		Login string `json:"login"`
		ID    int64  `json:"id"`
		Type  string `json:"type"`
	} `json:"account"`
}

// githubRepo is the API representation returned to the dashboard repo picker.
type githubRepo struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	FullName      string `json:"full_name"`
	Private       bool   `json:"private"`
	DefaultBranch string `json:"default_branch"`
	CloneURL      string `json:"clone_url"`
	HTMLURL       string `json:"html_url"`
}

// ---------------------------------------------------------------------------
// Routing
// ---------------------------------------------------------------------------

// RegisterGitHub wires the GitHub connect/webhook routes onto the shared mux.
// Called from Register so apps + github share one appsAPI.
func (a *appsAPI) RegisterGitHub(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/github/connect", a.githubConnect)
	mux.HandleFunc("GET /v1/github/callback", a.githubCallback)
	mux.HandleFunc("GET /v1/github/installations", a.githubListInstallations)
	mux.HandleFunc("DELETE /v1/github/installations/{id}", a.githubDeleteInstallation)
	mux.HandleFunc("GET /v1/github/repos", a.githubListRepos)

	// Webhook receiver (HMAC-verified, auth-skipped via /v1/webhooks/ prefix).
	mux.HandleFunc("POST /v1/webhooks/github", a.githubWebhook)
}

// ---------------------------------------------------------------------------
// Connect / Callback
// ---------------------------------------------------------------------------

func randomState() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// githubConnect (authed) mints a CSRF state bound to the caller's workspace and
// returns the GitHub install URL for the browser to navigate to.
func (a *appsAPI) githubConnect(w http.ResponseWriter, r *http.Request) {
	workspace, userID, ok := functionScope(r)
	if !ok {
		writeErrOrg(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !githubAppCredsConfigured() || githubAppSlug() == "" {
		writeErrOrg(w, http.StatusServiceUnavailable, "github app not configured")
		return
	}
	state, err := randomState()
	if err != nil {
		writeErrOrg(w, http.StatusInternalServerError, "internal server error")
		return
	}
	if _, err := a.db.ExecContext(r.Context(),
		`INSERT INTO github_oauth_states (state, workspace, user_id) VALUES ($1,$2,$3)`,
		state, workspace, userID); err != nil {
		a.log.Error("github connect: store state failed", "err", err)
		writeErrOrg(w, http.StatusInternalServerError, "internal server error")
		return
	}
	writeJSONOrg(w, http.StatusOK, map[string]string{"url": githubConnectURL(state)})
}

// githubCallback (UNAUTHED browser redirect) records a freshly installed
// installation against the workspace that started the flow, then redirects the
// browser back to the dashboard. State validation binds the installation to
// the correct workspace and prevents CSRF.
//
// GitHub sends two distinct redirect shapes here:
//   - Fresh install (Callback URL): ?code=…&state=…&installation_id=…&setup_action=install
//     → state is consumed and binds the installation to the initiating workspace.
//   - Repo-selection update (Setup URL with "Redirect on update" enabled):
//     ?installation_id=…&setup_action=update — NO state, NO code. We must NOT
//     reject this: the user just edited which repos the app can see and
//     clicked Save. It is safe IFF the installation is ALREADY bound to a
//     workspace — we only refresh metadata and bounce back to the dashboard.
//     A state-less redirect for an UNKNOWN installation is still rejected
//     (that is the CSRF case: it could bind someone else's install).
func (a *appsAPI) githubCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	state := strings.TrimSpace(q.Get("state"))
	instIDStr := strings.TrimSpace(q.Get("installation_id"))
	dash := dashboardURL()

	instID, instErr := strconv.ParseInt(instIDStr, 10, 64)
	if instErr != nil || instID == 0 {
		http.Redirect(w, r, dash+"/apps?github=error&reason=missing_installation", http.StatusFound)
		return
	}

	// Consume the state (single-use, 10-minute TTL) when present.
	var workspace, userID string
	stateOK := false
	if state != "" {
		err := a.db.QueryRowContext(r.Context(),
			`DELETE FROM github_oauth_states
			 WHERE state = $1 AND created_at > now() - interval '10 minutes'
			 RETURNING workspace, user_id`, state).Scan(&workspace, &userID)
		stateOK = err == nil
	}

	if !stateOK {
		// State-less (or expired-state) redirect: only acceptable as a
		// repo-selection update of an installation we already know. Refresh
		// metadata best-effort and send the user back to the dashboard so
		// the repo picker reflects the new selection.
		var known bool
		if err := a.db.QueryRowContext(r.Context(),
			`SELECT EXISTS(SELECT 1 FROM github_installations WHERE installation_id = $1)`,
			instID).Scan(&known); err != nil || !known {
			reason := "invalid_state"
			if state == "" {
				reason = "missing_state"
			}
			http.Redirect(w, r, dash+"/apps?github=error&reason="+reason, http.StatusFound)
			return
		}
		if meta, err := a.githubFetchInstallation(r.Context(), instID); err == nil {
			if _, uerr := a.db.ExecContext(r.Context(), `
				UPDATE github_installations
				SET account_login = $2, account_type = $3, account_id = $4, updated_at = now()
				WHERE installation_id = $1`,
				instID, meta.Account.Login, meta.Account.Type, meta.Account.ID); uerr != nil {
				a.log.Warn("github callback: refresh installation failed", "installation_id", instID, "err", uerr)
			}
		}
		http.Redirect(w, r, dash+"/apps?github=connected&installation_id="+instIDStr, http.StatusFound)
		return
	}

	// Best-effort: identify the connecting user via the OAuth code (only when
	// client credentials are configured). Failure here is non-fatal.
	connectedBy := userID
	if code := strings.TrimSpace(q.Get("code")); code != "" && githubClientID() != "" && githubClientSecret() != "" {
		if login := a.githubExchangeUserLogin(r.Context(), code); login != "" {
			connectedBy = login
		}
	}

	// Fetch installation account metadata via an App JWT.
	meta, err := a.githubFetchInstallation(r.Context(), instID)
	if err != nil {
		a.log.Error("github callback: fetch installation failed", "installation_id", instID, "err", err)
		http.Redirect(w, r, dash+"/apps?github=error&reason=installation_lookup", http.StatusFound)
		return
	}

	if _, err := a.db.ExecContext(r.Context(), `
		INSERT INTO github_installations
			(installation_id, workspace, account_login, account_type, account_id, connected_by, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6, now())
		ON CONFLICT (installation_id) DO UPDATE SET
			workspace     = EXCLUDED.workspace,
			account_login = EXCLUDED.account_login,
			account_type  = EXCLUDED.account_type,
			account_id    = EXCLUDED.account_id,
			connected_by  = EXCLUDED.connected_by,
			updated_at    = now()`,
		instID, workspace, meta.Account.Login, meta.Account.Type, meta.Account.ID, connectedBy); err != nil {
		a.log.Error("github callback: upsert installation failed", "err", err)
		http.Redirect(w, r, dash+"/apps?github=error&reason=persist", http.StatusFound)
		return
	}

	http.Redirect(w, r, dash+"/apps?github=connected&installation_id="+instIDStr, http.StatusFound)
}

// githubExchangeUserLogin exchanges an OAuth code for a user token and returns
// the authenticated user's login. Returns "" on any error (caller treats the
// connecting user as unknown).
func (a *appsAPI) githubExchangeUserLogin(ctx context.Context, code string) string {
	form := url.Values{}
	form.Set("client_id", githubClientID())
	form.Set("client_secret", githubClientSecret())
	form.Set("code", code)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://github.com/login/oauth/access_token", strings.NewReader(form.Encode()))
	if err != nil {
		return ""
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil || tok.AccessToken == "" {
		return ""
	}
	var user struct {
		Login string `json:"login"`
	}
	if _, err := githubAPIGet(ctx, &http.Client{Timeout: 15 * time.Second}, tok.AccessToken, "/user", &user); err != nil {
		return ""
	}
	return user.Login
}

// githubFetchInstallation reads installation account metadata via an App JWT.
func (a *appsAPI) githubFetchInstallation(ctx context.Context, instID int64) (githubInstallationMeta, error) {
	client, err := a.githubAppClient()
	if err != nil {
		return githubInstallationMeta{}, err
	}
	var meta githubInstallationMeta
	if _, err := githubAPIGet(ctx, client, "", "/app/installations/"+strconv.FormatInt(instID, 10), &meta); err != nil {
		return githubInstallationMeta{}, err
	}
	return meta, nil
}

// ---------------------------------------------------------------------------
// Installations / Repos
// ---------------------------------------------------------------------------

type connectedInstallation struct {
	InstallationID int64     `json:"installation_id"`
	AccountLogin   string    `json:"account_login"`
	AccountType    string    `json:"account_type"`
	ConnectedBy    string    `json:"connected_by,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

func (a *appsAPI) githubListInstallations(w http.ResponseWriter, r *http.Request) {
	workspace, _, ok := functionScope(r)
	if !ok {
		writeErrOrg(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	rows, err := a.db.QueryContext(r.Context(),
		`SELECT installation_id, account_login, account_type, connected_by, created_at
		 FROM github_installations WHERE workspace = $1 ORDER BY created_at DESC`, workspace)
	if err != nil {
		writeErrOrg(w, http.StatusInternalServerError, "internal server error")
		return
	}
	defer rows.Close()
	out := []connectedInstallation{}
	for rows.Next() {
		var ci connectedInstallation
		if err := rows.Scan(&ci.InstallationID, &ci.AccountLogin, &ci.AccountType, &ci.ConnectedBy, &ci.CreatedAt); err != nil {
			writeErrOrg(w, http.StatusInternalServerError, "internal server error")
			return
		}
		out = append(out, ci)
	}
	writeJSONOrg(w, http.StatusOK, map[string]any{"installations": out})
}

func (a *appsAPI) githubDeleteInstallation(w http.ResponseWriter, r *http.Request) {
	workspace, _, ok := functionScope(r)
	if !ok {
		writeErrOrg(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	instID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErrOrg(w, http.StatusBadRequest, "invalid installation id")
		return
	}
	res, err := a.db.ExecContext(r.Context(),
		`DELETE FROM github_installations WHERE workspace = $1 AND installation_id = $2`, workspace, instID)
	if err != nil {
		writeErrOrg(w, http.StatusInternalServerError, "internal server error")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeErrOrg(w, http.StatusNotFound, "installation not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// githubListRepos lists repositories accessible to a connected installation.
// Query param: installation_id (must belong to the caller's workspace).
func (a *appsAPI) githubListRepos(w http.ResponseWriter, r *http.Request) {
	workspace, _, ok := functionScope(r)
	if !ok {
		writeErrOrg(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	instID, err := strconv.ParseInt(strings.TrimSpace(r.URL.Query().Get("installation_id")), 10, 64)
	if err != nil || instID == 0 {
		writeErrOrg(w, http.StatusBadRequest, "installation_id is required")
		return
	}
	// Authorize: the installation must be connected by this workspace.
	var owned bool
	if err := a.db.QueryRowContext(r.Context(),
		`SELECT EXISTS(SELECT 1 FROM github_installations WHERE workspace = $1 AND installation_id = $2)`,
		workspace, instID).Scan(&owned); err != nil {
		writeErrOrg(w, http.StatusInternalServerError, "internal server error")
		return
	}
	if !owned {
		writeErrOrg(w, http.StatusNotFound, "installation not found")
		return
	}
	token, err := a.githubInstallationTokenFor(r.Context(), instID)
	if err != nil || token == "" {
		a.log.Error("github repos: token failed", "installation_id", instID, "err", err)
		writeErrOrg(w, http.StatusBadGateway, "could not mint installation token")
		return
	}
	repos, err := a.githubInstallationRepos(r.Context(), token)
	if err != nil {
		a.log.Error("github repos: list failed", "installation_id", instID, "err", err)
		writeErrOrg(w, http.StatusBadGateway, "could not list repositories")
		return
	}
	writeJSONOrg(w, http.StatusOK, map[string]any{"repos": repos})
}

// githubInstallationRepos pages through GET /installation/repositories (up to
// 5 pages of 100 = 500 repos, which is plenty for the picker).
func (a *appsAPI) githubInstallationRepos(ctx context.Context, token string) ([]githubRepo, error) {
	client := &http.Client{Timeout: 15 * time.Second}
	out := []githubRepo{}
	for page := 1; page <= 5; page++ {
		var body struct {
			TotalCount   int          `json:"total_count"`
			Repositories []githubRepo `json:"repositories"`
		}
		path := "/installation/repositories?per_page=100&page=" + strconv.Itoa(page)
		if _, err := githubAPIGet(ctx, client, token, path, &body); err != nil {
			return nil, err
		}
		out = append(out, body.Repositories...)
		if len(body.Repositories) < 100 {
			break
		}
	}
	return out, nil
}
