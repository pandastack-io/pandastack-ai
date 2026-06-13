// SPDX-License-Identifier: Apache-2.0
//
// apps_github.go — GitHub App authentication for private-repo clones.
//
// A single GitHub App installation (configured via env) lets the deploy
// pipeline clone PRIVATE github.com repositories. Public repos and non-GitHub
// hosts always clone unauthenticated, so this is purely additive.
//
// Config (single installation; all three required to enable auth):
//
//	GITHUB_APP_ID              — numeric App ID
//	GITHUB_APP_INSTALLATION_ID — numeric installation ID for the org/user
//	GITHUB_APP_PRIVATE_KEY     — PEM private key (literal \n escapes are
//	                             normalized to real newlines so it can live on
//	                             a single .env line)
//
// When unset, githubInstallationToken returns ("", nil) and the clone proceeds
// unauthenticated (public repos only).
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/bradleyfalzon/ghinstallation/v2"
)

// githubAppConfigured reports whether all three GitHub App env vars are set.
func githubAppConfigured() bool {
	return strings.TrimSpace(os.Getenv("GITHUB_APP_ID")) != "" &&
		strings.TrimSpace(os.Getenv("GITHUB_APP_INSTALLATION_ID")) != "" &&
		strings.TrimSpace(os.Getenv("GITHUB_APP_PRIVATE_KEY")) != ""
}

// normalizePEM converts a single-line env value with literal "\n" escapes into
// a real multi-line PEM block. Pass-through if it already contains newlines.
func normalizePEM(s string) string {
	s = strings.TrimSpace(s)
	if strings.Contains(s, "\n") {
		return s
	}
	return strings.ReplaceAll(s, `\n`, "\n")
}

// githubAppCredsConfigured reports whether the App ID + private key are set.
// Unlike githubAppConfigured this does NOT require a single installation ID —
// the production multi-installation flow mints tokens per installation.
func githubAppCredsConfigured() bool {
	return strings.TrimSpace(os.Getenv("GITHUB_APP_ID")) != "" &&
		strings.TrimSpace(os.Getenv("GITHUB_APP_PRIVATE_KEY")) != ""
}

// githubAppID parses GITHUB_APP_ID.
func githubAppID() (int64, error) {
	return strconv.ParseInt(strings.TrimSpace(os.Getenv("GITHUB_APP_ID")), 10, 64)
}

// githubAppPrivateKey returns the normalized PEM private key bytes.
func githubAppPrivateKey() []byte {
	return []byte(normalizePEM(os.Getenv("GITHUB_APP_PRIVATE_KEY")))
}

// githubInstallationToken mints a short-lived GitHub App installation token for
// the configured single installation. Returns ("", nil) when the App is not
// configured (caller then clones unauthenticated).
func (a *appsAPI) githubInstallationToken(ctx context.Context) (string, error) {
	if !githubAppConfigured() {
		return "", nil
	}
	instID, err := strconv.ParseInt(strings.TrimSpace(os.Getenv("GITHUB_APP_INSTALLATION_ID")), 10, 64)
	if err != nil {
		return "", fmt.Errorf("GITHUB_APP_INSTALLATION_ID: %w", err)
	}
	return a.githubInstallationTokenFor(ctx, instID)
}

// githubInstallationTokenFor mints a short-lived installation token for a
// specific installation ID. When instID is 0 it falls back to the env-
// configured single installation (githubInstallationToken). Returns ("", nil)
// when the App credentials are not configured (caller clones unauthenticated).
func (a *appsAPI) githubInstallationTokenFor(ctx context.Context, instID int64) (string, error) {
	if instID == 0 {
		return a.githubInstallationToken(ctx)
	}
	if !githubAppCredsConfigured() {
		return "", nil
	}
	appID, err := githubAppID()
	if err != nil {
		return "", fmt.Errorf("GITHUB_APP_ID: %w", err)
	}
	tr, err := ghinstallation.New(http.DefaultTransport, appID, instID, githubAppPrivateKey())
	if err != nil {
		return "", fmt.Errorf("ghinstallation: %w", err)
	}
	return tr.Token(ctx)
}

// isGitHubHTTPS reports whether u is an https github.com repo URL — the only
// case where we attach the installation token.
func isGitHubHTTPS(u string) bool {
	u = strings.ToLower(strings.TrimSpace(u))
	return strings.HasPrefix(u, "https://github.com/")
}

// gitCredentialSetup returns a shell command that writes a git credential-store
// entry for github.com using the installation token, so the subsequent plain
// `git clone https://github.com/...` calls authenticate transparently. The
// token lands in $HOME/.git-credentials inside the ephemeral sandbox — never in
// the deployment build logs (execStep logs only the step label, not the cmd).
func gitCredentialSetup(token string) string {
	cred := "https://x-access-token:" + token + "@github.com"
	return "git config --global credential.helper store && " +
		"echo " + shellQuote(cred) + " > $HOME/.git-credentials && " +
		"chmod 600 $HOME/.git-credentials"
}
