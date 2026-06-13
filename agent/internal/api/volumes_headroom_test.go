// SPDX-License-Identifier: Apache-2.0
package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const gib = int64(1) << 30

func TestHeadroomDecision(t *testing.T) {
	base := headroomState{
		FSSizeBytes:      200 * gib,
		FSFreeBytes:      150 * gib,
		OversubFactor:    3.0,
		FreeReserveBytes: 20 * gib,
	}

	cases := []struct {
		name       string
		mut        func(*headroomState)
		wantRefuse bool
		wantSubstr string
	}{
		{
			name: "healthy host admits",
			mut: func(st *headroomState) {
				st.ProvisionedBytes = 100 * gib
				st.RequestBytes = 10 * gib
			},
		},
		{
			name: "oversubscription budget exceeded",
			mut: func(st *headroomState) {
				st.ProvisionedBytes = 595 * gib // budget = 3 x 200 = 600
				st.RequestBytes = 10 * gib
			},
			wantRefuse: true,
			wantSubstr: "host budget",
		},
		{
			name: "exactly at budget admits (boundary)",
			mut: func(st *headroomState) {
				st.ProvisionedBytes = 590 * gib
				st.RequestBytes = 10 * gib // 600 == budget, not >
			},
		},
		{
			name: "free space below reserve refuses even when budget fine",
			mut: func(st *headroomState) {
				st.ProvisionedBytes = 10 * gib
				st.RequestBytes = 1 * gib
				st.FSFreeBytes = 19 * gib
			},
			wantRefuse: true,
			wantSubstr: "free space below reserve",
		},
		{
			name: "free space exactly at reserve admits (boundary)",
			mut: func(st *headroomState) {
				st.RequestBytes = 1 * gib
				st.FSFreeBytes = 20 * gib
			},
		},
		{
			name: "unknown filesystem size fails open",
			mut: func(st *headroomState) {
				st.FSSizeBytes = 0
				st.FSFreeBytes = 0
				st.ProvisionedBytes = 1000 * gib
				st.RequestBytes = 64 * gib
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := base
			tc.mut(&st)
			reason := headroomDecision(st)
			if tc.wantRefuse && reason == "" {
				t.Fatalf("expected refusal, got admission")
			}
			if !tc.wantRefuse && reason != "" {
				t.Fatalf("expected admission, got refusal: %s", reason)
			}
			if tc.wantSubstr != "" && !strings.Contains(reason, tc.wantSubstr) {
				t.Fatalf("reason %q does not contain %q", reason, tc.wantSubstr)
			}
		})
	}
}

// provisionedVolumeBytes must count legacy root, per-workspace, and DB
// volumes against the same host budget, and ignore non-.ext4 files.
func TestProvisionedVolumeBytes(t *testing.T) {
	dataDir := t.TempDir()
	write := func(rel string, size int64) {
		p := filepath.Join(dataDir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("volumes/legacy.ext4", 100)
	write("volumes/ws/acme/data.ext4", 200)
	write("volumes/ws/other/scratch.ext4", 300)
	write("volumes/db/sbx-1.ext4", 400)
	write("volumes/notes.txt", 999)        // ignored: not .ext4
	write("templates/base/rootfs.ext4", 5) // ignored: outside volumes/

	if got, want := provisionedVolumeBytes(dataDir), int64(1000); got != want {
		t.Fatalf("provisionedVolumeBytes = %d, want %d", got, want)
	}

	// Missing volumes dir is zero, not an error.
	if got := provisionedVolumeBytes(t.TempDir()); got != 0 {
		t.Fatalf("empty dataDir provisioned = %d, want 0", got)
	}
}
