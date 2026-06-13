// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"crypto/hmac"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// WAL archiving relay (managed databases → GCS).
//
// Guests must never hold GCP credentials, so postgres can't push WAL to GCS
// itself. Instead the guest's archive_command POSTs each 16 MiB WAL segment
// over plain HTTP to THIS relay on the host:
//
//	guest archive_command (pandastack-wal-archive %p %f)
//	  → POST http://<vh-host-ip>:7071/wal/{sandbox}/{segment}   (vh-* veth, host-internal)
//	  → relay spools to {DataDir}/wal-spool/{id}/wal/{segment}  (fsync, then 201)
//	  → background uploader: gsutil cp → gs://<bucket>/db/{id}/wal/{segment}
//
// The same relay accepts streamed base backups (POST/PUT /base/{id}/{name});
// a daily sweeper triggers `pg_basebackup | gzip | curl -T -` inside each
// database guest. Base + archived WAL is everything item-4 failover needs to
// rebuild a database on another agent.
//
// Reachability: the guest's default route exits the sandbox netns via the
// veth pair, so the root-netns vh-* address (Allocation.HostIP, 10.200.X.1)
// is dialable from inside the guest. The relay binds 0.0.0.0:<port>: the GCP
// VPC firewall default-denies external ingress on that port, and every
// request must additionally carry a per-sandbox bearer token.
//
// Auth is stateless: token = "pds_wal_" + hex(HMAC-SHA256(hostKey, sandboxID))
// with a random per-host key persisted at {DataDir}/wal-relay.key. The agent
// derives the same token when injecting /etc/pandastack/wal.env at phase 2,
// so nothing needs to survive in memory across agent restarts.

const (
	// walRelayDefaultAddr: listener for guest→host WAL traffic. Override via
	// PANDASTACK_WAL_RELAY_ADDR. Must stay firewalled from the outside world.
	walRelayDefaultAddr = "0.0.0.0:7071"
	// walMaxSegmentBytes: a WAL segment is 16 MiB; allow generous headroom
	// for future wal_segment_size tweaks.
	walMaxSegmentBytes = 64 << 20
	// walMaxBaseBytes: streamed pg_basebackup tarball ceiling.
	walMaxBaseBytes = 64 << 30
	// walUploadInterval: spool → GCS sweep cadence (also the worst-case
	// extra RPO on top of archive_timeout if the host dies).
	walUploadInterval = 10 * time.Second
	// walBaseEvery / walBaseSweepInterval: how often each database gets a
	// fresh base backup, and how often the sweeper looks for due databases.
	walBaseEvery         = 24 * time.Hour
	walBaseSweepInterval = 15 * time.Minute
)

// walNameRe allows WAL segment names, timeline history files and our base
// backup names; rejects path tricks ("..", "/", leading dot).
var walNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// WALRelay receives WAL segments / base backups from database guests and
// replicates them to GCS. One per agent; nil when archiving is disabled.
type WALRelay struct {
	m      *Manager
	log    *slog.Logger
	bucket string
	addr   string
	port   int
	key    []byte
	spool  string
}

// NewWALRelayFromEnv builds the relay, or returns (nil, nil) when WAL
// archiving is not configured (no PANDASTACK_SNAPSHOT_BUCKET).
func NewWALRelayFromEnv(m *Manager, log *slog.Logger) (*WALRelay, error) {
	bucket := strings.TrimSpace(os.Getenv("PANDASTACK_SNAPSHOT_BUCKET"))
	if bucket == "" {
		return nil, nil
	}
	addr := strings.TrimSpace(os.Getenv("PANDASTACK_WAL_RELAY_ADDR"))
	if addr == "" {
		addr = walRelayDefaultAddr
	}
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return nil, fmt.Errorf("wal relay addr %q: missing port", addr)
	}
	port, err := strconv.Atoi(addr[i+1:])
	if err != nil || port <= 0 {
		return nil, fmt.Errorf("wal relay addr %q: bad port", addr)
	}
	key, err := loadOrCreateRelayKey(filepath.Join(m.DataDir(), "wal-relay.key"))
	if err != nil {
		return nil, err
	}
	spool := filepath.Join(m.DataDir(), "wal-spool")
	if err := os.MkdirAll(spool, 0o700); err != nil {
		return nil, err
	}
	return &WALRelay{m: m, log: log, bucket: bucket, addr: addr, port: port, key: key, spool: spool}, nil
}

// loadOrCreateRelayKey reads the per-host HMAC key, generating it on first
// use. The key only needs to outlive the guests provisioned on this host.
func loadOrCreateRelayKey(path string) ([]byte, error) {
	if b, err := os.ReadFile(path); err == nil {
		if k, derr := hex.DecodeString(strings.TrimSpace(string(b))); derr == nil && len(k) >= 16 {
			return k, nil
		}
		return nil, fmt.Errorf("wal relay key %s: corrupt", path)
	}
	k := make([]byte, 32)
	if _, err := cryptorand.Read(k); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(hex.EncodeToString(k)+"\n"), 0o600); err != nil {
		return nil, err
	}
	return k, nil
}

func (w *WALRelay) Addr() string   { return w.addr }
func (w *WALRelay) Port() int      { return w.port }
func (w *WALRelay) Bucket() string { return w.bucket }

// Token derives the per-sandbox bearer token. Stateless: any agent process
// holding the host key produces the same value for the same sandbox.
func (w *WALRelay) Token(id string) string {
	mac := hmac.New(sha256.New, w.key)
	mac.Write([]byte(id))
	return "pds_wal_" + hex.EncodeToString(mac.Sum(nil))
}

// Run serves the relay until ctx is cancelled. Blocks; call in a goroutine.
func (w *WALRelay) Run(ctx context.Context) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /wal/{id}/{name}", w.ingest("wal", walMaxSegmentBytes))
	mux.HandleFunc("POST /base/{id}/{name}", w.ingest("base", walMaxBaseBytes))
	mux.HandleFunc("PUT /base/{id}/{name}", w.ingest("base", walMaxBaseBytes)) // curl -T defaults to PUT
	mux.HandleFunc("GET /wal/{id}/{name}", w.serveWAL())
	srv := &http.Server{
		Addr:              w.addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		// No ReadTimeout: base backups stream for minutes.
	}
	go w.runUploader(ctx)
	go w.runBaseBackups(ctx)
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		w.log.Error("wal relay listener failed", "addr", w.addr, "err", err)
	}
}

// ingest returns the handler for one artifact kind ("wal" | "base"). It
// authenticates the per-sandbox token, streams the body to the spool with
// fsync, and only then returns 201 — postgres treats a non-2xx/curl failure
// as "not archived" and retries, so durability on host disk is the contract.
func (w *WALRelay) ingest(kind string, maxBytes int64) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		id, name := r.PathValue("id"), r.PathValue("name")
		if !walNameRe.MatchString(id) || !walNameRe.MatchString(name) {
			http.Error(rw, "bad path", http.StatusBadRequest)
			return
		}
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(tok), []byte(w.Token(id))) != 1 {
			http.Error(rw, "unauthorized", http.StatusUnauthorized)
			return
		}
		// Only managed databases (the only sandboxes with a durable DB
		// volume on this host) may spool — a valid token for a deleted DB
		// must not recreate state.
		if _, err := os.Stat(w.m.dbVolumePath(id)); err != nil {
			http.Error(rw, "unknown database", http.StatusNotFound)
			return
		}
		dir := filepath.Join(w.spool, id, kind)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			http.Error(rw, "spool error", http.StatusInternalServerError)
			return
		}
		tmp := filepath.Join(dir, "."+name+".tmp")
		f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			http.Error(rw, "spool error", http.StatusInternalServerError)
			return
		}
		n, err := io.Copy(f, http.MaxBytesReader(rw, r.Body, maxBytes))
		if err == nil {
			err = f.Sync()
		}
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err == nil {
			err = os.Rename(tmp, filepath.Join(dir, name))
		}
		if err != nil {
			_ = os.Remove(tmp)
			w.log.Warn("wal relay: ingest failed", "id", id, "kind", kind, "name", name, "err", err)
			http.Error(rw, "spool error", http.StatusInternalServerError)
			return
		}
		w.log.Debug("wal relay: spooled", "id", id, "kind", kind, "name", name, "bytes", n)
		rw.WriteHeader(http.StatusCreated)
	}
}

// serveWAL handles GET /wal/{id}/{name}: postgres restore_command
// (pandastack-wal-restore) fetching archived WAL during point-in-time
// recovery after a failover restore. Spool is checked first (segments
// archived seconds ago may not have reached GCS yet), then GCS.
//
// Status codes are part of the recovery contract: postgres interprets a
// failing restore_command as "segment does not exist" and ENDS recovery, so
// only a genuine not-found may surface as a quick failure (404; curl -f
// exits 22 without retrying). Transient GCS errors return 502, which the
// guest's curl --retry retries — a flaky fetch must not truncate recovery.
func (w *WALRelay) serveWAL() http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		id, name := r.PathValue("id"), r.PathValue("name")
		if !walNameRe.MatchString(id) || !walNameRe.MatchString(name) {
			http.Error(rw, "bad path", http.StatusBadRequest)
			return
		}
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(tok), []byte(w.Token(id))) != 1 {
			http.Error(rw, "unauthorized", http.StatusUnauthorized)
			return
		}
		// Same gate as ingest: only databases with a durable volume on this
		// host may read the archive through this relay.
		if _, err := os.Stat(w.m.dbVolumePath(id)); err != nil {
			http.Error(rw, "unknown database", http.StatusNotFound)
			return
		}
		if local := filepath.Join(w.spool, id, "wal", name); w.streamFile(rw, local) {
			return
		}
		// Fetch from GCS into a dot-prefixed temp file (the uploader sweep
		// skips dot files) and stream it back.
		tmpf, err := os.CreateTemp(w.spool, ".walfetch-*")
		if err != nil {
			http.Error(rw, "spool error", http.StatusInternalServerError)
			return
		}
		tmp := tmpf.Name()
		_ = tmpf.Close()
		defer os.Remove(tmp)
		obj := "gs://" + w.bucket + "/db/" + id + "/wal/" + name
		gctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
		defer cancel()
		out, err := exec.CommandContext(gctx, "gsutil", "-q", "cp", obj, tmp).CombinedOutput()
		if err != nil {
			s := string(out)
			if strings.Contains(s, "No URLs matched") || strings.Contains(s, "NotFoundException") || strings.Contains(s, "matched no objects") {
				// Normal end of recovery (e.g. probing for the next segment
				// or a missing .history file) — keep it quiet.
				w.log.Debug("wal relay: segment not in archive", "id", id, "name", name)
				http.Error(rw, "not found", http.StatusNotFound)
				return
			}
			w.log.Warn("wal relay: gcs fetch failed", "id", id, "name", name,
				"err", err, "out", strings.TrimSpace(s))
			http.Error(rw, "fetch failed", http.StatusBadGateway)
			return
		}
		if !w.streamFile(rw, tmp) {
			http.Error(rw, "spool error", http.StatusInternalServerError)
		}
	}
}

// streamFile streams path to rw with a Content-Length so the guest's curl
// can detect truncation. Returns false if the file can't be served (nothing
// written yet → caller may still send an error response).
func (w *WALRelay) streamFile(rw http.ResponseWriter, path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		return false
	}
	rw.Header().Set("Content-Type", "application/octet-stream")
	rw.Header().Set("Content-Length", strconv.FormatInt(st.Size(), 10))
	_, _ = io.Copy(rw, f)
	return true
}

// runUploader drains the spool to GCS. Layout mirrors 1:1:
// {spool}/{id}/{kind}/{name} → gs://{bucket}/db/{id}/{kind}/{name}.
// Failures are retried on the next sweep (file stays in the spool).
func (w *WALRelay) runUploader(ctx context.Context) {
	t := time.NewTicker(walUploadInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		var files []string
		_ = filepath.WalkDir(w.spool, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || strings.HasPrefix(d.Name(), ".") {
				return nil //nolint:nilerr // skip unreadable entries; retry next sweep
			}
			files = append(files, path)
			return nil
		})
		for _, path := range files {
			rel, err := filepath.Rel(w.spool, path)
			if err != nil || strings.Count(rel, string(filepath.Separator)) != 2 {
				continue // not {id}/{kind}/{name}; leave for a human
			}
			obj := "gs://" + w.bucket + "/db/" + filepath.ToSlash(rel)
			uctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
			out, err := exec.CommandContext(uctx, "gsutil", "-q", "cp", path, obj).CombinedOutput()
			cancel()
			if err != nil {
				w.log.Warn("wal relay: gcs upload failed (will retry)",
					"file", rel, "err", err, "out", strings.TrimSpace(string(out)))
				continue
			}
			_ = os.Remove(path)
			w.log.Debug("wal relay: uploaded", "object", obj)
		}
	}
}

// runBaseBackups triggers a daily pg_basebackup inside each running managed
// database, streamed through the relay (so it lands in the same spool→GCS
// pipeline). In-memory schedule only: an agent restart causes one early
// re-backup per database, which is harmless.
func (w *WALRelay) runBaseBackups(ctx context.Context) {
	lastBase := map[string]time.Time{}
	t := time.NewTicker(walBaseSweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		w.m.mu.RLock()
		ids := make([]string, 0, len(w.m.drivers))
		for id := range w.m.drivers {
			ids = append(ids, id)
		}
		w.m.mu.RUnlock()
		for _, id := range ids {
			if _, err := os.Stat(w.m.dbVolumePath(id)); err != nil {
				continue // not a managed database
			}
			if time.Since(lastBase[id]) < walBaseEvery {
				continue
			}
			if err := w.baseBackupOne(ctx, id); err != nil {
				w.log.Warn("wal relay: base backup failed", "id", id, "err", err)
			}
			// Failures also wait a full period: a wedged database must not
			// be hammered with hour-long basebackup attempts every sweep.
			lastBase[id] = time.Now()
		}
		for id := range lastBase {
			if _, err := os.Stat(w.m.dbVolumePath(id)); err != nil {
				delete(lastBase, id)
			}
		}
	}
}

// baseBackupOne streams one gzipped pg_basebackup tarball from the guest
// through the relay. -X fetch makes the tar self-contained (includes the WAL
// needed to reach consistency) so a restore works even with archive gaps.
func (w *WALRelay) baseBackupOne(ctx context.Context, id string) error {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	gc, err := w.m.Guest(id)
	if err != nil {
		return fmt.Errorf("guest client: %w", err)
	}
	alloc, err := w.m.netPool.Lookup(cctx, id)
	if err != nil || alloc.HostIP == "" {
		return fmt.Errorf("network allocation lookup: %w", err)
	}
	name := "base-" + time.Now().UTC().Format("20060102T150405Z") + ".tar.gz"
	url := fmt.Sprintf("http://%s:%d/base/%s/%s", alloc.HostIP, w.port, id, name)
	// Token + URL are hex/base64url-safe inside single quotes. pipefail needs
	// bash (root's shell runs the command, but be explicit for vsock exec).
	cmd := fmt.Sprintf(
		"bash -c 'set -o pipefail; sudo -u postgres /usr/lib/postgresql/16/bin/pg_basebackup -D - -Ft -X fetch 2>/tmp/pg_basebackup.err"+
			" | gzip -1"+
			" | curl -fsS --max-time 1700 -X POST -T - -H \"Authorization: Bearer %s\" \"%s\""+
			" || { tail -3 /tmp/pg_basebackup.err 1>&2; exit 1; }'",
		w.Token(id), url)
	res, err := gc.Exec(cctx, cmd)
	if err != nil {
		return fmt.Errorf("guest exec: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("pg_basebackup exit %d: %s", res.ExitCode, strings.TrimSpace(res.Stdout+res.Stderr))
	}
	w.log.Info("wal relay: base backup complete", "id", id, "name", name)
	return nil
}

// SetWALRelay attaches the relay to the manager so kickPGPhase2 can inject
// /etc/pandastack/wal.env into new database guests.
func (m *Manager) SetWALRelay(w *WALRelay) { m.walRelay = w }

// walEnvCmds returns the guest commands that install /etc/pandastack/wal.env
// for a database sandbox, or nil when archiving is disabled or the network
// allocation can't be resolved. The env file is what flips the guest's
// archive_command from no-op to relay mode, so it must be written BEFORE
// creds-ready unblocks autostart.sh (postgres starts after that point).
func (m *Manager) walEnvCmds(ctx context.Context, id string) []string {
	w := m.walRelay
	if w == nil {
		return nil
	}
	alloc, err := m.netPool.Lookup(ctx, id)
	if err != nil || alloc.HostIP == "" {
		m.log.Warn("pg phase2: wal.env skipped (no network allocation)", "id", id, "err", err)
		return nil
	}
	url := fmt.Sprintf("http://%s:%d", alloc.HostIP, w.port)
	return []string{
		"mkdir -p /etc/pandastack",
		fmt.Sprintf("printf 'PANDASTACK_WAL_URL=%s\\nPANDASTACK_WAL_ID=%s\\nPANDASTACK_WAL_TOKEN=%s\\n' > /etc/pandastack/wal.env"+
			" && chmod 600 /etc/pandastack/wal.env && chown postgres:postgres /etc/pandastack/wal.env",
			url, id, w.Token(id)),
	}
}
