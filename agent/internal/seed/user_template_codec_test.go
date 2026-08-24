// SPDX-License-Identifier: Apache-2.0
package seed

import (
	"encoding/json"
	"testing"
)

// TestRootfsArtifact guards the codec→artifact mapping that keeps the zstd
// switch backward-compatible: legacy images (codec "" or "gzip") must still
// resolve to rootfs.tar.gz so they keep pulling after new bakes go zstd.
func TestRootfsArtifact(t *testing.T) {
	cases := map[string]string{
		"":      "rootfs.tar.gz",  // legacy manifest, no codec field
		"gzip":  "rootfs.tar.gz",  // explicit legacy
		"zstd":  "rootfs.tar.zst", // new
		"bogus": "rootfs.tar.gz",  // unknown → safe legacy default
	}
	for codec, want := range cases {
		if got := rootfsArtifact(codec); got != want {
			t.Errorf("rootfsArtifact(%q) = %q, want %q", codec, got, want)
		}
	}
}

// TestManifestCodecRoundTrips confirms a legacy manifest (no codec key)
// unmarshals to Codec=="" (→ gzip path) and a new one preserves "zstd".
func TestManifestCodecRoundTrips(t *testing.T) {
	// Legacy manifest JSON predates the codec field entirely.
	legacy := `{"schema":3,"workspace":"w","template":"app-x","generation":"1","tar_sha256":"abc","tar_bytes":10}`
	var m UserTemplateManifest
	if err := json.Unmarshal([]byte(legacy), &m); err != nil {
		t.Fatalf("unmarshal legacy: %v", err)
	}
	if m.Codec != "" {
		t.Fatalf("legacy manifest Codec = %q, want empty", m.Codec)
	}
	if rootfsArtifact(m.Codec) != "rootfs.tar.gz" {
		t.Fatalf("legacy manifest must resolve to rootfs.tar.gz")
	}

	// A new manifest round-trips zstd.
	nm := UserTemplateManifest{Schema: userTemplateSchema, Codec: userTplCodecZstd}
	b, _ := json.Marshal(nm)
	var back UserTemplateManifest
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal new: %v", err)
	}
	if back.Codec != userTplCodecZstd || rootfsArtifact(back.Codec) != "rootfs.tar.zst" {
		t.Fatalf("new manifest codec round-trip failed: %q", back.Codec)
	}
}
