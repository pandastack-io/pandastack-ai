// SPDX-License-Identifier: Apache-2.0
// pandastack-init is the in-guest reconfiguration agent that runs on every
// microVM boot (cold or snapshot-restore). It listens on a vsock port,
// receives an identity payload from the host (IP, MAC, gateway, hostname),
// applies it to the running guest, then exits so the rest of the boot
// (sshd, user workloads) can proceed.
//
// Why this exists: when Firecracker restores a snapshot, every restored
// VM starts with the same MAC + IP + hostname as the snapshotted source.
// To support template-snapshot-restore (the path to sub-second cold boot)
// we need a deterministic way to re-stamp each restored guest with its
// unique identity *before* the network is allowed to be used.
//
// Protocol: the host opens a connection to vsock CID 3 port 52525 inside
// the guest, writes a single JSON object terminated by '\n', and reads
// back a single status byte ('1' = applied, '0' = error). After ack the
// host closes the connection and the guest binary exits.
//
// systemd ordering: pandastack-init.service runs Before=network-online.target
// and Before=sshd.service, with Wants on both. If the host never opens a
// connection within the configured timeout we still exit clean so the
// guest boots in a degraded state (the original baked-in identity) — this
// keeps the binary safe to ship in templates used outside of Pandastack.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

// kmsgLogger writes timestamped lines to /dev/kmsg so they show up in the
// FC console.log even before journald is ready. Used for sub-second wake
// latency diagnosis.
var kmsg *os.File

func klog(format string, args ...any) {
	if kmsg == nil {
		return
	}
	// /dev/kmsg expects "<level>msg" lines. Level 6 = info.
	msg := fmt.Sprintf("<6>pandastack-init: "+format, args...)
	_, _ = kmsg.WriteString(msg + "\n")
}

// Config is the identity payload sent by the host. All fields are optional;
// any field left blank is skipped.
type Config struct {
	IP       string `json:"ip"`       // CIDR form, e.g. "172.20.1.42/24"
	MAC      string `json:"mac"`      // e.g. "06:00:AC:14:01:2A"
	Gateway  string `json:"gateway"`  // e.g. "172.20.1.1"
	Hostname string `json:"hostname"` // e.g. "sb-abc123"
	DNS      string `json:"dns"`      // optional, e.g. "1.1.1.1"
	Iface    string `json:"iface"`    // default "eth0"
}

func main() {
	var (
		port        = flag.Uint("port", 52525, "vsock port to listen on (cold-boot path)")
		hostPort    = flag.Uint("host-port", 52526, "host-side vsock port to dial after resume (snapshot path)")
		timeout     = flag.Duration("timeout", 120*time.Second, "give-up timeout")
		dialBackoff = flag.Duration("dial-backoff", 1*time.Millisecond, "initial guest→host dial retry backoff")
	)
	flag.Parse()

	log.SetFlags(0)
	log.SetPrefix("pandastack-init: ")
	if f, err := os.OpenFile("/dev/kmsg", os.O_WRONLY, 0); err == nil {
		kmsg = f
		defer kmsg.Close()
	}
	klog("startup pid=%d unix_ns=%d", os.Getpid(), time.Now().UnixNano())

	deadline := time.Now().Add(*timeout)

	// Race two delivery channels:
	//  (1) Cold-boot path: host→guest CONNECT to our vsock listener.
	//  (2) Snapshot-restore path: guest→host dial to (CID=2, hostPort) in
	//      a tight retry loop. The host blocks on accept() of a UDS at
	//      `<base>_<hostPort>`. The instant our vsock subsystem is alive
	//      after Resume, the dial succeeds → host's accept returns
	//      immediately. No host-side polling, no retry slop.
	//
	// First channel to deliver a successfully-decoded Config wins.
	type result struct {
		cfg Config
		err error
		via string
		c   net.Conn
	}
	results := make(chan result, 2)
	done := make(chan struct{})

	// Path 1: listen + accept loop.
	go func() {
		l, err := vsockListen(uint32(*port))
		if err != nil {
			klog("vsock listen FAILED: %v", err)
			results <- result{err: fmt.Errorf("listen: %w", err), via: "listen"}
			return
		}
		defer l.Close()
		klog("listening on vsock port=%d unix_ns=%d", *port, time.Now().UnixNano())
		for {
			select {
			case <-done:
				return
			default:
			}
			_ = l.(deadlineSetter).SetDeadline(time.Now().Add(500 * time.Millisecond))
			conn, err := l.Accept()
			if err != nil {
				if time.Now().After(deadline) {
					results <- result{err: err, via: "listen"}
					return
				}
				continue
			}
			klog("listen accepted unix_ns=%d", time.Now().UnixNano())
			cfg, ok := readConfig(conn)
			if !ok {
				conn.Close()
				continue
			}
			results <- result{cfg: cfg, via: "listen", c: conn}
			return
		}
	}()

	// Path 2: aggressive guest→host dial with exponential backoff.
	// This is the fast path for snapshot-restore: the moment our vsock
	// subsystem is alive post-resume, the dial succeeds and the host —
	// which has been blocked in accept() since BEFORE the restore — wakes
	// up instantly.
	go func() {
		backoff := *dialBackoff
		const maxBackoff = 25 * time.Millisecond
		for {
			select {
			case <-done:
				return
			default:
			}
			if time.Now().After(deadline) {
				results <- result{err: errors.New("dial deadline"), via: "dial"}
				return
			}
			c, err := vsockDialSyscall(vsockHost, uint32(*hostPort))
			if err == nil {
				klog("dial connected to host port=%d unix_ns=%d", *hostPort, time.Now().UnixNano())
				cfg, ok := readConfig(c)
				if !ok {
					c.Close()
					time.Sleep(backoff)
					continue
				}
				results <- result{cfg: cfg, via: "dial", c: c}
				return
			}
			time.Sleep(backoff)
			if backoff < maxBackoff {
				backoff *= 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
			}
		}
	}()

	// Path 3: MMDS HTTP poll — read identity from FC's MicroVM Metadata
	// Service at 169.254.169.254. MMDS state survives snapshot restore
	// and the host updates it via PUT /mmds BEFORE Resume. This path is
	// independent of virtio-vsock, which has a flaky post-restore wake
	// window (~13% of restores stuck at EOF). MMDS rides virtio-net,
	// which has proven reliable across the same restore boundary.
	go func() {
		client := &http.Client{Timeout: 250 * time.Millisecond}
		backoff := 25 * time.Millisecond
		const maxBackoff = 100 * time.Millisecond
		for {
			select {
			case <-done:
				return
			default:
			}
			if time.Now().After(deadline) {
				results <- result{err: errors.New("mmds deadline"), via: "mmds"}
				return
			}
			cfg, ok := pollMMDS(client)
			if ok {
				klog("mmds got identity unix_ns=%d", time.Now().UnixNano())
				results <- result{cfg: cfg, via: "mmds"}
				return
			}
			time.Sleep(backoff)
			if backoff < maxBackoff {
				backoff *= 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
			}
		}
	}()

	r := <-results
	close(done)
	if r.err != nil {
		klog("no config received: %v", r.err)
		log.Printf("no host config: %v", r.err)
		os.Exit(0)
	}
	klog("config received via=%s unix_ns=%d", r.via, time.Now().UnixNano())
	if r.cfg.Iface == "" {
		r.cfg.Iface = "eth0"
	}
	if err := apply(r.cfg); err != nil {
		klog("apply FAILED: %v", err)
		if r.c != nil {
			_, _ = r.c.Write([]byte("0\n"))
			r.c.Close()
		}
		log.Printf("apply: %v", err)
		os.Exit(1)
	}
	if r.c != nil {
		_, _ = r.c.Write([]byte("1\n"))
		r.c.Close()
	} else {
		// MMDS path: best-effort dial host to ack so deliverVsockConfig
		// returns successfully. If this fails, the host's wake-on-dial
		// path will hit deadline; that's recoverable because the guest's
		// network identity is already correct.
		if c, err := vsockDialSyscall(vsockHost, uint32(*hostPort)); err == nil {
			_, _ = c.Write([]byte("{\"ack\":\"mmds\"}\n1\n"))
			c.Close()
		}
	}
	klog("applied + exiting via=%s unix_ns=%d", r.via, time.Now().UnixNano())
	log.Printf("applied identity via=%s ip=%s mac=%s host=%s gw=%s",
		r.via, r.cfg.IP, r.cfg.MAC, r.cfg.Hostname, r.cfg.Gateway)
}

// pollMMDS does one HTTP GET against the MMDS endpoint and tries to decode
// an identity Config. The endpoint shape is `{"identity": {"ip":..., ...}}`.
// We require a non-placeholder body — the build VM seeds MMDS with
// `{"identity":{"placeholder":"1"}}` so until host PUTs real identity we
// keep polling. Returns (cfg, true) on success.
func pollMMDS(client *http.Client) (Config, bool) {
	var cfg Config
	req, err := http.NewRequest(http.MethodGet, "http://169.254.169.254/identity", nil)
	if err != nil {
		return cfg, false
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return cfg, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		_, _ = io.Copy(io.Discard, resp.Body)
		return cfg, false
	}
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
		return cfg, false
	}
	if cfg.IP == "" || cfg.MAC == "" || cfg.Gateway == "" {
		return cfg, false
	}
	return cfg, true
}

// readConfig pulls one JSON Config from c (3s deadline). Returns ok=false
// on probe/decode error so callers can re-try.
func readConfig(c net.Conn) (Config, bool) {
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	var cfg Config
	if err := json.NewDecoder(c).Decode(&cfg); err != nil {
		klog("decode: %v", err)
		return cfg, false
	}
	return cfg, true
}

// apply reconfigures the running guest. It is intentionally idempotent:
// safe to re-run, safe if the desired state is already correct.
func apply(c Config) error {
	steps := []struct {
		label string
		cmd   []string
		skip  bool
	}{
		// MAC must change while the link is down.
		{"link down", []string{"ip", "link", "set", c.Iface, "down"}, false},
		{"mac set", []string{"ip", "link", "set", c.Iface, "address", c.MAC}, c.MAC == ""},
		{"flush addrs", []string{"ip", "addr", "flush", "dev", c.Iface}, false},
		{"link up", []string{"ip", "link", "set", c.Iface, "up"}, false},
		{"addr add", []string{"ip", "addr", "add", c.IP, "dev", c.Iface}, c.IP == ""},
		{"route del default", []string{"ip", "route", "del", "default"}, c.Gateway == ""},
		{"route add", []string{"ip", "route", "add", "default", "via", c.Gateway, "dev", c.Iface}, c.Gateway == ""},
	}
	for _, s := range steps {
		if s.skip {
			continue
		}
		out, err := exec.Command(s.cmd[0], s.cmd[1:]...).CombinedOutput()
		// "route del default" is allowed to fail if no default exists yet.
		if err != nil && s.label != "route del default" {
			return fmt.Errorf("%s: %w (%s)", s.label, err, strings.TrimSpace(string(out)))
		}
	}

	if c.Hostname != "" {
		if err := os.WriteFile("/etc/hostname", []byte(c.Hostname+"\n"), 0o644); err != nil {
			return fmt.Errorf("write /etc/hostname: %w", err)
		}
		_ = exec.Command("hostname", c.Hostname).Run()
	}

	if c.DNS != "" {
		_ = os.WriteFile("/etc/resolv.conf", []byte("nameserver "+c.DNS+"\n"), 0o644)
	}

	return nil
}

// vsockListen wraps the Linux "vsock" network. It returns a net.Listener
// whose Accept yields connections from the host (CID 2).
func vsockListen(port uint32) (net.Listener, error) {
	// In modern Go (1.18+) AF_VSOCK is supported via the "vsock" network
	// string only on some platforms via golang.org/x/sys. The most portable
	// path is to call socket(2) directly.
	return vsockListenSyscall(port)
}

// deadlineSetter lets us SetDeadline on the listener.
type deadlineSetter interface {
	SetDeadline(time.Time) error
}
