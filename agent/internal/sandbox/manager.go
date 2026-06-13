// SPDX-License-Identifier: Apache-2.0
// Package sandbox is the high-level lifecycle manager for microVMs.
package sandbox

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/pandastack/agent/internal/config"
	"github.com/pandastack/agent/internal/events"
	"github.com/pandastack/agent/internal/firecracker"
	"github.com/pandastack/agent/internal/guest"
	"github.com/pandastack/agent/internal/network"
	"github.com/pandastack/agent/internal/obs"
	"github.com/pandastack/agent/internal/seed"
	"github.com/pandastack/agent/internal/snapstore"
	"github.com/pandastack/agent/internal/store"
)

type Status string

const (
	StatusCreating   Status = "creating"
	StatusRunning    Status = "running"
	StatusPaused     Status = "paused"
	StatusStopping   Status = "stopping"
	StatusDeleted    Status = "deleted"
	StatusFailed     Status = "failed"
	StatusHibernated Status = "hibernated"
)

// pgManagedTemplate is the template name for managed PostgreSQL sandboxes.
// These use phased boot: Phase 1 bootstraps PG (snapshotted), Phase 2
// injects per-sandbox credentials (delivered by the agent on every start).
const pgManagedTemplate = "postgres-16"

type Sandbox struct {
	ID        string            `json:"id"`
	Template  string            `json:"template"`
	CPU       int               `json:"cpu"`
	MemoryMB  int               `json:"memory_mb"`
	Status    Status            `json:"status"`
	GuestIP   string            `json:"guest_ip"`
	HostTAP   string            `json:"host_tap"`
	MAC       string            `json:"mac"`
	VsockCID  uint32            `json:"vsock_cid"`
	Metadata  map[string]string `json:"metadata,omitempty"`
	FromSnap  string            `json:"from_snapshot,omitempty"`
	BootMS    int64             `json:"boot_ms"`
	BootMode  string            `json:"boot_mode,omitempty"` // "cold" | "warm" | "snapshot"
	CreatedAt time.Time         `json:"created_at"`
}

type VolumeMount struct {
	Name     string `json:"name"`
	ReadOnly bool   `json:"read_only,omitempty"`
}

type CreateRequest struct {
	// ID pins the sandbox ID instead of generating one. In-process use only
	// (database failover restores recreate the DB under its original ID so
	// connection strings and the GCS archive layout survive); deliberately
	// NOT settable through the agent HTTP API (json:"-").
	ID           string            `json:"-"`
	Template     string            `json:"template"`
	CPU          int               `json:"cpu"`
	MemoryMB     int               `json:"memory_mb"`
	DiskGB       int               `json:"disk_gb,omitempty"`
	FromSnapshot string            `json:"from_snapshot,omitempty"`
	Metadata     map[string]string `json:"metadata,omitempty"`
	Volumes      []VolumeMount     `json:"volumes,omitempty"`
	TTLSeconds   *int              `json:"ttl_seconds,omitempty"`
	Persistent   bool              `json:"persistent,omitempty"`
}

type Manager struct {
	cfg     config.Config
	store   *store.Store
	netPool *network.Pool
	keys    *guest.KeyStore
	bus     *events.Bus
	log     *slog.Logger

	mu      sync.RWMutex
	drivers map[string]*firecracker.Driver
	guests  map[string]*guest.Client

	actMu        sync.Mutex
	lastActivity map[string]time.Time
	// activeTunnels counts live pg-tunnel connections per sandbox. While
	// non-zero the idle sweeper must not hibernate the sandbox: a pooled
	// client can hold a quiet-but-open connection for hours, and cutting
	// it mid-session would surface as a random "connection reset" to the
	// application. Guarded by actMu alongside lastActivity.
	activeTunnels map[string]int

	kernelCache cachedKernel

	prefetcher *templatePrefetcher
	cpuPinner  *cpuPinner

	lifecycle *lifecycleStore
	reaper    *reaper

	leases       LeaseSink
	leaseAgentID string
	leaseTTL     time.Duration

	snapStore *snapstore.Store

	seedStore *seed.Store

	ch      CHSink
	agentID string

	dmsnap *DMSnapManager

	walRelay *WALRelay
}

func NewManager(cfg config.Config, st *store.Store, np *network.Pool, ks *guest.KeyStore, bus *events.Bus, log *slog.Logger) *Manager {
	defaultTTL := envDurationSeconds("PANDASTACK_DEFAULT_TTL_SECONDS", 5*time.Minute)
	reaperInterval := envDurationSeconds("PANDASTACK_REAPER_INTERVAL_SECONDS", 30*time.Second)
	lc := newLifecycleStore(defaultTTL)
	m := &Manager{
		cfg:           cfg,
		store:         st,
		netPool:       np,
		keys:          ks,
		bus:           bus,
		log:           log,
		drivers:       make(map[string]*firecracker.Driver),
		guests:        make(map[string]*guest.Client),
		lastActivity:  make(map[string]time.Time),
		activeTunnels: make(map[string]int),
		lifecycle:     lc,
		snapStore:     snapstore.NewFromEnv(),
		seedStore:     seed.NewFromEnv(),
	}
	m.prefetcher = newTemplatePrefetcher(cfg.DataDir, m.keys.Fingerprint(), log)
	m.cpuPinner = newCPUPinner(log)
	// Prime any templates that are already built on disk so the first cold
	// restore after agent boot doesn't pay disk-read latency.
	if m.prefetcher != nil {
		go func() {
			tmpls, _ := listReadyTemplates(cfg.DataDir)
			for _, t := range tmpls {
				m.prefetcher.PrefetchTemplate(context.Background(), t)
			}
			m.prefetcher.Start()
		}()
	}
	m.startNATIDPrewarmer()
	BakeStartupFromEnv(m, log)

	// dm-snapshot CoW rootfs (Option B). Eliminates per-sandbox rootfs copy:
	// ~2ms dm device creation replaces 100-400ms file copy on ext4.
	// Disabled via PANDASTACK_DMSNAP=0.
	if os.Getenv("PANDASTACK_DMSNAP") != "0" {
		m.dmsnap = NewDMSnapManager(func(msg string, args ...any) {
			log.Info(msg, args...)
		})
		if m.dmsnap.Enabled() {
			// Remove any pdssnap-* devices from a previous (crashed) agent run.
			m.dmsnap.CleanupStale()
			// Attach base loops for all templates that are already baked.
			go func() {
				tmpls, _ := listReadyTemplates(cfg.DataDir)
				for _, t := range tmpls {
					rootfsPath := dmsnapBaseRootfs(cfg.DataDir, t)
					if rootfsPath == "" {
						continue
					}
					if err := m.dmsnap.InitBase(t, rootfsPath); err != nil {
						log.Warn("dmsnap InitBase failed (non-fatal)", "template", t, "err", err)
					}
				}
			}()
		}
	}

	m.reaper = newReaper(m, lc, reaperInterval, log)
	m.reaper.Start(context.Background())
	// Background pre-seed: make every public template fast "from second zero"
	// (seed-first, cold-bake only what the fleet hasn't published yet).
	go m.preseedPublicTemplates()
	return m
}

// dmsnapBaseRootfs returns the path to the read-only clone rootfs for a template.
// This is the file we attach as the dm-snapshot base loop.
// Returns "" if neither clone.ext4 nor the build-vm rootfs exists.
func dmsnapBaseRootfs(dataDir, template string) string {
	snapDir := templateSnapDir(dataDir, template)
	clone := filepath.Join(snapDir, "clone.ext4")
	if _, err := os.Stat(clone); err == nil {
		return clone
	}
	fallback := filepath.Join(snapDir, "build-vm", "rootfs.ext4")
	if _, err := os.Stat(fallback); err == nil {
		return fallback
	}
	return ""
}

// Bus exposes the event bus for API handlers.
func (m *Manager) Bus() *events.Bus { return m.bus }

// RegistrySnapshot returns aggregated process-level stats used by the agent's
// registry heartbeat. Numbers are best-effort and only used by the api
// scheduler to compare agents against each other, so absolute accuracy is not
// required.
func (m *Manager) RegistrySnapshot() (cpuUsed, memUsedMB, sandboxes int) {
	m.mu.RLock()
	sandboxes = len(m.drivers)
	m.mu.RUnlock()
	// Approximate: each sandbox costs 1 vCPU and 512 MB on average. Real
	// values come from per-sandbox CPU/Mem fields when available; until we
	// thread those through this struct is intentionally cheap.
	cpuUsed = sandboxes
	memUsedMB = sandboxes * 512
	return
}

func (m *Manager) Shutdown() {
	if m.reaper != nil {
		m.reaper.Stop()
	}
	if m.dmsnap != nil {
		m.dmsnap.Shutdown()
	}
}

// LifecycleInfo is the public lifecycle configuration for a sandbox.
type LifecycleInfo struct {
	TTLSeconds  int64 `json:"ttl_seconds"`
	Persistent  bool  `json:"persistent"`
	IdleSeconds int64 `json:"idle_seconds"`
}

func (m *Manager) Lifecycle(ctx context.Context, id string) (LifecycleInfo, error) {
	if err := m.ensureLifecycleState(ctx, id); err != nil {
		return LifecycleInfo{}, err
	}
	st, _ := m.lifecycle.Get(id)
	return LifecycleInfo{
		TTLSeconds:  int64(st.ttl / time.Second),
		Persistent:  st.persistent,
		IdleSeconds: int64(time.Since(m.lastActivityFor(id)) / time.Second),
	}, nil
}

func (m *Manager) UpdateLifecycle(id string, ttl *int, persistent *bool) error {
	if err := m.ensureLifecycleState(context.Background(), id); err != nil {
		return err
	}
	if ttl != nil {
		if *ttl < 0 {
			return errors.New("ttl_seconds must be non-negative")
		}
		m.lifecycle.SetTTL(id, time.Duration(*ttl)*time.Second)
	}
	if persistent != nil {
		m.lifecycle.SetPersistent(id, *persistent)
	}
	// Persist the merged lifecycle state so it survives an agent restart.
	if m.store != nil {
		st, _ := m.lifecycle.Get(id)
		if err := m.store.SetSandboxLifecycle(context.Background(), id, st.persistent, int64(st.ttl/time.Second)); err != nil {
			m.log.Warn("persist sandbox lifecycle failed (non-fatal)", "id", id, "err", err)
		}
	}
	return nil
}

func (m *Manager) ensureLifecycleState(ctx context.Context, id string) error {
	if id == "" {
		return errors.New("sandbox not found")
	}
	if _, ok := m.lifecycle.Get(id); ok {
		return nil
	}
	sb, err := m.store.GetSandbox(ctx, id)
	if err != nil {
		return err
	}
	if sb == nil {
		return errors.New("sandbox not found")
	}
	_, tmpl := extractWorkspaceTemplate(sb)
	ttl, persistent := m.rehydrateLifecycle(ctx, id, tmpl)
	m.lifecycle.Set(id, ttl, persistent)
	return nil
}

// rehydrateLifecycle computes the lifecycle TTL + persistent flag for a sandbox
// after an agent restart. It prefers the persisted lifecycle columns so an
// explicitly-persistent sandbox (e.g. an app) is not downgraded to the default
// TTL, and always treats durable templates (managed databases) as persistent as
// a belt-and-braces guard.
func (m *Manager) rehydrateLifecycle(ctx context.Context, id, template string) (time.Duration, bool) {
	ttl := m.lifecycle.defaultTTL
	persistent := isDurableTemplate(template)
	if m.store != nil {
		if p, ttlSec, found, lerr := m.store.GetSandboxLifecycle(ctx, id); lerr == nil && found {
			persistent = persistent || p
			if ttlSec > 0 {
				ttl = time.Duration(ttlSec) * time.Second
			}
		}
	}
	return ttl, persistent
}

func (m *Manager) setLifecycleForCreate(id string, req CreateRequest) {
	if m.lifecycle == nil {
		return
	}
	ttl := m.lifecycle.defaultTTL
	if req.TTLSeconds != nil {
		ttl = time.Duration(*req.TTLSeconds) * time.Second
	}
	// Managed databases (postgres-16) own a durable /dev/vdb volume and must
	// always be persistent so the idle reaper never deletes them — regardless
	// of whether the caller passed persistent:true. Honour an explicit
	// persistent:true for any other template too.
	persistent := req.Persistent || isDurableTemplate(req.Template)
	m.lifecycle.Set(id, ttl, persistent)
	// Persist lifecycle config so an agent restart can rehydrate it instead of
	// downgrading an explicitly-persistent sandbox back to the default TTL.
	// Best-effort: the in-memory store is the source of truth at runtime.
	if m.store != nil {
		if err := m.store.SetSandboxLifecycle(context.Background(), id, persistent, int64(ttl/time.Second)); err != nil {
			m.log.Warn("persist sandbox lifecycle failed (non-fatal)", "id", id, "err", err)
		}
	}
}

// isDurableTemplate reports whether sandboxes of this template own durable
// state (a per-database /dev/vdb volume) and must therefore always be treated
// as persistent: the idle reaper must never delete them, and an agent restart
// must not downgrade them to ephemeral. Managed databases are the current
// members — exactly the phased-boot templates.
func isDurableTemplate(template string) bool {
	return phasedBootTemplates[template]
}

// MarkActivity bumps the last-activity timestamp for a sandbox. Called by the
// router's middleware on every sandbox-scoped request.
func (m *Manager) MarkActivity(id string) {
	if id == "" {
		return
	}
	m.actMu.Lock()
	m.lastActivity[id] = time.Now()
	m.actMu.Unlock()
}

// TunnelOpened registers a live pg-tunnel connection against a sandbox.
// While the count is non-zero the idle sweeper will not hibernate it, so a
// connection that is open but quiet (connection-pooled apps idle for hours)
// is never cut mid-session by scale-to-zero.
func (m *Manager) TunnelOpened(id string) {
	if id == "" {
		return
	}
	m.actMu.Lock()
	m.activeTunnels[id]++
	m.lastActivity[id] = time.Now()
	m.actMu.Unlock()
}

// TunnelClosed unregisters a pg-tunnel connection and restarts the idle
// clock, so the scale-to-zero countdown begins at client disconnect — not
// at the last HTTP request that happened to precede a long-lived session.
func (m *Manager) TunnelClosed(id string) {
	if id == "" {
		return
	}
	m.actMu.Lock()
	if n := m.activeTunnels[id]; n <= 1 {
		delete(m.activeTunnels, id)
	} else {
		m.activeTunnels[id] = n - 1
	}
	m.lastActivity[id] = time.Now()
	m.actMu.Unlock()
}

// ActiveTunnels reports the number of live pg-tunnel connections for id.
func (m *Manager) ActiveTunnels(id string) int {
	m.actMu.Lock()
	defer m.actMu.Unlock()
	return m.activeTunnels[id]
}

// EnsureRunning transparently wakes a hibernated sandbox before its next
// request lands. Returns nil if the sandbox is running, hibernation is off,
// or the sandbox isn't ours. Errors only surface if the wake itself fails.
//
// Cheap fast path: an RLock map lookup. The wake itself only runs on the
// first request after a hibernation cycle. Subsequent requests see the
// driver in place and return immediately.
//
// Source of truth: presence of the hibernation snapshot file on this host.
// We deliberately do NOT consult store status, because the row may be
// stale across edges/agents (especially after a manual hibernate where
// the response races the cache invalidation).
func (m *Manager) EnsureRunning(ctx context.Context, id string) error {
	if id == "" {
		return nil
	}
	if m.driver(id) != nil {
		return nil
	}
	hiberSnap := filepath.Join(m.cfg.DataDir, "vms", id, "hibernation", "vm.state")
	if _, err := os.Stat(hiberSnap); err != nil {
		return nil // nothing to wake — sandbox isn't ours or never hibernated here
	}
	m.log.Info("auto-wake on request", "id", id)
	return m.Wake(ctx, id)
}

// lastActivityFor returns the last-activity timestamp, defaulting to now.
func (m *Manager) lastActivityFor(id string) time.Time {
	m.actMu.Lock()
	defer m.actMu.Unlock()
	t, ok := m.lastActivity[id]
	if !ok {
		t = time.Now()
		m.lastActivity[id] = t
	}
	return t
}

// DataDir exposes the agent's data root (used by templates/logs handlers).
func (m *Manager) DataDir() string { return m.cfg.DataDir }

// UploadUserTemplate publishes a workspace-owned template build to GCS for
// durability (see seed.Store.UploadUserTemplate). The build handler treats an
// error as a failed build; a no-op nil is returned when GCS replication is
// not configured (local/dev single-host agents).
func (m *Manager) UploadUserTemplate(ctx context.Context, p seed.UserTemplateParams) error {
	p.DataDir = m.cfg.DataDir
	return m.seedStore.UploadUserTemplate(ctx, p)
}

// DeleteUserTemplateGCS removes the durable bucket copy of a workspace-owned
// template (see seed.Store.DeleteUserTemplate). The DELETE handler calls this
// BEFORE removing local files so a successful delete never orphans the bucket
// copy; idempotent when nothing is published, no-op without GCS config.
func (m *Manager) DeleteUserTemplateGCS(ctx context.Context, workspace, template string) error {
	return m.seedStore.DeleteUserTemplate(ctx, workspace, template)
}

// Driver returns the firecracker driver for sandbox id, if known.
func (m *Manager) Driver(id string) *firecracker.Driver { return m.driver(id) }

// Guest returns (and lazily opens) an SSH client for sandbox id.
func (m *Manager) Guest(id string) (*guest.Client, error) {
	m.mu.RLock()
	c := m.guests[id]
	m.mu.RUnlock()
	if c != nil {
		return c, nil
	}
	sbAny, err := m.store.GetSandbox(context.Background(), id)
	if err != nil || sbAny == nil {
		return nil, errors.New("sandbox not found")
	}
	row, _ := sbAny.(map[string]any)
	ip, _ := row["guest_ip"].(string)
	if ip == "" {
		return nil, errors.New("sandbox has no guest_ip")
	}
	c = guest.NewClient(ip, "root", m.keys.Signer())
	// Phase-2 vsock fast-path: opt-in via PANDASTACK_VSOCK_EXEC. When enabled,
	// the client tries the in-guest pandastack-daemon over the per-sandbox
	// Firecracker vsock UDS and transparently falls back to SSH on any
	// transport failure. OFF by default → zero behaviour change.
	//
	// The host-visible UDS location depends on the spawn path that created the
	// VM:
	//   - NATID restore (startFromTemplateSnapNATID) bakes the device at the
	//     FIXED firecracker.BakedVsockPath and bind-mounts a per-sandbox dir
	//     (vmDir/vsock) over BakedVsockDir inside FC's private mount namespace,
	//     so the socket surfaces on the host at vmDir/vsock/<BakedVsockName>.
	//   - cold boot / non-NATID snapshot restore attach the device directly at
	//     vmDir/fc-vsock.sock (no bind-mount).
	// We prefer the NATID bind path when present, else the legacy per-VM path.
	// dialVsock os.Stat's the UDS and cleanly falls back to SSH if absent, so a
	// wrong/missing choice is non-fatal.
	if vsockExecEnabled() {
		vmDir := filepath.Join(m.cfg.DataDir, "vms", id)
		uds := filepath.Join(vmDir, "vsock", firecracker.BakedVsockName)
		if _, err := os.Stat(uds); err != nil {
			uds = filepath.Join(vmDir, "fc-vsock.sock")
		}
		c.EnableVsock(uds)
	}
	m.mu.Lock()
	m.guests[id] = c
	m.mu.Unlock()
	return c, nil
}

// vsockExecEnabled reports whether the Phase-1 vsock fast-path is opt-in via
// the PANDASTACK_VSOCK_EXEC environment flag. Accepts 1/true/yes/on.
func vsockExecEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("PANDASTACK_VSOCK_EXEC"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// Recover re-attaches after agent restart.
// MVP semantics: detect orphaned sandboxes whose firecracker process is gone
// and mark them failed (releasing their network allocation). True re-attach
// to live PIDs would require the firecracker-go-sdk to support it.
func (m *Manager) Recover(ctx context.Context) error {
	all, err := m.store.ListSandboxes(ctx)
	if err != nil {
		return err
	}
	live := make(map[string]struct{}, len(all))
	for _, raw := range all {
		row, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		id, _ := row["id"].(string)
		status, _ := row["status"].(string)
		tmpl, _ := row["template"].(string)
		// Only consider non-terminal sandboxes as "live" for vmDir GC
		// purposes. Failed sandboxes' working dirs are leaked disk and
		// should be reclaimed; users can recreate from the API.
		if id != "" && status != string(StatusFailed) {
			live[id] = struct{}{}
		}
		if id != "" && status == string(StatusHibernated) && m.lifecycle != nil {
			ttl, persistent := m.rehydrateLifecycle(ctx, id, tmpl)
			m.lifecycle.Set(id, ttl, persistent)
			m.MarkActivity(id)
		}
		if status != string(StatusRunning) && status != string(StatusPaused) {
			continue
		}
		// Phase 2 always-on: if a hibernation snapshot exists for this
		// sandbox, the prior process either hibernated it cleanly (Phase 1
		// shutdown) or crashed mid-write. In either case the safe answer is
		// to mark it hibernated — Wake() will validate the snapshot when the
		// next request arrives. The customer's rootfs is intact under
		// vmDir/rootfs.ext4; worst case is a wake-time error if vm.mem is
		// torn, which is still recoverable (delete and recreate; we do NOT
		// auto-delete here).
		hiberDir := filepath.Join(m.cfg.DataDir, "vms", id, "hibernation")
		if _, err := os.Stat(filepath.Join(hiberDir, "vm.mem")); err == nil {
			m.log.Info("recover: hibernation snapshot present; restoring as hibernated", "id", id)
			_ = m.store.SetStatus(ctx, id, string(StatusHibernated))
			if m.lifecycle != nil {
				ttl, persistent := m.rehydrateLifecycle(ctx, id, tmpl)
				m.lifecycle.Set(id, ttl, persistent)
				m.MarkActivity(id)
			}
			if m.bus != nil {
				m.bus.Emit(id, "recover.hibernated", map[string]any{"snap_dir": hiberDir})
			}
			// Release the FC unix socket if a stale one survived; Wake()
			// will create a fresh one. netPool stays bound to the sandbox.
			sockStale := filepath.Join("/tmp", fmt.Sprintf("fc-%s.socket", id))
			_ = os.Remove(sockStale)
			continue
		}
		// Sandbox claims to be alive but we have no driver: check if firecracker
		// socket still exists. If not, the process is gone.
		sock := filepath.Join("/tmp", fmt.Sprintf("fc-%s.socket", id))
		if _, err := os.Stat(sock); err != nil {
			m.log.Info("recover: marking orphaned sandbox failed", "id", id)
			_ = m.store.SetStatus(ctx, id, string(StatusFailed))
			_ = m.netPool.Release(ctx, id)
			if m.bus != nil {
				m.bus.Emit(id, "recover.orphaned", map[string]any{"prev_status": status})
			}
			continue
		}
		// Process still alive but unmanaged — we can't drive it without an SDK
		// handle. Mark failed so the user deletes + recreates.
		m.log.Warn("recover: live firecracker without SDK handle; marking failed", "id", id)
		_ = m.store.SetStatus(ctx, id, string(StatusFailed))
		if m.bus != nil {
			m.bus.Emit(id, "recover.unmanaged", map[string]any{"socket": sock})
		}
	}
	// GC orphan vm directories: any subdir of /var/lib/pandastack/vms whose name
	// isn't in the DB is leaked disk. Reclaim it (these are often 100s of MB).
	vmsRoot := filepath.Join(m.cfg.DataDir, "vms")
	entries, err := os.ReadDir(vmsRoot)
	if err == nil {
		var reclaimed int
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			if _, ok := live[e.Name()]; ok {
				continue
			}
			path := filepath.Join(vmsRoot, e.Name())
			if rmErr := os.RemoveAll(path); rmErr == nil {
				reclaimed++
				m.log.Info("recover: gc orphan vm dir", "path", path)
			}
		}
		if reclaimed > 0 {
			m.log.Info("recover: reclaimed orphan vm dirs", "count", reclaimed)
		}
	}
	return nil
}

// startHealthMonitor watches the firecracker process for a sandbox and flips
// status to failed if it dies unexpectedly.
func (m *Manager) startHealthMonitor(id string, drv *firecracker.Driver) {
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			// If driver was removed (deleted/hibernated), stop watching.
			m.mu.RLock()
			current := m.drivers[id]
			m.mu.RUnlock()
			if current != drv {
				return
			}
			pid := drv.PID()
			if pid == 0 {
				continue // still starting / transient
			}
			if !pidAlive(pid) {
				m.log.Warn("firecracker died", "id", id, "pid", pid)
				m.mu.Lock()
				delete(m.drivers, id)
				gc := m.guests[id]
				delete(m.guests, id)
				m.mu.Unlock()
				if gc != nil {
					_ = gc.Close()
				}
				_ = m.store.SetStatus(context.Background(), id, string(StatusFailed))
				if m.bus != nil {
					m.bus.Emit(id, "vm.died", map[string]any{"pid": pid})
				}
				return
			}
		}
	}()
}

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	if _, err := os.Stat(fmt.Sprintf("/proc/%d", pid)); err != nil {
		return false
	}
	return true
}

// cachedKernel memoizes the kernel path lookup so we don't re-scan the
// filesystem on every Create() call. The result is invalidated if the file
// disappears. This shaves a few ms per cold boot but mostly keeps Create()
// off the disk's I/O path.
type cachedKernel struct {
	mu   sync.RWMutex
	path string
}

func (c *cachedKernel) get(dataDir string) (string, error) {
	c.mu.RLock()
	p := c.path
	c.mu.RUnlock()
	if p != "" {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.path != "" {
		if _, err := os.Stat(c.path); err == nil {
			return c.path, nil
		}
	}
	p, err := findKernel(dataDir)
	if err != nil {
		return "", err
	}
	c.path = p
	return p, nil
}

func (m *Manager) Create(ctx context.Context, req CreateRequest) (sb *Sandbox, err error) {
	ctx, span := obs.Tracer("pandastack-agent/sandbox").Start(ctx, "sandbox.Create",
		oteltrace.WithAttributes(
			attribute.String("sandbox.template", req.Template),
			attribute.String("sandbox.from_snapshot", req.FromSnapshot),
			attribute.Int("sandbox.cpu", req.CPU),
			attribute.Int("sandbox.memory_mb", req.MemoryMB),
		),
	)
	defer func() {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		} else if sb != nil {
			m.setLifecycleForCreate(sb.ID, req)
			m.acquireLease(ctx, sb.ID, sb.Workspace())
			span.SetAttributes(
				attribute.String("sandbox.id", sb.ID),
				attribute.String("sandbox.boot_mode", sb.BootMode),
				attribute.Int64("sandbox.boot_ms", sb.BootMS),
			)
		}
		span.End()
	}()
	if req.CPU == 0 {
		req.CPU = 1
	}
	if req.MemoryMB == 0 {
		req.MemoryMB = 256
	}
	if req.Template == "" && req.FromSnapshot == "" {
		return nil, errors.New("either template or from_snapshot must be provided")
	}
	if req.TTLSeconds != nil && *req.TTLSeconds < 0 {
		return nil, errors.New("ttl_seconds must be non-negative")
	}
	// Volumes can only be attached on a fresh boot: a snapshot restore
	// reproduces the snapshot's baked device topology and Firecracker can
	// only PatchDrive drive IDs that already exist in vm.state (postgres-16
	// bakes a "vol1" placeholder for exactly this reason). Reject instead
	// of silently dropping the mounts — a sandbox the caller believes has a
	// durable volume but doesn't is data loss waiting to happen.
	if req.FromSnapshot != "" && len(req.Volumes) > 0 {
		return nil, errors.New("volumes cannot be attached when restoring from a snapshot; create a fresh sandbox with volumes instead")
	}

	// Lazy pull on create-miss: the scheduler may route a create for a
	// workspace-owned template to an agent that never built it (or to a
	// fresh MIG replacement of the agent that did). The durable copy lives
	// in the bucket; materialise it locally before anything reads the
	// template's meta.json (the sizing block below) or its rootfs. The
	// per-template lock single-flights concurrent creates: the first one
	// downloads, the rest block on the lock and then see the files.
	// Public templates never hit this path — they are push-replicated by
	// CI + seed-sync, so a miss on them still fails fast below.
	var pullMS int64
	pulled := false
	if req.Template != "" && req.FromSnapshot == "" && m.seedStore.Enabled() {
		tplRootfs := filepath.Join(m.cfg.DataDir, "templates", req.Template, "rootfs.ext4")
		if _, statErr := os.Stat(tplRootfs); statErr != nil {
			if ws := req.Metadata["workspace"]; ws != "" {
				tlk := tplLock(req.Template)
				tlk.Lock()
				if _, statErr := os.Stat(tplRootfs); statErr != nil {
					tPull := time.Now()
					// Detached context: a client that gives up mid-pull must
					// not abort the download — the NEXT create would just pay
					// it all over again. Worst case the work completes for a
					// caller that already left, and the retry is instant.
					pctx, pcancel := context.WithTimeout(context.Background(), 10*time.Minute)
					gen, perr := m.seedStore.PullUserTemplate(pctx, m.cfg.DataDir, ws, req.Template)
					pcancel()
					pullMS = time.Since(tPull).Milliseconds()
					if perr != nil {
						tlk.Unlock()
						return nil, fmt.Errorf("template %q is not on this host and could not be pulled: %w", req.Template, perr)
					}
					pulled = true
					m.log.Info("user template pulled from object store",
						"template", req.Template, "workspace", ws,
						"generation", gen, "took_ms", pullMS)
				}
				tlk.Unlock()
			}
		}
	}

	// Template-owned sizing: Firecracker snapshot restore cannot change vCPU
	// or memory at restore time, so any per-request override is a lie that
	// flows straight into the API response, the DB row, and (worst) the
	// billing event. Read the template's meta.json — that's the source of
	// truth — and force the request to match before *anything* persists.
	// Pool claim, store writes, usage emit, and the customer's API response
	// will all see the corrected values.
	if req.Template != "" && req.FromSnapshot == "" {
		ts := ReadTemplateSize(m.cfg.DataDir, req.Template)
		if req.CPU != ts.CPU || req.MemoryMB != ts.MemoryMB {
			m.log.Info("template owns size; overriding request",
				"template", req.Template,
				"req_cpu", req.CPU, "req_mem_mb", req.MemoryMB,
				"tpl_cpu", ts.CPU, "tpl_mem_mb", ts.MemoryMB,
			)
			req.CPU = ts.CPU
			req.MemoryMB = ts.MemoryMB
		}
		// Disk: template default, but allow per-request grow (never shrink).
		// Shrink would corrupt FS or destroy data; reject silently by clamping.
		if req.DiskGB <= 0 || req.DiskGB < ts.DiskGB {
			req.DiskGB = ts.DiskGB
		}
	}

	// --- timing: total wallclock from request to "running" ---------------
	bootStart := time.Now()
	phases := make(map[string]int64, 6)
	mark := func(name string, t time.Time) {
		phases[name] = time.Since(t).Milliseconds()
	}
	if pulled {
		phases["template_pull_ms"] = pullMS
	}

	id := req.ID
	if id == "" {
		id = uuid.NewString()
	}

	// Detect NATID fast path early so we can skip the wasted legacy /30+TAP
	// alloc (which costs ~500ms and is immediately released below).
	// MUST mirror the snapshot fast-path gate below exactly — in particular
	// the len(req.Volumes)==0 condition: volume-attached creates cold-boot,
	// and the cold-boot path needs the legacy alloc (TAP+MAC) made here.
	// If this said true while the gate below said false, the cold boot would
	// run with a zero-valued alloc and FC would reject the empty network cfg.
	natidFast := req.FromSnapshot == "" &&
		len(req.Volumes) == 0 &&
		os.Getenv("PANDASTACK_NATID") == "1" &&
		templateSnapReady(m.cfg.DataDir, req.Template, m.keys.Fingerprint())

	var alloc network.Allocation
	if !natidFast {
		tAlloc := time.Now()
		alloc, err = m.netPool.Allocate(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("network allocate: %w", err)
		}
		mark("network_ms", tAlloc)
	}

	bootMode := "cold"
	if pulled {
		// A just-pulled template has no baked snapshot yet, so this create
		// cold-boots; the distinct mode makes pull-induced cold boots
		// visible in boot events (the auto-bake then makes the NEXT create
		// a normal snapshot restore).
		bootMode = "cold-pull"
	}
	if req.FromSnapshot != "" {
		bootMode = "snapshot"
	}
	sb = &Sandbox{
		ID:        id,
		Template:  req.Template,
		CPU:       req.CPU,
		MemoryMB:  req.MemoryMB,
		Status:    StatusCreating,
		GuestIP:   alloc.GuestIP,
		HostTAP:   alloc.TAP,
		MAC:       alloc.MAC,
		VsockCID:  alloc.VsockCID,
		FromSnap:  req.FromSnapshot,
		BootMode:  bootMode,
		Metadata:  req.Metadata,
		CreatedAt: time.Now().UTC(),
	}

	if !natidFast {
		tIns := time.Now()
		if err := m.store.InsertSandbox(ctx, sb); err != nil {
			_ = m.netPool.Release(ctx, id)
			return nil, err
		}
		mark("db_insert_ms", tIns)
	}

	vmDir := filepath.Join(m.cfg.DataDir, "vms", id)
	if err := os.MkdirAll(vmDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir vm dir: %w", err)
	}

	// If this is a from_snapshot restore and the snapshot files are not on
	// this agent's local disk (i.e. the snapshot was taken on a different
	// agent), lazy-fetch them from GCS. Without this, cross-agent fork is
	// impossible because the edge proxy may route the restore to any agent.
	if req.FromSnapshot != "" && m.snapStore.Enabled() {
		snapDir := snapshotDir(m.cfg, req.FromSnapshot)
		if _, err := os.Stat(filepath.Join(snapDir, "vm.state")); err != nil {
			tDL := time.Now()
			dlCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			err := m.snapStore.Download(dlCtx, req.FromSnapshot, snapDir)
			cancel()
			if err != nil {
				return nil, fmt.Errorf("fetch snapshot %s from gcs: %w", req.FromSnapshot, err)
			}
			mark("snapshot_download_ms", tDL)
			m.log.Info("snapshot pulled from gcs",
				"snapshot", req.FromSnapshot,
				"took_ms", time.Since(tDL).Milliseconds())
		}
	}

	// --- Sub-second cold-start path -------------------------------------
	// If a per-template snapshot exists, restore from it + reconfigure
	// network identity via vsock. This typically lands in <300ms.
	//
	// Creates with user volumes MUST skip this fast path: a snapshot restore
	// reproduces the baked device topology and only postgres-16 bakes an
	// extra-drive placeholder that can be PatchDrive'd — for every other
	// template there is no drive ID to patch, so a restored guest would
	// never see /dev/vdb and the mount request would be silently dropped
	// (the exact bug this guards against). Cold boot (~3s) attaches the
	// volumes for real via Spec.Volumes; that latency is the documented
	// cost of volume-attached creates.
	if req.FromSnapshot == "" && len(req.Volumes) == 0 && templateSnapReady(m.cfg.DataDir, req.Template, m.keys.Fingerprint()) {
		// NAT-identity opt-in: if PANDASTACK_NATID=1, use the netns-isolated
		// path that skips ALL guest-side reconfig. Empirically ~3.7x faster.
		if os.Getenv("PANDASTACK_NATID") == "1" {
			// Release the legacy /30+TAP alloc we already made and re-allocate
			// as NATID. (Could be optimized by branching earlier.)
			_ = m.netPool.Release(ctx, id)

			snapDir := templateSnapDir(m.cfg.DataDir, req.Template)
			tapHostIP, guestIP, mac, idErr := loadTemplateIdentity(snapDir)
			if idErr != nil {
				m.log.Warn("NATID identity load failed, falling back to v1 snap restore",
					"template", req.Template, "err", idErr)
				// Re-allocate legacy and fall through.
				alloc, err = m.netPool.Allocate(ctx, id)
				if err != nil {
					return nil, fmt.Errorf("network re-allocate: %w", err)
				}
			} else {
				tNAT := time.Now()
				natidAlloc, err := m.netPool.AllocateNATID(ctx, id, tapHostIP, guestIP, mac, map[int]int{22: 22})
				mark("natid_alloc_ms", tNAT)
				if err != nil {
					m.log.Warn("NATID allocate failed, falling back", "err", err)
					alloc, err = m.netPool.Allocate(ctx, id)
					if err != nil {
						return nil, fmt.Errorf("network re-allocate: %w", err)
					}
				} else {
					// Phased-boot templates (postgres-16) run on a durable
					// per-database ext4 image, NOT in the ephemeral rootfs.
					// The agent provisions it LOCALLY (co-located with the
					// sandbox — avoids the multi-node director splitting the
					// volume and the compute across agents) and patches it
					// onto the snapshot's baked "vol1" placeholder before
					// Resume. autostart.sh formats-if-blank and initdb's.
					dataDrivePath := ""
					if phasedBootTemplates[req.Template] {
						dbImg, derr := m.ensureDBVolume(id)
						if derr != nil {
							m.log.Warn("db volume provision failed; booting without durable volume",
								"sandbox", id, "err", derr)
						} else {
							dataDrivePath = dbImg
						}
					}
					drv, snapPhases, err := m.startFromTemplateSnapNATID(
						ctx, req.Template, id, vmDir, natidAlloc, req.CPU, req.MemoryMB, dataDrivePath,
					)
					if err != nil {
						m.log.Warn("NATID restore failed", "err", err)
						_ = m.netPool.Release(ctx, id)
						// Re-allocate legacy & fall through to v1.
						alloc, _ = m.netPool.Allocate(ctx, id)
					} else {
						for k, v := range snapPhases {
							phases[k] = v
						}
						m.mu.Lock()
						m.drivers[id] = drv
						m.mu.Unlock()

						totalMS := time.Since(bootStart).Milliseconds()
						sb.Status = StatusRunning
						sb.GuestIP = natidAlloc.ProxyGuestIP // SSH-reachable proxy IP (DNAT target inside netns; ProxyHostIP is the host's own veth address)
						sb.HostTAP = natidAlloc.Netns
						sb.MAC = mac
						sb.VsockCID = 0
						sb.BootMS = totalMS
						sb.BootMode = "snapshot-natid"
						sbCopy := *sb
						go func() {
							bgCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
							defer cancel()
							_ = m.store.InsertSandbox(bgCtx, &sbCopy)
							_ = m.store.InsertBootEvent(bgCtx, store.BootEvent{
								SandboxID: sbCopy.ID, Workspace: sbCopy.Workspace(), Template: sbCopy.Template, BootMode: "snapshot-natid",
								BootMS: totalMS, TS: time.Now().UTC(),
							})
						}()
						m.log.Info("sandbox running (snapshot-natid)", "id", id, "proxy_ip", sb.GuestIP,
							"baked_guest_ip", guestIP, "boot_ms", totalMS, "phases", phases)
						if m.bus != nil {
							m.bus.Emit(id, "sandbox.running", map[string]any{
								"pid": drv.PID(), "ip": sb.GuestIP, "template": sb.Template,
								"boot_ms": totalMS, "boot_mode": "snapshot-natid", "phases": phases,
							})
						}
						m.chBoot(sb.Workspace(), id, sb.Template, "snapshot-natid", sb.FromSnap, totalMS)
						m.chEvent(sb.Workspace(), id, "running", "snapshot-natid", "", map[string]any{"boot_ms": totalMS, "template": sb.Template})
						m.MarkActivity(id)
						m.startHealthMonitor(id, drv)
						go m.prewarmSSH(id, sb.GuestIP, bootStart)
						if req.Template == pgManagedTemplate {
							go m.kickPGPhase2(id)
						}
						return sb, nil
					}
				}
			}
		}

		drv, snapPhases, err := m.startFromTemplateSnap(
			ctx, req.Template, id, vmDir, alloc, req.CPU, req.MemoryMB,
		)
		if err != nil {
			m.log.Warn("template-snap restore failed, falling back to cold boot",
				"template", req.Template, "err", err)
			// fall through to cold-boot below
		} else {
			for k, v := range snapPhases {
				phases[k] = v
			}
			m.mu.Lock()
			m.drivers[id] = drv
			m.mu.Unlock()

			totalMS := time.Since(bootStart).Milliseconds()
			sb.Status = StatusRunning
			sb.BootMS = totalMS
			sb.BootMode = "snapshot"
			sbCopy := *sb
			go func() {
				bgCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				_ = m.store.UpdateSandbox(bgCtx, &sbCopy)
				_ = m.store.InsertBootEvent(bgCtx, store.BootEvent{
					SandboxID: sbCopy.ID, Workspace: sbCopy.Workspace(), Template: sbCopy.Template, BootMode: "snapshot",
					BootMS: totalMS, TS: time.Now().UTC(),
				})
			}()
			m.log.Info("sandbox running (snapshot)", "id", id, "ip", sb.GuestIP,
				"boot_ms", totalMS, "phases", phases)
			if m.bus != nil {
				m.bus.Emit(id, "sandbox.running", map[string]any{
					"pid": drv.PID(), "ip": sb.GuestIP, "template": sb.Template,
					"boot_ms": totalMS, "boot_mode": "snapshot", "phases": phases,
				})
			}
			m.chBoot(sb.Workspace(), id, sb.Template, "snapshot", sb.FromSnap, totalMS)
			m.chEvent(sb.Workspace(), id, "running", "snapshot", "", map[string]any{"boot_ms": totalMS, "template": sb.Template})
			m.MarkActivity(id)
			m.startHealthMonitor(id, drv)
			go m.prewarmSSH(id, sb.GuestIP, bootStart)
			if req.Template == pgManagedTemplate {
				go m.kickPGPhase2(id)
			}
			return sb, nil
		}
	}

	rootfsPath := filepath.Join(vmDir, "rootfs.ext4")
	// For from_snapshot the user may omit `template`; recover it from the
	// snapshot's meta.json so we can materialize a fresh rootfs at the new
	// path (forks get an independent disk; on-disk changes from the parent
	// are NOT preserved — only the in-memory state is restored).
	effectiveTemplate := req.Template
	if req.FromSnapshot != "" && effectiveTemplate == "" {
		if meta, err := snapstore.ReadMeta(snapshotDir(m.cfg, req.FromSnapshot)); err == nil {
			effectiveTemplate = meta.Template
		}
	}
	{
		// Always copy a fresh template rootfs as the disk backing.
		// For cold boot: this is the standard path. For from_snapshot:
		// FC's snapshot blob references the *original* rootfs path; we
		// later symlink that path → this fresh copy so FC can open it.
		if effectiveTemplate == "" {
			return nil, fmt.Errorf("template required (either explicit or via snapshot meta)")
		}
		tpl := filepath.Join(m.cfg.DataDir, "templates", effectiveTemplate, "rootfs.ext4")

		// Cold-start optimization #1: bake the agent's SSH key into the
		// *template* rootfs once, instead of mounting + writing on every
		// create. Saves ~300-500ms per cold boot. The bake itself is the
		// same cost as the old per-sandbox InjectInto but only runs once
		// per (template, key-fingerprint) pair.
		tBake := time.Now()
		// Serialize the check-then-bake against concurrent creates and the
		// snapshot builder (which also bakes under the same per-template
		// lock): two creates racing here would loopback-mount and mutate the
		// same template rootfs read-write simultaneously.
		tlk := tplLock(effectiveTemplate)
		tlk.Lock()
		if !m.keys.IsBakedInto(tpl) {
			if err := m.keys.BakeInto(tpl); err != nil {
				tlk.Unlock()
				return nil, fmt.Errorf("bake template key: %w", err)
			}
			m.log.Info("template key baked", "template", effectiveTemplate, "fingerprint", m.keys.Fingerprint())
		}
		tlk.Unlock()
		mark("bake_check_ms", tBake)

		tCopy := time.Now()
		if err := cloneFile(tpl, rootfsPath); err != nil {
			return nil, fmt.Errorf("copy template rootfs: %w", err)
		}
		mark("rootfs_copy_ms", tCopy)

		// Phase 0 of always-on: per-sandbox disk grow. The clone above
		// produced an exact-size copy of the template image. If the
		// request asks for more, truncate up + resize2fs the ext4
		// inside. Sparse copy means the larger image only costs the
		// extra space the guest actually allocates.
		if req.DiskGB > 0 {
			tResize := time.Now()
			if err := growRootfs(rootfsPath, int64(req.DiskGB)*1024*1024*1024); err != nil {
				m.log.Warn("rootfs grow skipped", "err", err, "want_gb", req.DiskGB)
			} else {
				mark("rootfs_grow_ms", tResize)
			}
		}
	}

	tKernel := time.Now()
	kernelPath, err := m.kernelCache.get(m.cfg.DataDir)
	if err != nil {
		return nil, err
	}
	mark("kernel_lookup_ms", tKernel)

	// Cross-agent snapshot restores: the snapshot blob embeds an absolute
	// path to the original sandbox's rootfs.ext4 (FC opens it during
	// /snapshot/load). If that path differs from this agent's new sandbox
	// path, create a symlink so FC finds the backing file. Same-agent
	// restores would hit this no-op (paths only match for the original).
	if req.FromSnapshot != "" {
		snapDir := snapshotDir(m.cfg, req.FromSnapshot)
		if meta, mErr := snapstore.ReadMeta(snapDir); mErr == nil && meta.RootfsHostPath != "" && meta.RootfsHostPath != rootfsPath {
			// Only touch the path if it's missing or already a symlink we
			// own. Never clobber a regular file because that may be the
			// rootfs of the still-running parent sandbox on the same agent.
			needLink := false
			if fi, lerr := os.Lstat(meta.RootfsHostPath); lerr != nil {
				if os.IsNotExist(lerr) {
					needLink = true
				}
			} else if fi.Mode()&os.ModeSymlink != 0 {
				_ = os.Remove(meta.RootfsHostPath)
				needLink = true
			}
			// If a regular file already exists at the path (parent still
			// running on this agent), leave it alone — FC will open that
			// file directly. Forks on the same agent will share the
			// parent's rootfs at the original path; this is an accepted
			// limitation matching pre-GCS same-agent fork behavior.
			if needLink {
				if err := os.MkdirAll(filepath.Dir(meta.RootfsHostPath), 0o755); err != nil {
					return nil, fmt.Errorf("mkdir for snapshot rootfs symlink: %w", err)
				}
				if err := os.Symlink(rootfsPath, meta.RootfsHostPath); err != nil && !os.IsExist(err) {
					return nil, fmt.Errorf("symlink snapshot rootfs: %w", err)
				}
				m.log.Info("snapshot rootfs symlinked",
					"snapshot", req.FromSnapshot,
					"from", meta.RootfsHostPath,
					"to", rootfsPath)
			}
		}
	}

	var vmounts []firecracker.VolumeMount
	for _, v := range req.Volumes {
		ws := ""
		if req.Metadata != nil {
			ws = req.Metadata["workspace"]
		}
		hp, verr := m.resolveVolume(ws, v.Name)
		if verr != nil {
			return nil, fmt.Errorf("volume %q: %w", v.Name, verr)
		}
		vmounts = append(vmounts, firecracker.VolumeMount{
			Name: v.Name, HostPath: hp, ReadOnly: v.ReadOnly,
		})
	}

	// Phased-boot templates (postgres-16) require a durable data device at
	// /dev/vdb even on this cold-boot path. Cold boot is taken on the first
	// create for a template before its snapshot exists (e.g. right after a
	// spot-VM replacement). The NATID restore path patches the per-DB image
	// onto the snapshot's baked placeholder; here there is no snapshot, so we
	// attach the image directly as the first extra volume (vol1 -> /dev/vdb).
	// Fail closed: autostart.sh hard-requires /dev/vdb, so a missing volume
	// must abort the create rather than boot-loop a broken database.
	coldDBVolume := ""
	if req.FromSnapshot == "" && phasedBootTemplates[req.Template] {
		dbImg, derr := m.ensureDBVolume(id)
		if derr != nil {
			_ = m.netPool.Release(ctx, id)
			_ = os.RemoveAll(vmDir)
			return nil, fmt.Errorf("provision durable db volume: %w", derr)
		}
		coldDBVolume = dbImg
		// Prepend so the driver assigns it drive_id "vol1" (index 0) and the
		// guest sees it as /dev/vdb, matching autostart.sh's DATA_DEV and the
		// NATID PatchDrive(pgDataDriveID, ...) convention.
		vmounts = append([]firecracker.VolumeMount{{
			Name: pgDataDriveID, HostPath: dbImg,
		}}, vmounts...)
	}

	// --- NATID snapshot restore path -------------------------------------
	// If the snapshot was taken from a NATID sandbox AND we have the
	// template's baked identity locally, restore inside a fresh NATID
	// netns with the same baked guest IP/MAC. This is the cross-agent
	// time-travel fork path. The SDK driver's cold-boot network model
	// can't satisfy the network state encoded in vm.state.
	if req.FromSnapshot != "" {
		snapDir := snapshotDir(m.cfg, req.FromSnapshot)
		if meta, mErr := snapstore.ReadMeta(snapDir); mErr == nil && meta.NATID && meta.Template != "" {
			// Prefer the baked identity recorded in the snapshot
			// (origin's template). Falls back to local template's
			// identity for older snapshots that pre-date that field.
			tapHostIP, guestIP, mac := meta.BakedTapHostIP, meta.BakedGuestIP, meta.BakedMAC
			if guestIP == "" || mac == "" {
				tplSnap := templateSnapDir(m.cfg.DataDir, meta.Template)
				if th, g, m2, idErr := loadTemplateIdentity(tplSnap); idErr == nil {
					tapHostIP, guestIP, mac = th, g, m2
				} else {
					m.log.Warn("template identity not loadable for NATID restore", "template", meta.Template, "err", idErr)
					guestIP = ""
				}
			}
			if guestIP != "" {
				newSb, err := m.restoreFromSnapshotNATID(ctx, sb, req, meta, snapDir, vmDir, rootfsPath, alloc, tapHostIP, guestIP, mac, bootStart, mark, phases)
				if err != nil {
					m.log.Warn("NATID snapshot restore failed", "snapshot", req.FromSnapshot, "err", err)
					_ = m.netPool.Release(ctx, id)
					_ = m.store.DeleteSandbox(ctx, id)
					return nil, fmt.Errorf("snapshot restore: %w", err)
				}
				return newSb, nil
			}
		}
	}

	drv := firecracker.NewDriver(firecracker.Spec{
		ID:          id,
		KernelPath:  kernelPath,
		RootfsPath:  rootfsPath,
		SocketPath:  filepath.Join("/tmp", fmt.Sprintf("fc-%s.socket", id)),
		LogPath:     filepath.Join(vmDir, "firecracker.log"),
		ConsolePath: filepath.Join(vmDir, "console.log"),
		CPUs:        req.CPU,
		MemoryMB:    req.MemoryMB,
		Network:     alloc,
		Vsock: firecracker.VsockSpec{
			UDSPath: filepath.Join(vmDir, "fc-vsock.sock"),
			CID:     alloc.VsockCID,
		},
		FromSnapDir: snapshotDir(m.cfg, req.FromSnapshot),
		Volumes:     vmounts,
	}, m.log.With("sandbox", id))

	tFC := time.Now()
	if err := drv.Start(context.Background()); err != nil {
		sb.Status = StatusFailed
		_ = m.store.UpdateSandbox(ctx, sb)
		_ = m.netPool.Release(ctx, id)
		_ = os.RemoveAll(vmDir)
		if coldDBVolume != "" {
			_ = os.Remove(coldDBVolume)
		}
		return sb, fmt.Errorf("firecracker start: %w", err)
	}
	mark("firecracker_start_ms", tFC)

	m.mu.Lock()
	m.drivers[id] = drv
	m.mu.Unlock()

	totalMS := time.Since(bootStart).Milliseconds()
	sb.Status = StatusRunning
	sb.BootMS = totalMS
	sbCopy := *sb
	go func() {
		bgCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = m.store.UpdateSandbox(bgCtx, &sbCopy)
		_ = m.store.InsertBootEvent(bgCtx, store.BootEvent{
			SandboxID: sbCopy.ID,
			Workspace: sbCopy.Workspace(),
			Template:  sbCopy.Template,
			BootMode:  bootMode,
			BootMS:    totalMS,
			TS:        time.Now().UTC(),
		})
	}()
	m.log.Info("sandbox running",
		"id", id,
		"ip", sb.GuestIP,
		"boot_mode", bootMode,
		"boot_ms", totalMS,
		"phases", phases,
	)
	if m.bus != nil {
		m.bus.Emit(id, "sandbox.running", map[string]any{
			"pid": drv.PID(), "ip": sb.GuestIP, "template": sb.Template,
			"boot_ms": totalMS, "boot_mode": bootMode, "phases": phases,
		})
	}
	m.chBoot(sb.Workspace(), id, sb.Template, bootMode, sb.FromSnap, totalMS)
	m.chEvent(sb.Workspace(), id, "running", bootMode, "", map[string]any{"boot_ms": totalMS, "template": sb.Template})
	m.MarkActivity(id)
	m.startHealthMonitor(id, drv)

	// Pre-warm the SSH client so the first user request is fast. We also
	// record the SSH-ready timing — it's a separate budget from the boot
	// total (the API has already returned by then) but it tells us when
	// the sandbox is *truly* usable.
	go m.prewarmSSH(id, sb.GuestIP, bootStart)

	// For postgres-16 (phased boot): deliver per-sandbox credentials so
	// autostart.sh Phase 2 can rotate PG password and write ready.json.
	// Works for both cold boot and snapshot restore paths.
	if sb.Template == pgManagedTemplate {
		go m.kickPGPhase2(id)
	}

	// First cold boot of a template auto-triggers snapshot creation in the
	// background. Subsequent creates for the same template will land in the
	// sub-second restore path. Best-effort; failures are logged.
	if req.Template != "" && !templateSnapReady(m.cfg.DataDir, req.Template, m.keys.Fingerprint()) {
		go func(tpl string) {
			bgCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			if err := m.ensureTemplateSnapshot(bgCtx, tpl); err != nil {
				m.log.Warn("background template snapshot build failed", "template", tpl, "err", err)
			}
		}(req.Template)
	}
	return sb, nil
}

func (m *Manager) prewarmSSH(id, ip string, bootStart time.Time) {
	gc := guest.NewClient(ip, "root", m.keys.Signer())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := gc.WaitReady(ctx, 30*time.Second); err != nil {
		m.log.Warn("ssh prewarm failed", "id", id, "err", err)
		return
	}
	readyMS := time.Since(bootStart).Milliseconds()
	m.mu.Lock()
	m.guests[id] = gc
	m.mu.Unlock()
	m.log.Info("ssh ready", "id", id, "ssh_ready_ms", readyMS, "boot_to_ssh_ms", readyMS)
	if m.bus != nil {
		m.bus.Emit(id, "sandbox.ssh_ready", map[string]any{"ssh_ready_ms": readyMS})
	}
}

// kickPGPhase2 delivers per-sandbox credentials to a postgres-16 VM so
// autostart.sh Phase 2 can rotate the PG password, start PgBouncer + broker,
// and write /run/pandastack/ready.json. Must be called as a goroutine.
//
// Protocol:
//  1. Agent generates a random password + broker token.
//  2. Writes them as plaintext to /run/pandastack/pg.password.new and
//     broker.token.new inside the VM via SSH exec.
//  3. Touches /run/pandastack/creds-ready — autostart.sh unblocks.
//  4. autostart.sh reads + erases the temp files, rotates PG password,
//     starts PgBouncer + broker, writes ready.json.
func (m *Manager) kickPGPhase2(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	password, err := generateToken(24)
	if err != nil {
		m.log.Error("pg phase2: generate password", "id", id, "err", err)
		return
	}
	rawTok, err := generateToken(24)
	if err != nil {
		m.log.Error("pg phase2: generate broker token", "id", id, "err", err)
		return
	}
	brokerToken := "pds_pg_" + rawTok

	// Wait for SSH — retry with backoff (VM may need a moment post-resume).
	var gc *guest.Client
	for {
		c, cerr := m.Guest(id)
		if cerr == nil {
			res, execErr := c.Exec(ctx, "echo ok")
			if execErr == nil && res.ExitCode == 0 {
				gc = c
				break
			}
		}
		select {
		case <-ctx.Done():
			m.log.Warn("pg phase2: timed out waiting for SSH", "id", id)
			return
		case <-time.After(300 * time.Millisecond):
		}
	}

	// Tokens use base64url (A-Za-z0-9-_): safe inside single-quoted shell strings.
	cmds := []string{
		"mkdir -p /run/pandastack",
		fmt.Sprintf("printf '%%s' '%s' > /run/pandastack/pg.password.new && chmod 600 /run/pandastack/pg.password.new", password),
		fmt.Sprintf("printf '%%s' '%s' > /run/pandastack/broker.token.new && chmod 600 /run/pandastack/broker.token.new", brokerToken),
	}
	// WAL archiving env must land before creds-ready: postgres starts as soon
	// as the trigger file appears, and its first WAL segments should already
	// flow to the relay (no archiving gap at the start of the timeline).
	cmds = append(cmds, m.walEnvCmds(ctx, id)...)
	cmds = append(cmds, "touch /run/pandastack/creds-ready")
	for _, cmd := range cmds {
		delivered := false
		for attempt := 0; attempt < 5; attempt++ {
			res, execErr := gc.Exec(ctx, cmd)
			if execErr == nil && res.ExitCode == 0 {
				delivered = true
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		if !delivered {
			m.log.Error("pg phase2: credential delivery failed", "id", id, "cmd", cmd)
			return
		}
	}
	m.log.Info("pg phase2: credentials delivered", "id", id)
}

// generateToken returns n random bytes encoded as base64url (no padding).
// The alphabet A-Za-z0-9-_ is safe for PG passwords and shell single-quoting.
func generateToken(nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := cryptorand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// Get returns the stored representation as a generic map (decoupled from
// the sandbox.Sandbox type to avoid an import cycle with store).
func (m *Manager) Get(ctx context.Context, id string) (any, error) {
	return m.store.GetSandbox(ctx, id)
}

func (m *Manager) List(ctx context.Context) ([]any, error) {
	return m.store.ListSandboxes(ctx)
}

// ErrManagedSandbox is returned when a caller tries to delete a sandbox that
// backs a managed feature (a managed database or a hosted app) through the
// generic sandbox-delete path. These sandboxes own durable state (a database
// volume, an app's deployed runtime) that a stray delete would destroy, so they
// may only be torn down by the owning feature's delete path, which calls
// Delete with force=true. The HTTP layer maps this to 409 Conflict.
var ErrManagedSandbox = errors.New("managed sandbox: delete it from the feature that owns it (Databases or Apps)")

// managedKind reports the owning feature of a sandbox ("database" or "app"),
// or "" if it is an ordinary sandbox. Databases are postgres-16 sandboxes; app
// sandboxes carry metadata kind=app (set by the apps deploy pipeline).
func managedKind(sbAny any) string {
	row, ok := sbAny.(map[string]any)
	if !ok {
		return ""
	}
	if tpl, _ := row["template"].(string); tpl == pgManagedTemplate {
		return "database"
	}
	if md, ok := row["metadata"].(map[string]string); ok {
		if md["kind"] == "app" || md["app.id"] != "" {
			return "app"
		}
	}
	return ""
}

func (m *Manager) Delete(ctx context.Context, id string) error {
	return m.delete(ctx, id, false)
}

// DeleteManaged tears down a sandbox even if it backs a managed feature. It is
// for the owning feature's delete path (databases/apps API) only — the generic
// DELETE /sandboxes/{id} route always calls Delete (force=false).
func (m *Manager) DeleteManaged(ctx context.Context, id string) error {
	return m.delete(ctx, id, true)
}

func (m *Manager) delete(ctx context.Context, id string, force bool) error {
	// Guard: refuse to delete a managed (database/app) sandbox through the
	// generic path. The owning feature deletes via DeleteManaged (force=true).
	if !force {
		if sbAny, err := m.store.GetSandbox(ctx, id); err == nil && sbAny != nil {
			if kind := managedKind(sbAny); kind != "" {
				return fmt.Errorf("%w (this sandbox backs a %s)", ErrManagedSandbox, kind)
			}
		}
	}
	m.mu.Lock()
	drv := m.drivers[id]
	gc := m.guests[id]
	delete(m.drivers, id)
	delete(m.guests, id)
	m.mu.Unlock()

	if gc != nil {
		_ = gc.Close()
	}

	// Fast path: SIGKILL FC immediately (no graceful CtrlAltDel — guest is
	// ephemeral), release network, delete store row. Heavy disk cleanup
	// (RemoveAll of ~1GB rootfs.ext4) and event emit run async.
	if drv != nil {
		if err := drv.FastStop(ctx); err != nil {
			m.log.Warn("firecracker fast stop failed", "id", id, "err", err)
		}
	}
	// Network release must happen on the synchronous path: FC has released
	// the tap/netns after kill, and reusing those resources for a new
	// sandbox needs the pool entry freed first.
	_ = m.netPool.Release(ctx, id)

	m.actMu.Lock()
	delete(m.lastActivity, id)
	delete(m.activeTunnels, id)
	m.actMu.Unlock()
	if m.lifecycle != nil {
		m.lifecycle.Delete(id)
	}

	// Capture sandbox state for CH event emit before we lose it.
	var chWS, chTpl string
	if sbAny, err := m.store.GetSandbox(ctx, id); err == nil && sbAny != nil {
		chWS, chTpl = extractWorkspaceTemplate(sbAny)
	}

	// Delete the store row synchronously so a follow-up GET returns 404.
	storeErr := m.store.DeleteSandbox(ctx, id)

	// Release the routing lease so the API stops routing to us for this id.
	// Fire-and-forget — a stale lease will be reaped by the periodic sweeper.
	m.releaseLease(ctx, id)

	m.chEvent(chWS, id, "deleted", "", "", map[string]any{"template": chTpl})

	// Async: emit terminal event + reclaim disk. These are observability +
	// disk hygiene and should not block the API response.
	vmDir := filepath.Join(m.cfg.DataDir, "vms", id)
	bus := m.bus
	log := m.log
	go func() {
		if bus != nil {
			bus.Emit(id, "sandbox.deleted", nil)
		}
		// RemoveSnap MUST happen before RemoveAll(vmDir) because cow.img
		// lives inside vmDir and dm-snapshot must be detached first.
		if m.dmsnap != nil {
			if err := m.dmsnap.RemoveSnap(id); err != nil {
				log.Warn("dmsnap RemoveSnap failed (non-fatal)", "id", id, "err", err)
			}
		}
		if err := os.RemoveAll(vmDir); err != nil {
			log.Warn("vm dir cleanup failed", "id", id, "path", vmDir, "err", err)
		}
		// Phase 4: remove the durable per-database volume image. In 4A the
		// image is keyed by sandbox id, so destroying the sandbox destroys
		// its data (matches "user DELETE removes data"). 4B will decouple a
		// stable database id from the sandbox so reaps keep the volume.
		dbImg := m.dbVolumePath(id)
		if _, statErr := os.Stat(dbImg); statErr == nil {
			if err := os.Remove(dbImg); err != nil {
				log.Warn("db volume cleanup failed", "id", id, "path", dbImg, "err", err)
			} else {
				log.Info("removed durable db volume", "id", id, "path", dbImg)
			}
		}
	}()

	return storeErr
}

// dbVolumePath returns the host path of the durable per-database ext4 image
// for sandbox id. Phase 4 (postgres-16 durable storage).
func (m *Manager) dbVolumePath(id string) string {
	return filepath.Join(m.cfg.DataDir, "volumes", "db", id+".ext4")
}

// ensureDBVolume creates the durable per-database data image for sandbox id if
// it does not already exist, returning its host path. The image is a raw
// sparse file (NOT formatted here) — autostart.sh formats it ext4 on first
// boot and runs initdb. Sized at pgDataPlaceholderGB to match the placeholder
// drive baked into the template snapshot.
func (m *Manager) ensureDBVolume(id string) (string, error) {
	p := m.dbVolumePath(id)
	if _, err := os.Stat(p); err == nil {
		return p, nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", fmt.Errorf("mkdir db volume dir: %w", err)
	}
	if err := makeSparseImage(p, int64(pgDataPlaceholderGB)*1024*1024*1024); err != nil {
		return "", fmt.Errorf("create db volume image: %w", err)
	}
	m.log.Info("provisioned durable db volume", "sandbox", id, "path", p, "size_gb", pgDataPlaceholderGB)
	return p, nil
}

func (m *Manager) Pause(ctx context.Context, id string) error {
	drv := m.driver(id)
	if drv == nil {
		return errors.New("sandbox not found or not running")
	}
	if err := drv.Pause(ctx); err != nil {
		return err
	}
	if m.bus != nil {
		m.bus.Emit(id, "sandbox.paused", nil)
	}
	return m.store.SetStatus(ctx, id, string(StatusPaused))
}

func (m *Manager) Resume(ctx context.Context, id string) error {
	drv := m.driver(id)
	if drv == nil {
		return errors.New("sandbox not found")
	}
	if err := drv.Resume(ctx); err != nil {
		return err
	}
	if m.bus != nil {
		m.bus.Emit(id, "sandbox.resumed", nil)
	}
	return m.store.SetStatus(ctx, id, string(StatusRunning))
}

type SnapshotResult struct {
	ID        string    `json:"id"`
	Sandbox   string    `json:"sandbox_id"`
	Created   time.Time `json:"created_at"`
	MemPath   string    `json:"mem_path"`
	StatePath string    `json:"state_path"`
}

func (m *Manager) Snapshot(ctx context.Context, sandboxID string) (*SnapshotResult, error) {
	drv := m.driver(sandboxID)
	if drv == nil {
		return nil, errors.New("sandbox not found")
	}
	snapID := uuid.NewString()
	dir := snapshotDir(m.cfg, snapID)
	if err := drv.CreateSnapshot(ctx, dir); err != nil {
		return nil, err
	}
	// Persist meta.json so cross-agent restores can rebuild the symlink
	// FC expects for the rootfs backing file. The rootfs path is the
	// canonical per-sandbox VM dir layout we use everywhere.
	rootfsHostPath := filepath.Join(m.cfg.DataDir, "vms", sandboxID, "rootfs.ext4")
	template := ""
	natid := false
	if sbAny, err := m.store.GetSandbox(ctx, sandboxID); err == nil && sbAny != nil {
		if row, ok := sbAny.(map[string]any); ok {
			if t, ok := row["template"].(string); ok {
				template = t
			}
			// host_tap is reused for "netns name" on NATID slots
			// (Allocation.TAP convention).
			if tap, ok := row["host_tap"].(string); ok && strings.HasPrefix(tap, "ns-") {
				natid = true
			}
		}
	}
	// Record the BAKED NATID identity from the local template so
	// cross-agent restores can request a matching slot even if the
	// destination agent's local template was baked with different
	// IP/MAC values (independent counter drift).
	var bakedTapHost, bakedGuest, bakedMAC string
	if natid && template != "" {
		if th, g, m2, err := loadTemplateIdentity(templateSnapDir(m.cfg.DataDir, template)); err == nil {
			bakedTapHost, bakedGuest, bakedMAC = th, g, m2
		}
	}
	if err := snapstore.WriteMeta(dir, snapstore.Meta{
		OriginalSandboxID: sandboxID,
		RootfsHostPath:    rootfsHostPath,
		Template:          template,
		NATID:             natid,
		VsockUDSPath:      filepath.Join(m.cfg.DataDir, "vms", sandboxID, "fc-vsock.sock"),
		BakedTapHostIP:    bakedTapHost,
		BakedGuestIP:      bakedGuest,
		BakedMAC:          bakedMAC,
	}); err != nil {
		m.log.Warn("snapshot meta write failed", "snapshot", snapID, "err", err)
	}
	res := &SnapshotResult{
		ID:        snapID,
		Sandbox:   sandboxID,
		Created:   time.Now().UTC(),
		MemPath:   filepath.Join(dir, "vm.mem"),
		StatePath: filepath.Join(dir, "vm.state"),
	}
	if err := m.store.InsertSnapshot(ctx, res); err != nil {
		return nil, err
	}
	// Mirror to GCS synchronously so a subsequent cross-agent fork
	// request can immediately find the blobs. Same-agent forks read
	// directly from the local snapshot dir so the wait is only paid by
	// API callers who themselves opted into cross-region replication
	// (by setting PANDASTACK_SNAPSHOT_BUCKET). Typical 512MB snapshot
	// uploads in ~8s on n2-standard-4.
	if m.snapStore.Enabled() {
		upCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		t0 := time.Now()
		if err := m.snapStore.Upload(upCtx, snapID, dir); err != nil {
			cancel()
			m.log.Warn("snapshot gcs upload failed", "snapshot", snapID, "err", err)
			// Don't fail the snapshot create — local copy is authoritative
			// and same-agent forks still work.
		} else {
			cancel()
			m.log.Info("snapshot mirrored to gcs", "snapshot", snapID, "took_ms", time.Since(t0).Milliseconds())
		}
	}
	return res, nil
}

func (m *Manager) driver(id string) *firecracker.Driver {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.drivers[id]
}

func snapshotDir(cfg config.Config, snapID string) string {
	if snapID == "" {
		return ""
	}
	return filepath.Join(cfg.DataDir, "snapshots", snapID)
}

// findKernel picks the newest vmlinux-* under DataDir/kernels.
func findKernel(dataDir string) (string, error) {
	matches, err := filepath.Glob(filepath.Join(dataDir, "kernels", "vmlinux-*"))
	if err != nil {
		return "", err
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("no kernel found under %s/kernels", dataDir)
	}
	return matches[len(matches)-1], nil
}

// copyFile copies src to dst. On Linux, prefer reflink-style copy via os/exec
// (caller can replace with `cp --reflink=auto` if XFS/Btrfs is in use).
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// asInt extracts an int from a JSON-decoded map value (which can be float64 or int64).
func asInt(v any) int {
	switch x := v.(type) {
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	}
	return 0
}

// ---- Phase 3: Fork ----

// ForkResult describes the outcome of a fork() call.
type ForkResult struct {
	Parent   string    `json:"parent_id"`
	Snapshot string    `json:"snapshot_id"`
	Children []string  `json:"children"`
	At       time.Time `json:"at"`
}

// Fork creates `count` children that inherit the parent's *disk* state.
//
// Phase 3a semantics: parent is paused → rootfs copied for a consistent disk
// view → parent resumed → each child boots FRESH from that rootfs copy with
// a brand-new network identity. Children get the parent's filesystem (packages
// installed, files written, etc.) but NOT the parent's running processes/memory.
//
// This is the "fork" use-case people actually want: pre-install deps in a
// parent template, then spawn N workers that already have those deps on disk.
// True warm-clone (snapshot-restore) fork is deferred to Phase 3b — it needs
// firecracker-go-sdk support for per-restore network/vsock overrides.
func (m *Manager) Fork(ctx context.Context, parentID string, count int) (*ForkResult, error) {
	if count <= 0 {
		count = 1
	}
	if count > 16 {
		return nil, errors.New("fork count capped at 16")
	}
	parentDrv := m.driver(parentID)
	if parentDrv == nil {
		return nil, errors.New("parent sandbox not found or not running")
	}

	snapID := uuid.NewString()
	stagingDir := filepath.Join(m.cfg.DataDir, "forks", snapID)
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir staging dir: %w", err)
	}

	parentSb, err := m.store.GetSandbox(ctx, parentID)
	if err != nil || parentSb == nil {
		return nil, errors.New("parent sandbox not in store")
	}
	parentRoot := filepath.Join(m.cfg.DataDir, "vms", parentID, "rootfs.ext4")
	stagedRoot := filepath.Join(stagingDir, "rootfs.ext4")

	// Pause parent → copy rootfs (consistent view) → resume parent.
	if err := parentDrv.PauseCopyResume(ctx, parentRoot, stagedRoot); err != nil {
		return nil, fmt.Errorf("fork copy rootfs: %w", err)
	}
	if m.bus != nil {
		m.bus.Emit(parentID, "fork.staged", map[string]any{"snapshot": snapID, "count": count})
	}

	children := make([]string, 0, count)
	for i := 0; i < count; i++ {
		childID, err := m.spawnChildFromRootfs(ctx, parentID, snapID, stagedRoot)
		if err != nil {
			m.log.Warn("fork child failed", "i", i, "err", err)
			if m.bus != nil {
				m.bus.Emit(parentID, "fork.child_failed", map[string]any{"i": i, "err": err.Error()})
			}
			continue
		}
		children = append(children, childID)
	}
	res := &ForkResult{Parent: parentID, Snapshot: snapID, Children: children, At: time.Now().UTC()}
	if m.bus != nil {
		m.bus.Emit(parentID, "fork.completed", map[string]any{"snapshot": snapID, "children": children})
	}
	return res, nil
}

func (m *Manager) spawnChildFromRootfs(ctx context.Context, parentID, snapID, stagedRoot string) (string, error) {
	id := uuid.NewString()
	alloc, err := m.netPool.Allocate(ctx, id)
	if err != nil {
		return "", fmt.Errorf("net alloc: %w", err)
	}

	parentAny, _ := m.store.GetSandbox(ctx, parentID)
	row, _ := parentAny.(map[string]any)
	cpu := asInt(row["cpu"])
	mem := asInt(row["memory_mb"])
	tpl, _ := row["template"].(string)
	// Inherit the parent's workspace so the child is visible to the same
	// auth scope and (critically) so acquireLease writes a sandbox→agent
	// lease the edge MultiNodeDirector can resolve. Without this the child
	// has no lease, the edge falls through to scheduler.Pick, and ops on the
	// child route to the wrong agent → "sandbox not found".
	ws, _ := extractWorkspaceTemplate(parentAny)

	childMeta := map[string]string{"forked_from": parentID}
	if ws != "" {
		childMeta["workspace"] = ws
	}
	sb := &Sandbox{
		ID: id, Template: tpl, CPU: cpu, MemoryMB: mem,
		Status: StatusCreating, GuestIP: alloc.GuestIP, HostTAP: alloc.TAP,
		MAC: alloc.MAC, VsockCID: alloc.VsockCID, FromSnap: snapID,
		Metadata:  childMeta,
		CreatedAt: time.Now().UTC(),
	}
	if err := m.store.InsertSandbox(ctx, sb); err != nil {
		_ = m.netPool.Release(ctx, id)
		return "", err
	}

	vmDir := filepath.Join(m.cfg.DataDir, "vms", id)
	if err := os.MkdirAll(vmDir, 0o755); err != nil {
		return "", err
	}
	rootfsPath := filepath.Join(vmDir, "rootfs.ext4")
	if err := cloneFile(stagedRoot, rootfsPath); err != nil {
		return "", fmt.Errorf("copy staged rootfs: %w", err)
	}
	// Re-inject our SSH key (the parent's rootfs already has it, but the
	// machine-id will differ — re-injecting is safe & idempotent).
	if err := m.keys.InjectInto(rootfsPath); err != nil {
		return "", fmt.Errorf("inject ssh key: %w", err)
	}
	kernelPath, err := findKernel(m.cfg.DataDir)
	if err != nil {
		return "", err
	}
	drv := firecracker.NewDriver(firecracker.Spec{
		ID:          id,
		KernelPath:  kernelPath,
		RootfsPath:  rootfsPath,
		SocketPath:  filepath.Join("/tmp", fmt.Sprintf("fc-%s.socket", id)),
		LogPath:     filepath.Join(vmDir, "firecracker.log"),
		ConsolePath: filepath.Join(vmDir, "console.log"),
		CPUs:        cpu,
		MemoryMB:    mem,
		Network:     alloc,
		// Fresh boot (no FromSnapDir): child gets its own kernel + memory.
	}, m.log.With("sandbox", id))

	if err := drv.Start(context.Background()); err != nil {
		sb.Status = StatusFailed
		_ = m.store.UpdateSandbox(ctx, sb)
		return id, fmt.Errorf("start child: %w", err)
	}
	m.mu.Lock()
	m.drivers[id] = drv
	m.mu.Unlock()
	sb.Status = StatusRunning
	_ = m.store.UpdateSandbox(ctx, sb)
	m.acquireLease(ctx, id, ws)
	if m.bus != nil {
		m.bus.Emit(id, "sandbox.forked", map[string]any{"parent": parentID, "snapshot": snapID})
	}
	m.chBoot(sb.Workspace(), id, sb.Template, "fork", snapID, 0)
	m.chEvent(sb.Workspace(), id, "forked", "", "", map[string]any{"parent": parentID, "snapshot": snapID, "template": sb.Template})
	m.MarkActivity(id)
	m.startHealthMonitor(id, drv)
	return id, nil
}

// Deprecated: kept for reference / future Phase 3b warm-fork.
func (m *Manager) spawnChildFromSnap(ctx context.Context, parentID, snapID, snapDir string) (string, error) {
	return "", errors.New("use spawnWarmChild")
}

// WarmFork creates ONE child that inherits the parent's *memory and disk*
// state via firecracker snapshot-restore. Boots in <500ms vs ~3s for
// rootfs-clone fork, skipping kernel boot + userspace init entirely.
//
// IMPORTANT LIMITATIONS (Phase 3b preview):
//   - Only count=1 is supported. FC's NetworkOverride lets us override the
//     host TAP but NOT the guest MAC, and our guest derives its IP from its
//     MAC at boot via fcnet-setup.sh. So every warm child inherits the
//     parent's exact (IP, MAC) network identity. Running >1 warm child
//     simultaneously would create unroutable host-side TAP collisions.
//   - The child REUSES the parent's network allocation. The parent is paused
//     before the child boots and is NOT auto-resumed (call /resume to
//     restore the parent — but you cannot have both running at once).
//   - Multi-child warm-fork is unlocked by Phase 4 network v2 (shared
//     bridge + DHCP + per-child netns).
//
// Flow: pause parent → snapshot mem+state → reuse parent's net alloc →
// copy rootfs → spawn FC, load_snapshot, PATCH rootfs path, resume VM.
func (m *Manager) WarmFork(ctx context.Context, parentID string, count int) (*ForkResult, error) {
	if count != 1 {
		return nil, errors.New("warm fork currently supports count=1 only (see Manager.WarmFork docstring)")
	}
	parentDrv := m.driver(parentID)
	if parentDrv == nil {
		return nil, errors.New("parent sandbox not found or not running")
	}
	parentAny, _ := m.store.GetSandbox(ctx, parentID)
	parentRow, _ := parentAny.(map[string]any)
	if parentRow == nil {
		return nil, errors.New("parent sandbox not in store")
	}

	snapID := uuid.NewString()
	snapDir := filepath.Join(m.cfg.DataDir, "forks", snapID)
	if err := os.MkdirAll(snapDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir snap dir: %w", err)
	}

	parentRoot := filepath.Join(m.cfg.DataDir, "vms", parentID, "rootfs.ext4")

	// Pause and snapshot parent. Do NOT resume — child will inherit parent's
	// network identity and they cannot coexist on the same TAP.
	if err := parentDrv.PauseAndSnapshot(ctx, snapDir); err != nil {
		return nil, fmt.Errorf("warm fork snapshot: %w", err)
	}
	parentSb, _ := m.store.GetSandbox(ctx, parentID)
	if parentRow, ok := parentSb.(map[string]any); ok {
		// Mark parent paused in DB so dashboard reflects reality.
		parentRow["status"] = string(StatusPaused)
	}
	if pSb, err := m.GetTyped(ctx, parentID); err == nil && pSb != nil {
		pSb.Status = StatusPaused
		_ = m.store.UpdateSandbox(ctx, pSb)
	}
	if m.bus != nil {
		m.bus.Emit(parentID, "warmfork.staged", map[string]any{"snapshot": snapID})
		m.bus.Emit(parentID, "sandbox.paused", map[string]any{"reason": "warm-fork"})
	}

	// Stop parent's FC process so its TAP/socket are free for the child.
	if err := parentDrv.Stop(ctx); err != nil {
		m.log.Warn("warm fork parent stop failed", "err", err)
	}
	m.mu.Lock()
	delete(m.drivers, parentID)
	m.mu.Unlock()

	// Inherit parent's network allocation (TAP, IP, MAC, CID) for the child.
	parentTAP, _ := parentRow["host_tap"].(string)
	parentIP, _ := parentRow["guest_ip"].(string)
	parentMAC, _ := parentRow["mac"].(string)
	parentCID := uint32(asInt(parentRow["vsock_cid"]))
	alloc := network.Allocation{TAP: parentTAP, GuestIP: parentIP, MAC: parentMAC, VsockCID: parentCID}

	cpu := asInt(parentRow["cpu"])
	memMB := asInt(parentRow["memory_mb"])
	tpl, _ := parentRow["template"].(string)
	// Inherit parent workspace so the warm child gets a routable lease (see
	// spawnChildFromRootfs for the full rationale).
	ws, _ := extractWorkspaceTemplate(parentAny)

	childID := uuid.NewString()
	childMeta := map[string]string{"forked_from": parentID, "fork_mode": "warm"}
	if ws != "" {
		childMeta["workspace"] = ws
	}
	sb := &Sandbox{
		ID: childID, Template: tpl, CPU: cpu, MemoryMB: memMB,
		Status: StatusCreating, GuestIP: parentIP, HostTAP: parentTAP,
		MAC: parentMAC, VsockCID: parentCID, FromSnap: snapID,
		Metadata:  childMeta,
		CreatedAt: time.Now().UTC(),
	}
	if err := m.store.InsertSandbox(ctx, sb); err != nil {
		return nil, err
	}

	vmDir := filepath.Join(m.cfg.DataDir, "vms", childID)
	if err := os.MkdirAll(vmDir, 0o755); err != nil {
		return nil, err
	}
	rootfsPath := filepath.Join(vmDir, "rootfs.ext4")
	if err := cloneFile(parentRoot, rootfsPath); err != nil {
		return nil, fmt.Errorf("copy rootfs: %w", err)
	}

	drv := firecracker.NewDriver(firecracker.Spec{
		ID:          childID,
		RootfsPath:  rootfsPath,
		SocketPath:  filepath.Join("/tmp", fmt.Sprintf("fc-%s.socket", childID)),
		LogPath:     filepath.Join(vmDir, "firecracker.log"),
		ConsolePath: filepath.Join(vmDir, "console.log"),
		CPUs:        cpu,
		MemoryMB:    memMB,
		Network:     alloc,
	}, m.log.With("sandbox", childID))

	if err := drv.StartWarmFork(context.Background(), snapDir, rootfsPath); err != nil {
		sb.Status = StatusFailed
		_ = m.store.UpdateSandbox(ctx, sb)
		return &ForkResult{Parent: parentID, Snapshot: snapID, Children: nil, At: time.Now().UTC()},
			fmt.Errorf("start warm child: %w", err)
	}
	m.mu.Lock()
	m.drivers[childID] = drv
	m.mu.Unlock()
	sb.Status = StatusRunning
	_ = m.store.UpdateSandbox(ctx, sb)
	m.acquireLease(ctx, childID, ws)
	if m.bus != nil {
		m.bus.Emit(childID, "sandbox.warmforked", map[string]any{"parent": parentID, "snapshot": snapID})
	}
	m.MarkActivity(childID)
	m.startHealthMonitor(childID, drv)

	return &ForkResult{
		Parent: parentID, Snapshot: snapID,
		Children: []string{childID}, At: time.Now().UTC(),
	}, nil
}

// GetTyped is a thin helper returning *Sandbox (panics on type mismatch
// avoided). Centralised here for the warm-fork helper.
func (m *Manager) GetTyped(ctx context.Context, id string) (*Sandbox, error) {
	v, err := m.store.GetSandbox(ctx, id)
	if err != nil || v == nil {
		return nil, err
	}
	row, ok := v.(map[string]any)
	if !ok {
		return nil, nil
	}
	sb := &Sandbox{
		ID:       row["id"].(string),
		Template: stringOr(row["template"]),
		CPU:      asInt(row["cpu"]),
		MemoryMB: asInt(row["memory_mb"]),
		Status:   Status(stringOr(row["status"])),
		GuestIP:  stringOr(row["guest_ip"]),
		HostTAP:  stringOr(row["host_tap"]),
		MAC:      stringOr(row["mac"]),
		VsockCID: uint32(asInt(row["vsock_cid"])),
		FromSnap: stringOr(row["from_snapshot"]),
	}
	if md, ok := row["metadata"].(map[string]string); ok {
		sb.Metadata = md
	} else if md, ok := row["metadata"].(map[string]any); ok {
		sb.Metadata = map[string]string{}
		for k, v := range md {
			if s, ok := v.(string); ok {
				sb.Metadata[k] = s
			}
		}
	}
	return sb, nil
}

// Workspace returns the workspace tag stored in metadata, or "" if untagged.
func (s *Sandbox) Workspace() string {
	if s == nil || s.Metadata == nil {
		return ""
	}
	return s.Metadata["workspace"]
}

// UpdateMetadata mutates the sandbox's metadata map by applying patch (nil
// values delete keys) and persists. Returns the resulting metadata map.
func (m *Manager) UpdateMetadata(ctx context.Context, id string, patch map[string]*string) (map[string]string, error) {
	sb, err := m.GetTyped(ctx, id)
	if err != nil {
		return nil, err
	}
	if sb == nil {
		return nil, errors.New("sandbox not found")
	}
	if sb.Metadata == nil {
		sb.Metadata = map[string]string{}
	}
	for k, v := range patch {
		if v == nil {
			delete(sb.Metadata, k)
		} else {
			sb.Metadata[k] = *v
		}
	}
	if err := m.store.UpdateSandbox(ctx, sb); err != nil {
		return nil, err
	}
	return sb.Metadata, nil
}

func stringOr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func (m *Manager) spawnWarmChild(_ context.Context, _, _, _, _ string) (string, error) {
	return "", errors.New("spawnWarmChild superseded by WarmFork single-child design")
}

// ---- Phase 3: Hibernate / Wake ----

func (m *Manager) Hibernate(ctx context.Context, id string) error {
	drv := m.driver(id)
	if drv == nil {
		return errors.New("sandbox not found or not running")
	}
	vmDir := filepath.Join(m.cfg.DataDir, "vms", id)
	hiberDir := filepath.Join(vmDir, "hibernation")
	if err := os.MkdirAll(hiberDir, 0o755); err != nil {
		return err
	}
	// Pause+snapshot+stop. CreateSnapshot internally pauses and would resume,
	// so we use the lower-level pause-and-snapshot via ForkSnapshot semantics
	// (without rootfs copy — rootfs stays in vmDir).
	if err := drv.PauseAndSnapshot(ctx, hiberDir); err != nil {
		obs.HibernationTotal.WithLabelValues("hibernate_failed").Inc()
		return fmt.Errorf("hibernate snapshot: %w", err)
	}
	if err := drv.Stop(ctx); err != nil {
		m.log.Warn("hibernate stop failed", "id", id, "err", err)
	}
	m.mu.Lock()
	delete(m.drivers, id)
	gc := m.guests[id]
	delete(m.guests, id)
	m.mu.Unlock()
	if gc != nil {
		_ = gc.Close()
	}
	if m.bus != nil {
		m.bus.Emit(id, "sandbox.hibernated", map[string]any{"snap_dir": hiberDir})
	}
	if sbAny, gerr := m.store.GetSandbox(ctx, id); gerr == nil && sbAny != nil {
		ws, tpl := extractWorkspaceTemplate(sbAny)
		m.chEvent(ws, id, "hibernated", "", "", map[string]any{"template": tpl, "snap_dir": hiberDir})
	}
	obs.HibernationTotal.WithLabelValues("hibernated").Inc()
	return m.store.SetStatus(ctx, id, string(StatusHibernated))
}

func (m *Manager) Wake(ctx context.Context, id string) error {
	if m.driver(id) != nil {
		return errors.New("sandbox already running")
	}
	vmDir := filepath.Join(m.cfg.DataDir, "vms", id)
	hiberDir := filepath.Join(vmDir, "hibernation")
	if _, err := os.Stat(filepath.Join(hiberDir, "vm.state")); err != nil {
		return fmt.Errorf("hibernation snapshot missing: %w", err)
	}
	// Fetch cpu/mem from store. Store lookup CAN fail across edges, so we
	// fall back to defaults rather than abort the wake.
	cpu, mem := 1, 256
	if sbAny, err := m.store.GetSandbox(ctx, id); err == nil && sbAny != nil {
		if row, ok := sbAny.(map[string]any); ok {
			if c := asInt(row["cpu"]); c > 0 {
				cpu = c
			}
			if mm := asInt(row["memory_mb"]); mm > 0 {
				mem = mm
			}
		}
	}
	rootfsPath := filepath.Join(vmDir, "rootfs.ext4")
	kernelPath, err := findKernel(m.cfg.DataDir)
	if err != nil {
		return err
	}
	// Re-allocate (or re-use existing) network. For simplicity, allocate fresh:
	// the old allocation was already released? Actually Hibernate doesn't release
	// it, so we look it up.
	alloc, err := m.netPool.Lookup(ctx, id)
	if err != nil {
		return fmt.Errorf("net lookup: %w", err)
	}

	// NATID detection: NATID slots persist alloc.TAP as the netns name
	// (e.g. "ns-p0000xxx"). Cold-boot slots persist it as the host TAP
	// device (e.g. "tap0xxxxx"). Routing requirement: NATID needs FC
	// re-spawned inside the existing netns, against the baked tap0 — the
	// SDK's snapshot loader can't override network at restore time, so we
	// use the same raw-HTTP /snapshot/load path NATID restore uses on
	// first boot.
	isNATID := strings.HasPrefix(alloc.TAP, "ns-")
	var drv *firecracker.Driver
	if isNATID {
		// The hibernation snapshot froze the guest's vsock device at the baked
		// absolute path (BakedVsockPath). Re-binding it at that shared path on
		// wake collides with any stale socket from the pre-hibernate VM and
		// fails with "Address in use (os error 98)". Mirror the NATID create
		// path (template_snap.go): hand FC a per-sandbox bind dir so it spawns
		// in a private mount namespace and the vsock socket lands on its own
		// inode. The dir already exists from the original create; MkdirAll is
		// idempotent and also covers a cross-host wake where it doesn't.
		vsockBindDir := filepath.Join(vmDir, "vsock")
		if err := os.MkdirAll(vsockBindDir, 0o755); err != nil {
			_ = m.store.SetStatus(ctx, id, string(StatusFailed))
			obs.HibernationTotal.WithLabelValues("wake_failed").Inc()
			return fmt.Errorf("wake mkdir vsock bind dir: %w", err)
		}
		// A socket left in the bind dir by the pre-hibernate VM would also
		// collide once bind-mounted over BakedVsockDir. Clear it.
		_ = os.Remove(filepath.Join(vsockBindDir, firecracker.BakedVsockName))
		drv = firecracker.NewDriver(firecracker.Spec{
			ID:           id,
			KernelPath:   kernelPath,
			RootfsPath:   rootfsPath,
			SocketPath:   filepath.Join("/tmp", fmt.Sprintf("fc-%s.socket", id)),
			LogPath:      filepath.Join(vmDir, "firecracker.log"),
			ConsolePath:  filepath.Join(vmDir, "console.log"),
			CPUs:         cpu,
			MemoryMB:     mem,
			Netns:        alloc.TAP, // netns name lives here in NATID mode
			VsockBindDir: vsockBindDir,
		}, m.log.With("sandbox", id, "natid", true))
		if err := drv.StartFromSnapNoDrive(context.Background(), hiberDir, "tap0"); err != nil {
			_ = m.store.SetStatus(ctx, id, string(StatusFailed))
			obs.HibernationTotal.WithLabelValues("wake_failed").Inc()
			return fmt.Errorf("wake start (natid): %w", err)
		}
		// Re-attach the rootfs disk (snapshot only carries metadata).
		if err := drv.PatchRootfs(context.Background(), rootfsPath); err != nil {
			_ = drv.Stop(context.Background())
			_ = m.store.SetStatus(ctx, id, string(StatusFailed))
			obs.HibernationTotal.WithLabelValues("wake_failed").Inc()
			return fmt.Errorf("wake patch rootfs: %w", err)
		}
		// Managed databases carry a durable data drive ("vol1") in the
		// snapshot's device topology. The hibernation snapshot recorded
		// the volume's real on-disk path, but re-patch it explicitly —
		// mirroring the rootfs treatment above — so a wake never depends
		// on Firecracker silently reopening a possibly-stale path.
		if vol := m.dbVolumePath(id); func() bool { _, err := os.Stat(vol); return err == nil }() {
			if err := drv.PatchDrive(context.Background(), pgDataDriveID, vol); err != nil {
				_ = drv.Stop(context.Background())
				_ = m.store.SetStatus(ctx, id, string(StatusFailed))
				obs.HibernationTotal.WithLabelValues("wake_failed").Inc()
				return fmt.Errorf("wake patch data drive: %w", err)
			}
		}
		if err := drv.Resume(context.Background()); err != nil {
			_ = drv.Stop(context.Background())
			_ = m.store.SetStatus(ctx, id, string(StatusFailed))
			obs.HibernationTotal.WithLabelValues("wake_failed").Inc()
			return fmt.Errorf("wake resume: %w", err)
		}
	} else {
		drv = firecracker.NewDriver(firecracker.Spec{
			ID:          id,
			KernelPath:  kernelPath,
			RootfsPath:  rootfsPath,
			SocketPath:  filepath.Join("/tmp", fmt.Sprintf("fc-%s.socket", id)),
			LogPath:     filepath.Join(vmDir, "firecracker.log"),
			ConsolePath: filepath.Join(vmDir, "console.log"),
			CPUs:        cpu,
			MemoryMB:    mem,
			Network:     alloc,
			FromSnapDir: hiberDir,
		}, m.log.With("sandbox", id))
		if err := drv.Start(context.Background()); err != nil {
			_ = m.store.SetStatus(ctx, id, string(StatusFailed))
			obs.HibernationTotal.WithLabelValues("wake_failed").Inc()
			return fmt.Errorf("wake start: %w", err)
		}
	}
	m.mu.Lock()
	m.drivers[id] = drv
	m.mu.Unlock()

	// Re-establish guest SSH client + block until reachable so the
	// auto-wake middleware can hand the request straight to the next
	// handler without an "i/o timeout" race.
	guestIP := guestIPFromHiber(hiberDir)
	if guestIP == "" {
		if sbAny, err := m.store.GetSandbox(ctx, id); err == nil && sbAny != nil {
			if row, ok := sbAny.(map[string]any); ok {
				guestIP, _ = row["guest_ip"].(string)
			}
		}
	}
	if guestIP != "" {
		gc := guest.NewClient(guestIP, "root", m.keys.Signer())
		readyCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
		if err := gc.WaitReady(readyCtx, 8*time.Second); err == nil {
			m.mu.Lock()
			m.guests[id] = gc
			m.mu.Unlock()
		} else {
			m.log.Warn("wake: ssh not ready within deadline", "id", id, "err", err)
		}
		cancel()
	}

	if m.bus != nil {
		m.bus.Emit(id, "sandbox.woken", nil)
	}
	if sbAny, gerr := m.store.GetSandbox(ctx, id); gerr == nil && sbAny != nil {
		ws, tpl := extractWorkspaceTemplate(sbAny)
		m.chBoot(ws, id, tpl, "wake", "", 0)
		m.chEvent(ws, id, "woken", "", "", map[string]any{"template": tpl})
	}
	obs.HibernationTotal.WithLabelValues("woken").Inc()
	m.MarkActivity(id)
	m.startHealthMonitor(id, drv)
	return m.store.SetStatus(ctx, id, string(StatusRunning))
}

// guestIPFromHiber is a placeholder for any future on-disk metadata we
// stash alongside the hibernation snapshot. For now we always fall back
// to the store record (which carries guest_ip).
func guestIPFromHiber(_ string) string { return "" }

// ---- Phase 3: Idle sweeper ----

// RunIdleSweeper auto-hibernates running sandboxes after idleAfter of no
// activity. Cancel ctx to stop.
// HibernateAllPersistent walks every running/paused sandbox marked
// persistent and hibernates it. Intended for graceful agent shutdown
// (SIGTERM): each call to Hibernate writes a snapshot under
// <vmDir>/hibernation/ which Recover() / Wake() / EnsureRunning() pick up
// when the agent restarts. Ephemeral sandboxes are skipped (they were
// never promised persistence; the reaper handles them).
//
// Concurrency: hibernates in parallel up to `parallelism`. Per-sandbox
// hibernate uses its own ctx-bounded budget. Returns the count of
// sandboxes successfully hibernated and a slice of per-sandbox errors
// (nil entries omitted).
func (m *Manager) HibernateAllPersistent(ctx context.Context, parallelism int) (int, []error) {
	if parallelism <= 0 {
		parallelism = 4
	}
	if m.lifecycle == nil {
		return 0, nil
	}
	m.mu.RLock()
	ids := make([]string, 0, len(m.drivers))
	for id := range m.drivers {
		ids = append(ids, id)
	}
	m.mu.RUnlock()

	var targets []string
	for _, id := range ids {
		st, ok := m.lifecycle.Get(id)
		if !ok || !st.persistent {
			continue
		}
		targets = append(targets, id)
	}
	if len(targets) == 0 {
		return 0, nil
	}
	m.log.Info("graceful shutdown: hibernating persistent sandboxes", "count", len(targets), "parallelism", parallelism)

	type result struct {
		id  string
		err error
	}
	work := make(chan string, len(targets))
	out := make(chan result, len(targets))
	for _, id := range targets {
		work <- id
	}
	close(work)
	for i := 0; i < parallelism; i++ {
		go func() {
			for id := range work {
				err := m.Hibernate(ctx, id)
				out <- result{id: id, err: err}
			}
		}()
	}
	var (
		ok   int
		errs []error
	)
	for i := 0; i < len(targets); i++ {
		r := <-out
		if r.err != nil {
			m.log.Warn("graceful hibernate failed", "id", r.id, "err", r.err)
			errs = append(errs, fmt.Errorf("%s: %w", r.id, r.err))
			continue
		}
		ok++
	}
	m.log.Info("graceful shutdown: hibernation complete", "hibernated", ok, "failed", len(errs))
	return ok, errs
}

func (m *Manager) RunIdleSweeper(ctx context.Context, idleAfter time.Duration) {
	if idleAfter <= 0 {
		return
	}
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		now := time.Now()
		m.mu.RLock()
		ids := make([]string, 0, len(m.drivers))
		for id := range m.drivers {
			ids = append(ids, id)
		}
		m.mu.RUnlock()
		for _, id := range ids {
			if now.Sub(m.lastActivityFor(id)) < idleAfter {
				continue
			}
			// Never hibernate under a live pg-tunnel: managed-database
			// clients hold long, often-quiet connections (poolers), and
			// the tunnel teardown bumps lastActivity so the countdown
			// restarts at disconnect.
			if m.ActiveTunnels(id) > 0 {
				continue
			}
			// Only auto-hibernate sandboxes that opted in via persistent:true.
			// Ephemeral sandboxes are owned by the reaper (TTL-based delete),
			// which is the correct lifecycle for them: hibernating an
			// ephemeral sandbox just delays the inevitable while consuming
			// disk for the memory snapshot.
			if m.lifecycle == nil {
				continue
			}
			st, ok := m.lifecycle.Get(id)
			if !ok || !st.persistent {
				continue
			}
			m.log.Info("idle sweep: hibernating", "id", id, "idle_for", now.Sub(m.lastActivityFor(id)))
			if err := m.Hibernate(ctx, id); err != nil {
				m.log.Warn("idle hibernate failed", "id", id, "err", err)
			}
		}
	}
}

func (m *Manager) resolveVolume(workspace, name string) (string, error) {
	if !validVolumeName(name) {
		return "", fmt.Errorf("invalid volume name")
	}
	if workspace != "" && workspace != "admin" && workspace != "default" {
		p := filepath.Join(m.cfg.DataDir, "volumes", "ws", workspace, name+".ext4")
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
		return "", fmt.Errorf("not found")
	}
	// Admin/dev — legacy global path.
	p := filepath.Join(m.cfg.DataDir, "volumes", name+".ext4")
	if _, err := os.Stat(p); err != nil {
		return "", fmt.Errorf("not found")
	}
	return p, nil
}

func (m *Manager) VolumeDir() string { return filepath.Join(m.cfg.DataDir, "volumes") }

// Store returns the underlying SQLite store. Exposed for the API layer to read
// auxiliary tables (boot_events, audit_log, workspaces) without each new
// concern adding a new Manager method.
func (m *Manager) Store() *store.Store { return m.store }

// ListBootEvents is a thin convenience wrapper used by /stats/boot.
func (m *Manager) ListBootEvents(ctx context.Context, workspace string, limit int) ([]store.BootEvent, error) {
	return m.store.ListBootEvents(ctx, workspace, limit)
}

func validVolumeName(n string) bool {
	if n == "" || len(n) > 64 {
		return false
	}
	for _, c := range n {
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '.' || c == '_' || c == '-'
		if !ok {
			return false
		}
	}
	return true
}

func envDurationSeconds(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return fallback
	}
	return time.Duration(n) * time.Second
}

// restoreFromSnapshotNATID is the cross-agent time-travel fork path. It
// receives a sandbox+req that has already been allocated a (cold-boot)
// network slot and had its rootfs materialized at rootfsPath; it then:
//
//  1. Releases the cold-boot alloc and allocates a fresh NATID slot using
//     the template's baked guest identity (so the snapshot's encoded
//     network state matches host topology).
//  2. mkdir's the parent dir of the snapshot-encoded vsock UDS path so
//     FC can recreate the socket file on /snapshot/load.
//  3. Spawns FC inside the new netns and restores via raw HTTP
//     (StartFromSnapNoDrive + PatchRootfs + Resume).
//  4. Updates the sandbox row with the new NATID alloc, registers the
//     driver, and SSH-probes via ProxyGuestIP (kernel routes through the
//     veth + DNAT into the guest).
//
// This mirrors startFromTemplateSnapNATID but reads vm.mem/vm.state from
// the per-sandbox snapshot dir instead of the template snap dir.
func (m *Manager) restoreFromSnapshotNATID(
	ctx context.Context,
	sb *Sandbox,
	req CreateRequest,
	meta snapstore.Meta,
	snapDir, vmDir, rootfsPath string,
	coldAlloc network.Allocation,
	tapHostIP, guestIP, mac string,
	bootStart time.Time,
	mark func(string, time.Time),
	phases map[string]int64,
) (*Sandbox, error) {
	id := sb.ID

	// Release the cold-boot /30+TAP alloc we made earlier in Create.
	tRel := time.Now()
	_ = m.netPool.Release(ctx, id)
	mark("natid_release_cold_ms", tRel)

	// Allocate NATID slot with the template's baked identity. Fast path
	// (~5ms) if the prewarm pool has one; otherwise build from scratch.
	tAlloc := time.Now()
	natAlloc, err := m.netPool.AllocateNATID(ctx, id, tapHostIP, guestIP, mac, nil)
	if err != nil {
		return nil, fmt.Errorf("allocate natid: %w", err)
	}
	mark("natid_alloc_ms", tAlloc)

	// Vsock handling. Snapshots carrying the "vsock" marker (taken from a
	// NATID restores carry no vsock device. Legacy (pre-NATID) snapshots
	// may encode a per-template UDS path: pre-create its parent dir and
	// remove any stale socket so FC's bind succeeds.
	if meta.VsockUDSPath != "" {
		if err := os.MkdirAll(filepath.Dir(meta.VsockUDSPath), 0o755); err != nil {
			m.log.Warn("vsock parent mkdir failed", "path", meta.VsockUDSPath, "err", err)
		}
		_ = os.Remove(meta.VsockUDSPath)
	}

	kernelPath, err := m.kernelCache.get(m.cfg.DataDir)
	if err != nil {
		return nil, err
	}

	drv := firecracker.NewDriver(firecracker.Spec{
		ID:          id,
		KernelPath:  kernelPath,
		RootfsPath:  rootfsPath,
		SocketPath:  filepath.Join("/tmp", fmt.Sprintf("fc-%s.socket", id)),
		LogPath:     filepath.Join(vmDir, "firecracker.log"),
		ConsolePath: filepath.Join(vmDir, "console.log"),
		CPUs:        req.CPU,
		MemoryMB:    req.MemoryMB,
		Netns:       natAlloc.Netns,
	}, m.log.With("sandbox", id, "natid", true, "fork", true))

	tLoad := time.Now()
	if err := drv.StartFromSnapNoDrive(ctx, snapDir, natAlloc.TapName); err != nil {
		_ = m.netPool.ReleaseNATID(ctx, id)
		return nil, fmt.Errorf("snap load: %w", err)
	}
	mark("fc_load_snap_ms", tLoad)

	tPatch := time.Now()
	if err := drv.PatchRootfs(ctx, rootfsPath); err != nil {
		_ = drv.Stop(ctx)
		_ = m.netPool.ReleaseNATID(ctx, id)
		return nil, fmt.Errorf("patch rootfs: %w", err)
	}
	mark("drive_patch_ms", tPatch)

	// Start the SSH probe concurrently with Resume so the first SYN is
	// in-flight the moment guest vCPUs unfreeze (same overlap as the
	// template-seed restore path). Probe goes via ProxyGuestIP — host
	// kernel routes through the veth into the netns; iptables DNATs to
	// guestIP:22.
	tReady := time.Now()
	sshDone := make(chan error, 1)
	go func() { sshDone <- waitTCP(natAlloc.ProxyGuestIP+":22", 30*time.Second) }()

	tResume := time.Now()
	if err := drv.Resume(ctx); err != nil {
		_ = drv.Stop(ctx)
		_ = m.netPool.ReleaseNATID(ctx, id)
		return nil, fmt.Errorf("resume: %w", err)
	}
	mark("vm_resume_ms", tResume)

	// Collect SSH probe result (started before Resume above).
	if err := <-sshDone; err != nil {
		_ = drv.Stop(ctx)
		_ = m.netPool.ReleaseNATID(ctx, id)
		return nil, fmt.Errorf("ssh probe: %w", err)
	}
	mark("ssh_ready_ms", tReady)

	// Update sandbox row with the NATID alloc — orchestrator/edge proxy
	// reads guest_ip / host_tap to address the sandbox; for NATID the
	// orchestrator-facing IP is the ProxyGuestIP (kernel-routed).
	sb.GuestIP = natAlloc.ProxyGuestIP
	sb.HostTAP = natAlloc.Netns
	sb.MAC = natAlloc.MAC
	sb.VsockCID = 0
	sb.Status = StatusRunning
	sb.BootMS = time.Since(bootStart).Milliseconds()
	if err := m.store.UpdateSandbox(ctx, sb); err != nil {
		m.log.Warn("update sandbox after natid restore failed", "id", id, "err", err)
	}

	m.mu.Lock()
	m.drivers[id] = drv
	m.mu.Unlock()

	gc := guest.NewClient(natAlloc.ProxyGuestIP, "root", m.keys.Signer())
	readyCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	if err := gc.WaitReady(readyCtx, 8*time.Second); err == nil {
		m.mu.Lock()
		m.guests[id] = gc
		m.mu.Unlock()
	}
	cancel()

	if m.bus != nil {
		m.bus.Emit(id, "sandbox.running", map[string]any{
			"pid": drv.PID(), "ip": natAlloc.ProxyGuestIP, "template": sb.Template,
			"boot_ms": sb.BootMS, "boot_mode": "snapshot_natid", "phases": phases,
		})
	}
	m.MarkActivity(id)
	m.startHealthMonitor(id, drv)

	m.log.Info("sandbox restored from snapshot (NATID)",
		"id", id, "snapshot", req.FromSnapshot, "ip", natAlloc.ProxyGuestIP,
		"boot_ms", sb.BootMS, "phases", phases)

	return sb, nil
}
