// SPDX-License-Identifier: Apache-2.0
package api

import "runtime"

// These are injected via -ldflags at build time:
//   go build -ldflags "-X .../api.commit=$(git rev-parse --short HEAD) \
//                      -X .../api.buildTime=$(date -u +%Y-%m-%dT%H:%M:%SZ) \
//                      -X .../api.semver=v1.0.0"
var (
	semver    = "v0.0.0-dev"
	commit    = "unknown"
	buildTime = "unknown"
)

type VersionInfo struct {
	Service   string `json:"service"`
	Semver    string `json:"semver"`
	Commit    string `json:"commit"`
	BuildTime string `json:"build_time"`
	Go        string `json:"go"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
}

func Version() VersionInfo {
	return VersionInfo{
		Service:   "pandastack-agent",
		Semver:    semver,
		Commit:    commit,
		BuildTime: buildTime,
		Go:        runtime.Version(),
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
	}
}
