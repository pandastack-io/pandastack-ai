// SPDX-License-Identifier: Apache-2.0
//
// apps_deploy.go — deployment orchestration for git-driven apps.
//
// A deployment runs entirely on the local stack with no cloud dependencies:
//
//  1. Boot a fresh persistent sandbox (blue) on the app's template.
//  2. git clone the repo into /app, record the commit.
//  3. Detect the framework (or honor the user's pin) and resolve
//     install/build/start commands.
//  4. Run install + build, streaming output into the deployment's build_logs
//     (surfaced live by the SSE /logs endpoint).
//  5. Start the app in the background on its port, then health-check it via the
//     agent port proxy.
//  6. Flip traffic: point the app at the new sandbox, mark the deployment live,
//     supersede the previous deployment, and tear down the old (green) sandbox.
//
// Step 6 (blue-green/rollback) and Step 5 (routing) build on the sandbox_id /
// active_deployment_id bookkeeping established here.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// deployTimeout bounds a single deployment end to end (clone+install+build+boot).
const deployTimeout = 12 * time.Minute

// buildPlan is the resolved set of commands + working directory for a deploy.
type buildPlan struct {
	framework string
	install   string
	build     string
	start     string
	workdir   string
}

// misePreamble makes the baked mise toolchain (shims + pre-warmed runtimes, and
// anything installed on demand) resolvable inside an exec step. Build/runtime
// commands run via a non-login `sh -c`, so /etc/profile.d is NOT sourced — we
// set the env explicitly. MISE_YES suppresses interactive prompts during
// `mise install`/`mise use`. Keep in sync with the base template's layout
// (scripts/mac-local-e2e.sh seed_base_template, templates/base/Dockerfile).
const misePreamble = "export MISE_DATA_DIR=/opt/mise MISE_CONFIG_DIR=/opt/mise MISE_YES=1 PATH=/opt/mise/shims:$PATH"

// miseStep prefixes a build command with the mise env and a cd into the work
// directory, so every install/build step sees the right runtime on PATH.
func miseStep(workdir, cmd string) string {
	return misePreamble + "; cd " + shellQuote(workdir) + " && " + cmd
}

// startDeployment drives a deployment from queued → live. Runs in its own
// goroutine with a detached, time-bounded context.
func (a *appsAPI) startDeployment(parent context.Context, app AppInfo, deployID, gitRef string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), deployTimeout)
	defer cancel()

	a.setDeploymentStatus(ctx, deployID, "building", "")
	a.setAppStatus(ctx, app.ID, "building")
	a.appendDeployLog(ctx, deployID, fmt.Sprintf("==> deploying %s (%s@%s)\n", app.Name, app.GitURL, gitRef))

	// 1. Fresh sandbox for this deployment (blue-green: never mutate the live one).
	a.appendDeployLog(ctx, deployID, fmt.Sprintf("==> provisioning %s sandbox (cpu=%d mem=%dMB)\n", app.Template, app.CPU, app.MemoryMB))
	sandboxID, err := a.createAppSandbox(ctx, app)
	if err != nil {
		a.failDeploy(ctx, app, deployID, "", fmt.Sprintf("provision sandbox: %v", err))
		return
	}
	a.setDeploymentSandbox(ctx, deployID, sandboxID)
	if err := a.waitForSandboxReady(ctx, app.Workspace, sandboxID); err != nil {
		a.failDeploy(ctx, app, deployID, sandboxID, fmt.Sprintf("sandbox not ready: %v", err))
		return
	}
	a.appendDeployLog(ctx, deployID, "==> sandbox ready: "+sandboxID+"\n")

	// 2-4. Clone + detect + install + build.
	plan, commit, err := a.runBuild(ctx, app, deployID, sandboxID, gitRef)
	if err != nil {
		a.failDeploy(ctx, app, deployID, sandboxID, err.Error())
		return
	}
	if commit != "" {
		_, _ = a.db.ExecContext(ctx, `UPDATE deployments SET git_commit = $2 WHERE id = $1`, deployID, commit)
	}

	// 5. Start runtime + health check.
	if err := a.startRuntime(ctx, app, deployID, sandboxID, plan); err != nil {
		a.failDeploy(ctx, app, deployID, sandboxID, err.Error())
		return
	}

	// 6. Flip live (blue-green) + tear down the previous sandbox.
	a.flipLive(ctx, app, deployID, sandboxID, plan)
	a.appendDeployLog(ctx, deployID, "==> deployment live\n")
}

// runBuild clones the repo, detects the framework, and runs install + build.
func (a *appsAPI) runBuild(ctx context.Context, app AppInfo, deployID, sandboxID, gitRef string) (buildPlan, string, error) {
	// 0. Ensure outbound DNS works before any network step. The app template
	// bakes a resolver, but restored snapshots (or future custom templates)
	// can boot with an empty /etc/resolv.conf — without a nameserver, the
	// git clone and npm install below fail with confusing "could not resolve
	// host" errors. This is idempotent and best-effort (never fails a deploy).
	a.ensureResolver(ctx, app.Workspace, deployID, sandboxID)

	// Private GitHub repos: mint an installation token and stage a git
	// credential-store entry inside the sandbox so the plain
	// `git clone https://github.com/...` below authenticates transparently.
	// When the app is linked to a connected installation (github_installation_id),
	// we mint a token for THAT installation; otherwise we fall back to the
	// env-configured single installation. Public repos and non-GitHub hosts
	// skip this and clone unauthenticated. The token never reaches the build
	// logs (it lands in $HOME/.git-credentials; execStep logs only labels).
	if isGitHubHTTPS(app.GitURL) {
		if token, err := a.githubInstallationTokenFor(ctx, app.GitHubInstallationID); err != nil {
			a.appendDeployLog(ctx, deployID, "==> GitHub App token unavailable, cloning unauthenticated (public repos only)\n")
		} else if token != "" {
			a.appendDeployLog(ctx, deployID, "==> authenticating GitHub clone via GitHub App installation\n")
			if _, err := a.execInSandbox(ctx, app.Workspace, sandboxID, gitCredentialSetup(token)); err != nil {
				a.appendDeployLog(ctx, deployID, "   (warning: git credential setup failed; continuing unauthenticated)\n")
			}
		}
	}

	// Clone (shallow) then checkout the requested ref if it isn't the default.
	clone := fmt.Sprintf("rm -rf /app && git clone --depth 1 %s /app", shellQuote(app.GitURL))
	if gitRef != "" {
		clone = fmt.Sprintf("rm -rf /app && git clone --depth 1 --branch %s %s /app 2>/dev/null || (rm -rf /app && git clone %s /app && git -C /app checkout %s)",
			shellQuote(gitRef), shellQuote(app.GitURL), shellQuote(app.GitURL), shellQuote(gitRef))
	}
	if err := a.execStep(ctx, app.Workspace, deployID, sandboxID, "git clone", clone); err != nil {
		return buildPlan{}, "", fmt.Errorf("git clone failed: %v", err)
	}

	commit := ""
	if res, err := a.execInSandbox(ctx, app.Workspace, sandboxID, "git -C /app rev-parse HEAD"); err == nil {
		commit = strings.TrimSpace(res.Stdout)
	}

	workdir := appWorkdir(app)
	// Repo-level config (pandastack.json) is a Vercel/Render-style convention:
	// it lets a repo declare its own build/run plan (e.g. a Next.js static
	// export that must be *served* from out/ rather than run with `next start`).
	manifest := a.readAppManifest(ctx, app.Workspace, sandboxID, workdir)
	// Framework precedence: explicit app pin > manifest.type > auto-detect.
	framework := strings.TrimSpace(app.Framework)
	if framework == "" {
		framework = strings.TrimSpace(manifest.Type)
	}
	if framework == "" {
		framework = a.detectFramework(ctx, app.Workspace, sandboxID, workdir)
		a.appendDeployLog(ctx, deployID, "==> detected framework: "+framework+"\n")
	} else {
		a.appendDeployLog(ctx, deployID, "==> framework: "+framework+"\n")
	}
	plan := resolveBuildPlan(app, framework, workdir, manifest)

	// Make sure the right language runtime is available before install/build.
	a.setupRuntime(ctx, app, deployID, sandboxID, workdir)

	if plan.install != "" {
		if err := a.execStep(ctx, app.Workspace, deployID, sandboxID, "install", miseStep(workdir, plan.install)); err != nil {
			return buildPlan{}, commit, fmt.Errorf("install failed: %v", err)
		}
		// Reshim so console scripts installed by the install step become
		// resolvable on PATH. pip/pipx/npm-global place entrypoints (uvicorn,
		// gunicorn, flask, …) under the runtime's own bin dir, which is NOT on
		// PATH — only /opt/mise/shims is. `mise reshim` regenerates shims for
		// those new entrypoints so a commands-first start like `uvicorn ...`
		// just works. Best-effort: a failure here shouldn't fail the build.
		if err := a.execStep(ctx, app.Workspace, deployID, sandboxID, "reshim", miseStep(workdir, "mise reshim || true")); err != nil {
			a.appendDeployLog(ctx, deployID, "==> warning: mise reshim skipped: "+err.Error()+"\n")
		}
	}
	if plan.build != "" {
		if err := a.execStep(ctx, app.Workspace, deployID, sandboxID, "build", miseStep(workdir, plan.build)); err != nil {
			return buildPlan{}, commit, fmt.Errorf("build failed: %v", err)
		}
	}
	return plan, commit, nil
}

// setupRuntime ensures the language runtime(s) the app needs are installed via
// mise before install/build runs. Two complementary sources:
//
//   - An explicit per-app pin (runtime [+ runtime_version]) — e.g. node@20 or
//     python@3.12 — is written to the workdir with `mise use`, installing it if
//     not already present. This is the commands-first escape hatch for stacks
//     the repo doesn't declare a version for.
//   - Repo-declared versions (.nvmrc / .python-version / .tool-versions /
//     mise.toml) are honoured by `mise install`, which reads those files in the
//     workdir.
//
// Both are best-effort: the base template pre-warms Node 20 + Python 3.12, so a
// repo needing those is a no-op, and a repo with no version files just uses the
// baked runtimes. A genuine missing-runtime problem surfaces later as a clear
// install/build failure rather than aborting here.
func (a *appsAPI) setupRuntime(ctx context.Context, app AppInfo, deployID, sandboxID, workdir string) {
	if rt := strings.TrimSpace(app.Runtime); rt != "" {
		spec := rt
		if v := strings.TrimSpace(app.RuntimeVersion); v != "" {
			spec = rt + "@" + v
		}
		a.appendDeployLog(ctx, deployID, "==> runtime: pinning "+spec+" via mise\n")
		if err := a.execStep(ctx, app.Workspace, deployID, sandboxID, "runtime",
			miseStep(workdir, "mise use "+shellQuote(spec))); err != nil {
			a.appendDeployLog(ctx, deployID, "==> warning: mise use failed: "+err.Error()+"\n")
		}
	}
	// Install whatever the repo declares (no-op when already pre-warmed).
	if _, err := a.execInSandbox(ctx, app.Workspace, sandboxID,
		miseStep(workdir, "mise install || true")); err != nil {
		a.appendDeployLog(ctx, deployID, "==> warning: mise install skipped: "+err.Error()+"\n")
	}
}

// startRuntime launches the app in the background and health-checks its port.
func (a *appsAPI) startRuntime(ctx context.Context, app AppInfo, deployID, sandboxID string, plan buildPlan) error {
	a.setDeploymentStatus(ctx, deployID, "deploying", "")
	// Commands-first contract: with no framework default to fall back on (python
	// and generic deliberately have none), an empty start command is a user
	// configuration error, not a runtime failure. Fail fast with a clear message
	// instead of silently "succeeding" with nothing listening.
	if strings.TrimSpace(plan.start) == "" {
		return fmt.Errorf("no start command: set start_command on the app (or startCommand in pandastack.json) — e.g. \"uvicorn main:app --host 0.0.0.0 --port $PORT\"")
	}
	// Background the process; logs go to a file inside the VM for `pandastack
	// logs`-style retrieval later. The shell returns immediately.
	start := backgroundStartCmd(plan.workdir, runtimeEnvPrefix(app), plan.start)
	a.appendDeployLog(ctx, deployID, "==> starting: "+plan.start+"\n")
	if err := a.execStep(ctx, app.Workspace, deployID, sandboxID, "start", start); err != nil {
		return fmt.Errorf("start failed: %v", err)
	}
	a.appendDeployLog(ctx, deployID, fmt.Sprintf("==> waiting for app to listen on :%d\n", app.Port))
	if err := a.healthCheck(ctx, app.Workspace, sandboxID, app.Port, 60); err != nil {
		// Surface a tail of the app log to help debugging.
		if res, e := a.execInSandbox(ctx, app.Workspace, sandboxID, "tail -n 40 /var/log/pandastack-app.log 2>/dev/null"); e == nil && res.Stdout != "" {
			a.appendDeployLog(ctx, deployID, "---- app log tail ----\n"+res.Stdout+"\n----------------------\n")
		}
		return fmt.Errorf("health check failed: %v", err)
	}
	a.appendDeployLog(ctx, deployID, "==> health check passed\n")
	return nil
}

// flipLive points the app at the new sandbox, marks the deployment live, and
// retires the previously-live sandbox + deployment (zero-downtime cutover).
func (a *appsAPI) flipLive(ctx context.Context, app AppInfo, deployID, sandboxID string, plan buildPlan) {
	prevSandbox := app.SandboxID
	prevDeploy := app.ActiveDeploymentID

	// Persist the resolved build/run config so the health monitor (and future
	// deploys without overrides) can reproduce the runtime.
	_, _ = a.db.ExecContext(ctx, `
		UPDATE apps SET sandbox_id = $2, active_deployment_id = $3, status = 'running',
		                framework = $4, install_command = $5, build_command = $6,
		                start_command = $7, updated_at = now()
		WHERE id = $1`,
		app.ID, sandboxID, deployID, plan.framework, plan.install, plan.build, plan.start)

	a.setDeploymentStatus(ctx, deployID, "live", "")
	if prevDeploy != "" && prevDeploy != deployID {
		a.setDeploymentStatus(ctx, prevDeploy, "superseded", "")
	}
	// Tear down the old sandbox now that the new one is serving.
	if prevSandbox != "" && prevSandbox != sandboxID {
		a.appendDeployLog(ctx, deployID, "==> retiring previous sandbox "+prevSandbox+"\n")
		if resp, err := a.agentCall(ctx, "DELETE", "/v1/sandboxes/"+prevSandbox, app.Workspace, nil); err == nil {
			resp.Body.Close()
		}
	}
}

// failDeploy records a failure, tears down the (orphaned) sandbox, and resets
// the app status. The previously-live deployment/sandbox is left untouched.
func (a *appsAPI) failDeploy(ctx context.Context, app AppInfo, deployID, sandboxID, msg string) {
	a.appendDeployLog(ctx, deployID, "!! deploy failed: "+msg+"\n")
	a.setDeploymentStatus(ctx, deployID, "failed", msg)
	if sandboxID != "" {
		if resp, err := a.agentCall(ctx, "DELETE", "/v1/sandboxes/"+sandboxID, app.Workspace, nil); err == nil {
			resp.Body.Close()
		}
	}
	// If the app has no live sandbox, mark it errored; otherwise it keeps serving.
	status := "error"
	if app.SandboxID != "" {
		status = "running"
	}
	a.setAppStatus(ctx, app.ID, status)
}

// ---------------------------------------------------------------------------
// Framework detection + command resolution
// ---------------------------------------------------------------------------

// detectFramework inspects the cloned repo to choose a framework. This is a
// convenience layer that only fills *blanks* in the commands-first contract.
// Node (package.json) and Python (requirements.txt/pyproject/Pipfile/setup.py)
// are recognised; anything else falls through to "generic" — run exactly what
// the user gave, assume nothing (no implicit install).
func (a *appsAPI) detectFramework(ctx context.Context, workspace, sandboxID, workdir string) string {
	probe := fmt.Sprintf(`cd %s 2>/dev/null || exit 0
if [ -f package.json ]; then
  if grep -q '"next"' package.json; then echo nextjs;
  elif grep -q '"vite"' package.json; then echo vite;
  elif grep -q '"react-scripts"' package.json; then echo cra;
  else echo node; fi
elif [ -f index.html ]; then echo static;
elif [ -f requirements.txt ] || [ -f pyproject.toml ] || [ -f Pipfile ] || [ -f setup.py ]; then echo python;
else echo generic; fi`, shellQuote(workdir))
	res, err := a.execInSandbox(ctx, workspace, sandboxID, probe)
	if err != nil {
		return "generic"
	}
	fw := strings.TrimSpace(res.Stdout)
	if fw == "" {
		return "generic"
	}
	return fw
}

// appManifest is the optional repo-level config file (pandastack.json) a
// project can commit to declare its own build/run plan, à la vercel.json. All
// fields are optional; populated ones win over framework defaults but lose to
// explicit per-app overrides set through the API.
type appManifest struct {
	// Type pins the framework (nextjs|vite|cra|node|static); empty = auto.
	Type string `json:"type"`
	// OutputDir is the directory of static build artifacts to serve (static/
	// vite/cra). For a Next.js `output: 'export'` app this is typically "out".
	OutputDir      string `json:"outputDir"`
	InstallCommand string `json:"installCommand"`
	BuildCommand   string `json:"buildCommand"`
	StartCommand   string `json:"startCommand"`
}

// readAppManifest fetches and parses pandastack.json from the cloned repo (at
// the app's working directory). A missing or malformed file yields a zero-value
// manifest — it is purely additive, never fatal.
func (a *appsAPI) readAppManifest(ctx context.Context, workspace, sandboxID, workdir string) appManifest {
	cmd := fmt.Sprintf("cat %s/pandastack.json 2>/dev/null || true", shellQuote(workdir))
	res, err := a.execInSandbox(ctx, workspace, sandboxID, cmd)
	if err != nil {
		return appManifest{}
	}
	body := strings.TrimSpace(res.Stdout)
	if body == "" {
		return appManifest{}
	}
	var m appManifest
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		a.log.Warn("ignoring malformed pandastack.json", "sandbox_id", sandboxID, "err", err)
		return appManifest{}
	}
	return m
}

// ensureResolver guarantees the sandbox has a usable DNS resolver before any
// network step. Best-effort and idempotent: only writes when no nameserver is
// present, so a template-baked resolver is left untouched.
func (a *appsAPI) ensureResolver(ctx context.Context, workspace, deployID, sandboxID string) {
	const fix = `grep -q '^nameserver' /etc/resolv.conf 2>/dev/null || printf 'nameserver 1.1.1.1\nnameserver 8.8.8.8\n' > /etc/resolv.conf`
	if _, err := a.execInSandbox(ctx, workspace, sandboxID, fix); err != nil {
		a.appendDeployLog(ctx, deployID, "==> warning: could not verify DNS resolver: "+err.Error()+"\n")
	}
}

// resolveBuildPlan fills in install/build/start commands using the precedence
// user override (app.*) > repo manifest (pandastack.json) > framework default.
func resolveBuildPlan(app AppInfo, framework, workdir string, m appManifest) buildPlan {
	p := buildPlan{
		framework: framework,
		install:   strings.TrimSpace(app.InstallCommand),
		build:     strings.TrimSpace(app.BuildCommand),
		start:     strings.TrimSpace(app.StartCommand),
		workdir:   workdir,
	}
	// Manifest values fill in wherever the user left a blank.
	def(&p.install, strings.TrimSpace(m.InstallCommand))
	def(&p.build, strings.TrimSpace(m.BuildCommand))
	def(&p.start, strings.TrimSpace(m.StartCommand))

	port := app.Port
	out := strings.Trim(strings.TrimSpace(m.OutputDir), "/")
	switch framework {
	case "nextjs":
		def(&p.install, "npm install")
		def(&p.build, "npm run build")
		def(&p.start, fmt.Sprintf("npx --yes next start -p %d -H 0.0.0.0", port))
	case "vite":
		def(&p.install, "npm install")
		def(&p.build, "npm run build")
		def(&p.start, fmt.Sprintf("npx --yes serve -s %s -l %d", serveDir(out, "dist"), port))
	case "cra":
		def(&p.install, "npm install")
		def(&p.build, "npm run build")
		def(&p.start, fmt.Sprintf("npx --yes serve -s %s -l %d", serveDir(out, "build"), port))
	case "static":
		// A "static" deploy may still need a node build step — e.g. a Next.js
		// `output: 'export'` app that emits an out/ directory which we then
		// serve. When a build command is in play, assume npm dependencies.
		if p.build != "" {
			def(&p.install, "npm install")
		}
		def(&p.start, fmt.Sprintf("npx --yes serve -s %s -l %d", serveDir(out, "."), port))
	case "node":
		def(&p.install, "npm install")
		def(&p.start, "npm start")
	case "python":
		// Python is commands-first: install deps if a requirements file exists,
		// but never guess a start command — e.g. uvicorn vs gunicorn vs a plain
		// script differ enough that assuming one would be wrong. The user (or a
		// pandastack.json) must provide start. resolveBuildPlan's caller fails
		// fast with a clear message when start stays empty.
		def(&p.install, "pip install -r requirements.txt")
	default: // "generic" and anything unrecognised: assume nothing.
		// Run exactly what the user gave. No implicit install/build/start.
	}
	return p
}

// serveDir picks the manifest-declared output directory when set, else the
// framework's conventional build output directory.
func serveDir(manifestOut, fallback string) string {
	if manifestOut != "" {
		return shellQuote(manifestOut)
	}
	return fallback
}

func def(dst *string, fallback string) {
	if strings.TrimSpace(*dst) == "" {
		*dst = fallback
	}
}

// appWorkdir is /app plus the app's optional root_directory subpath.
func appWorkdir(app AppInfo) string {
	root := strings.Trim(strings.TrimSpace(app.RootDirectory), "/")
	if root == "" {
		return "/app"
	}
	return "/app/" + root
}

// backgroundStartCmd wraps a start command so it runs detached inside the VM,
// writing its output to a well-known log file. Shared by the deploy path and
// the health monitor's restart path so both launch the app identically.
func backgroundStartCmd(workdir, envPrefix, start string) string {
	// The start command must be FULLY detached from the exec session: we run it
	// under setsid (new session, no controlling terminal) with all three std
	// streams redirected away from the SSH channel — stdin from /dev/null and
	// stdout/stderr to a log file. If stdin stays wired to the exec channel, the
	// long-running server keeps the channel open and the exec call never returns,
	// hanging the deploy at the "start" step.
	//
	// The env (PATH=/opt/mise/shims:.. PORT=.. HOST=.. + user env) is *exported*
	// in the current shell scope BEFORE launching. This matters for two reasons:
	//
	//   1. Commands-first ergonomics: a user start command almost always
	//      references $PORT (e.g. `uvicorn app:app --port $PORT`). An inline
	//      `PORT=8000 setsid uvicorn … --port $PORT` assignment does NOT take
	//      effect for the shell's own parameter expansion on the same line — the
	//      assignment only populates the exec'd process's environment, so $PORT
	//      expands to empty and the flag loses its argument. Exporting first puts
	//      PORT in the shell env so $PORT expands correctly.
	//   2. The detached child still inherits the toolchain (mise shims on PATH)
	//      and the injected env, since export propagates to children.
	//
	// `setsid` (util-linux, present in the base image) gives the server a new
	// session with no controlling terminal; the `|| …` branch falls back to
	// nohup. stdin from /dev/null + stdout/stderr to a log file fully detaches it
	// from the SSH exec channel so the exec call returns immediately.
	return fmt.Sprintf("cd %s && export %s; { setsid %s </dev/null >/var/log/pandastack-app.log 2>&1 & } "+
		"|| { nohup %s </dev/null >/var/log/pandastack-app.log 2>&1 & }; echo started pid $!",
		shellQuote(workdir), envPrefix, start, start)
}

// appStartCommand reconstructs the runtime start command for a live app, used
// by the monitor to restart a crashed process in its existing sandbox. Falls
// back to the framework default when no resolved start command is stored.
func appStartCommand(app AppInfo) string {
	start := strings.TrimSpace(app.StartCommand)
	if start == "" {
		// The live app's resolved start command is persisted on flipLive, so
		// this fallback only runs for legacy rows; no repo manifest is needed.
		start = resolveBuildPlan(app, app.Framework, appWorkdir(app), appManifest{}).start
	}
	return backgroundStartCmd(appWorkdir(app), runtimeEnvPrefix(app), start)
}

// runtimeEnvPrefix builds a `K='v' ...` prefix (PORT/HOST plus the app's env)
// suitable for prepending to the start command.
func runtimeEnvPrefix(app AppInfo) string {
	var b strings.Builder
	// mise shims first so the runtime (node/python/uvicorn/…) resolves, and the
	// data/config dirs so child processes inherit the right toolchain. Emitted as
	// a space-separated `K=v` list that backgroundStartCmd feeds to `export` in
	// the launch shell, so $PORT/$HOST in the user's start command expand and the
	// detached child inherits the env. `$PATH` here expands against the live env.
	b.WriteString("MISE_DATA_DIR=/opt/mise MISE_CONFIG_DIR=/opt/mise PATH=/opt/mise/shims:$PATH ")
	fmt.Fprintf(&b, "PORT=%d HOST=0.0.0.0 ", app.Port)
	for k, v := range app.Env {
		if k == "" {
			continue
		}
		fmt.Fprintf(&b, "%s=%s ", k, shellQuote(v))
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Sandbox plumbing (mirrors functionsAPI helpers, scoped to apps)
// ---------------------------------------------------------------------------

func (a *appsAPI) createAppSandbox(ctx context.Context, app AppInfo) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"template":   app.Template,
		"cpu":        app.CPU,
		"memory_mb":  app.MemoryMB,
		"persistent": true, // app sandboxes are long-lived; exempt from the idle reaper
		"metadata": map[string]string{
			"app.id":   app.ID,
			"app.name": app.Name,
			"kind":     "app",
		},
	})
	resp, err := a.agentCall(ctx, "POST", "/v1/sandboxes", app.Workspace, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("create sandbox: %d %s", resp.StatusCode, string(b))
	}
	var info struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return "", err
	}
	return info.ID, nil
}

func (a *appsAPI) waitForSandboxReady(ctx context.Context, workspace, sandboxID string) error {
	// A "running" status from the agent only means the microVM is resumed; the
	// in-guest SSH/exec bridge may take several more seconds to accept
	// connections (especially on a cold boot + first-spawn snapshot bake).
	// So we first wait for status==running, then probe
	// the exec bridge with a trivial command until it actually responds. This
	// avoids the "dial tcp ...:22: connect: connection refused" race where the
	// build's git clone fires before sshd is listening.
	deadline := time.Now().Add(3 * time.Minute)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	running := false
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if time.Now().After(deadline) {
				return fmt.Errorf("timed out waiting for sandbox %s", sandboxID)
			}
			if !running {
				resp, err := a.agentCall(ctx, "GET", "/v1/sandboxes/"+sandboxID, workspace, nil)
				if err != nil {
					continue
				}
				var info struct {
					Status string `json:"status"`
				}
				_ = json.NewDecoder(resp.Body).Decode(&info)
				resp.Body.Close()
				if info.Status == "running" {
					running = true
				}
				continue
			}
			// Status is running: confirm the guest exec bridge is actually up.
			if res, err := a.execInSandbox(ctx, workspace, sandboxID, "true"); err == nil && res.ExitCode == 0 {
				return nil
			}
		}
	}
}

// execInSandbox runs cmd via the agent exec endpoint, retrying transient 5xx.
func (a *appsAPI) execInSandbox(ctx context.Context, workspace, sandboxID, cmd string) (agentExecResult, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return agentExecResult{}, ctx.Err()
			case <-time.After(time.Duration(attempt) * 500 * time.Millisecond):
			}
		}
		body, _ := json.Marshal(map[string]string{"cmd": cmd})
		resp, err := a.agentCall(ctx, "POST", "/v1/sandboxes/"+sandboxID+"/exec", workspace, bytes.NewReader(body))
		if err != nil {
			lastErr = err
			continue
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound || resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("exec attempt %d: %d %s", attempt+1, resp.StatusCode, string(b))
			continue
		}
		var result agentExecResult
		if err := json.Unmarshal(b, &result); err != nil {
			return agentExecResult{}, err
		}
		return result, nil
	}
	return agentExecResult{}, lastErr
}

// execStep runs a build step, appends its output to the deploy log, and returns
// an error if the command exits non-zero.
func (a *appsAPI) execStep(ctx context.Context, workspace, deployID, sandboxID, label, cmd string) error {
	a.appendDeployLog(ctx, deployID, "$ "+label+"\n")
	res, err := a.execInSandbox(ctx, workspace, sandboxID, cmd)
	if err != nil {
		a.appendDeployLog(ctx, deployID, "   (exec error: "+err.Error()+")\n")
		return err
	}
	if res.Stdout != "" {
		a.appendDeployLog(ctx, deployID, res.Stdout)
		if !strings.HasSuffix(res.Stdout, "\n") {
			a.appendDeployLog(ctx, deployID, "\n")
		}
	}
	if res.Stderr != "" {
		a.appendDeployLog(ctx, deployID, res.Stderr)
		if !strings.HasSuffix(res.Stderr, "\n") {
			a.appendDeployLog(ctx, deployID, "\n")
		}
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("%s exited %d", label, res.ExitCode)
	}
	return nil
}

// healthCheck polls the app's port from inside the sandbox until it responds or
// the attempt budget is exhausted.
func (a *appsAPI) healthCheck(ctx context.Context, workspace, sandboxID string, port, attempts int) error {
	probe := fmt.Sprintf("curl -s -m 3 -o /dev/null -w '%%{http_code}' http://localhost:%d/ 2>/dev/null", port)
	for i := 0; i < attempts; i++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		res, err := a.execInSandbox(ctx, workspace, sandboxID, probe)
		if err == nil {
			code := strings.TrimSpace(res.Stdout)
			if code != "" && code != "000" {
				return nil
			}
		}
		time.Sleep(1 * time.Second)
	}
	return fmt.Errorf("app did not respond on :%d", port)
}

func (a *appsAPI) setAppStatus(ctx context.Context, appID, status string) {
	if _, err := a.db.ExecContext(ctx, `UPDATE apps SET status = $2, updated_at = now() WHERE id = $1`, appID, status); err != nil {
		a.log.Warn("set app status failed", "app_id", appID, "status", status, "err", err)
	}
}

func (a *appsAPI) setDeploymentSandbox(ctx context.Context, depID, sandboxID string) {
	if _, err := a.db.ExecContext(ctx, `UPDATE deployments SET sandbox_id = $2, updated_at = now() WHERE id = $1`, depID, sandboxID); err != nil {
		a.log.Warn("set deployment sandbox failed", "deploy_id", depID, "err", err)
	}
}
