// SPDX-License-Identifier: Apache-2.0
// Package network manages per-sandbox TAP devices and /30 IP allocations.
//
// Each sandbox gets:
//   - tap<short-id> on the Lima VM
//   - a /30 carved from the configured CIDR (e.g. 172.20.0.0/16 → /30s)
//   - guest MAC derived from the IP
//   - vsock CID (monotonic counter, persisted in SQLite)
//
// Allocation must be persisted so we can recover after an agent crash.
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
}

type Pool struct {
	mu      sync.Mutex
	store   *store.Store
	base    *net.IPNet
	next    uint32 // index of next /30 (0..N)
	nextCID uint32

	// natidFree is a per-template-identity free list of pre-built NATID
	// slots. The key is identityKey(tapHostIP, guestIP, mac). Pre-building
	// netns + veth + tap + iptables ahead of POST /sandboxes lets
	// AllocateNATID return in O(1) (~5ms) instead of ~500ms.
	natidFree map[string][]NATIDAlloc
	// natidRefill, if set, is invoked (in a goroutine) after each Claim so
	// the manager can top the pool back up.
	natidRefill func(identityKey string)
}

const minVsockCID = 3 // 0,1,2 are reserved by virtio-vsock spec

func NewPool(cidr string, st *store.Store) (*Pool, error) {
	_, n, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, fmt.Errorf("parse cidr: %w", err)
	}
	used, err := st.LoadNetworkState(context.Background())
	if err != nil {
		return nil, err
	}
	p := &Pool{store: st, base: n, next: used.NextSubnet, nextCID: used.NextVsockCID,
		natidFree: map[string][]NATIDAlloc{}}
	if p.nextCID < minVsockCID {
		p.nextCID = minVsockCID
	}
	return p, nil
}

func (p *Pool) Allocate(ctx context.Context, sandboxID string) (Allocation, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	idx := p.next
	subnet, host, guest, err := carve(p.base, idx)
	if err != nil {
		return Allocation{}, err
	}
	cid := p.nextCID
	tap := tapName(sandboxID)
	mac := macFromIP(guest)

	alloc := Allocation{
		SandboxID: sandboxID,
		TAP:       tap,
		HostIP:    host.String(),
		GuestIP:   guest.String(),
		MAC:       mac,
		VsockCID:  cid,
		Subnet:    subnet.String(),
	}

	if err := setupTAP(alloc); err != nil {
		return Allocation{}, fmt.Errorf("setup tap: %w", err)
	}

	if err := p.store.SaveAllocation(ctx, alloc); err != nil {
		_ = teardownTAP(alloc.TAP)
		return Allocation{}, err
	}

	p.next = idx + 1
	p.nextCID = cid + 1
	if err := p.store.SaveNetworkState(ctx, store.NetworkState{NextSubnet: p.next, NextVsockCID: p.nextCID}); err != nil {
		return Allocation{}, err
	}
	return alloc, nil
}

func (p *Pool) Release(ctx context.Context, sandboxID string) error {
	p.mu.Lock()

	alloc, err := p.store.GetAllocation(ctx, sandboxID)
	if err != nil {
		p.mu.Unlock()
		return err
	}
	// Delete the DB record while holding the lock, then release before the
	// slow kernel-level teardown. netnsDestroy can block for seconds if the
	// guest process is still alive (conntrack cleanup) — holding the mutex
	// during that would starve every concurrent Release/AllocateNATID call.
	storeErr := p.store.DeleteAllocation(ctx, sandboxID)
	p.mu.Unlock()

	if strings.HasPrefix(alloc.TAP, "ns-") {
		_ = netnsDestroy(alloc.TAP, alloc.Subnet)
	} else {
		_ = teardownTAP(alloc.TAP)
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
