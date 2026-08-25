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
	"os"
	"os/exec"
	"path/filepath"
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
		// CROSS-TENANT ISOLATION (security): a guest must never reach another
		// guest. Without this, tenant A could scan the pool /16 from inside its
		// VM and hit neighbor guests' SSH:22 / app ports / Postgres:5432 (the
		// per-netns DNAT exposes every guest port, ip_forward routes connected
		// /30s, and the egress ACCEPTs below would otherwise permit it). Legit
		// traffic is unaffected: WAN egress and host-mediated DB access (via the
		// db-proxy in root netns) do not transit pool->pool. This DROP must be
		// the FIRST FORWARD rule so it wins over the appended egress ACCEPTs.
		ensureRootFirst("FORWARD", "-s", s.PoolCIDR, "-d", s.PoolCIDR, "-j", "DROP")

		// CLOUD-METADATA SSRF BLOCK (security, CRITICAL): a guest must never
		// reach the cloud metadata service at the link-local 169.254.169.254. On
		// GCP that endpoint hands out the HOST VM's service-account OAuth token
		// (full cloud-platform scope) — a tenant sandbox could `curl` it and steal
		// a token that reads EVERY customer's GCS seeds/snapshots and, depending on
		// the SA's IAM, compromise the whole project. NATID mode wires no
		// Firecracker MMDS, and ip_forward + the WAN MASQUERADE below would
		// otherwise route guest packets straight to the metadata IP. Drop the
		// entire 169.254.0.0/16 link-local range (a guest never legitimately routes
		// link-local off-host). Inserted FIRST, before the egress ACCEPTs.
		ensureRootFirst("FORWARD", "-s", s.PoolCIDR, "-d", "169.254.0.0/16", "-j", "DROP")

		// CRYPTO-MINING ABUSE PREVENTION (security): drop guest egress to the
		// well-known Stratum mining-pool ports. We run untrusted tenant code, so
		// a free-tier abuser spinning up a miner is a recurring abuse vector (and
		// trips the cloud provider's crypto-mining abuse detector, which can get
		// the whole host/project flagged). Blocking the Stratum control ports at
		// the host FORWARD chain kills the pool handshake for the overwhelming
		// majority of miners with zero impact on legitimate workloads — these are
		// not ports any normal app dials outbound. Inserted FIRST (like the
		// cross-tenant DROP) so it precedes the egress ACCEPTs. Ports are tunable
		// via PANDASTACK_BLOCKED_EGRESS_PORTS (comma-separated; empty disables).
		for _, port := range blockedEgressPorts() {
			ensureRootFirst("FORWARD", "-s", s.PoolCIDR, "-o", s.WANIface,
				"-p", "tcp", "--dport", port, "-j", "DROP")
		}

		ensureRoot("-t", "nat", "POSTROUTING",
			"-s", s.PoolCIDR, "-o", s.WANIface, "-j", "MASQUERADE")
		ensureRoot("FORWARD", "-s", s.PoolCIDR, "-o", s.WANIface, "-j", "ACCEPT")
		ensureRoot("FORWARD", "-d", s.PoolCIDR, "-i", s.WANIface, "-j", "ACCEPT")
	}
	return nil
}

// defaultBlockedEgressPorts are the well-known Stratum / cryptocurrency
// mining-pool TCP ports. Blocking the pool handshake stops the miner before it
// can hash. This is a denylist, not a panacea (a miner can use a custom port or
// tunnel over 443), but it kills the default config of essentially every
// off-the-shelf miner and every public pool's standard endpoints — which is the
// abuse we actually see. Pairs with a CPU-abuse watchdog (future) for miners
// that evade the port block.
var defaultBlockedEgressPorts = []string{
	"3333",  // Stratum (most common default)
	"4444",  // Stratum / XMRig default
	"5555",  // Stratum alt
	"7777",  // Stratum alt
	"8333",  // Bitcoin p2p (also a mining vector)
	"9999",  // Stratum alt
	"14444", // XMRig / Monero pools
	"45700", // Monero (supportxmr et al.)
}

// blockedEgressPorts returns the mining-pool ports to drop from guest egress.
// Override via PANDASTACK_BLOCKED_EGRESS_PORTS (comma-separated TCP ports).
// Setting it to an empty value (PANDASTACK_BLOCKED_EGRESS_PORTS="") disables the
// block — an explicit opt-out for a single-tenant/trusted deployment.
func blockedEgressPorts() []string {
	v, set := os.LookupEnv("PANDASTACK_BLOCKED_EGRESS_PORTS")
	if !set {
		return defaultBlockedEgressPorts
	}
	if v = strings.TrimSpace(v); v == "" {
		return nil // explicit opt-out
	}
	var ports []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			ports = append(ports, p)
		}
	}
	return ports
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

// ensureRootFirst is like ensureRoot but INSERTS the rule at the top of the
// chain (-I <chain> 1) instead of appending, so it is evaluated before any
// already-present rules. Idempotent via the same -C guard, so re-running on
// every Create does not stack duplicates. Used for the cross-tenant DROP, which
// must precede the egress ACCEPT rules to take effect.
func ensureRootFirst(args ...string) {
	if run("iptables", iptablesCheckArgs(args)...) == nil {
		return // already present
	}
	_ = run("iptables", iptablesInsertArgs(args)...)
}

// iptablesVerbAt returns the index where the iptables verb (-A/-C/-I) belongs,
// skipping a leading "-t <table>".
func iptablesVerbAt(args []string) int {
	if len(args) >= 2 && args[0] == "-t" {
		return 2
	}
	return 0
}

// iptablesCheckArgs builds the "-C" existence-check form of a rule.
func iptablesCheckArgs(args []string) []string {
	verbAt := iptablesVerbAt(args)
	out := append(append([]string{}, args[:verbAt]...), "-C")
	return append(out, args[verbAt:]...)
}

// iptablesInsertArgs builds the "-I <chain> 1" insert-at-top form of a rule, so
// the rule is evaluated before any already-present rules in the chain.
func iptablesInsertArgs(args []string) []string {
	verbAt := iptablesVerbAt(args)
	chain := args[verbAt]
	rest := args[verbAt+1:]
	out := append(append([]string{}, args[:verbAt]...), "-I", chain, "1")
	return append(out, rest...)
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

// Exists reports whether a network namespace of the given name is present.
// iproute2 mount-binds each named netns at /var/run/netns/<name> (a bind to
// /proc/<pid>/ns/net), so a stat there is the authoritative, race-free check —
// no need to shell out to `ip netns list`. Used by Wake to detect a netns that
// was torn down (e.g. the prewarmer rebuilt the slot pool across an agent
// restart) before launching Firecracker into a namespace that no longer exists.
func Exists(name string) bool {
	if name == "" {
		return false
	}
	if _, err := os.Stat(filepath.Join("/var/run/netns", name)); err == nil {
		return true
	}
	return false
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

// List returns all named network namespaces. iproute2 bind-mounts each at
// /var/run/netns/<name>, so a readdir is the exec-free source of truth (same
// place Exists checks). Returns nil (no error) when the dir is absent.
func List() ([]string, error) {
	ents, err := os.ReadDir("/var/run/netns")
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names, nil
}

// ClearStaleHolders removes any ROOT-netns veth (other than keepVeth) that still
// carries hostVethIP — a leftover from a crashed sandbox on the same /30. Such a
// leftover makes the connected route for that /30 ambiguous, so a dial to the
// peer (guest proxy) address can route into the dead namespace instead of the
// live one → i/o timeout. Deleting the stale root veth alone removes the
// ambiguous route and fully cures the poison; we deliberately do NOT also delete
// a namespace derived from the veth name (a name derivation could match an
// unrelated live namespace) — the boot sweep reaps namespaces by an authoritative
// predicate. Returns the veth names cleared.
//
// One address-filtered query (`ip -o -4 addr show to <ip>/32`), no per-netns
// fan-out: in the healthy case it returns only keepVeth and deletes nothing.
func ClearStaleHolders(hostVethIP, keepVeth string) []string {
	if hostVethIP == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ip", "-o", "-4", "addr", "show", "to", hostVethIP+"/32").CombinedOutput()
	if err != nil {
		return nil
	}
	var cleared []string
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		// `ip -o addr` format: "<ifindex>: <dev> ..." — field[1] is the device
		// name (already colon-free in this format; TrimSuffix is defensive).
		dev := strings.TrimSuffix(f[1], ":")
		// Only touch our own per-sandbox host veths (vh-*), never keepVeth (the
		// slot we're about to use), and never an unrelated interface.
		if dev == "" || dev == keepVeth || !strings.HasPrefix(dev, "vh-") {
			continue
		}
		// Deleting the veth auto-removes its peer in the (now-dead) namespace.
		_ = run("ip", "link", "del", dev)
		cleared = append(cleared, dev)
	}
	return cleared
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
