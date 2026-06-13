// SPDX-License-Identifier: Apache-2.0
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// testDSN returns the Postgres DSN to use for DB-gated integration tests. We
// prefer PANDASTACK_TEST_DB_DSN, then PANDASTACK_DB_DSN, then the local-dev
// default that scripts/mac-local-e2e.sh uses. Empty string => skip.
func testDSN() string {
	for _, k := range []string{"PANDASTACK_TEST_DB_DSN", "PANDASTACK_DB_DSN"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return "postgres://pandastack:pandastack@localhost:5432/pandastack?sslmode=disable"
}

// openTestDB opens the control-plane Postgres, or skips the test if it is not
// reachable (so the suite stays green on machines without local Postgres).
func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", testDSN())
	if err != nil {
		t.Skipf("skip: cannot open postgres: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		t.Skipf("skip: postgres not reachable (%v)", err)
	}
	return db
}

// TestGitHubPushTriggersDeploy proves the end-to-end webhook→deploy wiring:
// a signed `push` delivery for a repo linked to an auto_deploy app must enqueue
// a deployment row. The v1 agent proxy is stubbed to 503 so startDeployment's
// background goroutine fails cleanly without touching a real agent.
func TestGitHubPushTriggersDeploy(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	// Stub agent proxy: every internal sandbox call returns 503 so the deploy
	// goroutine fails gracefully (no panic, no real Firecracker).
	stub := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"stub agent"}`))
	})
	a := newAppsAPI(db, log, stub, nil)

	ctx := context.Background()
	if err := a.SetupSchema(ctx); err != nil {
		t.Fatalf("SetupSchema: %v", err)
	}

	// Unique test fixtures so parallel/repeat runs don't collide.
	workspace := fmt.Sprintf("test-gh-ws-%d", time.Now().UnixNano())
	const repoID = int64(987654321)
	const repoFull = "pandastack-test/webhook-demo"
	const branch = "main"

	var appID string
	if err := db.QueryRowContext(ctx, `
		INSERT INTO apps (workspace, name, git_url, git_branch,
		                  github_repo_id, github_repo_full_name, auto_deploy)
		VALUES ($1,$2,$3,$4,$5,$6,true)
		RETURNING id::text`,
		workspace, "webhook-demo",
		"https://github.com/"+repoFull+".git", branch,
		repoID, repoFull,
	).Scan(&appID); err != nil {
		t.Fatalf("insert app: %v", err)
	}
	// Clean up app + any deployments it spawns.
	defer func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM deployments WHERE app_id = $1`, appID)
		_, _ = db.ExecContext(context.Background(), `DELETE FROM apps WHERE id = $1`, appID)
	}()

	// Configure the webhook secret for verification.
	const secret = "integration-test-webhook-secret"
	t.Setenv("GITHUB_APP_WEBHOOK_SECRET", secret)

	// Build a signed push payload for the linked repo + branch.
	payload := map[string]any{
		"ref":     "refs/heads/" + branch,
		"after":   "deadbeefcafe1234567890",
		"deleted": false,
		"repository": map[string]any{
			"id":        repoID,
			"full_name": repoFull,
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/github", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-Hub-Signature-256", sign(secret, body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	a.githubWebhook(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("webhook status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}

	// enqueueDeploy inserts the 'queued' deployments row synchronously before
	// spawning the background goroutine, so the row must exist now.
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM deployments WHERE app_id = $1`, appID).Scan(&n); err != nil {
		t.Fatalf("count deployments: %v", err)
	}
	if n < 1 {
		t.Fatalf("expected >=1 deployment enqueued for app %s, got %d", appID, n)
	}
}
