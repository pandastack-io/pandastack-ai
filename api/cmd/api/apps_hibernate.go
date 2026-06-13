// SPDX-License-Identifier: Apache-2.0
//
// apps_hibernate.go — auto-hibernate (scale-to-zero) for git-driven apps.
//
// Three cooperating pieces, all control-plane side (the agent already has the
// hibernate/wake primitives — POST /v1/sandboxes/{id}/hibernate takes a full
// Firecracker memory snapshot and stops the VM; /wake restores it in ~1s with
// the in-guest app process intact, same sandbox ID/IP/MAC):
//
//  1. Activity tracking (noteAppActivity): every proxied request (host router
//     + /v1/apps/{id}/proxy) bumps apps.last_request_at, throttled in-memory
//     to one DB write per app per minute so traffic never hammers Postgres.
//
//  2. Idle sweep (sweepIdleApps): piggybacks on the 30s apps monitor tick.
//     Running apps with auto_hibernate enabled whose last_request_at (or
//     updated_at if never proxied) is older than idle_timeout_seconds get
//     their sandbox hibernated and status set to "hibernated". The health
//     monitor's `status = 'running'` filter naturally skips hibernated apps.
//
//  3. Wake-on-request (wakeApp): when a request hits a hibernated app, the
//     proxy path wakes the sandbox first (per-app singleflight so a burst of
//     requests triggers exactly one wake per process), flips status back to
//     "running", then forwards the request. The caller just sees a slow
//     (~1-2s) first response instead of an error.
//
// Deliberately NOT used: the agent's global PANDASTACK_IDLE_AFTER sweeper.
// It would also hibernate database sandboxes, whose traffic flows through
// db-proxy and never touches the agent API — they would look idle forever
// and managed DBs must stay 100% available. Apps-only logic lives here.

package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	// appActivityFlushEvery throttles last_request_at writes: at most one
	// UPDATE per app per interval, regardless of request rate.
	appActivityFlushEvery = time.Minute
	// appWakeTimeout bounds a wake-on-request (agent restore is ~1s; leave
	// headroom for cross-host GCS pulls and slow disks).
	appWakeTimeout = 30 * time.Second
)

// ---------------------------------------------------------------------------
// Activity tracking
// ---------------------------------------------------------------------------

// noteAppActivity records that a proxied request just hit the app. The DB
// write happens asynchronously and at most once per appActivityFlushEvery per
// app, so this is safe to call on every request in the hot proxy path.
func (a *appsAPI) noteAppActivity(appID string) {
	now := time.Now()
	a.activityMu.Lock()
	if last, ok := a.lastActivityFlush[appID]; ok && now.Sub(last) < appActivityFlushEvery {
		a.activityMu.Unlock()
		return
	}
	a.lastActivityFlush[appID] = now
	a.activityMu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := a.db.ExecContext(ctx,
			`UPDATE apps SET last_request_at = now() WHERE id = $1`, appID); err != nil {
			a.log.Warn("apps: bump last_request_at failed", "app_id", appID, "err", err)
		}
	}()
}

// ---------------------------------------------------------------------------
// Idle sweep (called from the apps monitor tick)
// ---------------------------------------------------------------------------

// sweepIdleApps hibernates every running app with auto_hibernate enabled whose
// last proxied request (or last config change, when never proxied) is older
// than its idle_timeout_seconds.
func (a *appsAPI) sweepIdleApps(ctx context.Context) {
	rows, err := a.db.QueryContext(ctx, `
		SELECT `+appColumns+` FROM apps
		WHERE status = 'running' AND sandbox_id <> '' AND auto_hibernate
		  AND COALESCE(last_request_at, updated_at) < now() - make_interval(secs => idle_timeout_seconds)`)
	if err != nil {
		a.log.Warn("apps: list idle apps failed", "err", err)
		return
	}
	idle := []AppInfo{}
	for rows.Next() {
		app, scanErr := scanAppInfo(rows)
		if scanErr != nil {
			a.log.Warn("apps: scan idle app failed", "err", scanErr)
			continue
		}
		idle = append(idle, app)
	}
	rows.Close()

	for _, app := range idle {
		a.hibernateApp(ctx, app)
	}
}

// hibernateApp snapshots + stops the app's sandbox via the agent, then flips
// the app to "hibernated". On agent failure the app stays "running" and the
// next sweep retries. Race note: a request landing between the agent call and
// the status flip may see a brief 5xx; the request after that hits the
// hibernated status and wakes the app.
func (a *appsAPI) hibernateApp(ctx context.Context, app AppInfo) {
	idleFor := time.Duration(app.IdleTimeoutSeconds) * time.Second
	a.log.Info("apps: hibernating idle app",
		"app_id", app.ID, "sandbox_id", app.SandboxID, "idle_timeout", idleFor.String())
	resp, err := a.agentCall(ctx, "POST", "/v1/sandboxes/"+app.SandboxID+"/hibernate", app.Workspace, nil)
	if err != nil {
		a.log.Warn("apps: hibernate call failed", "app_id", app.ID, "err", err)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		a.log.Warn("apps: hibernate rejected", "app_id", app.ID,
			"http", resp.StatusCode, "body", strings.TrimSpace(string(body)))
		return
	}
	a.setAppStatus(ctx, app.ID, "hibernated")
	a.log.Info("apps: app hibernated", "app_id", app.ID, "sandbox_id", app.SandboxID)
}

// ---------------------------------------------------------------------------
// Wake-on-request
// ---------------------------------------------------------------------------

// appWake is the per-app singleflight slot: the first request to a hibernated
// app performs the wake; concurrent requests block on done and share the
// result instead of issuing duplicate wake calls.
type appWake struct {
	done chan struct{}
	app  AppInfo
	err  error
}

// wakeApp restores a hibernated app's sandbox and flips it back to "running",
// deduplicating concurrent callers per app ID. The wake itself runs on a
// background context so a caller that gives up (ctx cancelled) doesn't abort
// the restore for everyone else.
func (a *appsAPI) wakeApp(ctx context.Context, app AppInfo) (AppInfo, error) {
	a.wakeMu.Lock()
	if w, ok := a.wakes[app.ID]; ok {
		a.wakeMu.Unlock()
		select {
		case <-w.done:
			return w.app, w.err
		case <-ctx.Done():
			return AppInfo{}, ctx.Err()
		}
	}
	w := &appWake{done: make(chan struct{})}
	a.wakes[app.ID] = w
	a.wakeMu.Unlock()

	w.app, w.err = a.doWakeApp(app)
	close(w.done)

	a.wakeMu.Lock()
	delete(a.wakes, app.ID)
	a.wakeMu.Unlock()

	if w.err != nil {
		return AppInfo{}, w.err
	}
	select {
	case <-ctx.Done():
		return AppInfo{}, ctx.Err()
	default:
	}
	return w.app, nil
}

// doWakeApp performs the actual agent wake + status flip. Tolerates losing a
// cross-replica race: if the agent rejects the wake but the sandbox turns out
// to be running (another API replica woke it first), that counts as success.
func (a *appsAPI) doWakeApp(app AppInfo) (AppInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), appWakeTimeout)
	defer cancel()
	start := time.Now()
	a.log.Info("apps: waking hibernated app", "app_id", app.ID, "sandbox_id", app.SandboxID)

	resp, err := a.agentCall(ctx, "POST", "/v1/sandboxes/"+app.SandboxID+"/wake", app.Workspace, nil)
	if err != nil {
		return AppInfo{}, fmt.Errorf("wake call: %w", err)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		alive, running := a.sandboxState(ctx, app.Workspace, app.SandboxID)
		if !alive || !running {
			return AppInfo{}, fmt.Errorf("wake http %d: %s",
				resp.StatusCode, strings.TrimSpace(string(body)))
		}
		// Sandbox is already running — another replica won the wake race.
	}

	if _, err := a.db.ExecContext(ctx,
		`UPDATE apps SET status = 'running', last_request_at = now(), updated_at = now() WHERE id = $1`,
		app.ID); err != nil {
		a.log.Warn("apps: mark woken app running failed", "app_id", app.ID, "err", err)
	}
	app.Status = "running"
	now := time.Now()
	app.LastRequestAt = &now
	a.log.Info("apps: app woken", "app_id", app.ID, "sandbox_id", app.SandboxID,
		"wake_ms", time.Since(start).Milliseconds())
	return app, nil
}
