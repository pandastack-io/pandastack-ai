// SPDX-License-Identifier: Apache-2.0
// Package netns provides per-sandbox Linux network namespace lifecycle for
// NAT-identity restore: every microVM is spawned inside its own netns so
// it can re-use the same baked guest IP/MAC/gateway without collision.
//
// Layout per sandbox `ns-<short>`:
//
//	root netns                       sandbox netns
//	  veth-h<short> 10.200.X.1/30 <-> veth-g<short> 10.200.X.2/30
//	                                  tap0          172.20.6.117/30 (host side of /30)
//	                                  guest         172.20.6.118    (baked, NEVER changes)
//
//	nat rules inside netns:
//	  PREROUTING -d <vethGuestIP> -p tcp --dport N -j DNAT --to <guest>:N
//	  POSTROUTING -o tap0          -j SNAT --to <tapHostIP>
//
// To talk to the guest from root netns: dial veth-host's peer IP
// (10.200.X.2). The kernel routes via veth-host, DNAT rewrites to
// 172.20.6.118, SNAT rewrites source so the guest sees 172.20.6.117 (its
// baked gateway) and replies via its default route. Reply traffic
// untraverses both NATs back to the caller.
package netns

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Spec defines a sandbox netns. All fields are caller-provided so the
// network package owns ID allocation.
type Spec struct {
	Name           string // netns name, e.g. "ns-abc123"
	VethHost       string // root-side veth, e.g. "vh-abc123"
	VethGuest      string // ns-side veth, e.g. "vg-abc123"
	VethHostIP     string // root-side IPv4, e.g. "10.200.0.1"
	VethGuestIP    string // ns-side IPv4, e.g. "10.200.0.2"
	VethSubnetMask int    // /30
	TapName        string // always "tap0" inside ns
	TapHostIP      string // baked host-side gateway IP, e.g. "172.20.6.117"
	TapSubnetMask  int    // /30
	GuestIP        string // baked guest IP, e.g. "172.20.6.118"
	// PortMap maps a TCP port on the veth-guest IP to the same port on the
	// baked guest IP. {22:22} forwards SSH; orchestrator may add more.
	PortMap map[int]int

	// WANIface is the host's egress interface (e.g. "ens4"/"eth0"). When set
	// (together with PoolCIDR), Create wires outbound internet for the guest:
	// a default route inside the netns + a shared root MASQUERADE for the
	// pool. Empty disables egress wiring (e.g. unit tests).
	WANIface string
	// PoolCIDR is the whole NATID veth pool (e.g. "10.200.0.0/16"). A single
	// shared root MASQUERADE/FORWARD pair covers every sandbox, so there is
	// no per-sandbox root rule to clean up on teardown.
	PoolCIDR string
}

// Create sets up the netns and all interfaces + iptables rules. Idempotent:
// removes any pre-existing netns of the same name first. On error, attempts
// teardown before returning.
func Create(s Spec) error {
	_ = Destroy(s)

	steps := [][]string{
		{"ip", "netns", "add", s.Name},
		{"ip", "-n", s.Name, "link", "set", "lo", "up"},

		{"ip", "link", "add", s.VethHost, "type", "veth", "peer", "name", s.VethGuest},
		{"ip", "link", "set", s.VethGuest, "netns", s.Name},

		{"ip", "addr", "add", fmt.Sprintf("%s/%d", s.VethHostIP, s.VethSubnetMask), "dev", s.VethHost},
		{"ip", "link", "set", s.VethHost, "up"},
		{"ip", "-n", s.Name, "addr", "add", fmt.Sprintf("%s/%d", s.VethGuestIP, s.VethSubnetMask), "dev", s.VethGuest},
		{"ip", "-n", s.Name, "link", "set", s.VethGuest, "up"},

		{"ip", "netns", "exec", s.Name, "ip", "tuntap", "add", "dev", s.TapName, "mode", "tap"},
		{"ip", "-n", s.Name, "addr", "add", fmt.Sprintf("%s/%d", s.TapHostIP, s.TapSubnetMask), "dev", s.TapName},
		{"ip", "-n", s.Name, "link", "set", s.TapName, "up"},

		{"ip", "netns", "exec", s.Name, "sysctl", "-q", "-w", "net.ipv4.ip_forward=1"},
		{"ip", "netns", "exec", s.Name, "sysctl", "-q", "-w", "net.ipv4.conf.all.route_localnet=1"},
	}
	for _, cmd := range steps {
		if err := run(cmd[0], cmd[1:]...); err != nil {
			_ = Destroy(s)
			return err
		}
	}

	for hostPort, guestPort := range s.PortMap {
		if err := run("ip",
			"netns", "exec", s.Name,
			"iptables", "-t", "nat", "-A", "PREROUTING",
			"-d", s.VethGuestIP,
			"-p", "tcp", "--dport", fmt.Sprintf("%d", hostPort),
			"-j", "DNAT", "--to-destination", fmt.Sprintf("%s:%d", s.GuestIP, guestPort),
		); err != nil {
			_ = Destroy(s)
			return err
		}
	}
	// Wildcard DNAT: forward every other TCP port on the veth-guest IP to the
	// same port on the baked guest IP. Lets the agent's /proxy/{port}/ handler
	// reach arbitrary user-listening ports (e.g. 3000 for Next.js) without
	// having to mutate iptables on every request. Explicit per-port rules
	// above are matched first (iptables PREROUTING is ordered), so port 22
	// keeps its original destination.
	if err := run("ip",
		"netns", "exec", s.Name,
		"iptables", "-t", "nat", "-A", "PREROUTING",
		"-d", s.VethGuestIP,
		"-p", "tcp",
		"-j", "DNAT", "--to-destination", s.GuestIP,
	); err != nil {
		_ = Destroy(s)
		return err
	}
	if err := run("ip",
		"netns", "exec", s.Name,
		"iptables", "-t", "nat", "-A", "POSTROUTING",
		"-o", s.TapName,
		"-j", "SNAT", "--to-source", s.TapHostIP,
	); err != nil {
		_ = Destroy(s)
		return err
	}

	// ── Outbound internet egress for the guest ──────────────────────────────
	// Without this the netns has no default route, so guest-originated traffic
	// (DNS, git clone, npm install) fails with "Network is unreachable". Only
	// wired when the caller supplies the host WAN iface + pool CIDR.
	if s.WANIface != "" {
		if err := setupEgress(s); err != nil {
			_ = Destroy(s)
			return err
		}
	}

	return nil
}

// setupEgress gives the guest outbound internet. Two netns-local rules (torn
// down automatically when the netns is deleted) plus shared, idempotent root
// rules for the whole pool (no per-sandbox cleanup):
//
//	netns:  default route via the root-side veth peer
//	netns:  SNAT guest /30 -> the UNIQUE veth-guest IP on egress out the veth
//	        (the baked guest IP is SHARED across sandboxes of the same template,
//	        so it must be rewritten to a unique source before reaching root,
//	        else conntrack collides)
//	root:   MASQUERADE + FORWARD for the pool CIDR out the WAN iface
func setupEgress(s Spec) error {
	tapNet := cidrNetwork(s.TapHostIP, s.TapSubnetMask)

	// netns: default route so the guest can reach anything off-subnet.
	if err := run("ip", "-n", s.Name, "route", "replace", "default",
		"via", s.VethHostIP, "dev", s.VethGuest); err != nil {
		return fmt.Errorf("netns default route: %w", err)
	}
	// netns: rewrite the (shared) baked guest source to the unique veth-guest
	// IP as it leaves toward root, so the shared /16 root MASQUERADE matches
	// and return traffic disambiguates per sandbox.
	if err := run("ip", "netns", "exec", s.Name,
		"iptables", "-t", "nat", "-A", "POSTROUTING",
		"-s", tapNet, "-o", s.VethGuest,
		"-j", "SNAT", "--to-source", s.VethGuestIP,
	); err != nil {
		return fmt.Errorf("netns egress SNAT: %w", err)
	}

	// root: shared pool-wide rules. Idempotent (-C guard), so re-adding on
	// every Create is a no-op after the first and there is nothing to remove
	// in Destroy.
	if s.PoolCIDR != "" {
		ensureRoot("-t", "nat", "POSTROUTING",
			"-s", s.PoolCIDR, "-o", s.WANIface, "-j", "MASQUERADE")
		ensureRoot("FORWARD", "-s", s.PoolCIDR, "-o", s.WANIface, "-j", "ACCEPT")
		ensureRoot("FORWARD", "-d", s.PoolCIDR, "-i", s.WANIface, "-j", "ACCEPT")
	}
	return nil
}

// ensureRoot adds a root-netns iptables rule only if an identical one is not
// already present. The chain is the first element after any leading "-t
// <table>"; the -C/-A verb is inserted automatically. Best-effort: failures
// are swallowed because the rule may race with a concurrent Create.
func ensureRoot(args ...string) {
	verbAt := 0
	if len(args) >= 2 && args[0] == "-t" {
		verbAt = 2
	}
	check := append(append([]string{}, args[:verbAt]...), "-C")
	check = append(check, args[verbAt:]...)
	if run("iptables", check...) == nil {
		return // already present
	}
	add := append(append([]string{}, args[:verbAt]...), "-A")
	add = append(add, args[verbAt:]...)
	_ = run("iptables", add...)
}

// cidrNetwork returns the network CIDR (e.g. "172.20.6.116/30") containing ip
// at the given prefix length. Falls back to a /32 host route on parse error.
func cidrNetwork(ip string, mask int) string {
	_, n, err := net.ParseCIDR(fmt.Sprintf("%s/%d", ip, mask))
	if err != nil {
		return ip + "/32"
	}
	return n.String()
}

// Destroy tears down the netns and root-side veth. Errors are swallowed
// because partial state (e.g. from a crashed agent) is normal.
func Destroy(s Spec) error {
	if s.VethHost != "" {
		_ = run("ip", "link", "del", s.VethHost)
	}
	if s.Name != "" {
		_ = run("ip", "netns", "del", s.Name)
	}
	return nil
}

func run(name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

var (
	wanOnce  sync.Once
	wanIface string
)

// DetectWAN returns the host's primary egress interface — the one carrying the
// default route to the internet (e.g. "ens4" on GCP, "eth0" on AWS/Lima). The
// result is cached after the first call. Falls back to "eth0" if detection
// fails. Hardcoding "eth0" was a latent bug: GCP's primary NIC is "ens4", so
// the legacy FORWARD rules never matched.
func DetectWAN() string {
	wanOnce.Do(func() { wanIface = detectWAN() })
	return wanIface
}

func detectWAN() string {
	// The iface used to reach a public address is the most reliable signal.
	if dev := devFromRoute("get", "1.1.1.1"); dev != "" {
		return dev
	}
	if dev := devFromRoute("show", "default"); dev != "" {
		return dev
	}
	return "eth0"
}

// devFromRoute runs `ip -o route <args...>` and returns the token after "dev".
func devFromRoute(args ...string) string {
	full := append([]string{"-o", "route"}, args...)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ip", full...).CombinedOutput()
	if err != nil {
		return ""
	}
	fields := strings.Fields(string(out))
	for i, f := range fields {
		if f == "dev" && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	return ""
}
