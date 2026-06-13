// SPDX-License-Identifier: Apache-2.0
package main

import "runtime"

var (
	semver    = "v0.0.0-dev"
	commit    = "unknown"
	buildTime = "unknown"
)

type versionInfo struct {
	Service   string `json:"service"`
	Semver    string `json:"semver"`
	Commit    string `json:"commit"`
	BuildTime string `json:"build_time"`
	Go        string `json:"go"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
}

func version() versionInfo {
	return versionInfo{
		Service:   "pandastack-api",
		Semver:    semver,
		Commit:    commit,
		BuildTime: buildTime,
		Go:        runtime.Version(),
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
	}
}
