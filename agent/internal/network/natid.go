// SPDX-License-Identifier: Apache-2.0
package network

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"

	"github.com/pandastack/agent/internal/netns"
)

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

	// Idx is the /30 slot index this slot occupies. The slotstore owns the index
	// authoritatively; this field carries it from prebuild→claim so the fast path
	// can adopt the exact row and the persisted payload can record it.
	Idx uint32 `json:"idx"`
}

// natidPoolBase is the /16 we carve /30s from for the proxy veth pairs.
// 10.200.0.0/16 gives us 16384 sandboxes per agent before exhaustion.
const natidPoolBase = "10.200.0.0/16"

// natidShort formats the idx-derived short name used for the prebuilt netns/veth.
func natidShort(idx uint32) string { return fmt.Sprintf("p%08x", idx) }

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

// carveNATID returns the host/guest veth IPs for a /30 slot index in the NATID
// base. Errors if idx is beyond the pool.
func carveNATID(idx uint32) (hostIP, guestVethIP string, err error) {
	_, base, perr := net.ParseCIDR(natidPoolBase)
	if perr != nil {
		return "", "", perr
	}
	baseStart := binary.BigEndian.Uint32(base.IP.To4())
	subStart := baseStart + idx*4
	if subStart+4 > baseStart+(1<<16) {
		return "", "", fmt.Errorf("NATID pool exhausted")
	}
	return ipFromUint32(subStart + 1).String(), ipFromUint32(subStart + 2).String(), nil
}

// PrebuildNATID creates one ready-to-claim NATID slot (netns + veth + tap +
// iptables) for the given baked identity and parks it on the free list. The /30
// index is claimed from the slotstore under a "prebuilt:<idx>" sentinel so a
// crash can never leak it — startup Reconcile reclaims any prebuilt sentinel
// whose netns is gone. Safe to call concurrently. Returns the count after build.
func (p *Pool) PrebuildNATID(ctx context.Context, tapHostIP, guestIP, mac string, ports map[int]int) (int, error) {
	if ports == nil {
		ports = map[int]int{22: 22}
	}
	// Claim a slot index (authoritative, atomic, lowest-free) under a prebuilt
	// sentinel. ErrPoolExhausted surfaces cleanly when full.
	idx, err := p.slots.ClaimPrebuilt(ctx, "prebuilt")
	if err != nil {
		return 0, err
	}
	hostIP, guestVethIP, err := carveNATID(idx)
	if err != nil {
		_, _ = p.slots.Release(ctx, slotPrebuiltOwner(idx))
		return 0, err
	}
	short := natidShort(idx)
	nsName := "ns-" + short
	vh := "vh-" + short
	vg := "vg-" + short

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
		// Build failed — release the claimed slot so it isn't leaked.
		_, _ = p.slots.Release(ctx, slotPrebuiltOwner(idx))
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
		Idx:          idx,
	}
	key := natidIdentityKey(tapHostIP, guestIP, mac, ports)
	p.mu.Lock()
	p.natidFree[key] = append(p.natidFree[key], slot)
	n := len(p.natidFree[key])
	p.mu.Unlock()
	return n, nil
}

// slotPrebuiltOwner mirrors slotstore.PrebuiltOwner for the network package.
func slotPrebuiltOwner(idx uint32) string {
	return fmt.Sprintf("prebuilt:%08x", idx)
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

// AllocateNATID returns a NATID slot — preferring a pre-built one from the free
// list (~5ms) and falling back to building one from scratch (~500ms).
//
// The baked guest identity (TapHostIP/GuestIP/MAC) is fixed by the template
// snapshot; we read it from the caller. Callers should use
// TemplateBakedIdentity() to learn what was baked.
func (p *Pool) AllocateNATID(ctx context.Context, sandboxID, tapHostIP, guestIP, mac string, ports map[int]int) (NATIDAlloc, error) {
	if ports == nil {
		ports = map[int]int{22: 22}
	}
	key := natidIdentityKey(tapHostIP, guestIP, mac, ports)

	// Fast path: pop a pre-built slot from the in-memory free list.
	p.mu.Lock()
	if list := p.natidFree[key]; len(list) > 0 {
		slot := list[len(list)-1]
		p.natidFree[key] = list[:len(list)-1]
		refill := p.natidRefill
		p.mu.Unlock()

		// Adopt the slot's index from the prebuilt sentinel to the real sandbox,
		// atomically. If adoption fails (slot reconciled away under us — e.g. a
		// concurrent startup reconcile freed it), fall through to a fresh build.
		adopted, aerr := p.slots.Reassign(ctx, slotPrebuiltOwner(slot.Idx), sandboxID, "natid", slot.Idx)
		if aerr == nil && adopted {
			slot.SandboxID = sandboxID
			alloc := Allocation{
				SandboxID: sandboxID,
				TAP:       slot.Netns,
				HostIP:    slot.ProxyHostIP,
				GuestIP:   slot.GuestIP,
				MAC:       slot.MAC,
				Subnet:    slot.VethHost,
				Idx:       slot.Idx,
			}
			// Persist the payload SYNCHRONOUSLY. The slot index is already durably
			// owned by sandboxID (Reassign committed above), but the payload row
			// is what records the netns NAME (ns-p<idx>, derived from the slot
			// index, not the sandbox id) so teardown can find the kernel objects.
			// If we persisted async and crashed in between, the slot would be owned
			// but the netns name unrecoverable → orphaned netns/veth. This is a
			// sub-ms LOCAL SQLite write (the async form was a holdover from when the
			// store was remote), so it doesn't meaningfully slow the ~5ms fast path.
			if serr := p.store.SaveAllocation(ctx, alloc); serr != nil {
				// Roll back the adoption so the slot + netns aren't half-owned.
				_, _ = p.slots.Release(ctx, sandboxID)
				_ = netns.Destroy(netns.Spec{Name: slot.Netns, VethHost: slot.VethHost})
				if refill != nil {
					go refill(key)
				}
				return NATIDAlloc{}, fmt.Errorf("persist natid allocation: %w", serr)
			}
			// Self-heal: if a crashed sandbox on this /30 left a stale root-side
			// veth still carrying this slot's ProxyHostIP, the connected route for
			// the /30 is ambiguous and a dial to the guest proxy could route into
			// the dead namespace → i/o timeout (the netns-poison loop). Clear any
			// such stale holder before returning — never touching THIS slot's own
			// veth (keepVeth). Cheap: one address-filtered query; a no-op in the
			// healthy case.
			if cleared := netns.ClearStaleHolders(slot.ProxyHostIP, "vh-"+natidShort(slot.Idx)); len(cleared) > 0 {
				slog.Warn("natid: cleared stale veth holders on adopt",
					"slot", slot.Idx, "proxy_host_ip", slot.ProxyHostIP, "cleared", cleared)
			}
			if refill != nil {
				go refill(key)
			}
			return slot, nil
		}
		// Adoption failed: the parked netns is now orphaned (its sentinel slot is
		// gone). Best-effort destroy so we don't leak the kernel objects, then
		// fall through to build a fresh slot below.
		_ = netns.Destroy(netns.Spec{Name: slot.Netns, VethHost: slot.VethHost})
		if refill != nil {
			go refill(key)
		}
	} else {
		p.mu.Unlock()
	}

	// Slow path: claim a fresh slot index and build from scratch.
	idx, err := p.slots.Claim(ctx, sandboxID, "natid")
	if err != nil {
		return NATIDAlloc{}, err
	}
	hostIP, guestVethIP, err := carveNATID(idx)
	if err != nil {
		_, _ = p.slots.Release(ctx, sandboxID)
		return NATIDAlloc{}, err
	}

	short := shortHex(sandboxID)
	nsName := "ns-" + short
	vh := "vh-" + short
	vg := "vg-" + short

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
		_, _ = p.slots.Release(ctx, sandboxID)
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
		Idx:          idx,
	}

	// Persist as the same Allocation struct shape so Delete/Lookup work.
	// Convention: TAP = netns name (Release spots the "ns-" prefix), Subnet =
	// veth-host name. The slotstore (not the Subnet) owns the reclaimable index.
	if err := p.store.SaveAllocation(ctx, Allocation{
		SandboxID: sandboxID,
		TAP:       nsName,
		HostIP:    hostIP,
		GuestIP:   guestIP,
		MAC:       mac,
		Subnet:    vh,
		Idx:       idx,
	}); err != nil {
		_ = netns.Destroy(spec)
		_, _ = p.slots.Release(ctx, sandboxID)
		return NATIDAlloc{}, err
	}
	return a, nil
}

// ReleaseNATID tears down the netns + veth created by AllocateNATID and frees
// the slot. Safe to call multiple times (the slotstore release is idempotent).
func (p *Pool) ReleaseNATID(ctx context.Context, sandboxID string) error {
	// Read the payload to know which kernel objects to destroy. The fast path now
	// persists the payload SYNCHRONOUSLY before returning (see AllocateNATID), so
	// a present slot ownership implies a present payload row in the normal case.
	// If the row is genuinely missing we do NOT guess the netns name from the slot
	// index: the index→name mapping only holds for prebuilt slots (ns-p<idx>),
	// NOT slow-path slots (ns-<shortHex(sandboxID)>), so a guess could destroy an
	// unrelated prebuilt netns at the same index. We free the slot index
	// regardless (the atomic guarantee); an orphaned netns is harmlessly
	// overwritten when that index is next prebuilt (netns.Create destroys the
	// same-named ns first).
	// (1) Read payload FIRST — the payload row is the only record of the netns
	// NAME, so it must be read before DeleteAllocation drops it.
	payload, perr := p.store.GetAllocationJSON(ctx, sandboxID)
	var alloc Allocation
	if perr == nil {
		_ = json.Unmarshal([]byte(payload), &alloc)
	}

	// (2) DESTROY KERNEL OBJECTS FIRST, while the slot is STILL OWNED. This is
	// the ordering that closes the poison: freeing the /30 slot index before its
	// netns was destroyed (the old order) let a SIGKILL in between leave the
	// index reusable while the dead netns still answered ARP for the guest IP —
	// a new sandbox adopting that index then routed into the dead namespace and
	// its create failed in a loop (the static-builder-host incident). With
	// destroy-first/free-last, a crash leaves an OWNED-but-dead slot (reclaimed
	// by startup Reconcile), never a FREED-but-poisoned one. Destroy is
	// idempotent (ip link/netns del no-op if absent), so a re-run after a crash
	// or a double release is harmless. Payload-missing / TAP=="" still SKIPS
	// destroy: the index→name mapping only holds for prebuilt slots (ns-p<idx>),
	// not slow-path (ns-<shortHex>), so guessing could destroy an unrelated
	// prebuilt netns at the same index.
	if alloc.TAP != "" {
		if strings.HasPrefix(alloc.TAP, "ns-") {
			_ = netns.Destroy(netns.Spec{Name: alloc.TAP, VethHost: alloc.Subnet})
		} else {
			_ = teardownTAP(alloc.TAP)
		}
	}

	// (3) Drop the payload row next. If we crash between (2) and (3), the payload
	// survives and the SAME idempotent destroy re-runs on the retry/recovery path
	// (still slot-owned).
	storeErr := p.store.DeleteAllocation(ctx, sandboxID)

	// (4) FREE THE SLOT LAST — the ownership hand-off. Once committed, the /30
	// index is reusable, and by construction its kernel objects are already gone.
	if _, serr := p.slots.Release(ctx, sandboxID); serr != nil {
		return serr
	}
	return storeErr
}

func shortHex(sandboxID string) string {
	clean := strings.ReplaceAll(sandboxID, "-", "")
	if len(clean) > 10 {
		clean = clean[:10]
	}
	return clean
}

// liveNetnsNames returns the set of netns names recorded in the allocation
// payload of each live sandbox — the ONLY reliable source, because an
// adopted-prebuilt sandbox's netns is "ns-p<idx>" (encodes the slot index, not
// the sandbox id) and cannot be re-derived from the id. The bool return is
// ok=false when ANY live sandbox's payload could not be read (a real store
// error, distinct from a clean not-found): the caller MUST then skip the sweep,
// because sweeping while the keep-set is incomplete could delete a live
// (possibly persistent DB/app) netns — a live incident, whereas a missed orphan
// self-heals on the next boot. Fail closed.
func (p *Pool) liveNetnsNames(ctx context.Context, liveIDs []string) (keep map[string]bool, ok bool) {
	keep = make(map[string]bool, len(liveIDs))
	for _, id := range liveIDs {
		payload, err := p.store.GetAllocationJSON(ctx, id)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				// No allocation row for this id (e.g. a non-NATID or not-yet-
				// networked sandbox): nothing to keep, not an error.
				continue
			}
			return nil, false // real read error → keep-set incomplete → abort sweep.
		}
		var a Allocation
		if json.Unmarshal([]byte(payload), &a) == nil && strings.HasPrefix(a.TAP, "ns-") {
			keep[a.TAP] = true
		}
	}
	return keep, true
}

// SweepOrphanNetns deletes every "ns-*" network namespace not backed by a live
// sandbox. Reclaims namespaces leaked by a crash/SIGKILL mid-teardown (a
// non-atomic kernel op that no ordering can make crash-proof) so a leaked netns
// never accumulates or poisons a reused /30 across a restart.
//
// MUST be called ONCE at boot, BEFORE the NATID prewarmer starts: at that point
// the in-memory free list is empty and Reconcile has already freed every
// prebuilt sentinel slot, so any surviving "ns-p*" is a dead prior-generation
// pool leftover (the prewarmer rebuilds it fresh via netns.Create's
// destroy-first). Running it concurrently with an active prewarmer could race a
// namespace the prewarmer just created — hence the boot-only, pre-prewarm
// requirement. Fails closed: if the live keep-set can't be fully built, it
// sweeps nothing.
func (p *Pool) SweepOrphanNetns(ctx context.Context, liveIDs []string) (int, error) {
	keep, ok := p.liveNetnsNames(ctx, liveIDs)
	if !ok {
		return 0, fmt.Errorf("skip netns sweep: incomplete live keep-set (store read error)")
	}
	all, err := netns.List()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, name := range orphanNetnsToSweep(all, keep) {
		// Derive the root-side veth from the netns name (both schemes:
		// ns-p<idx>→vh-p<idx>, ns-<hex>→vh-<hex>) so Destroy removes it too.
		vh := "vh-" + strings.TrimPrefix(name, "ns-")
		_ = netns.Destroy(netns.Spec{Name: name, VethHost: vh})
		n++
	}
	return n, nil
}

// orphanNetnsToSweep is the pure decision core of SweepOrphanNetns (testable
// without touching the kernel): of all namespaces, return the "ns-*" ones NOT in
// the live keep-set. Non-"ns-" namespaces (CNI, operator, other tenants on a
// shared host) are NEVER touched.
func orphanNetnsToSweep(all []string, keep map[string]bool) []string {
	var del []string
	for _, name := range all {
		if !strings.HasPrefix(name, "ns-") || keep[name] {
			continue
		}
		del = append(del, name)
	}
	return del
}
