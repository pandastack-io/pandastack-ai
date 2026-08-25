// SPDX-License-Identifier: Apache-2.0

// Package ociregistry pulls custom-template rootfs images from an OCI container
// registry (GCP Artifact Registry) and flattens them into a single rootfs tar
// stream that the template build path turns into an ext4 image.
//
// This is the ingest path that replaces uploading multi-GB rootfs tarballs
// through the control-plane API (which Cloudflare rejects with 413). Instead a
// client does `docker build` + `docker push` to the registry and the agent
// pulls the image here — bytes never traverse the API.
//
// Port of sandflare's shared/pkg/artifacts-registry GCP path, simplified to a
// pull-only client (the agent never pushes). Named ociregistry to avoid
// colliding with internal/registry (agent heartbeat/self-registration).
package ociregistry

import (
	"context"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/google"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

const (
	defaultRegion  = "us-central1"
	defaultProject = "pandastack-production"
	defaultRepo    = "pandastack-templates"
)

// Puller resolves and pulls custom-template images from a single trusted GCP
// Artifact Registry repository. It is safe for concurrent use.
type Puller struct {
	region  string
	project string
	repo    string
	host    string // "<region>-docker.pkg.dev" — the only host we will pull from
	auth    authn.Authenticator
}

// New constructs a Puller from PANDASTACK_REGISTRY_* env (with sane defaults
// for the pandastack-production setup) and resolves GCP credentials.
//
// Auth resolution order:
//  1. GOOGLE_APPLICATION_CREDENTIALS (service-account JSON file) — used as
//     `_json_key` basic auth, matching the prod SA handoff.
//  2. google.Keychain — Application Default Credentials / metadata server
//     (works on GCE/Cloud Run agents without an explicit key file).
func New() (*Puller, error) {
	region := envOr("PANDASTACK_REGISTRY_REGION", defaultRegion)
	project := envOr("PANDASTACK_REGISTRY_PROJECT", defaultProject)
	repo := envOr("PANDASTACK_REGISTRY_REPO", defaultRepo)

	p := &Puller{
		region:  region,
		project: project,
		repo:    repo,
		host:    region + "-docker.pkg.dev",
	}

	auth, err := resolveAuth(p.host)
	if err != nil {
		return nil, err
	}
	p.auth = auth
	return p, nil
}

// resolveAuth prefers an explicit service-account JSON key (the prod handoff)
// and falls back to Application Default Credentials via google.Keychain.
func resolveAuth(host string) (authn.Authenticator, error) {
	if keyPath := strings.TrimSpace(os.Getenv("GOOGLE_APPLICATION_CREDENTIALS")); keyPath != "" {
		data, err := os.ReadFile(keyPath)
		if err != nil {
			return nil, fmt.Errorf("ociregistry: read GOOGLE_APPLICATION_CREDENTIALS %q: %w", keyPath, err)
		}
		// Artifact Registry accepts the raw SA JSON as the password under the
		// special "_json_key" username over Basic auth.
		return &authn.Basic{Username: "_json_key", Password: string(data)}, nil
	}
	// ADC / metadata server. google.Keychain.Resolve mints a short-lived token.
	reg, err := name.NewRegistry(host)
	if err != nil {
		return nil, fmt.Errorf("ociregistry: bad registry host %q: %w", host, err)
	}
	auth, err := google.Keychain.Resolve(reg)
	if err != nil {
		// If both paths fail there's nothing to pull with, so surface it now.
		return nil, fmt.Errorf("ociregistry: no GOOGLE_APPLICATION_CREDENTIALS and ADC unavailable: %w", err)
	}
	return auth, nil
}

// Ref returns the canonical image reference for a template build:
//
//	<region>-docker.pkg.dev/<project>/<repo>/<template>:<buildID>
func (p *Puller) Ref(template, buildID string) string {
	return fmt.Sprintf("%s/%s/%s/%s:%s", p.host, p.project, p.repo, template, buildID)
}

// platform returns the OCI platform to pull, matched to the agent's own
// architecture. A prod amd64 agent pulls linux/amd64; a Lima arm64 dev agent
// pulls linux/arm64. The image must have been pushed for that arch.
func platform() v1.Platform {
	return v1.Platform{OS: "linux", Architecture: runtime.GOARCH}
}

// allowed reports whether ref points at our trusted registry host+project+repo.
// We never pull arbitrary external images — only images a client pushed into
// the configured pandastack repository.
func (p *Puller) allowed(ref name.Reference) bool {
	ctx := ref.Context()
	if ctx.RegistryStr() != p.host {
		return false
	}
	// RepositoryStr() is "<project>/<repo>/<template>"; require our prefix.
	wantPrefix := p.project + "/" + p.repo + "/"
	return strings.HasPrefix(ctx.RepositoryStr(), wantPrefix)
}

// ExtractToTar pulls imageRef, flattens its layers (resolving whiteouts) and
// writes the resulting single rootfs tar stream to dst. Returns the number of
// bytes written. The image must be in the configured trusted registry.
func (p *Puller) ExtractToTar(ctx context.Context, imageRef string, dst io.Writer) (int64, error) {
	ref, err := name.ParseReference(imageRef)
	if err != nil {
		return 0, fmt.Errorf("ociregistry: invalid image reference %q: %w", imageRef, err)
	}
	if !p.allowed(ref) {
		return 0, fmt.Errorf("ociregistry: refusing to pull %q: not in trusted repo %s/%s/%s", imageRef, p.host, p.project, p.repo)
	}

	img, err := remote.Image(ref,
		remote.WithAuth(p.auth),
		remote.WithPlatform(platform()),
		remote.WithContext(ctx),
	)
	if err != nil {
		return 0, fmt.Errorf("ociregistry: pull %q: %w", imageRef, err)
	}

	// mutate.Extract returns a tar stream of the fully-flattened filesystem:
	// layers applied in order with whiteouts/opaque dirs resolved — equivalent
	// to `docker export` of a container created from the image.
	rc := mutate.Extract(img)
	defer rc.Close()

	n, err := io.Copy(dst, rc)
	if err != nil {
		return n, fmt.Errorf("ociregistry: extract %q: %w", imageRef, err)
	}
	return n, nil
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}
