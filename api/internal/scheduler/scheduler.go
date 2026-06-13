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
	"sync"
	"time"

	"github.com/pandastack/api/internal/obs"
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

// Scheduler queries the metadata DB for agents and applies a scoring rule.
type Scheduler struct {
	db *sql.DB

	mu       sync.RWMutex
	cache    []Agent
	cachedAt time.Time
	cacheTTL time.Duration

	// localLeases is a per-edge in-memory write-through cache for sandbox→
	// agent ownership. Populated by RememberLease on every successful Create
	// proxied by this edge. Saves a Supabase round-trip on the user's very
	// next request (the typical SDK pattern: create → exec → exec → …).
	// Cross-edge requests miss this cache and fall back to LookupLease on PG.
	leasesMu       sync.RWMutex
	localLeases    map[string]localLeaseEntry
	leaseCacheTTL  time.Duration
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
	}
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
	candidates := make([]scoredAgent, 0, len(agents))
	for _, a := range agents {
		if a.Status != "active" {
			continue
		}
		if req.Region != "" && a.Region != "" && req.Region != a.Region {
			continue
		}
		freeCPU := a.Capacity.CPUTotal - a.Capacity.CPUUsed
		freeMem := a.Capacity.MemoryMB - a.Capacity.MemoryUsed
		if req.CPU > 0 && freeCPU < req.CPU {
			continue
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
