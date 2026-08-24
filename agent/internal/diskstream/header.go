// SPDX-License-Identifier: Apache-2.0

// Package diskstream implements a chunked, zero-eliding, integrity-checked
// representation of a Firecracker rootfs image (clone.ext4) so it can be
// streamed on demand from object storage at restore time instead of downloaded
// in full.
//
// It is the disk analog of the memstream package. Where memstream streams the
// memory snapshot (vm.mem) on guest page faults via userfaultfd, diskstream
// streams the rootfs on block-device reads via a userspace NBD server: the
// guest cold-restores on any host by paging only its disk working set (measured
// at tens to a couple hundred MiB for a base boot + runtime) instead of
// downloading the whole multi-GB ext4 image. The streamed layer is strictly
// read-only and immutable; all guest writes land in the existing per-sandbox
// dm-snapshot copy-on-write overlay, never in this path.
//
// Three things make diskstream distinct from memstream, all because rootfs
// bytes are user-controlled and persistent rather than VMM-produced and
// ephemeral:
//
//   - The on-disk magics use a "PSD"/"PSC" family ("PandaStack Disk") so a
//     memory artifact can never be mis-applied to a disk resolver and vice versa.
//   - The Header carries a per-present-chunk SHA-256 so served bytes can be
//     verified against the bake-time manifest (the memory path inherits integrity
//     implicitly from Firecracker's own snapshot loader; the disk path makes it
//     explicit and is checked on serve).
//   - The device transport is NBD (kernel nbd driver + userspace server) rather
//     than UFFD, chosen in Phase 0 after ublk proved to kernel-panic across the
//     production GCP 6.17 kernel line. The codec/cache layer in this package is
//     transport-agnostic; only the daemon that consumes a Resolver differs.
//
// Like memstream v1, this format does NOT implement cross-build block-level
// dedup. It chunks a single, self-contained image. A future v2 can layer
// base/diff dedup on top without changing the Resolver's chunk-fetch contract.
package diskstream

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

const (
	// Magic identifies a v1 diskstream header ("PandaStack Disk v1").
	Magic = "PSD1"

	// DefaultChunkSize is the granularity at which clone.ext4 is fetched from
	// object storage. 1 MiB (vs memstream's 4 MiB) was chosen from the Phase 0c
	// working-set trace: rootfs faults are a mix of scattered ext4 metadata and
	// runtime-binary reads, so 1 MiB amortizes the GCS round-trip (~30-60ms
	// TTFB) while keeping read amplification low. memstream uses 4 MiB because
	// guest RAM faults are spatially denser.
	DefaultChunkSize = 1 << 20 // 1 MiB

	// headerVersion is the current on-disk format version.
	headerVersion = 1

	// sha256Len is the length in bytes of each per-present-chunk digest.
	sha256Len = 32
)

// Header describes how a rootfs image is split into fixed-size chunks, which of
// those chunks contain non-zero data, and the SHA-256 of every present chunk.
// It is produced once at bake time (scanning clone.ext4) and uploaded alongside
// the image as clone.ext4.header. The NBD server's Resolver consults it to
// decide, for a read offset, whether to range-fetch a chunk from object storage
// or simply zero-fill — and, on fetch, to verify the bytes against blockSHA.
type Header struct {
	// Version is the on-disk format version (currently 1).
	Version uint32

	// ChunkSize is the byte size of every chunk except possibly the last, which
	// may be shorter when TotalSize is not a multiple of ChunkSize.
	ChunkSize uint32

	// TotalSize is the full logical size of the rootfs image in bytes. It MUST
	// equal the NBD device capacity advertised to the kernel.
	TotalSize uint64

	// present[i] is true when chunk i contains at least one non-zero byte and
	// therefore must be fetched. A false entry is reconstructed by zero-fill.
	present []bool

	// blockSHA[i] is the SHA-256 of present chunk i's bytes (the clamped chunk
	// length for the final short chunk). Absent chunks have a zero digest and
	// are never verified (they are zero-filled). Indexed by chunk index; absent
	// entries are the zero value.
	blockSHA [][sha256Len]byte
}

// NumChunks returns the number of chunks the image is divided into.
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

// ChunkForOffset maps a byte offset within the image to its chunk index.
func (h *Header) ChunkForOffset(off int64) int {
	if h.ChunkSize == 0 {
		return 0
	}
	return int(off / int64(h.ChunkSize))
}

// IsPresent reports whether chunk i holds non-zero data and must be fetched.
// Out-of-range indices report false (treated as zero-fill) so a malformed or
// truncated header degrades to zero blocks rather than a panic.
func (h *Header) IsPresent(i int) bool {
	if i < 0 || i >= len(h.present) {
		return false
	}
	return h.present[i]
}

// ChunkSHA returns the bake-time SHA-256 of present chunk i and whether a digest
// is recorded for it. Absent chunks (and out-of-range indices) return ok=false;
// callers must not verify those (they are zero-filled, never fetched).
func (h *Header) ChunkSHA(i int) (sum [sha256Len]byte, ok bool) {
	if i < 0 || i >= len(h.blockSHA) || !h.IsPresent(i) {
		return [sha256Len]byte{}, false
	}
	return h.blockSHA[i], true
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

// BuildHeader scans the rootfs image at path and produces a Header recording,
// for each chunkSize-aligned chunk, whether it contains non-zero bytes and (for
// present chunks) its SHA-256. chunkSize<=0 uses DefaultChunkSize. The file is
// read sequentially once; it is not mapped or modified.
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
	h.blockSHA = make([][sha256Len]byte, n)

	buf := make([]byte, chunkSize)
	for i := 0; i < n; i++ {
		nr, rerr := io.ReadFull(f, buf)
		if rerr == io.ErrUnexpectedEOF || rerr == io.EOF {
			// Final short chunk: only the first nr bytes are valid.
			rerr = nil
		} else if rerr != nil {
			return nil, fmt.Errorf("diskstream: read chunk %d: %w", i, rerr)
		}
		if anyNonZero(buf[:nr]) {
			h.present[i] = true
			h.blockSHA[i] = sha256.Sum256(buf[:nr])
		}
	}
	return h, nil
}

// anyNonZero reports whether b contains a non-zero byte.
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
//	"PSD1"            4 bytes magic
//	version          uint32 little-endian
//	chunkSize        uint32 little-endian
//	totalSize        uint64 little-endian
//	numChunks        uint32 little-endian
//	presence bitmap  ceil(numChunks/8) bytes, LSB-first within each byte
//	blockSHA[]       32 bytes per PRESENT chunk, in ascending chunk-index order
//
// Only present chunks carry a digest, so the SHA section length is
// PresentChunks()*32 — absent chunks cost one bit and no digest.
func (h *Header) Encode() []byte {
	n := h.NumChunks()
	bitmapLen := (n + 7) / 8
	present := h.PresentChunks()
	out := make([]byte, 0, 4+4+4+8+4+bitmapLen+present*sha256Len)
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
	out = append(out, bitmap...)
	for i := 0; i < n; i++ {
		if i < len(h.present) && h.present[i] {
			out = append(out, h.blockSHA[i][:]...)
		}
	}
	return out
}

// WriteFile encodes the header and writes it to path. The caller typically
// writes <snapdir>/clone.ext4.header, alongside vm.mem.header in the seed.
func (h *Header) WriteFile(path string) error {
	return os.WriteFile(path, h.Encode(), 0o644)
}

// DecodeHeader parses the on-disk byte form produced by Encode.
func DecodeHeader(b []byte) (*Header, error) {
	const fixed = 4 + 4 + 4 + 8 + 4
	if len(b) < fixed {
		return nil, fmt.Errorf("diskstream: header too short (%d bytes)", len(b))
	}
	if string(b[:4]) != Magic {
		return nil, fmt.Errorf("diskstream: bad magic %q", b[:4])
	}
	h := &Header{}
	off := 4
	h.Version = binary.LittleEndian.Uint32(b[off:])
	off += 4
	if h.Version != headerVersion {
		return nil, fmt.Errorf("diskstream: unsupported version %d", h.Version)
	}
	h.ChunkSize = binary.LittleEndian.Uint32(b[off:])
	off += 4
	h.TotalSize = binary.LittleEndian.Uint64(b[off:])
	off += 8
	n := int(binary.LittleEndian.Uint32(b[off:]))
	off += 4
	if h.ChunkSize == 0 {
		return nil, fmt.Errorf("diskstream: zero chunk size")
	}
	if got := h.NumChunks(); got != n {
		return nil, fmt.Errorf("diskstream: chunk count mismatch (header %d, derived %d)", n, got)
	}
	bitmapLen := (n + 7) / 8
	if len(b)-off < bitmapLen {
		return nil, fmt.Errorf("diskstream: truncated bitmap (need %d, have %d)", bitmapLen, len(b)-off)
	}
	bitmap := b[off : off+bitmapLen]
	off += bitmapLen
	h.present = make([]bool, n)
	present := 0
	for i := 0; i < n; i++ {
		if bitmap[i/8]&(1<<uint(i%8)) != 0 {
			h.present[i] = true
			present++
		}
	}
	if need := present * sha256Len; len(b)-off < need {
		return nil, fmt.Errorf("diskstream: truncated digest section (need %d, have %d)", need, len(b)-off)
	}
	h.blockSHA = make([][sha256Len]byte, n)
	for i := 0; i < n; i++ {
		if h.present[i] {
			copy(h.blockSHA[i][:], b[off:off+sha256Len])
			off += sha256Len
		}
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
