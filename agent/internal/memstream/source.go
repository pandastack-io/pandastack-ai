// SPDX-License-Identifier: Apache-2.0
package memstream

import (
	"context"
	"io"
	"os"
)

// ChunkSource is the upstream that backs a streamed memory file. The resolver
// asks it for byte ranges (chunk-aligned in practice) and copies the result
// into the local cache. Implementations must be safe for concurrent ReadAt
// calls from multiple fault-handler goroutines.
type ChunkSource interface {
	// ReadAt fills p with bytes starting at logical offset off in the memory
	// file, returning the number of bytes read. It behaves like io.ReaderAt:
	// a short read MUST return a non-nil error (io.EOF/io.ErrUnexpectedEOF).
	ReadAt(ctx context.Context, p []byte, off int64) (int, error)

	// Close releases any upstream resources (open file, HTTP idle conns).
	Close() error
}

// fileSource serves chunks from a full local copy of vm.mem. It is used both
// in tests and on warm hosts where the seed already pulled the whole file —
// the streaming machinery then just becomes a lazy local reader, which keeps a
// single restore code path regardless of whether the bytes are local or remote.
type fileSource struct {
	f *os.File
}

// NewFileSource opens path read-only as a ChunkSource.
func NewFileSource(path string) (ChunkSource, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	return &fileSource{f: f}, nil
}

func (s *fileSource) ReadAt(_ context.Context, p []byte, off int64) (int, error) {
	n, err := s.f.ReadAt(p, off)
	// os.File.ReadAt returns io.EOF on a short read at end-of-file; the
	// resolver clamps ranges to TotalSize so a full-length read at the tail
	// can legitimately hit EOF. Treat a complete fill as success.
	if err == io.EOF && n == len(p) {
		err = nil
	}
	return n, err
}

func (s *fileSource) Close() error { return s.f.Close() }
