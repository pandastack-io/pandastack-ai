// SPDX-License-Identifier: Apache-2.0
//go:build !linux

// Stub for non-Linux dev platforms (macOS). NBD is a Linux kernel facility; the
// real implementation is in server_linux.go. This keeps the package buildable so
// the agent compiles on a developer Mac, while any attempt to actually start a
// device errors clearly. Mirrors how the agent stubs the uffd path.
package nbdstream

import (
	"context"
	"fmt"
	"time"
)

// Backend is the read source the device serves from (see server_linux.go).
type Backend interface {
	ReadAt(ctx context.Context, dst []byte, off int64) (int, error)
	Size() int64
}

// Config mirrors the Linux Config so callers compile cross-platform.
type Config struct {
	Device       string
	BlockSize    int
	IOTimeout    time.Duration
	FetchBudget  time.Duration
	MaxReadBytes int
	Logf         func(level, msg string, kv ...any)
}

// Stats mirrors the Linux Stats.
type Stats struct {
	Reads     int64
	ReadBytes int64
	Breaker   int64
	Errs      int64
}

// OnBreakerOpen mirrors the Linux hook (unused on non-Linux).
var OnBreakerOpen func()

// DefaultIOTimeout mirrors the Linux constant.
const DefaultIOTimeout = 90 * time.Second

// Server is a no-op handle on non-Linux platforms.
type Server struct{}

// Start always fails on non-Linux: there is no NBD device to drive.
func Start(_ Backend, _ Config) (*Server, error) {
	return nil, fmt.Errorf("nbdstream: NBD streaming is only supported on Linux")
}

// Stop is a no-op.
func (s *Server) Stop() {}

// Done returns an already-closed channel.
func (s *Server) Done() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

// Stats returns the zero value.
func (s *Server) Stats() Stats { return Stats{} }

// StalledFor is always false on non-Linux (no device).
func (s *Server) StalledFor(time.Duration) bool { return false }

// LastCompletedIO returns the zero time on non-Linux.
func (s *Server) LastCompletedIO() time.Time { return time.Time{} }
