// SPDX-License-Identifier: Apache-2.0
//go:build !linux

package nbdstream

import (
	"fmt"
	"time"
)

// Origin is a no-op handle on non-Linux platforms.
type Origin struct {
	Device string
}

// OriginConfig mirrors the Linux config so callers compile cross-platform.
type OriginConfig struct {
	CacheRoot             string
	CacheMaxBytes         int64
	Template              string
	IOTimeout             time.Duration
	FetchBudget           time.Duration
	EgressRateBytesPerSec float64
	EgressCapMultiplier   float64
	Logf                  func(level, msg string, kv ...any)
}

// OpenOrigin always fails on non-Linux: NBD is Linux-only.
func OpenOrigin(_ string, _ OriginConfig) (*Origin, error) {
	return nil, fmt.Errorf("nbdstream: NBD streaming is only supported on Linux")
}

// Close is a no-op.
func (o *Origin) Close() error { return nil }

// Stats returns the zero value.
func (o *Origin) Stats() Stats { return Stats{} }

// StalledFor is always false on non-Linux.
func (o *Origin) StalledFor(time.Duration) bool { return false }
