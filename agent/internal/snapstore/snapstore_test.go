// SPDX-License-Identifier: Apache-2.0
package snapstore

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// Meta round-trip: BakedTapHostIP/BakedGuestIP/BakedMAC must survive
// JSON encode/decode. These three fields are load-bearing for cross-agent
// time-travel fork — losing them silently routes traffic to the wrong
// guest IP on the destination agent after template bake-counter drift.
func TestMetaRoundtrip_BakedIdentity(t *testing.T) {
	dir := t.TempDir()
	in := Meta{
		OriginalSandboxID: "sb-001",
		RootfsHostPath:    "/var/lib/pandastack/vms/sb-001/rootfs.ext4",
		Template:          "code-interpreter",
		NATID:             true,
		VsockUDSPath:      "/var/lib/pandastack/vms/sb-001/fc-vsock.sock",
		BakedTapHostIP:    "172.20.14.37",
		BakedGuestIP:      "172.20.14.38",
		BakedMAC:          "06:00:AC:14:0E:26",
	}
	if err := WriteMeta(dir, in); err != nil {
		t.Fatalf("WriteMeta: %v", err)
	}
	out, err := ReadMeta(dir)
	if err != nil {
		t.Fatalf("ReadMeta: %v", err)
	}
	if out != in {
		t.Fatalf("meta mismatch:\n got:  %+v\n want: %+v", out, in)
	}
}

// Pre-Feb-2026 snapshots have no baked-identity fields. Reading them
// must succeed and leave those fields as zero values (back-compat).
func TestMetaRoundtrip_LegacyMeta(t *testing.T) {
	dir := t.TempDir()
	legacy := `{
	  "original_sandbox_id": "sb-old",
	  "rootfs_host_path": "/x/rootfs.ext4",
	  "template": "py",
	  "natid": true,
	  "vsock_uds_path": "/x/v.sock"
	}`
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := ReadMeta(dir)
	if err != nil {
		t.Fatalf("ReadMeta legacy: %v", err)
	}
	if m.OriginalSandboxID != "sb-old" || !m.NATID {
		t.Fatalf("legacy meta fields lost: %+v", m)
	}
	if m.BakedGuestIP != "" || m.BakedMAC != "" || m.BakedTapHostIP != "" {
		t.Fatalf("legacy meta should have zero baked identity, got %+v", m)
	}
}

// downloadLock must return the SAME mutex for the same snap id, so
// concurrent Downloads of the same id serialize. Different ids return
// different mutexes (no global serialization).
func TestDownloadLock_SamePerID_DifferentAcross(t *testing.T) {
	a1 := downloadLock("snap-aaa")
	a2 := downloadLock("snap-aaa")
	b1 := downloadLock("snap-bbb")
	if a1 != a2 {
		t.Fatalf("expected same mutex for same id")
	}
	if a1 == b1 {
		t.Fatalf("expected different mutexes for different ids")
	}
}

// Concurrency: two goroutines holding downloadLock("X") never both run
// at the same time. The second waits for the first to release.
func TestDownloadLock_Serializes(t *testing.T) {
	var (
		inCritical atomic.Int32
		maxSeen    atomic.Int32
		wg         sync.WaitGroup
	)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lk := downloadLock("contended-snap")
			lk.Lock()
			defer lk.Unlock()
			n := inCritical.Add(1)
			for {
				prev := maxSeen.Load()
				if n <= prev || maxSeen.CompareAndSwap(prev, n) {
					break
				}
			}
			// hold briefly so any racing goroutine has time to violate
			for j := 0; j < 1000; j++ {
				_ = j
			}
			inCritical.Add(-1)
		}()
	}
	wg.Wait()
	if maxSeen.Load() != 1 {
		t.Fatalf("downloadLock failed to serialize: saw %d concurrent holders", maxSeen.Load())
	}
}

// recordingGsutil puts a `gsutil` on PATH that only records that it was run
// and fails, and returns a function that reports how often it was. No test in
// this package may reach the real tool: it would talk to a real bucket with
// whatever credentials the machine has.
func recordingGsutil(t *testing.T) (calls func() int) {
	t.Helper()
	bin := t.TempDir()
	log := filepath.Join(t.TempDir(), "calls")
	stub := "#!/bin/sh\necho \"$*\" >> " + log + "\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "gsutil"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() int {
		b, err := os.ReadFile(log)
		if err != nil {
			return 0
		}
		return strings.Count(string(b), "\n")
	}
}

// Download of a snapshot that is already complete locally (vm.mem, vm.state
// and meta.json) must return nil without touching gsutil. This is the
// same-agent fork fast path: no GCS round-trip.
//
// The test used to write only vm.mem and vm.state. Since the fast path also
// asks for meta.json, that took the repair path instead and ran the machine's
// real gsutil four times against a bucket named after this test, while the
// comment above still said "without touching gsutil".
func TestDownload_LocalShortCircuit(t *testing.T) {
	calls := recordingGsutil(t)
	s := &Store{Bucket: "irrelevant-because-files-exist"}
	dir := t.TempDir()
	for name, body := range map[string]string{"vm.mem": "x", "vm.state": "y", "meta.json": "{}"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Download(context.Background(), "any-id", dir); err != nil {
		t.Fatalf("Download of a locally complete snapshot should be nil, got: %v", err)
	}
	if n := calls(); n != 0 {
		t.Fatalf("the local fast path ran gsutil %d time(s)", n)
	}
}

// Without meta.json the snapshot is not known to be complete, so Download
// goes to the store for the optional pieces: that is the repair path, and it
// does run gsutil. vm.mem and vm.state are local, so they are not fetched
// again.
func TestDownload_WithoutMetaAsksTheStore(t *testing.T) {
	calls := recordingGsutil(t)
	s := &Store{Bucket: "irrelevant-because-files-exist"}
	dir := t.TempDir()
	for name, body := range map[string]string{"vm.mem": "x", "vm.state": "y"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Download(context.Background(), "any-id", dir); err != nil {
		t.Fatalf("a legacy snapshot without meta or disk should still restore, got: %v", err)
	}
	if n := calls(); n == 0 {
		t.Fatal("the repair path did not ask the store for the optional pieces")
	}
	for _, name := range []string{"vm.mem", "vm.state"} {
		if b, _ := os.ReadFile(filepath.Join(dir, name)); len(b) != 1 {
			t.Errorf("%s was replaced: %q", name, b)
		}
	}
}
