// SPDX-License-Identifier: Apache-2.0
//go:build !linux

package uffd

import (
	"context"
	"errors"

	"github.com/pandastack/agent/internal/memstream"
)

// ErrUnsupported is returned by every Handler method off Linux. userfaultfd is
// a Linux-only facility, so the streaming restore path can never run on the
// macOS dev machine; the stub exists purely so the agent package compiles and
// callers can gate on the returned error.
var ErrUnsupported = errors.New("uffd: streaming restore requires linux")

// Handler is the non-Linux stub. It holds no state and every operation fails
// with ErrUnsupported.
type Handler struct {
	// Workers mirrors the Linux field so call sites compile unchanged.
	Workers int
}

// New returns a stub Handler. The resolver is accepted and ignored.
func New(_ string, _ *memstream.Resolver) *Handler { return &Handler{} }

// Listen always fails off Linux.
func (h *Handler) Listen() error { return ErrUnsupported }

// Serve always fails off Linux.
func (h *Handler) Serve(_ context.Context) error { return ErrUnsupported }

// Close is a no-op off Linux.
func (h *Handler) Close() error { return nil }

// Stats returns zeroed counters off Linux.
func (h *Handler) Stats() Stats { return Stats{} }
