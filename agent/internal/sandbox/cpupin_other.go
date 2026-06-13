// SPDX-License-Identifier: Apache-2.0
//go:build !linux

package sandbox

func setAffinity(tid, core int) error { return nil }
