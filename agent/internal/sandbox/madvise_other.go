// SPDX-License-Identifier: Apache-2.0
//go:build !linux

package sandbox

func madvise(b []byte, advice int) error { return nil }
