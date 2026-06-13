// SPDX-License-Identifier: Apache-2.0
//go:build linux && (amd64 || arm64)

package sandbox

// copy_file_range syscall numbers on Linux:
//   amd64: 326
//   arm64: 285
// We declare per-arch in build-tagged files.
