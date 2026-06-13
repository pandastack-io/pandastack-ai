// SPDX-License-Identifier: Apache-2.0
//
// apps_monitor.go — health monitor + auto-restart for running apps.
//
// A single background goroutine (StartMonitor) ticks every 30s. Each tick
// first runs the auto-hibernate idle sweep (apps_hibernate.go) — which may
// flip idle apps to "hibernated" — then reconciles every app whose status is
// "running" (the filter naturally skips hibernated apps). For each it:
//
//   - Confirms the runtime sandbox still exists. A persistent sandbox that has
//     gone missing (e.g. host loss) flips the app to "error".
//   - Health-checks the app's port from inside the sandbox. After a couple of
//     consecutive failures it re-launches the stored start command in place
//     (the sandbox is persistent, so the build artifacts are still there).
//   - Caps restarts per app to avoid crash-loop storms; exceeding the budget
//     parks the app in "error" until a human (or a new deploy) intervenes.
//
// State (consecutive failures + restart counts) lives in the loop's local maps,
// so no locking is needed — reconciliation is strictly sequential.

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

const (
	monitorInterval     = 30 * time.Second // reconcile cadence
	healthFailThreshold = 2                // consecutive failures before a restart
	maxRestartsPerApp   = 5                // restart budget before parking in "error"
)

// StartMonitor runs the app health/restart loop until ctx is cancelled. Call
// once from main after the schema is ready.
func (a *appsAPI) StartMonitor(ctx context.Context) {
	ticker := time.NewTicker(monitorInterval)
	defer ticker.Stop()
	a.log.Info("app health monitor started", "interval", monitorInterval.String())

	failures := map[string]int{} // app id -> consecutive health-check failures
	restarts := map[string]int{} // app id -> restarts performed this lifetime

	for {
		select {
		case <-ctx.Done():
			a.log.Info("app health monitor stopped")
			return
		case <-ticker.C:
			a.sweepIdleApps(ctx)
			a.reconcileApps(ctx, failures, restarts)
		}
	}
}

// reconcileApps walks every running app once, restarting unhealthy ones.
func (a *appsAPI) reconcileApps(ctx context.Context, failures, restarts map[string]int) {
	rows, err := a.db.QueryContext(ctx,
		`SELECT `+appColumns+` FROM apps WHERE status = 'running' AND sandbox_id <> ''`)
	if err != nil {
		a.log.Warn("monitor: list running apps failed", "err", err)
		return
	}
	apps := []AppInfo{}
	for rows.Next() {
		app, scanErr := scanAppInfo(rows)
		if scanErr != nil {
			a.log.Warn("monitor: scan app failed", "err", scanErr)
			continue
		}
		apps = append(apps, app)
	}
	rows.Close()

	// Track which apps we still see so stale counters can be pruned.
	seen := make(map[string]struct{}, len(apps))
	for _, app := range apps {
		seen[app.ID] = struct{}{}
		a.reconcileApp(ctx, app, failures, restarts)
	}
	for id := range failures {
		if _, ok := seen[id]; !ok {
			delete(failures, id)
		}
	}
	for id := range restarts {
		if _, ok := seen[id]; !ok {
			delete(restarts, id)
		}
	}
}

// reconcileApp checks a single app and restarts it if it has been unhealthy for
// healthFailThreshold consecutive cycles.
func (a *appsAPI) reconcileApp(ctx context.Context, app AppInfo, failures, restarts map[string]int) {
	alive, running := a.sandboxState(ctx, app.Workspace, app.SandboxID)
	if !alive {
		a.log.Warn("monitor: app sandbox missing; marking error", "app_id", app.ID, "sandbox_id", app.SandboxID)
		a.setAppStatus(ctx, app.ID, "error")
		delete(failures, app.ID)
		delete(restarts, app.ID)
		return
	}
	if !running {
		// Sandbox is present but not yet/again running (booting, pausing).
		// Skip this cycle without counting it as an app-level failure.
		return
	}

	if a.healthCheck(ctx, app.Workspace, app.SandboxID, app.Port, 1) == nil {
		failures[app.ID] = 0
		restarts[app.ID] = 0
		return
	}

	failures[app.ID]++
	if failures[app.ID] < healthFailThreshold {
		a.log.Info("monitor: app health check failed", "app_id", app.ID, "consecutive", failures[app.ID])
		return
	}
	if restarts[app.ID] >= maxRestartsPerApp {
		a.log.Error("monitor: app exceeded restart budget; parking in error", "app_id", app.ID, "restarts", restarts[app.ID])
		a.setAppStatus(ctx, app.ID, "error")
		return
	}

	restarts[app.ID]++
	a.log.Warn("monitor: restarting unhealthy app", "app_id", app.ID, "sandbox_id", app.SandboxID, "attempt", restarts[app.ID])
	a.restartApp(ctx, app)
	failures[app.ID] = 0
}

// restartApp re-launches the app's start command in its existing (persistent)
// sandbox and verifies it comes back up.
func (a *appsAPI) restartApp(ctx context.Context, app AppInfo) {
	if _, err := a.execInSandbox(ctx, app.Workspace, app.SandboxID, appStartCommand(app)); err != nil {
		a.log.Warn("monitor: restart exec failed", "app_id", app.ID, "err", err)
		return
	}
	if err := a.healthCheck(ctx, app.Workspace, app.SandboxID, app.Port, 15); err != nil {
		a.log.Warn("monitor: app still unhealthy after restart", "app_id", app.ID, "err", err)
		return
	}
	a.log.Info("monitor: app recovered after restart", "app_id", app.ID)
}

// sandboxState reports whether the sandbox still exists (alive) and whether it
// is currently running. A transient lookup error is treated as alive-but-not-
// confirmed so a blip never flips a healthy app to "error".
func (a *appsAPI) sandboxState(ctx context.Context, workspace, sandboxID string) (alive, running bool) {
	resp, err := a.agentCall(ctx, "GET", "/v1/sandboxes/"+sandboxID, workspace, nil)
	if err != nil {
		return true, false
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return false, false
	}
	if resp.StatusCode != http.StatusOK {
		return true, false
	}
	var info struct {
		Status string `json:"status"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&info)
	return true, info.Status == "running"
}
