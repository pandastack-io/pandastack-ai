// SPDX-License-Identifier: Apache-2.0
package memstream

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// gcsRangeSource fetches byte ranges of a GCS object over the XML API using
// HTTP Range requests. It deliberately avoids cloud.google.com/go/storage
// (consistent with the snapstore package's rationale) — the only thing we need
// here is an authenticated ranged GET, and pulling the full SDK would add
// several MB of dependencies plus its own auth/retry machinery.
//
// Auth uses the agent VM's instance service-account token from the GCE
// metadata server, cached and refreshed shortly before expiry. This is the
// same identity gsutil uses today for the full-download path.
type gcsRangeSource struct {
	bucket string
	object string
	client *http.Client
	tok    tokenProvider
}

// NewGCSRangeSource builds a ChunkSource for gs://bucket/object. tok supplies
// OAuth bearer tokens; pass NewMetadataTokenProvider() in production.
func NewGCSRangeSource(bucket, object string, tok tokenProvider) ChunkSource {
	return &gcsRangeSource{
		bucket: bucket,
		object: object,
		tok:    tok,
		client: &http.Client{Timeout: 60 * time.Second},
	}
}

func (s *gcsRangeSource) ReadAt(ctx context.Context, p []byte, off int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	url := fmt.Sprintf("https://storage.googleapis.com/%s/%s", s.bucket, s.object)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	tok, err := s.tok.Token(ctx)
	if err != nil {
		return 0, fmt.Errorf("memstream: get token: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	// Inclusive byte range per RFC 7233.
	end := off + int64(len(p)) - 1
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", off, end))

	resp, err := s.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	// 206 Partial Content for a satisfied range; 200 if the server ignored the
	// range and returned the whole object (we still only read len(p) bytes).
	if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("memstream: GET %s range %d-%d: %s", s.object, off, end, resp.Status)
	}
	n, err := io.ReadFull(resp.Body, p)
	if err == io.ErrUnexpectedEOF || err == io.EOF {
		// Tail chunk shorter than len(p): acceptable, the resolver sizes the
		// request to the clamped chunk length so this only happens on a
		// genuinely short object.
		err = nil
	}
	return n, err
}

func (s *gcsRangeSource) Close() error {
	s.client.CloseIdleConnections()
	return nil
}

// tokenProvider yields OAuth2 bearer tokens for GCS requests.
type tokenProvider interface {
	Token(ctx context.Context) (string, error)
}

// metadataTokenProvider fetches and caches an access token from the GCE
// metadata server. Tokens are valid ~1h; we refresh 5 minutes early.
type metadataTokenProvider struct {
	client *http.Client

	mu     sync.Mutex
	token  string
	expiry time.Time
}

// NewMetadataTokenProvider returns a tokenProvider backed by the GCE metadata
// server (http://metadata.google.internal). Only usable on GCP VMs.
func NewMetadataTokenProvider() tokenProvider {
	return &metadataTokenProvider{client: &http.Client{Timeout: 5 * time.Second}}
}

func (m *metadataTokenProvider) Token(ctx context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.token != "" && time.Now().Before(m.expiry) {
		return m.token, nil
	}
	const url = "http://metadata.google.internal/computeMetadata/v1/" +
		"instance/service-accounts/default/token"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Metadata-Flavor", "Google")
	resp, err := m.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("memstream: metadata token: %s", resp.Status)
	}
	var body struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := decodeJSON(resp.Body, &body); err != nil {
		return "", err
	}
	if body.AccessToken == "" {
		return "", fmt.Errorf("memstream: empty access token")
	}
	m.token = body.AccessToken
	ttl := time.Duration(body.ExpiresIn) * time.Second
	if ttl > 5*time.Minute {
		ttl -= 5 * time.Minute
	}
	m.expiry = time.Now().Add(ttl)
	return m.token, nil
}
