// SPDX-License-Identifier: Apache-2.0
package network

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/pandastack/agent/internal/netns"
	"github.com/pandastack/agent/internal/store"
)

func storeNetState(nextSubnet, nextCID uint32) store.NetworkState {
	return store.NetworkState{NextSubnet: nextSubnet, NextVsockCID: nextCID}
}

// NATIDAlloc is the NAT-identity allocation: a per-sandbox netns + veth pair
// that isolates a FIXED baked guest IP/MAC. The orchestrator uses
// ProxyGuestIP as the address it dials for SSH/HTTP into the guest:
// packets to that IP are routed across the veth into the netns, where
// a DNAT rule rewrites the destination to the baked guest IP.
type NATIDAlloc struct {
	SandboxID string `json:"sandbox_id"`

	// Netns isolation
	Netns     string `json:"netns"`
	VethHost  string `json:"veth_host"`
	VethGuest string `json:"veth_guest"`

	// Veth /30: host-side reachable address that orchestrator dials
	ProxyHostIP string `json:"proxy_host_ip"` // root-netns address (host-side veth, not dial target)
	ProxyGuestIP string `json:"proxy_guest_ip"` // sandbox-netns veth address — DNAT target; orchestrator dials this

	// Baked guest identity (must match snapshot)
	TapName    string `json:"tap_name"`     // always "tap0"
	TapHostIP  string `json:"tap_host_ip"`  // baked gateway
	GuestIP    string `json:"guest_ip"`     // baked guest IP
	MAC        string `json:"mac"`          // baked MAC
}

// natidPoolBase is the /16 we carve /30s from for the proxy veth pairs.
// 10.200.0.0/16 gives us 16384 sandboxes per agent before exhaustion.
const natidPoolBase = "10.200.0.0/16"

// NATIDIdentityKey is the public form of natidIdentityKey, exported for
// callers that need to mirror the pool's key format (e.g. refill maps).
func NATIDIdentityKey(tapHostIP, guestIP, mac string, ports map[int]int) string {
	return natidIdentityKey(tapHostIP, guestIP, mac, ports)
}

// natidIdentityKey is the cache key for pre-built NATID slots: a slot can be
// claimed only by a sandbox restoring from a snapshot with the SAME baked
// identity (because TapHostIP/GuestIP/MAC are wired into the iptables and
// `ip` commands inside the netns at pre-build time).
func natidIdentityKey(tapHostIP, guestIP, mac string, ports map[int]int) string {
	parts := []string{tapHostIP, guestIP, mac}
	keys := make([]int, 0, len(ports))
	for k := range ports {
		keys = append(keys, k)
	}
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%d:%d", k, ports[k]))
	}
	return strings.Join(parts, "|")
}

// SetNATIDRefill registers a callback that gets invoked (in a goroutine) after
// each successful Claim so the caller can top the pool back up.
func (p *Pool) SetNATIDRefill(fn func(identityKey string)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.natidRefill = fn
}

// PrebuildNATID creates one ready-to-claim NATID slot (netns + veth + tap +
// iptables) for the given baked identity and parks it on the free list.
// Safe to call concurrently. Returns the count after the build.
func (p *Pool) PrebuildNATID(ctx context.Context, tapHostIP, guestIP, mac string, ports map[int]int) (int, error) {
	if ports == nil {
		ports = map[int]int{22: 22}
	}
	p.mu.Lock()
	idx := p.next
	_, base, err := net.ParseCIDR(natidPoolBase)
	if err != nil {
		p.mu.Unlock()
		return 0, err
	}
	baseStart := binary.BigEndian.Uint32(base.IP.To4())
	subStart := baseStart + idx*4
	if subStart+4 > baseStart+(1<<16) {
		p.mu.Unlock()
		return 0, fmt.Errorf("NATID pool exhausted")
	}
	hostIP := ipFromUint32(subStart + 1).String()
	guestVethIP := ipFromUint32(subStart + 2).String()
	short := fmt.Sprintf("p%08x", idx)
	nsName := "ns-" + short
	vh := "vh-" + short
	vg := "vg-" + short
	p.next = idx + 1
	if err := p.store.SaveNetworkState(ctx, storeNetState(p.next, p.nextCID)); err != nil {
		p.mu.Unlock()
		return 0, err
	}
	p.mu.Unlock()

	spec := netns.Spec{
		Name:           nsName,
		VethHost:       vh,
		VethGuest:      vg,
		VethHostIP:     hostIP,
		VethGuestIP:    guestVethIP,
		VethSubnetMask: 30,
		TapName:        "tap0",
		TapHostIP:      tapHostIP,
		TapSubnetMask:  30,
		GuestIP:        guestIP,
		PortMap:        ports,
		WANIface:       netns.DetectWAN(),
		PoolCIDR:       natidPoolBase,
	}
	if err := netns.Create(spec); err != nil {
		return 0, fmt.Errorf("netns create: %w", err)
	}
	slot := NATIDAlloc{
		Netns:        nsName,
		VethHost:     vh,
		VethGuest:    vg,
		ProxyHostIP:  hostIP,
		ProxyGuestIP: guestVethIP,
		TapName:      "tap0",
		TapHostIP:    tapHostIP,
		GuestIP:      guestIP,
		MAC:          mac,
	}
	key := natidIdentityKey(tapHostIP, guestIP, mac, ports)
	p.mu.Lock()
	p.natidFree[key] = append(p.natidFree[key], slot)
	n := len(p.natidFree[key])
	p.mu.Unlock()
	return n, nil
}

// NATIDPoolSize returns the number of free pre-built slots for the given
// identity.
func (p *Pool) NATIDPoolSize(tapHostIP, guestIP, mac string, ports map[int]int) int {
	if ports == nil {
		ports = map[int]int{22: 22}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.natidFree[natidIdentityKey(tapHostIP, guestIP, mac, ports)])
}


//
// The baked guest identity (TapHostIP/GuestIP/MAC) is fixed by the template
// snapshot; we read it from the caller. Callers should use
// TemplateBakedIdentity() to learn what was baked.
// AllocateNATID returns a NATID slot — preferring a pre-built one from the
// free list (~5ms) and falling back to building one from scratch (~500ms).
//
// The baked guest identity (TapHostIP/GuestIP/MAC) is fixed by the template
// snapshot; we read it from the caller. Callers should use
// TemplateBakedIdentity() to learn what was baked.
func (p *Pool) AllocateNATID(ctx context.Context, sandboxID, tapHostIP, guestIP, mac string, ports map[int]int) (NATIDAlloc, error) {
	if ports == nil {
		ports = map[int]int{22: 22}
	}
	key := natidIdentityKey(tapHostIP, guestIP, mac, ports)

	// Fast path: pop a pre-built slot from the free list.
	p.mu.Lock()
	if list := p.natidFree[key]; len(list) > 0 {
		slot := list[len(list)-1]
		p.natidFree[key] = list[:len(list)-1]
		refill := p.natidRefill
		p.mu.Unlock()
		slot.SandboxID = sandboxID
		alloc := Allocation{
			SandboxID: sandboxID,
			TAP:       slot.Netns,
			HostIP:    slot.ProxyHostIP,
			GuestIP:   slot.GuestIP,
			MAC:       slot.MAC,
			Subnet:    slot.VethHost,
		}
		// Persist asynchronously. The slot is already wired up (netns + veth
		// + tap + iptables) so the sandbox can boot immediately; the DB row
		// just enables crash-recovery cleanup. On agent restart any
		// orphaned "ns-p*" netns is reaped by the prewarm path's idx.
		go func() {
			pctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = p.store.SaveAllocation(pctx, alloc)
		}()
		if refill != nil {
			go refill(key)
		}
		return slot, nil
	}
	p.mu.Unlock()

	// Slow path: build from scratch (original behaviour).
	p.mu.Lock()
	defer p.mu.Unlock()

	idx := p.next
	_, base, err := net.ParseCIDR(natidPoolBase)
	if err != nil {
		return NATIDAlloc{}, err
	}
	baseStart := binary.BigEndian.Uint32(base.IP.To4())
	subStart := baseStart + idx*4
	if subStart+4 > baseStart+(1<<16) {
		return NATIDAlloc{}, fmt.Errorf("NATID pool exhausted")
	}
	hostIP := ipFromUint32(subStart + 1).String()
	guestVethIP := ipFromUint32(subStart + 2).String()

	short := shortHex(sandboxID)
	nsName := "ns-" + short
	vh := "vh-" + short
	vg := "vg-" + short

	if ports == nil {
		ports = map[int]int{22: 22}
	}

	spec := netns.Spec{
		Name:           nsName,
		VethHost:       vh,
		VethGuest:      vg,
		VethHostIP:     hostIP,
		VethGuestIP:    guestVethIP,
		VethSubnetMask: 30,
		TapName:        "tap0",
		TapHostIP:      tapHostIP,
		TapSubnetMask:  30,
		GuestIP:        guestIP,
		PortMap:        ports,
		WANIface:       netns.DetectWAN(),
		PoolCIDR:       natidPoolBase,
	}
	if err := netns.Create(spec); err != nil {
		return NATIDAlloc{}, fmt.Errorf("netns create: %w", err)
	}

	a := NATIDAlloc{
		SandboxID:    sandboxID,
		Netns:        nsName,
		VethHost:     vh,
		VethGuest:    vg,
		ProxyHostIP:  hostIP,
		ProxyGuestIP: guestVethIP,
		TapName:      "tap0",
		TapHostIP:    tapHostIP,
		GuestIP:      guestIP,
		MAC:          mac,
	}

	// Persist as the same Allocation struct shape so existing Delete/Lookup
	// paths work. We stash NATID-specific fields in TAP/Subnet by convention:
	// TAP = netns name, Subnet = veth host IP (CIDR-less).
	if err := p.store.SaveAllocation(ctx, Allocation{
		SandboxID: sandboxID,
		TAP:       nsName,         // netns name (Delete will spot the "ns-" prefix)
		HostIP:    hostIP,          // proxy host IP
		GuestIP:   guestIP,         // baked guest IP
		MAC:       mac,
		Subnet:    vh,              // veth-host name
	}); err != nil {
		_ = netns.Destroy(spec)
		return NATIDAlloc{}, err
	}

	p.next = idx + 1
	if err := p.store.SaveNetworkState(ctx, storeNetState(p.next, p.nextCID)); err != nil {
		return NATIDAlloc{}, err
	}
	return a, nil
}

// ReleaseNATID tears down the netns + veth created by AllocateNATID.
// Safe to call multiple times.
func (p *Pool) ReleaseNATID(ctx context.Context, sandboxID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	alloc, err := p.store.GetAllocation(ctx, sandboxID)
	if err != nil {
		return err
	}
	// NATID stashes netns name in TAP and veth-host name in Subnet.
	if strings.HasPrefix(alloc.TAP, "ns-") {
		_ = netns.Destroy(netns.Spec{Name: alloc.TAP, VethHost: alloc.Subnet})
	} else {
		_ = teardownTAP(alloc.TAP)
	}
	return p.store.DeleteAllocation(ctx, sandboxID)
}

func shortHex(sandboxID string) string {
	clean := strings.ReplaceAll(sandboxID, "-", "")
	if len(clean) > 10 {
		clean = clean[:10]
	}
	return clean
}
