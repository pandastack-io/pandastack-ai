// SPDX-License-Identifier: Apache-2.0

// Package memstream implements a chunked, zero-eliding representation of a
// Firecracker memory snapshot (vm.mem) so it can be streamed on demand from
// object storage at restore time instead of downloaded in full.
//
// Background. PandaStack's default restore path downloads the entire multi-GB
// vm.mem to local disk (gsutil cp) before Firecracker can mmap it. On a fresh
// host that download dominates first-boot latency. The streaming path instead
// boots Firecracker against a userfaultfd backend and fetches only the memory
// chunks the guest actually touches, range-read from GCS.
//
// The Header is the metadata that makes per-chunk fetches possible. It is
// produced once at template-bake time and uploaded alongside vm.mem as
// vm.mem.header. It records, for a fixed chunk size, which chunks contain any
// non-zero bytes ("present") versus which are entirely zero. A fresh guest's
// RAM is mostly zero pages, so eliding zero chunks both shrinks the uploaded
// object and removes those chunks from the fault path (they are reconstructed
// by zero-filling, never fetched).
//
// This v1 format intentionally does NOT implement cross-build page-level
// dedup (base/diff columnar layouts). It chunks a single, self-contained memfile.
// That keeps the on-disk format and the fault resolver simple while still
// removing the full-download cliff. A future v2 can layer base/diff dedup on
// top without changing the UFFD resolver's chunk-fetch contract.
package memstream

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

const (
	// Magic identifies a v1 memstream header ("PandaStack Mem v1").
	Magic = "PSM1"

	// DefaultChunkSize is the granularity at which vm.mem is fetched from
	// object storage. 4 MiB is the industry-standard chunk size and a good
	// balance: large enough to amortize per-request overhead on a GCS range
	// GET, small enough that a single page fault doesn't pull tens of MB.
	DefaultChunkSize = 4 << 20 // 4 MiB

	// PageSize is the guest page size. Headers track presence at chunk
	// granularity, not page granularity, but the resolver still aligns
	// UFFDIO_COPY installs to PageSize.
	PageSize = 4096

	// headerVersion is the current on-disk format version.
	headerVersion = 1
)

// Header describes how a vm.mem file is split into fixed-size chunks and which
// of those chunks contain non-zero data. It is the manifest the UFFD resolver
// consults to decide, for a faulting offset, whether to range-fetch a chunk
// from object storage or simply zero-fill.
type Header struct {
	// Version is the on-disk format version (currently 1).
	Version uint32

	// ChunkSize is the byte size of every chunk except possibly the last,
	// which may be shorter when TotalSize is not a multiple of ChunkSize.
	ChunkSize uint32

	// TotalSize is the full logical size of the memory file in bytes. This
	// MUST equal the guest RAM size; Firecracker requires the restored memory
	// backing to be exactly the snapshot's memory size.
	TotalSize uint64

	// present[i] is true when chunk i contains at least one non-zero byte and
	// therefore must be fetched. A false entry is reconstructed by zero-fill.
	present []bool
}

// NumChunks returns the number of chunks the memory file is divided into.
func (h *Header) NumChunks() int {
	if h.ChunkSize == 0 {
		return 0
	}
	return int((h.TotalSize + uint64(h.ChunkSize) - 1) / uint64(h.ChunkSize))
}

// ChunkRange returns the byte [offset, offset+length) covered by chunk i. The
// final chunk is clamped to TotalSize so callers never read past EOF.
func (h *Header) ChunkRange(i int) (offset int64, length int64) {
	offset = int64(i) * int64(h.ChunkSize)
	end := offset + int64(h.ChunkSize)
	if end > int64(h.TotalSize) {
		end = int64(h.TotalSize)
	}
	if offset > int64(h.TotalSize) {
		offset = int64(h.TotalSize)
	}
	return offset, end - offset
}

// ChunkForOffset maps a byte offset within the memory file to its chunk index.
func (h *Header) ChunkForOffset(off int64) int {
	if h.ChunkSize == 0 {
		return 0
	}
	return int(off / int64(h.ChunkSize))
}

// IsPresent reports whether chunk i holds non-zero data and must be fetched.
// Out-of-range indices report false (treated as zero-fill) so a malformed or
// truncated header degrades to zero pages rather than a panic.
func (h *Header) IsPresent(i int) bool {
	if i < 0 || i >= len(h.present) {
		return false
	}
	return h.present[i]
}

// PresentChunks counts the chunks that must actually be fetched. Together with
// NumChunks this yields the zero-elision ratio, useful for metrics/logging.
func (h *Header) PresentChunks() int {
	n := 0
	for _, p := range h.present {
		if p {
			n++
		}
	}
	return n
}

// BuildHeader scans the memory file at path and produces a Header recording
// which chunkSize-aligned chunks contain non-zero bytes. Passing chunkSize<=0
// uses DefaultChunkSize. The file is read sequentially once; it is not mapped
// or modified.
func BuildHeader(path string, chunkSize int) (*Header, error) {
	if chunkSize <= 0 {
		chunkSize = DefaultChunkSize
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	total := st.Size()
	h := &Header{
		Version:   headerVersion,
		ChunkSize: uint32(chunkSize),
		TotalSize: uint64(total),
	}
	n := h.NumChunks()
	h.present = make([]bool, n)

	buf := make([]byte, chunkSize)
	for i := 0; i < n; i++ {
		nr, rerr := io.ReadFull(f, buf)
		if rerr == io.ErrUnexpectedEOF || rerr == io.EOF {
			// Final short chunk: only the first nr bytes are valid.
			rerr = nil
		} else if rerr != nil {
			return nil, fmt.Errorf("read chunk %d: %w", i, rerr)
		}
		h.present[i] = anyNonZero(buf[:nr])
	}
	return h, nil
}

// anyNonZero reports whether b contains a non-zero byte. Kept simple and
// branch-predictable; the compiler vectorizes the loop well enough that a
// word-at-a-time hand roll is not worth the complexity here.
func anyNonZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return true
		}
	}
	return false
}

// Encode serializes the header to its on-disk byte form:
//
//	"PSM1"            4 bytes magic
//	version          uint32 little-endian
//	chunkSize        uint32 little-endian
//	totalSize        uint64 little-endian
//	numChunks        uint32 little-endian
//	presence bitmap  ceil(numChunks/8) bytes, LSB-first within each byte
func (h *Header) Encode() []byte {
	n := h.NumChunks()
	bitmapLen := (n + 7) / 8
	out := make([]byte, 0, 4+4+4+8+4+bitmapLen)
	out = append(out, Magic...)
	out = binary.LittleEndian.AppendUint32(out, h.Version)
	out = binary.LittleEndian.AppendUint32(out, h.ChunkSize)
	out = binary.LittleEndian.AppendUint64(out, h.TotalSize)
	out = binary.LittleEndian.AppendUint32(out, uint32(n))
	bitmap := make([]byte, bitmapLen)
	for i := 0; i < n; i++ {
		if i < len(h.present) && h.present[i] {
			bitmap[i/8] |= 1 << uint(i%8)
		}
	}
	return append(out, bitmap...)
}

// WriteFile encodes the header and writes it atomically-ish to path. The
// caller typically writes <snapdir>/vm.mem.header.
func (h *Header) WriteFile(path string) error {
	return os.WriteFile(path, h.Encode(), 0o644)
}

// DecodeHeader parses the on-disk byte form produced by Encode.
func DecodeHeader(b []byte) (*Header, error) {
	const fixed = 4 + 4 + 4 + 8 + 4
	if len(b) < fixed {
		return nil, fmt.Errorf("memstream: header too short (%d bytes)", len(b))
	}
	if string(b[:4]) != Magic {
		return nil, fmt.Errorf("memstream: bad magic %q", b[:4])
	}
	h := &Header{}
	off := 4
	h.Version = binary.LittleEndian.Uint32(b[off:])
	off += 4
	if h.Version != headerVersion {
		return nil, fmt.Errorf("memstream: unsupported version %d", h.Version)
	}
	h.ChunkSize = binary.LittleEndian.Uint32(b[off:])
	off += 4
	h.TotalSize = binary.LittleEndian.Uint64(b[off:])
	off += 8
	n := int(binary.LittleEndian.Uint32(b[off:]))
	off += 4
	if h.ChunkSize == 0 {
		return nil, fmt.Errorf("memstream: zero chunk size")
	}
	if got := h.NumChunks(); got != n {
		return nil, fmt.Errorf("memstream: chunk count mismatch (header %d, derived %d)", n, got)
	}
	bitmapLen := (n + 7) / 8
	if len(b)-off < bitmapLen {
		return nil, fmt.Errorf("memstream: truncated bitmap (need %d, have %d)", bitmapLen, len(b)-off)
	}
	bitmap := b[off : off+bitmapLen]
	h.present = make([]bool, n)
	for i := 0; i < n; i++ {
		h.present[i] = bitmap[i/8]&(1<<uint(i%8)) != 0
	}
	return h, nil
}

// ReadHeaderFile loads and decodes a header from path.
func ReadHeaderFile(path string) (*Header, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return DecodeHeader(b)
}
