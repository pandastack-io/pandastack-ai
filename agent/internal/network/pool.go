// SPDX-License-Identifier: Apache-2.0
// Package network manages per-sandbox TAP devices and /30 IP allocations.
//
// Each sandbox gets:
//   - tap<short-id> on the Lima VM (legacy path) or a netns+veth (NATID path)
//   - a /30 carved from the configured CIDR (legacy 172.20.0.0/16) or the NATID
//     base (10.200.0.0/16); the /30 SLOT INDEX is owned by the local slotstore
//   - guest MAC derived from the IP
//   - vsock CID (monotonic counter, persisted in the main store)
//
// SLOT OWNERSHIP lives in ONE place: the local slotstore (internal/slotstore),
// a per-host SQLite ledger with one row per /30 index. This replaced the old
// in-memory freeIdx + monotonic-high-water scheme that was patched three times
// (v0.3.11 leak, v0.3.12 NATID-leak, v0.3.13 concurrent double-free) because
// ownership lived in two hand-synced structures. Claim = atomic lowest-free
// UPDATE; release = RowsAffected-gated UPDATE. Both the legacy Allocate and the
// NATID mint paths claim from the same slotstore, so a slot freed by one path is
// reused by the other (one index space; different base CIDRs, but the index is
// what's allocated).
//
// The allocation PAYLOAD (TAP name, IPs, MAC) is still persisted in the main
// store so teardown/Lookup can find the kernel objects to destroy. But the slot
// INDEX is no longer derived from that payload — slotstore owns it.
package network

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"sync"

	"github.com/pandastack/agent/internal/netns"
	"github.com/pandastack/agent/internal/slotstore"
	"github.com/pandastack/agent/internal/store"
)

// netnsDestroy tears down a NATID netns + root-side veth. Defined as a
// function (not netns.Destroy directly) to keep pool.go decoupled from
// the netns package's Spec shape.
func netnsDestroy(nsName, vethHost string) error {
	return netns.Destroy(netns.Spec{Name: nsName, VethHost: vethHost})
}

type Allocation struct {
	SandboxID string `json:"sandbox_id"`
	TAP       string `json:"tap"`
	HostIP    string `json:"host_ip"`
	GuestIP   string `json:"guest_ip"`
	MAC       string `json:"mac"`
	VsockCID  uint32 `json:"vsock_cid"`
	Subnet    string `json:"subnet"`

	// Idx is the /30 slot index this allocation occupies, persisted on the
	// payload purely for observability/diagnostics. The AUTHORITATIVE owner of
	// the index is the slotstore, not this field — Release frees the slot by
	// sandbox_id in the slotstore, never by parsing this back out.
	Idx uint32 `json:"idx"`
}

type Pool struct {
	mu      sync.Mutex
	store   *store.Store
	slots   *slotstore.Store
	base    *net.IPNet
	nextCID uint32 // vsock CID high-water (separate uint32 space, never reclaimed)

	// natidFree is a per-template-identity free list of pre-built NATID
	// slots. The key is identityKey(tapHostIP, guestIP, mac). Pre-building
	// netns + veth + tap + iptables ahead of POST /sandboxes lets
	// AllocateNATID return in O(1) (~5ms) instead of ~500ms. This is a CACHE of
	// wired-up kernel objects, NOT slot ownership — each parked slot already
	// holds its slotstore claim (under a "prebuilt:<idx>" sentinel) so a crash
	// can't leak it.
	natidFree map[string][]NATIDAlloc
	// natidRefill, if set, is invoked (in a goroutine) after each Claim so
	// the manager can top the pool back up.
	natidRefill func(identityKey string)
}

const minVsockCID = 3 // 0,1,2 are reserved by virtio-vsock spec

// NewPool builds the allocator. cidr is the legacy /30 base; slots is the local
// slotstore that owns slot indices. The slotstore is seeded to the pool
// capacity (number of /30s in the /16) so claims always find rows.
func NewPool(cidr string, st *store.Store, slots *slotstore.Store) (*Pool, error) {
	_, n, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, fmt.Errorf("parse cidr: %w", err)
	}
	used, err := st.LoadNetworkState(context.Background())
	if err != nil {
		return nil, err
	}
	p := &Pool{store: st, slots: slots, base: n, nextCID: used.NextVsockCID,
		natidFree: map[string][]NATIDAlloc{}}
	if p.nextCID < minVsockCID {
		p.nextCID = minVsockCID
	}
	// Seed the slot ledger to capacity. The legacy and NATID bases are both /16
	// (16,384 /30s), so one capacity covers both index spaces.
	if err := slots.Seed(context.Background(), poolCapacity(n)); err != nil {
		return nil, fmt.Errorf("seed slots: %w", err)
	}
	return p, nil
}

// Reconcile frees slot rows whose owner is not among the live sandboxes the
// manager recovered. Call once at startup AFTER Recover() has determined the
// live set. This reclaims slots stranded by a crash mid-allocate (a slot claimed
// before the sandbox row existed) — which the sandbox-row-walking Recover() path
// cannot see. Returns the number of slots reclaimed.
func (p *Pool) Reconcile(ctx context.Context, liveSandboxIDs []string) (int, error) {
	return p.slots.Reconcile(ctx, liveSandboxIDs)
}

// SlotStats exposes pool occupancy (for metrics + soak assertions).
func (p *Pool) SlotStats(ctx context.Context) (slotstore.Stats, error) {
	return p.slots.Stats(ctx)
}

// poolCapacity returns the number of /30 subnets in base.
func poolCapacity(base *net.IPNet) uint32 {
	ones, _ := base.Mask.Size()
	total := uint32(1) << (32 - ones)
	return total / 4
}

func (p *Pool) Allocate(ctx context.Context, sandboxID string) (Allocation, error) {
	// Claim an index from the authoritative slot ledger. Atomic + lowest-free;
	// returns slotstore.ErrPoolExhausted when full (no silent dup, no leak).
	idx, err := p.slots.Claim(ctx, sandboxID, "legacy")
	if err != nil {
		return Allocation{}, err
	}

	p.mu.Lock()
	cid := p.nextCID
	p.nextCID = cid + 1
	p.mu.Unlock()

	subnet, host, guest, err := carve(p.base, idx)
	if err != nil {
		_, _ = p.slots.Release(ctx, sandboxID)
		return Allocation{}, err
	}
	alloc := Allocation{
		SandboxID: sandboxID,
		TAP:       tapName(sandboxID),
		HostIP:    host.String(),
		GuestIP:   guest.String(),
		MAC:       macFromIP(guest),
		VsockCID:  cid,
		Subnet:    subnet.String(),
		Idx:       idx,
	}

	if err := setupTAP(alloc); err != nil {
		_, _ = p.slots.Release(ctx, sandboxID)
		return Allocation{}, fmt.Errorf("setup tap: %w", err)
	}
	if err := p.store.SaveAllocation(ctx, alloc); err != nil {
		_ = teardownTAP(alloc.TAP)
		_, _ = p.slots.Release(ctx, sandboxID)
		return Allocation{}, err
	}
	// Persist the vsock CID high-water (separate uint32 space; not reclaimed).
	// Update ONLY the CID column — next_subnet is vestigial (slotstore owns slot
	// indices) and must not be clobbered.
	if err := p.store.SaveVsockCID(ctx, cid+1); err != nil {
		// Non-fatal: a lost CID just means the next boot may reissue it; vsock
		// CIDs are per-sandbox and short-lived, and the space is ~4 billion.
		_ = err
	}
	return alloc, nil
}

func (p *Pool) Release(ctx context.Context, sandboxID string) error {
	// Read the allocation payload so we know which kernel objects to tear down.
	// Missing row = already released; still attempt the slot release (idempotent).
	payload, err := p.store.GetAllocationJSON(ctx, sandboxID)
	var alloc Allocation
	if err == nil {
		_ = json.Unmarshal([]byte(payload), &alloc)
	}

	// Kernel teardown FIRST, while the slot is still owned. Mirrors the
	// destroy-first/free-last ordering in ReleaseNATID: freeing the slot before
	// destroying its netns lets a SIGKILL in between leave a reusable index whose
	// dead netns still poisons the next adopter. Destroy is idempotent, so a
	// re-run after a crash is harmless.
	if alloc.TAP != "" {
		if strings.HasPrefix(alloc.TAP, "ns-") {
			_ = netnsDestroy(alloc.TAP, alloc.Subnet)
		} else {
			_ = teardownTAP(alloc.TAP)
		}
	}
	// Remove the payload row next.
	storeErr := p.store.DeleteAllocation(ctx, sandboxID)
	// Free the slot LAST — the ownership hand-off. RowsAffected gate lives inside
	// slotstore, so concurrent same-id releases can't double-free.
	if _, serr := p.slots.Release(ctx, sandboxID); serr != nil {
		return serr
	}
	return storeErr
}

// Lookup returns a previously-allocated Allocation for a sandbox. Used by
// Wake to recover network identity after hibernation. Does NOT re-setup the
// TAP — caller must ensure it still exists (Hibernate keeps it up).
func (p *Pool) Lookup(ctx context.Context, sandboxID string) (Allocation, error) {
	payload, err := p.store.GetAllocationJSON(ctx, sandboxID)
	if err != nil {
		return Allocation{}, err
	}
	var a Allocation
	if err := json.Unmarshal([]byte(payload), &a); err != nil {
		return Allocation{}, err
	}
	return a, nil
}

// carve returns the idx-th /30 in base, with .1 = host, .2 = guest.
func carve(base *net.IPNet, idx uint32) (*net.IPNet, net.IP, net.IP, error) {
	baseStart := binary.BigEndian.Uint32(base.IP.To4())
	ones, _ := base.Mask.Size()
	free := uint32(1) << (32 - ones)
	if idx*4+4 > free {
		return nil, nil, nil, fmt.Errorf("CIDR pool exhausted")
	}
	subStart := baseStart + idx*4
	host := ipFromUint32(subStart + 1)
	guest := ipFromUint32(subStart + 2)
	subnet := &net.IPNet{IP: ipFromUint32(subStart), Mask: net.CIDRMask(30, 32)}
	return subnet, host, guest, nil
}

func ipFromUint32(v uint32) net.IP {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return net.IP(b)
}

func macFromIP(ip net.IP) string {
	v4 := ip.To4()
	return fmt.Sprintf("06:00:AC:%02X:%02X:%02X", v4[1], v4[2], v4[3])
}

func tapName(sandboxID string) string {
	// Linux iface names are capped at 15 chars. "fc" + 12 hex chars = 14.
	clean := strings.ReplaceAll(sandboxID, "-", "")
	if len(clean) > 12 {
		clean = clean[:12]
	}
	return "fc" + clean
}

func setupTAP(a Allocation) error {
	_ = run("ip", "link", "del", a.TAP) // ignore if absent
	steps := [][]string{
		{"ip", "tuntap", "add", "dev", a.TAP, "mode", "tap"},
		{"ip", "addr", "add", a.HostIP + "/30", "dev", a.TAP},
		{"ip", "link", "set", "dev", a.TAP, "up"},
	}
	for _, s := range steps {
		if err := run(s[0], s[1:]...); err != nil {
			return err
		}
	}
	// NAT (idempotent): add if not present
	if err := run("iptables", "-t", "nat", "-C", "POSTROUTING", "-s", a.Subnet, "-j", "MASQUERADE"); err != nil {
		if err := run("iptables", "-t", "nat", "-A", "POSTROUTING", "-s", a.Subnet, "-j", "MASQUERADE"); err != nil {
			return fmt.Errorf("add MASQUERADE: %w", err)
		}
	}
	// Allow forwarding between tap and the host's egress NIC. Auto-detect the
	// WAN iface (GCP=ens4, AWS/Lima=eth0) instead of hardcoding "eth0", which
	// silently no-op'd on GCP and left FORWARD-policy=DROP hosts without egress.
	wan := netns.DetectWAN()
	_ = run("iptables", "-C", "FORWARD", "-i", a.TAP, "-o", wan, "-j", "ACCEPT")
	_ = run("iptables", "-A", "FORWARD", "-i", a.TAP, "-o", wan, "-j", "ACCEPT")
	_ = run("iptables", "-C", "FORWARD", "-o", a.TAP, "-i", wan, "-j", "ACCEPT")
	_ = run("iptables", "-A", "FORWARD", "-o", a.TAP, "-i", wan, "-j", "ACCEPT")
	return nil
}

func teardownTAP(tap string) error {
	return run("ip", "link", "del", tap)
}

func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, string(out))
	}
	return nil
}
