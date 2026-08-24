// SPDX-License-Identifier: Apache-2.0
package memstream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"sync"
	"time"
)

// Fast, bounded retry for transient GCS/metadata failures on the latency-
// critical fault path. These absorb the common blips (a dropped keep-alive
// connection, a 503/429, a momentary timeout) invisibly and quickly. Sustained
// outages are handled one layer up by the UFFD handler's fault-retry budget —
// the point here is to keep p50 fault latency low, not to ride out a brownout.
const (
	gcsMaxAttempts = 4                      // 1 try + 3 retries
	gcsBaseBackoff = 100 * time.Millisecond // *2 each retry, capped
	gcsMaxBackoff  = 1500 * time.Millisecond
)

// retryableStatus reports whether an HTTP status warrants a retry: 5xx (server/
// transient), 429 (throttle), 408 (request timeout). 4xx auth/not-found are
// permanent and must not be retried.
func retryableStatus(code int) bool {
	return code >= 500 || code == http.StatusTooManyRequests || code == http.StatusRequestTimeout
}

// backoffFor returns an exponential backoff with full jitter for attempt n (0-based).
func backoffFor(n int) time.Duration {
	d := gcsBaseBackoff << n
	if d > gcsMaxBackoff {
		d = gcsMaxBackoff
	}
	// Full jitter: sleep in [0, d). Spreads a fleet's retries so a recovering
	// GCS isn't hit by a synchronized wave.
	return time.Duration(rand.Int63n(int64(d) + 1))
}

// sleepCtx sleeps for d unless ctx is cancelled first (returns ctx.Err()).
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

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
	var lastErr error
	for attempt := 0; attempt < gcsMaxAttempts; attempt++ {
		if attempt > 0 {
			if err := sleepCtx(ctx, backoffFor(attempt-1)); err != nil {
				return 0, err // context cancelled: caller (fault worker) is going away
			}
		}
		n, retryable, err := s.readOnce(ctx, p, off)
		if err == nil {
			return n, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		if !retryable {
			return 0, err
		}
	}
	return 0, fmt.Errorf("memstream: range GET %s off %d exhausted %d attempts: %w", s.object, off, gcsMaxAttempts, lastErr)
}

// readOnce performs a single ranged GET. retryable reports whether a failure is
// transient (network error, 5xx/429/408) and worth another attempt.
func (s *gcsRangeSource) readOnce(ctx context.Context, p []byte, off int64) (n int, retryable bool, err error) {
	url := fmt.Sprintf("https://storage.googleapis.com/%s/%s", s.bucket, s.object)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, false, err
	}
	tok, err := s.tok.Token(ctx)
	if err != nil {
		// Token fetch already retries internally; a failure here is worth one
		// more outer attempt (the metadata server may have blipped).
		return 0, true, fmt.Errorf("memstream: get token: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	// Inclusive byte range per RFC 7233.
	end := off + int64(len(p)) - 1
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", off, end))

	resp, err := s.client.Do(req)
	if err != nil {
		// Transport-level failure (conn reset, dial fail, response timeout) —
		// net/http never retries these; they are the common transient case.
		return 0, true, err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	// 206 Partial Content for a satisfied range; 200 if the server ignored the
	// range and returned the whole object (we still only read len(p) bytes).
	if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
		return 0, retryableStatus(resp.StatusCode),
			fmt.Errorf("memstream: GET %s range %d-%d: %s", s.object, off, end, resp.Status)
	}
	n, err = io.ReadFull(resp.Body, p)
	if err == io.ErrUnexpectedEOF || err == io.EOF {
		// Tail chunk shorter than len(p): acceptable, the resolver sizes the
		// request to the clamped chunk length so this only happens on a
		// genuinely short object.
		return n, false, nil
	}
	if err != nil {
		// Body read cut short mid-stream — transient, retry the whole range.
		return 0, true, err
	}
	return n, false, nil
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
	var lastErr error
	for attempt := 0; attempt < gcsMaxAttempts; attempt++ {
		if attempt > 0 {
			if err := sleepCtx(ctx, backoffFor(attempt-1)); err != nil {
				return "", err
			}
		}
		tok, retryable, err := m.fetchOnce(ctx)
		if err == nil {
			return tok, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if !retryable {
			return "", err
		}
	}
	return "", fmt.Errorf("memstream: metadata token exhausted %d attempts: %w", gcsMaxAttempts, lastErr)
}

func (m *metadataTokenProvider) fetchOnce(ctx context.Context) (tok string, retryable bool, err error) {
	const url = "http://metadata.google.internal/computeMetadata/v1/" +
		"instance/service-accounts/default/token"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", false, err
	}
	req.Header.Set("Metadata-Flavor", "Google")
	resp, err := m.client.Do(req)
	if err != nil {
		return "", true, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", retryableStatus(resp.StatusCode), fmt.Errorf("memstream: metadata token: %s", resp.Status)
	}
	var body struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := decodeJSON(resp.Body, &body); err != nil {
		return "", true, err
	}
	if body.AccessToken == "" {
		return "", true, errors.New("memstream: empty access token")
	}
	m.token = body.AccessToken
	ttl := time.Duration(body.ExpiresIn) * time.Second
	if ttl > 5*time.Minute {
		ttl -= 5 * time.Minute
	}
	m.expiry = time.Now().Add(ttl)
	return m.token, false, nil
}
