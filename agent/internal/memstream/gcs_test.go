// SPDX-License-Identifier: Apache-2.0
package memstream

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"
)

// stubToken is a tokenProvider that returns a fixed bearer token.
type stubToken struct{ tok string }

func (s stubToken) Token(context.Context) (string, error) { return s.tok, nil }

// redirectTransport rewrites the scheme+host of every request to the test
// server so a gcsRangeSource (which hardcodes storage.googleapis.com) can be
// pointed at an httptest server while preserving path, Range and Auth headers.
type redirectTransport struct{ base *url.URL }

func (rt redirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.URL.Scheme = rt.base.Scheme
	req.URL.Host = rt.base.Host
	return http.DefaultTransport.RoundTrip(req)
}

// newRangeServer serves data honoring HTTP Range requests (206) via
// http.ServeContent, and asserts the bearer token was forwarded.
func newRangeServer(t *testing.T, data []byte, wantTok string) (*httptest.Server, *int) {
	t.Helper()
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if got := r.Header.Get("Authorization"); got != "Bearer "+wantTok {
			http.Error(w, "missing/incorrect token: "+got, http.StatusUnauthorized)
			return
		}
		http.ServeContent(w, r, "vm.mem", time.Time{}, bytes.NewReader(data))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func newTestGCSSource(t *testing.T, srv *httptest.Server, tok string) ChunkSource {
	t.Helper()
	base, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return &gcsRangeSource{
		bucket: "test-bucket",
		object: "seeds/x/1/vm.mem",
		client: &http.Client{Transport: redirectTransport{base}},
		tok:    stubToken{tok},
	}
}

func TestGCSRangeSourceReadAt(t *testing.T) {
	const n = 1 << 16
	data := make([]byte, n)
	for i := range data {
		data[i] = byte(i % 251) // deterministic, avoids all-zero
	}
	srv, _ := newRangeServer(t, data, "tok-123")
	src := newTestGCSSource(t, srv, "tok-123")
	defer src.Close()

	// Mid-object range.
	buf := make([]byte, 4096)
	got, err := src.ReadAt(context.Background(), buf, 10000)
	if err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if got != len(buf) {
		t.Fatalf("short read: got %d want %d", got, len(buf))
	}
	if !bytes.Equal(buf, data[10000:10000+4096]) {
		t.Fatal("mid-object bytes mismatch")
	}

	// Tail range that runs to EOF (shorter than buf is acceptable).
	tail := make([]byte, 8192)
	off := int64(n - 100)
	got, err = src.ReadAt(context.Background(), tail, off)
	if err != nil {
		t.Fatalf("tail ReadAt: %v", err)
	}
	if got != 100 {
		t.Fatalf("tail short read: got %d want 100", got)
	}
	if !bytes.Equal(tail[:100], data[off:]) {
		t.Fatal("tail bytes mismatch")
	}
}

func TestGCSRangeSourceRejectsBadToken(t *testing.T) {
	srv, _ := newRangeServer(t, make([]byte, 4096), "right-token")
	src := newTestGCSSource(t, srv, "wrong-token")
	defer src.Close()
	if _, err := src.ReadAt(context.Background(), make([]byte, 16), 0); err == nil {
		t.Fatal("expected error on 401")
	}
}

// TestResolverOverGCSRangeSource drives the full fault path: a header built
// from a local fixture, served by the resolver from a ranged GCS source.
func TestResolverOverGCSRangeSource(t *testing.T) {
	const chunk = 1 << 20
	size := int64(3 * chunk)
	p := writeMem(t, size)
	patternFill(t, p, 0, chunk, 0x11)     // chunk 0 present
	patternFill(t, p, 2*chunk, chunk, 0x22) // chunk 2 present; chunk 1 stays zero

	h, err := BuildHeader(p, chunk)
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, size)
	for i := int64(0); i < chunk; i++ {
		data[i] = 0x11
		data[2*chunk+i] = 0x22
	}
	srv, hits := newRangeServer(t, data, "tok")
	src := newTestGCSSource(t, srv, "tok")

	r, err := NewResolver(h, src, filepath.Join(t.TempDir(), "c.mem"), PageSize)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx := context.Background()
	page := make([]byte, PageSize)

	if err := r.ResolvePage(ctx, 4096, page); err != nil {
		t.Fatal(err)
	}
	if !allEqual(page, 0x11) {
		t.Fatalf("chunk 0 not 0x11: %x", page[:8])
	}
	if err := r.ResolvePage(ctx, chunk+4096, page); err != nil {
		t.Fatal(err)
	}
	if !allEqual(page, 0) {
		t.Fatalf("chunk 1 (zero) faulted bytes: %x", page[:8])
	}
	if err := r.ResolvePage(ctx, 2*chunk+8192, page); err != nil {
		t.Fatal(err)
	}
	if !allEqual(page, 0x22) {
		t.Fatalf("chunk 2 not 0x22: %x", page[:8])
	}

	st := r.Stats()
	if st.Fetches != 2 {
		t.Errorf("Fetches = %d, want 2", st.Fetches)
	}
	if st.ZeroFill != 1 {
		t.Errorf("ZeroFill = %d, want 1", st.ZeroFill)
	}
	// The zero chunk must NOT have hit the network.
	if *hits != 2 {
		t.Errorf("server hits = %d, want 2 (zero chunk served locally)", *hits)
	}
}
