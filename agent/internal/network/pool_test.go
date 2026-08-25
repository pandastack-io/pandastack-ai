// SPDX-License-Identifier: Apache-2.0
package network

import (
	"net"
	"testing"
)

// carve must produce the idx-th /30 with .1=host .2=guest, and reject indices
// beyond the pool. (Slot-index ownership/reuse is tested in internal/slotstore;
// here we only cover the pure IP math.)
func TestCarve(t *testing.T) {
	_, base, err := net.ParseCIDR("172.20.0.0/16")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	cases := []struct {
		idx               uint32
		wantSub, wantHost string
		wantGuest         string
	}{
		{0, "172.20.0.0/30", "172.20.0.1", "172.20.0.2"},
		{1, "172.20.0.4/30", "172.20.0.5", "172.20.0.6"},
		{2, "172.20.0.8/30", "172.20.0.9", "172.20.0.10"},
		{100, "172.20.1.144/30", "172.20.1.145", "172.20.1.146"},
	}
	for _, c := range cases {
		sub, host, guest, err := carve(base, c.idx)
		if err != nil {
			t.Fatalf("carve(%d): %v", c.idx, err)
		}
		if sub.String() != c.wantSub || host.String() != c.wantHost || guest.String() != c.wantGuest {
			t.Errorf("carve(%d) = %s/%s/%s, want %s/%s/%s",
				c.idx, sub, host, guest, c.wantSub, c.wantHost, c.wantGuest)
		}
	}
}

func TestCarveExhaustion(t *testing.T) {
	_, base, _ := net.ParseCIDR("172.20.0.0/16")
	// /16 has 16384 /30s (indices 0..16383); idx 16384 is out of range.
	if _, _, _, err := carve(base, 16384); err == nil {
		t.Fatal("carve past pool should error")
	}
	if _, _, _, err := carve(base, 16383); err != nil {
		t.Fatalf("carve at last valid index errored: %v", err)
	}
}

func TestPoolCapacity(t *testing.T) {
	for _, c := range []struct {
		cidr string
		want uint32
	}{
		{"172.20.0.0/16", 16384},
		{"10.200.0.0/16", 16384},
		{"10.0.0.0/24", 64},
		{"10.0.0.0/30", 1},
	} {
		_, n, _ := net.ParseCIDR(c.cidr)
		if got := poolCapacity(n); got != c.want {
			t.Errorf("poolCapacity(%s) = %d, want %d", c.cidr, got, c.want)
		}
	}
}

func TestMacFromIP(t *testing.T) {
	mac := macFromIP(net.ParseIP("172.20.1.146"))
	if mac != "06:00:AC:14:01:92" {
		t.Errorf("macFromIP = %s, want 06:00:AC:14:01:92", mac)
	}
}

func TestTapNameCappedTo14Chars(t *testing.T) {
	name := tapName("0123456789abcdef-0123")
	if len(name) > 15 {
		t.Errorf("tap name %q exceeds 15-char iface limit", name)
	}
	if name != "fc0123456789ab" {
		t.Errorf("tapName = %q, want fc0123456789ab", name)
	}
}
