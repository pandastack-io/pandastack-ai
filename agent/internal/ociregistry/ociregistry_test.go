// SPDX-License-Identifier: Apache-2.0
package ociregistry

import (
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
)

// newTestPuller builds a Puller without touching credentials/network so the
// pure functions (Ref, allowed) can be unit-tested.
func newTestPuller() *Puller {
	region, project, repo := "us-central1", "pandastack-production", "pandastack-templates"
	return &Puller{
		region:  region,
		project: project,
		repo:    repo,
		host:    region + "-docker.pkg.dev",
	}
}

func TestRefFormat(t *testing.T) {
	p := newTestPuller()
	got := p.Ref("static-builder", "20260615T230000Z")
	want := "us-central1-docker.pkg.dev/pandastack-production/pandastack-templates/static-builder:20260615T230000Z"
	if got != want {
		t.Fatalf("Ref() = %q, want %q", got, want)
	}
	// The ref we produce must itself be parseable and pass our own allowlist.
	ref, err := name.ParseReference(got)
	if err != nil {
		t.Fatalf("our own Ref is not a valid reference: %v", err)
	}
	if !p.allowed(ref) {
		t.Fatalf("our own Ref %q rejected by allowed()", got)
	}
}

func TestAllowed(t *testing.T) {
	p := newTestPuller()
	cases := []struct {
		ref  string
		want bool
		why  string
	}{
		{"us-central1-docker.pkg.dev/pandastack-production/pandastack-templates/x:y", true, "exact trusted repo"},
		{"us-central1-docker.pkg.dev/pandastack-production/pandastack-templates/nested/x:y", true, "deeper path under repo still in repo"},
		{"us-central1-docker.pkg.dev/pandastack-production/other-repo/x:y", false, "wrong repo"},
		{"us-central1-docker.pkg.dev/other-project/pandastack-templates/x:y", false, "wrong project"},
		{"us-east1-docker.pkg.dev/pandastack-production/pandastack-templates/x:y", false, "wrong region/host"},
		{"docker.io/library/ubuntu:24.04", false, "external registry"},
		{"gcr.io/pandastack-production/pandastack-templates/x:y", false, "wrong host (gcr not AR)"},
	}
	for _, c := range cases {
		ref, err := name.ParseReference(c.ref)
		if err != nil {
			t.Fatalf("parse %q: %v", c.ref, err)
		}
		if got := p.allowed(ref); got != c.want {
			t.Errorf("allowed(%q) = %v, want %v (%s)", c.ref, got, c.want, c.why)
		}
	}
}
