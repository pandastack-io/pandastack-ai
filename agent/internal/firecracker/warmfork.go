// SPDX-License-Identifier: Apache-2.0
package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// StartWarmFork spawns a firecracker process and loads it from `snapDir`
// (containing vm.mem + vm.state). It overrides the network host TAP via
// /snapshot/load network_overrides, then PATCHes the rootfs drive path to
// childRootfs, then resumes the VM.
//
// This bypasses firecracker-go-sdk because the SDK's WithSnapshot helper
// does not expose per-restore network or drive overrides.
//
// The caller is responsible for:
//   - allocating d.spec.Network (TAP, MAC, IP, CID) for the child
//   - copying parent rootfs to childRootfs before calling
//   - removing d.spec.SocketPath if a stale socket exists
func (d *Driver) StartWarmFork(ctx context.Context, snapDir, childRootfs string) error {
	if err := os.MkdirAll(filepath.Dir(d.spec.LogPath), 0o755); err != nil {
		return err
	}
	// FC's --log-path requires the target file to already exist.
	if f, err := os.OpenFile(d.spec.LogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
		_ = f.Close()
	}
	_ = os.Remove(d.spec.SocketPath)

	// Open console capture (same pattern as Start).
	var stdout, stderr io.Writer = os.Stdout, os.Stderr
	if d.spec.ConsolePath != "" {
		f, err := os.OpenFile(d.spec.ConsolePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			return fmt.Errorf("open console: %w", err)
		}
		d.console = f
		stdout = io.MultiWriter(os.Stdout, f)
		stderr = io.MultiWriter(os.Stderr, f)
	}

	// Decide the memory backend up front and, when it's UFFD, stand the
	// handler up CONCURRENTLY with FC fork+exec + the API-socket wait. The
	// setup (header read, resolver, shared-cache attach, UDS listen) is pure
	// host-side IO with no dependency on the FC process — FC only connects to
	// the handoff socket during /snapshot/load. Hugepage-backed snapshots
	// (marker file in snapDir) can ONLY restore via UFFD — firecracker
	// rejects mem_file_path for them — so the marker forces the UFFD path
	// regardless of the streaming env gate.
	if snapshotHasHugepages(snapDir) {
		d.hugepages = true
	}
	uffdCh := d.beginUffdRestoreAsync(snapDir)

	cmd := exec.Command("/usr/local/bin/firecracker",
		"--api-sock", d.spec.SocketPath,
		"--log-path", d.spec.LogPath,
		"--level", "Info",
	)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		drainUffdSetup(d, uffdCh)
		return fmt.Errorf("spawn firecracker: %w", err)
	}
	d.proc = cmd.Process

	// Wait for API socket to appear (FC creates it after a few ms).
	hc := newUnixHTTP(d.spec.SocketPath)
	waitFCSocket(hc, d.spec.SocketPath)

	// PUT /snapshot/load with network_overrides. The memory backend is either
	// the whole-file path (default) or the userfaultfd handoff socket set up
	// above that streams pages on demand.
	loadBody := map[string]any{
		"snapshot_path":     filepath.Join(snapDir, "vm.state"),
		"resume_vm":         false,
		"network_overrides": []map[string]string{{"iface_id": "1", "host_dev_name": d.spec.Network.TAP}},
	}
	if uffdCh != nil {
		r := <-uffdCh
		if r.err != nil {
			_ = cmd.Process.Kill()
			return fmt.Errorf("uffd restore setup: %w", r.err)
		}
		loadBody["mem_backend"] = map[string]any{"backend_type": "Uffd", "backend_path": r.sock}
		// Start the fault loop BEFORE issuing /snapshot/load. Firecracker
		// reads guest memory while restoring device + vCPU state DURING the
		// load, which page-faults; those faults block inside the load call
		// until the UFFD handler services them. Serving only after load
		// returns deadlocks (FC parks in Dl, load times out). The Serve
		// goroutine blocks on Accept until FC connects mid-load, then
		// streams the load-time faults concurrently.
		d.serveUffd()
	} else {
		loadBody["mem_file_path"] = filepath.Join(snapDir, "vm.mem")
	}
	if err := putJSON(hc, "/snapshot/load", loadBody); err != nil {
		d.closeUffd()
		_ = cmd.Process.Kill()
		return fmt.Errorf("snapshot/load: %w", err)
	}

	// PATCH /drives/rootfs with new path
	patchDrive := map[string]any{
		"drive_id":     "rootfs",
		"path_on_host": childRootfs,
	}
	if err := patchJSON(hc, "/drives/rootfs", patchDrive); err != nil {
		_ = cmd.Process.Kill()
		return fmt.Errorf("patch rootfs: %w", err)
	}

	// PATCH /vm state=Resumed
	if err := patchJSON(hc, "/vm", map[string]string{"state": "Resumed"}); err != nil {
		_ = cmd.Process.Kill()
		return fmt.Errorf("resume: %w", err)
	}
	return nil
}

// StartFromSnapNoDrive is the template-snapshot restore path. Like
// StartWarmFork it bypasses the SDK to access /snapshot/load's
// network_overrides, but it leaves the VM PAUSED so the caller can do an
// out-of-band reconfig before resuming, and it skips the /drives/rootfs
// PATCH so callers can parallelize the (slow) per-VM rootfs copy with FC
// process startup + /snapshot/load. The VM ends in a loaded-paused state;
// call PatchRootfs(child) once the file exists, then Resume(). When the
// snapshot's vsock UDS path is baked in, the caller is responsible for
// serializing concurrent restores on that path (see sandbox.tplLock).
func (d *Driver) StartFromSnapNoDrive(ctx context.Context, snapDir, hostTAP string) error {
	if err := os.MkdirAll(filepath.Dir(d.spec.LogPath), 0o755); err != nil {
		return err
	}
	if f, err := os.OpenFile(d.spec.LogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
		_ = f.Close()
	}
	_ = os.Remove(d.spec.SocketPath)

	var stdout, stderr io.Writer = os.Stdout, os.Stderr
	if d.spec.ConsolePath != "" {
		f, err := os.OpenFile(d.spec.ConsolePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			return fmt.Errorf("open console: %w", err)
		}
		d.console = f
		stdout = io.MultiWriter(os.Stdout, f)
		stderr = io.MultiWriter(os.Stderr, f)
	}

	// Hugepage-backed snapshots can ONLY restore via UFFD (FC rejects
	// mem_file_path); the marker forces it regardless of the streaming gate.
	// The UFFD handler setup is pure host-side IO (no FC dependency — FC only
	// connects to the handoff socket during /snapshot/load), so it runs
	// concurrently with FC fork+exec + the API-socket wait.
	if snapshotHasHugepages(snapDir) {
		d.hugepages = true
	}
	uffdCh := d.beginUffdRestoreAsync(snapDir)

	cmd := fcExecCommand(d.spec.Netns, d.spec.VsockBindDir,
		"/usr/local/bin/firecracker",
		"--api-sock", d.spec.SocketPath,
		"--log-path", d.spec.LogPath,
		"--level", "Info",
	)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		drainUffdSetup(d, uffdCh)
		return fmt.Errorf("spawn firecracker: %w", err)
	}
	d.proc = cmd.Process

	hc := newUnixHTTP(d.spec.SocketPath)
	waitFCSocket(hc, d.spec.SocketPath)

	loadBody := map[string]any{
		"snapshot_path":     filepath.Join(snapDir, "vm.state"),
		"resume_vm":         false,
		"network_overrides": []map[string]string{{"iface_id": "1", "host_dev_name": hostTAP}},
	}
	if uffdCh != nil {
		r := <-uffdCh
		if r.err != nil {
			_ = cmd.Process.Kill()
			return fmt.Errorf("uffd restore setup: %w", r.err)
		}
		loadBody["mem_backend"] = map[string]any{"backend_type": "Uffd", "backend_path": r.sock}
		// Serve faults BEFORE /snapshot/load: FC faults guest pages while
		// restoring device/vCPU state mid-load and blocks until they are
		// serviced. Starting Serve after the load deadlocks (FC in Dl, load
		// times out). Accept blocks until FC connects mid-load, then the
		// load-time faults stream concurrently.
		d.serveUffd()
	} else {
		loadBody["mem_file_path"] = filepath.Join(snapDir, "vm.mem")
	}
	if err := putJSON(hc, "/snapshot/load", loadBody); err != nil {
		d.closeUffd()
		_ = cmd.Process.Kill()
		return fmt.Errorf("snapshot/load: %w", err)
	}
	return nil
}

// uffdSetupResult is what beginUffdRestoreAsync delivers: the handoff socket
// path for /snapshot/load's Uffd backend, or the setup error.
type uffdSetupResult struct {
	sock string
	err  error
}

// beginUffdRestoreAsync runs beginUffdRestore in a goroutine when this restore
// needs the UFFD backend (streaming enabled or hugepage snapshot), returning a
// 1-buffered channel that delivers exactly one result. Returns nil when the
// plain mem_file_path backend applies. Callers MUST receive from a non-nil
// channel before touching d.uffd (serveUffd/closeUffd) — the receive is the
// happens-before edge for the goroutine's write to d.uffd.
func (d *Driver) beginUffdRestoreAsync(snapDir string) chan uffdSetupResult {
	if !streamRestoreEnabled() && !d.hugepages {
		return nil
	}
	ch := make(chan uffdSetupResult, 1)
	go func() {
		sock, err := d.beginUffdRestore(snapDir)
		ch <- uffdSetupResult{sock: sock, err: err}
	}()
	return ch
}

// drainUffdSetup joins an in-flight beginUffdRestoreAsync and tears down
// whatever it built. Used on failure paths that abort before /snapshot/load.
func drainUffdSetup(d *Driver, ch chan uffdSetupResult) {
	if ch == nil {
		return
	}
	if r := <-ch; r.err == nil {
		d.closeUffd()
	}
}

// waitFCSocket polls for the freshly-exec'd firecracker process's API socket
// to come up, returning as soon as /version answers. FC creates the socket
// within single-digit milliseconds of exec, so the poll is tight (1ms): a
// coarser interval would add its full granularity to every create's critical
// path. Timeboxed; on timeout the caller's next API call surfaces the error.
func waitFCSocket(hc *http.Client, sockPath string) {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sockPath); err == nil {
			if r, err := hc.Get("http://unix/version"); err == nil {
				_ = r.Body.Close()
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
}

// PatchRootfs switches the loaded snapshot's rootfs drive to childRootfs.
// Must be called after StartFromSnapNoDrive and before Resume.
func (d *Driver) PatchRootfs(ctx context.Context, childRootfs string) error {
	return d.PatchDrive(ctx, "rootfs", childRootfs)
}

// PatchDrive repoints a drive that already exists in the loaded snapshot to a
// new host path. Firecracker snapshots freeze the device topology, so the
// drive_id MUST have been present when the snapshot was taken (rootfs always
// is; a data drive such as "vdb" must be baked into the template snapshot).
// Must be called after StartFromSnapNoDrive and before Resume.
//
// This is how the durable-volume path attaches a per-database ext4 image: the
// template snapshot carries a placeholder "vdb" drive, and at restore we patch
// it to point at the database's own image before resuming.
func (d *Driver) PatchDrive(ctx context.Context, driveID, pathOnHost string) error {
	hc := newUnixHTTP(d.spec.SocketPath)
	patchDrive := map[string]any{
		"drive_id":     driveID,
		"path_on_host": pathOnHost,
	}
	if err := patchJSON(hc, "/drives/"+driveID, patchDrive); err != nil {
		if d.proc != nil {
			_ = d.proc.Kill()
		}
		return fmt.Errorf("patch drive %q: %w", driveID, err)
	}
	return nil
}

// PatchDriveLive re-issues PATCH /drives/{id} on a RUNNING VM. Firecracker
// re-opens the backing file and raises a virtio config-change interrupt, so
// the guest re-reads the (possibly larger) device capacity — this is the
// documented live disk-resize flow (grow file → PATCH same path → in-guest
// resize2fs). Unlike PatchDrive (restore path), a failure here must NOT kill
// the firecracker process: the VM is healthy and serving traffic; we just
// report the error and let the caller retry on the next sweep.
func (d *Driver) PatchDriveLive(ctx context.Context, driveID, pathOnHost string) error {
	hc := newUnixHTTP(d.spec.SocketPath)
	body := map[string]any{
		"drive_id":     driveID,
		"path_on_host": pathOnHost,
	}
	if err := patchJSON(hc, "/drives/"+driveID, body); err != nil {
		return fmt.Errorf("live patch drive %q: %w", driveID, err)
	}
	return nil
}

// fcExecCommand returns an *exec.Cmd to run firecracker. If nsName is
// non-empty, the command is wrapped with `ip netns exec <nsName>` to
// place firecracker inside the named netns (NAT-identity restore path).
//
// If vsockBindDir is non-empty, the command is additionally wrapped with
// `unshare --mount --propagation private` and a tiny shell that bind-mounts
// vsockBindDir over BakedVsockDir BEFORE exec'ing firecracker. This gives the
// restored VM a per-sandbox inode for the (frozen, baked) vsock UDS path, so
// concurrent NATID restores never collide on the single baked path — no
// per-template restore lock required. The mount lives only in firecracker's
// private mount namespace and vanishes when it exits.
//
// Setpgid=true puts the process (and any children it forks, like the
// firecracker child of "ip netns exec") into a new process group. FastStop
// then kills the entire group with -PGID so the orphaned firecracker child
// is also killed before we tear down the netns.
func fcExecCommand(nsName, vsockBindDir, bin string, args ...string) *exec.Cmd {
	// Innermost command is always `bin args...`. When a per-sandbox vsock
	// bind dir is requested, wrap it in an unshared mount namespace that
	// bind-mounts the per-VM dir over the baked dir, then exec's firecracker
	// so it replaces the shell (preserving Setpgid/process-group semantics).
	var head string
	var rest []string
	if vsockBindDir != "" {
		script := fmt.Sprintf(
			"mkdir -p %s %s && mount --bind %s %s && exec %s",
			shArg(BakedVsockDir), shArg(vsockBindDir),
			shArg(vsockBindDir), shArg(BakedVsockDir),
			shArg(bin),
		)
		for _, a := range args {
			script += " " + shArg(a)
		}
		head = "unshare"
		rest = []string{"--mount", "--propagation", "private", "sh", "-c", script}
	} else {
		head = bin
		rest = args
	}

	var cmd *exec.Cmd
	if nsName == "" {
		cmd = exec.Command(head, rest...)
	} else {
		full := append([]string{"netns", "exec", nsName, head}, rest...)
		cmd = exec.Command("ip", full...)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd
}

// shArg single-quotes a string for safe interpolation into the `sh -c`
// bind-mount script. Mirrors the shellQuote used elsewhere; paths here are
// agent-controlled (dirs + the firecracker bin/args), never guest input.
func shArg(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// --- HTTP-over-unix-socket helpers -----------------------------------------

func newUnixHTTP(sock string) *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", sock)
			},
		},
	}
}

// newUnixHTTPLong is like newUnixHTTP but with a generous timeout for
// operations that are bounded by disk IO and guest memory size, namely
// snapshot create. With 4-8 GB guests on commodity SSDs the snapshot can
// take 30-40s; we allow ~3x headroom before failing the agent.
func newUnixHTTPLong(sock string) *http.Client {
	return &http.Client{
		Timeout: 120 * time.Second,
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", sock)
			},
		},
	}
}

func putJSON(c *http.Client, path string, body any) error {
	return doJSON(c, http.MethodPut, path, body)
}
func patchJSON(c *http.Client, path string, body any) error {
	return doJSON(c, http.MethodPatch, path, body)
}
func doJSON(c *http.Client, method, path string, body any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(method, "http://unix"+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		buf, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, string(buf))
	}
	return nil
}
