// SPDX-License-Identifier: Apache-2.0
package uffd

import (
	"encoding/json"
	"testing"
)

// TestMappingDecode confirms the JSON field names match what Firecracker
// serialises onto the handoff socket (uffd_utils.rs: `page_size` in BYTES,
// stable across v1.13.0..v1.16.0). A rename here would make every restore
// fail to locate the faulting region.
func TestMappingDecode(t *testing.T) {
	const in = `[{"base_host_virt_addr":4096,"size":8192,"offset":0,"page_size":4096}]`
	var got []GuestRegionUffdMapping
	if err := json.Unmarshal([]byte(in), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	m := got[0]
	if m.BaseHostVirtAddr != 4096 || m.Size != 8192 || m.Offset != 0 || m.PageSize != 4096 {
		t.Fatalf("decoded mapping wrong: %+v", m)
	}
	if m.pageSize() != 4096 {
		t.Fatalf("pageSize() = %d, want 4096", m.pageSize())
	}
}

// TestMappingDecodeHugepage covers the 2 MiB hugetlbfs case: FC reports the
// page size in bytes (2097152), and a missing/zero field falls back to 4 KiB.
func TestMappingDecodeHugepage(t *testing.T) {
	const in = `[{"base_host_virt_addr":0,"size":2097152,"offset":0,"page_size":2097152}]`
	var got []GuestRegionUffdMapping
	if err := json.Unmarshal([]byte(in), &got); err != nil {
		t.Fatal(err)
	}
	if got[0].pageSize() != 2<<20 {
		t.Fatalf("pageSize() = %d, want %d", got[0].pageSize(), 2<<20)
	}
	var zero GuestRegionUffdMapping
	if zero.pageSize() != 4096 {
		t.Fatalf("zero-value pageSize() = %d, want 4096", zero.pageSize())
	}
}

// TestContainsAndFileOffset checks region membership and the host-address ->
// file-offset translation that drives every resolver lookup.
func TestContainsAndFileOffset(t *testing.T) {
	// Region at host VA [0x10000, 0x10000+0x2000) maps to file offset 0x5000.
	m := GuestRegionUffdMapping{BaseHostVirtAddr: 0x10000, Size: 0x2000, Offset: 0x5000}

	if m.contains(0x0FFFF) {
		t.Error("addr below base should not be contained")
	}
	if !m.contains(0x10000) {
		t.Error("base addr should be contained")
	}
	if !m.contains(0x11FFF) {
		t.Error("last byte should be contained")
	}
	if m.contains(0x12000) {
		t.Error("one past end should not be contained")
	}

	// A page at base maps to Offset; a page 0x1000 in maps to Offset+0x1000.
	if off := m.fileOffset(0x10000); off != 0x5000 {
		t.Errorf("fileOffset(base) = %#x, want 0x5000", off)
	}
	if off := m.fileOffset(0x11000); off != 0x6000 {
		t.Errorf("fileOffset(base+0x1000) = %#x, want 0x6000", off)
	}
}
