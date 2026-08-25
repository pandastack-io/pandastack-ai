// SPDX-License-Identifier: Apache-2.0
// Package scheduler picks the best agent for a new sandbox by reading the
// shared `agents` and `leases` tables that pandastack-agent populates. It is
// strictly read-only from the api side; ownership writes happen on the agent
// itself when it accepts a placement.
package scheduler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pandastack/api/internal/obs"
	"os"
	"strconv"
)

// StaleAfter mirrors agent/internal/registry.StaleAfter. Duplicated here to
// avoid a cross-module import; if they drift, schedule will undershoot fresh
// agents — never a correctness issue.
const StaleAfter = 30 * time.Second

// streamBoost is the score bonus added to an agent that advertises UFFD
// streaming restore. Every create is a snapshot restore (there is no warm
// pool of VMs), so a streaming-capable host — which boots a template
// without first downloading the entire vm.mem — is strictly preferable.
// The boost is small relative to resource-fit terms so it acts as a
// tiebreaker, not an override.
const streamBoost = 5.0

// cpuOvercommit is the vCPU oversubscription factor applied to an agent's
// physical core count when admitting creates (PANDASTACK_CPU_OVERCOMMIT, min 1).
//
// UNSET BY DEFAULT, and that is deliberate. Every template now bakes 8 vCPUs as
// a BURST CEILING (not a reservation): cgroup cpu.weight arbitrates cores under
// contention, so a guest only consumes what it runs. Subtracting 8 per guest from a
// cores×factor budget therefore double-counts the very thing burst is supposed
// to share, and the arithmetic caps the fleet at (cores × factor) / 8 guests
// regardless of how idle they are. With the previous default of 4 on an 8-core
// host that was 32/8 = FOUR concurrent sandboxes fleet-wide — below the 5 the
// free tier advertises — while ~17 GB of the host's 32 GB sat unused and every
// further create returned "no available compute node".
//
// Memory remains the hard admission gate (RAM is never oversold at admission;
// the working-set flip is still pending), the agent keeps its own 507 backstop,
// and the PSI pressure ladder protects the host. Set the env var to a finite
// factor to re-enable CPU admission control.
var cpuOvercommit, cpuAdmissionEnabled = cpuOvercommitFromEnv()

func cpuOvercommitFromEnv() (float64, bool) {
	if v := os.Getenv("PANDASTACK_CPU_OVERCOMMIT"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 1 {
			return f, true
		}
	}
	return 0, false
}

// Capacity is the shape an agent reports via capacity_json.
type Capacity struct {
	CPUTotal    int     `json:"cpu_total"`
	CPUUsed     int     `json:"cpu_used"`
	MemoryMB    int     `json:"memory_mb_total"`
	MemoryUsed  int     `json:"memory_mb_used"`
	Sandboxes   int     `json:"sandboxes"`
	LoadAverage float64 `json:"load_average"`

	// StreamRestoreEnabled mirrors registry.Capacity: the agent has UFFD
	// streaming restore turned on. Used as a tiebreaker in Pick.
	StreamRestoreEnabled bool `json:"stream_restore_enabled"`

	// Volume storage telemetry (mirrors registry.Capacity). Volumes are
	// host-pinned sparse ext4 images, so volume creation is placed by
	// storage headroom, not CPU. Zero values mean "older agent, unknown"
	// — such agents stay eligible but lose to any agent with known
	// positive headroom.
	VolumeProvisionedBytes int64 `json:"volume_provisioned_bytes"`
	VolumesFSSizeBytes     int64 `json:"volumes_fs_size_bytes"`
	VolumesFSFreeBytes     int64 `json:"volumes_fs_free_bytes"`

	// Pool is the fleet MIG this agent belongs to (mirrors
	// registry.Capacity.Pool). Empty for agents that predate the pool split —
	// ALWAYS read it through agentPool() so the stateful-by-default fallback
	// is applied. Reading this field directly is a bug.
	Pool string `json:"pool"`
}

// Fleet pools. The stateful pool carries the durable pandastack-volumes disk
// and never autoscales; the ephemeral pool has no durable disk and may scale
// in at any moment.
const (
	PoolStateful  = "stateful"
	PoolEphemeral = "ephemeral"
)

// agentPool returns the agent's pool, defaulting to PoolStateful when the
// agent doesn't report one.
//
// The default direction is deliberate and load-bearing: a host that predates
// the pool split may be holding customer volumes or managed-DB PGDATA, and
// treating such a host as ephemeral would let a scale-in strand that data.
// Calling an ephemeral host "stateful" only costs a suboptimal placement;
// the reverse costs someone's data. Default toward the safe error.
func agentPool(a Agent) string {
	if p := strings.TrimSpace(a.Capacity.Pool); p != "" {
		return p
	}
	return PoolStateful
}

// Agent is the placement candidate.
type Agent struct {
	ID            string
	Endpoint      string
	Region        string
	Zone          string
	Version       string
	Status        string
	Capacity      Capacity
	LastHeartbeat time.Time
}

// Request describes the sandbox resource ask.
type Request struct {
	CPU      int    // vCPUs requested
	MemoryMB int    // RAM requested
	Region   string // preferred region (empty = any)

	// DiskBytes, when >0, marks this as a VOLUME placement: the request is
	// provisioning DiskBytes of host-pinned volume storage rather than a
	// sandbox. Pick then scores by volume-storage headroom (mirroring the
	// agent's own oversubscription + free-reserve admission gate) instead
	// of free CPU/memory.
	DiskBytes int64

	// RequirePool, when set, restricts placement to agents in that fleet pool
	// (PoolStateful | PoolEphemeral). Empty = any pool.
	//
	// Volume placement (DiskBytes > 0) forces PoolStateful in Pick regardless
	// of what the caller passed: a volume is a host-pinned sparse ext4 image
	// with no GCS archive behind it, so placing one on an autoscalable host
	// means a scale-in can strand it. Managed databases inherit the same
	// protection through the volume they sit on.
	RequirePool string
}

// Scheduler-side mirror of the agent's volume headroom defaults
// (agent/internal/api/volumes_headroom.go). Advisory only — the agent's
// 507 gate re-checks with its live (possibly env-overridden) limits.
const (
	volumeOversubFactor    = 3.0
	volumeFreeReserveBytes = int64(20) << 30 // 20 GiB
)

// volumeHeadroomBytes estimates how many more provisioned volume bytes the
// agent can admit: the tighter of (oversub budget − provisioned) and
// (fs free − reserve). ok=false when the agent doesn't report volume
// telemetry (older build) — callers treat that as unknown, not zero.
func volumeHeadroomBytes(c Capacity) (int64, bool) {
	if c.VolumesFSSizeBytes <= 0 {
		return 0, false
	}
	budget := int64(volumeOversubFactor*float64(c.VolumesFSSizeBytes)) - c.VolumeProvisionedBytes
	reserve := c.VolumesFSFreeBytes - volumeFreeReserveBytes
	if reserve < budget {
		return reserve, true
	}
	return budget, true
}

// --- in-flight reservations --------------------------------------------------
//
// THE BURST PROBLEM (observed live 2026-08-22): five creates arrived inside one
// second; all five landed on the same host (load average 40 on 8 cores) while a
// second, idle agent sat at load 0.08. Nothing was wrong with the scoring — the
// inputs were simply identical for all five. Pick() scores against the agent
// list from List(), which is cached for cacheTTL (30s) and, even uncached, lags
// reality by the agent heartbeat interval (10s). A placement therefore does not
// show up in the capacity Pick reads until seconds later, so every create in a
// burst sees the same "best" agent and picks it. Capacity-aware scoring cannot
// spread a burst that fits inside its own staleness window.
//
// THE FIX: remember what we just placed. Pick records a reservation against the
// chosen agent and subtracts outstanding reservations from that agent's free
// capacity on subsequent calls, so the second create in a burst sees the first
// one's cost and moves on. The reserve is taken while holding resMu across
// scoring + selection, which is what makes it work under concurrency: without
// that, N goroutines would all read capacity before any of them wrote a
// reservation and we would be back to the original race.
//
// Reservations expire on a timer rather than on explicit release, so a create
// that fails after placement cannot leak capacity — the worst case is that an
// agent looks busier than it is for reservationTTL and we spread slightly wider.
// Erring toward spreading is the safe direction for this bug.
const reservationTTL = 15 * time.Second

// reservation is capacity claimed by a Pick that the agent's own heartbeat has
// not reported back yet.
type reservation struct {
	cpu   int
	memMB int
	disk  int64
	at    time.Time
}

// Scheduler queries the metadata DB for agents and applies a scoring rule.
type Scheduler struct {
	db *sql.DB

	mu       sync.RWMutex
	cache    []Agent
	cachedAt time.Time
	cacheTTL time.Duration

	// reservations tracks capacity handed out by recent Picks, keyed by agent
	// id. Guarded by resMu, which Pick holds across scoring + selection so
	// concurrent placements observe each other. Entries older than
	// reservationTTL are pruned lazily on each Pick.
	resMu        sync.Mutex
	reservations map[string][]reservation

	// localLeases is a per-edge in-memory write-through cache for sandbox→
	// agent ownership. Populated by RememberLease on every successful Create
	// proxied by this edge. Saves a Supabase round-trip on the user's very
	// next request (the typical SDK pattern: create → exec → exec → …).
	// Cross-edge requests miss this cache and fall back to LookupLease on PG.
	leasesMu      sync.RWMutex
	localLeases   map[string]localLeaseEntry
	leaseCacheTTL time.Duration
}

type localLeaseEntry struct {
	agent     Agent
	expiresAt time.Time
}

// New builds a Scheduler over an *sql.DB. cacheTTL <=0 disables caching.
func New(db *sql.DB, cacheTTL time.Duration) *Scheduler {
	return &Scheduler{
		db:            db,
		cacheTTL:      cacheTTL,
		localLeases:   make(map[string]localLeaseEntry),
		leaseCacheTTL: 5 * time.Minute,
		reservations:  make(map[string][]reservation),
	}
}

// pruneReservationsLocked drops reservations older than reservationTTL — by
// then the agent's heartbeat has reported the placement, so the capacity read
// from the DB already accounts for it and keeping the reservation would
// double-count. Caller must hold s.resMu.
func (s *Scheduler) pruneReservationsLocked(now time.Time) {
	for id, rs := range s.reservations {
		kept := rs[:0]
		for _, r := range rs {
			if now.Sub(r.at) < reservationTTL {
				kept = append(kept, r)
			}
		}
		if len(kept) == 0 {
			delete(s.reservations, id)
			continue
		}
		s.reservations[id] = kept
	}
}

// reservedLocked totals the outstanding claims against one agent. Caller must
// hold s.resMu (and should have pruned first).
func (s *Scheduler) reservedLocked(agentID string) (cpu, memMB int, disk int64) {
	for _, r := range s.reservations[agentID] {
		cpu += r.cpu
		memMB += r.memMB
		disk += r.disk
	}
	return cpu, memMB, disk
}

// defaultReservedCPU is the CPU cost assumed for a create that does not declare
// a size. This matters more than it looks: the template owns the guest size (the
// agent overrides the caller's cpu/mem to match the baked snapshot), so a plain
// `POST /v1/sandboxes` with no body sizes — which is the common SDK call, and
// exactly what the 2026-08-22 burst was — arrives here with CPU == 0. Reserving
// zero for those would make this whole mechanism a no-op for the very case it
// exists to fix. Every first-party template bakes 8 vCPU, so one unsized create
// costs a full 8-vCPU slot.
//
// Only CPU is defaulted, deliberately. freeCPU carries the dominant score weight
// (0.6), so defaulting it is what produces spreading — while CPU admission is
// off by default, meaning an over-estimate here can never reject a placement.
// Memory is NOT defaulted: it is a hard admission gate, so guessing high there
// could turn a burst into spurious "no agents available" errors. Unsized creates
// therefore reserve CPU (spreads) and no memory (cannot wrongly reject).
const defaultReservedCPU = 8

// reserveLocked records a placement so the next Pick in a burst sees its cost.
// Caller must hold s.resMu.
func (s *Scheduler) reserveLocked(agentID string, req Request, now time.Time) {
	if s.reservations == nil {
		s.reservations = make(map[string][]reservation)
	}
	cpu := req.CPU
	if cpu <= 0 {
		cpu = defaultReservedCPU
	}
	s.reservations[agentID] = append(s.reservations[agentID], reservation{
		cpu:   cpu,
		memMB: req.MemoryMB,
		disk:  req.DiskBytes,
		at:    now,
	})
}

// RememberLease populates the in-memory lease cache after the edge proxies
// a successful Create. The very next request from the same client (whose
// connection is normally pinned to this edge by GCLB) avoids a Supabase
// round-trip; cross-edge requests still fall back to LookupLease on PG.
func (s *Scheduler) RememberLease(sandboxID string, a Agent) {
	s.rememberLeaseWithTTL(sandboxID, a, s.leaseCacheTTL)
}

// RememberLeasePersistent caches with a much longer TTL for sandboxes the
// caller marked persistent:true. These survive the default 5min window and
// are commonly hit minutes/hours later — keeping them resident saves PG
// round-trips on cross-edge requests for the lifetime of the agent process.
func (s *Scheduler) RememberLeasePersistent(sandboxID string, a Agent) {
	const persistentTTL = time.Hour
	s.rememberLeaseWithTTL(sandboxID, a, persistentTTL)
}

func (s *Scheduler) rememberLeaseWithTTL(sandboxID string, a Agent, ttl time.Duration) {
	if sandboxID == "" {
		return
	}
	s.leasesMu.Lock()
	s.localLeases[sandboxID] = localLeaseEntry{
		agent:     a,
		expiresAt: time.Now().Add(ttl),
	}
	s.leasesMu.Unlock()
	obs.LeaseCacheTotal.WithLabelValues("stored").Inc()
}

// ForgetLease evicts a sandbox from the local cache after deletion.
func (s *Scheduler) ForgetLease(sandboxID string) {
	if sandboxID == "" {
		return
	}
	s.leasesMu.Lock()
	delete(s.localLeases, sandboxID)
	s.leasesMu.Unlock()
}

// Pick returns the best agent for req. Returns ErrNoAgents if none qualify.
func (s *Scheduler) Pick(ctx context.Context, req Request) (*Agent, error) {
	agents, err := s.List(ctx)
	if err != nil {
		return nil, err
	}
	// Hold resMu across scoring AND selection AND the reserve. Taking it only
	// around the reserve would leave the original race intact: concurrent Picks
	// would each read capacity before any of them recorded a claim, and all
	// would choose the same agent. Everything inside is in-memory over a handful
	// of agents, so the serialization is cheap.
	now := time.Now()
	s.resMu.Lock()
	defer s.resMu.Unlock()
	s.pruneReservationsLocked(now)

	candidates := make([]scoredAgent, 0, len(agents))
	for _, a := range agents {
		if a.Status != "active" {
			continue
		}
		if req.Region != "" && a.Region != "" && req.Region != a.Region {
			continue
		}
		// Pool filter. A volume placement (DiskBytes > 0) is pinned to the
		// stateful pool no matter what the caller asked for — see
		// Request.RequirePool for why that override is unconditional.
		wantPool := req.RequirePool
		if req.DiskBytes > 0 {
			wantPool = PoolStateful
		}
		if wantPool != "" && agentPool(a) != wantPool {
			continue
		}
		// Subtract capacity already promised to in-flight placements the agent
		// has not heartbeated back yet. Without this the whole burst scores
		// against identical, pre-burst capacity and piles onto one host.
		resCPU, resMem, resDisk := s.reservedLocked(a.ID)
		freeCPU := a.Capacity.CPUTotal - a.Capacity.CPUUsed - resCPU
		freeMem := a.Capacity.MemoryMB - a.Capacity.MemoryUsed - resMem
		// CPU is burstable, not reserved: vCPUs schedule onto the host
		// at up to cpuOvercommit× physical cores — cgroup cpu.weight arbitrates
		// under contention, so packing 8-vCPU guests onto an 8-core host is the
		// intended model. Memory
		// below stays a hard gate: RAM is never oversold at admission (the
		// working-set admission flip is still pending).
		if cpuAdmissionEnabled {
			schedulableCPU := int(float64(a.Capacity.CPUTotal)*cpuOvercommit) - a.Capacity.CPUUsed - resCPU
			if req.CPU > 0 && schedulableCPU < req.CPU {
				continue
			}
		}
		if req.MemoryMB > 0 && freeMem < req.MemoryMB {
			continue
		}
		var score float64
		if req.DiskBytes > 0 {
			// Volume placement: volumes are host-pinned, so this choice is
			// sticky for the life of the volume. Score by storage headroom
			// (in GiB) so creates spread across disks instead of piling onto
			// whichever agent has the most idle CPU. Agents whose advertised
			// headroom can't even admit this request are skipped — they'd
			// just 507. Agents that don't report volume telemetry yet score
			// 0 (eligible fallback, but lose to any agent with known room).
			if hr, known := volumeHeadroomBytes(a.Capacity); known {
				// Discount volume bytes already promised to in-flight creates,
				// so a burst of volume placements spreads across disks too.
				hr -= resDisk
				if hr < req.DiskBytes {
					continue
				}
				score = float64(hr) / float64(1<<30)
			}
			// Tiny CPU tiebreaker so equal-disk agents still spread load.
			score += float64(freeCPU) * 0.01
		} else {
			// Resource-fit score: prefer agents with the most free CPU
			// (spreads load, avoids piling on busy agents), with free memory
			// as a secondary term. Every create takes the same NATID +
			// snapshot-restore fast path on any agent that has the template's
			// seed, so placement is purely a load-spreading decision.
			score = float64(freeCPU)*0.6 + float64(freeMem)/1024.0*0.3
			// Tiebreaker: prefer an agent that can UFFD-stream the restore
			// over one that must download the whole vm.mem first.
			if a.Capacity.StreamRestoreEnabled {
				score += streamBoost
			}
		}
		candidates = append(candidates, scoredAgent{Agent: a, score: score})
	}
	if len(candidates) == 0 {
		return nil, ErrNoAgents
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].score > candidates[j].score })
	a := candidates[0].Agent
	// Claim the capacity before releasing resMu so the next Pick in this burst
	// scores against what we just handed out rather than the same stale numbers.
	s.reserveLocked(a.ID, req, now)
	return &a, nil
}

// LookupLease returns the agent endpoint for an existing sandbox or "" if no
// active lease. Checks the in-memory cache first, then falls back to PG.
func (s *Scheduler) LookupLease(ctx context.Context, sandboxID string) (*Agent, error) {
	if sandboxID == "" {
		return nil, errors.New("scheduler: empty sandbox id")
	}
	// Hot path: in-memory cache populated by RememberLease on the same edge.
	s.leasesMu.RLock()
	if entry, ok := s.localLeases[sandboxID]; ok && time.Now().Before(entry.expiresAt) {
		a := entry.agent
		s.leasesMu.RUnlock()
		obs.LeaseCacheTotal.WithLabelValues("hit").Inc()
		return &a, nil
	}
	s.leasesMu.RUnlock()
	obs.LeaseCacheTotal.WithLabelValues("miss").Inc()
	const q = `
		SELECT a.id, a.endpoint, a.region, a.zone, a.version, a.status, a.capacity_json, a.last_heartbeat
		FROM leases l
		JOIN agents a ON a.id = l.agent_id
		WHERE l.sandbox_id = $1 AND l.expires_at > now()
		LIMIT 1`
	row := s.db.QueryRowContext(ctx, q, sandboxID)
	a, err := scanAgent(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return a, err
}

// List returns all agents currently alive. Cached up to cacheTTL.
// Returned slice is a defensive copy of the cache.
func (s *Scheduler) List(ctx context.Context) ([]Agent, error) {
	if s.cacheTTL > 0 {
		s.mu.RLock()
		fresh := time.Since(s.cachedAt) < s.cacheTTL && s.cache != nil
		if fresh {
			out := s.snapshotLocked()
			s.mu.RUnlock()
			return out, nil
		}
		s.mu.RUnlock()
	}
	const q = `
		SELECT id, endpoint, region, zone, version, status, capacity_json, last_heartbeat
		FROM agents
		WHERE status = 'active' AND last_heartbeat > now() - interval '30 seconds'
		ORDER BY last_heartbeat DESC`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("scheduler list: %w", err)
	}
	defer rows.Close()
	var out []Agent
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if s.cacheTTL > 0 {
		s.mu.Lock()
		s.cache = out
		s.cachedAt = time.Now()
		got := s.snapshotLocked()
		s.mu.Unlock()
		return got, nil
	}
	return out, nil
}

// snapshotLocked returns a defensive copy of the cache. Caller must hold
// s.mu (read or write).
func (s *Scheduler) snapshotLocked() []Agent {
	out := make([]Agent, len(s.cache))
	copy(out, s.cache)
	return out
}

// scanner is the small interface QueryRowContext and rows.Scan both satisfy.
type scanner interface {
	Scan(dest ...any) error
}

func scanAgent(sc scanner) (*Agent, error) {
	var a Agent
	var capJSON sql.NullString
	if err := sc.Scan(&a.ID, &a.Endpoint, &a.Region, &a.Zone, &a.Version, &a.Status, &capJSON, &a.LastHeartbeat); err != nil {
		return nil, err
	}
	if capJSON.Valid && capJSON.String != "" {
		_ = json.Unmarshal([]byte(capJSON.String), &a.Capacity)
	}
	return &a, nil
}

type scoredAgent struct {
	Agent
	score float64
}

// ErrNoAgents is returned by Pick when no fresh agent has capacity.
var ErrNoAgents = errors.New("scheduler: no agents with capacity available")
