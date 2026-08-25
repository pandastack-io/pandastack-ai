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
	"sync"
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
	// walBaseEvery / walBaseSweepInterval / walBaseRetry: how often each
	// database gets a fresh base backup, how often the sweeper looks for due
	// databases, and how soon a FAILED attempt is retried. The sweep is cheap
	// (map lookups + one stat per driver), so it runs every minute — a brand
	// new database gets its FIRST base backup within ~a minute of postgres
	// being ready instead of waiting for a long sweep period; until that
	// first backup lands, failover has nothing to restore from (P1 item 2).
	walBaseEvery         = 24 * time.Hour
	walBaseSweepInterval = 1 * time.Minute
	walBaseRetry         = 5 * time.Minute
	// walSegmentBytes: the padded size served for partial segments. Matches
	// Postgres' default wal_segment_size; our templates never change it.
	walSegmentBytes = 16 << 20
	// walPartialDefaultSecs: cadence of the partial-WAL uploader.
	// Bounds object-storage RPO to roughly this value plus one upload sweep —
	// versus a full 16 MiB segment (or archive_timeout) before it. Override
	// with PANDASTACK_WAL_PARTIAL_SECS; "0" disables.
	walPartialDefaultSecs = 20
)

// walBaseRatio returns the WAL-since-base ÷ base-size ratio that triggers an
// early base backup. Default 1.0; tune with PANDASTACK_BASE_RATIO.
func walBaseRatio() float64 {
	if v := strings.TrimSpace(os.Getenv("PANDASTACK_BASE_RATIO")); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			return f
		}
	}
	return 1.0
}

// walPartialInterval returns the partial-WAL upload cadence; 0 = disabled.
func walPartialInterval() time.Duration {
	if v := strings.TrimSpace(os.Getenv("PANDASTACK_WAL_PARTIAL_SECS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n <= 0 {
				return 0
			}
			return time.Duration(n) * time.Second
		}
	}
	return walPartialDefaultSecs * time.Second
}

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

	// recoverySources are database ids whose ARCHIVE may be read through
	// this relay even though their durable volume is not on this host —
	// i.e. clone/PITR sources being replayed by a clone guest running here.
	// Time-bounded so the read grant dies with the recovery it served.
	recMu           sync.Mutex
	recoverySources map[string]time.Time // id -> grant expiry

	// Archive-fencing state (all under archMu):
	//   genCache    — cached db.archive_gen from the sandbox row (5-min TTL);
	//                 stamps base backups + partial-WAL uploads so a stale
	//                 host's writes are distinguishable from the current
	//                 owner's (T1.1).
	//   walBytes    — WAL bytes uploaded since the last base backup, and
	//   baseSize    — the last base backup's size: together the ratio trigger
	//                 that bounds restore replay time (T1.3).
	//   lastPartial — "<seg>:<off>" of the last partial upload per DB, so a
	//                 quiet DB doesn't re-upload identical partials (T1.2).
	//   baseFails / baseParkedTo — per-DB circuit breaker: 5 consecutive
	//                 base-backup failures parks that DB's backups for 24 h
	//                 instead of hammering a wedged guest every 5 min (T1.6).
	archMu       sync.Mutex
	genCache     map[string]relayGenEntry
	walBytes     map[string]int64
	baseSize     map[string]int64
	lastPartial  map[string]string
	baseFails    map[string]int
	baseParkedTo map[string]time.Time
}

type relayGenEntry struct {
	gen string
	at  time.Time
}

// archiveGenFor returns the archive generation this relay must stamp on its
// uploads for id: the sandbox row's db.archive_gen metadata, defaulting to
// "1" for rows that predate fencing (the control plane's seed value).
//
// PINNED-AT-EPOCH. The value is read
// ONCE, on first sight of id by this relay, and then held for the life of this
// serving epoch — it is NOT re-read on a timer. This is the property that makes
// the fence actually fence: a split-brain zombie old host that keeps running
// after a failover keeps stamping its OWN (older) generation forever, so its
// abandoned-timeline bases always sort below the new owner's. Re-reading the
// shared control-plane row (its previous behaviour) let the zombie ADOPT the
// new owner's generation once its cache expired, defeating the entire scheme.
//
// The only legitimate way a live database's generation changes is a local
// ownership change (in-place restore / failover landing here), which restarts
// the VM on THIS host and calls InvalidateGen(id) — the explicit re-pin hook.
// A zombie never restarts the VM here and so never re-pins.
func (w *WALRelay) archiveGenFor(ctx context.Context, id string) string {
	w.archMu.Lock()
	if e, ok := w.genCache[id]; ok {
		w.archMu.Unlock()
		return e.gen
	}
	w.archMu.Unlock()
	gen := "1"
	if sbAny, err := w.m.store.GetSandbox(ctx, id); err == nil && sbAny != nil {
		if rmap, _ := sbAny.(map[string]any); rmap != nil {
			if md, _ := rmap["metadata"].(map[string]string); md != nil && md["db.archive_gen"] != "" {
				// Digits only — this value becomes part of GCS object names.
				if _, perr := strconv.ParseInt(md["db.archive_gen"], 10, 64); perr == nil {
					gen = md["db.archive_gen"]
				}
			}
		}
	}
	w.archMu.Lock()
	w.genCache[id] = relayGenEntry{gen: gen, at: time.Now()}
	w.archMu.Unlock()
	return gen
}

// InvalidateGen drops the pinned archive generation for id so the next
// archiveGenFor re-reads it from the control-plane row. Call this ONLY when
// THIS host (re)starts the database's VM under a new epoch — i.e. after a
// failover or in-place restore that has already stamped the bumped
// db.archive_gen on the row. A zombie old host never reaches this path, which
// is precisely why it keeps its stale generation and loses base selection.
func (w *WALRelay) InvalidateGen(id string) {
	w.archMu.Lock()
	delete(w.genCache, id)
	w.archMu.Unlock()
}

// AllowRecoverySource grants read access to id's WAL archive through this
// relay's GET endpoint for ttl. Called when a clone of id is staged on this
// host; the clone's guest authenticates with the source's per-id token, and
// this grant replaces the local-volume existence check.
func (w *WALRelay) AllowRecoverySource(id string, ttl time.Duration) {
	w.recMu.Lock()
	defer w.recMu.Unlock()
	if w.recoverySources == nil {
		w.recoverySources = map[string]time.Time{}
	}
	w.recoverySources[id] = time.Now().Add(ttl)
}

// recoverySourceAllowed reports whether id has an unexpired recovery grant.
// The in-memory grant (armed by runClone) is the fast path; on a miss the
// relay RE-DERIVES the grant from the store — any sandbox row owned by this
// agent whose metadata says db.recover_from == id proves a clone here is
// entitled to read that source's archive. This makes a WAL replay survive an
// agent restart mid-recovery (the in-memory map dies with the process; the
// row marker does not). Successful derivations are memoized for an hour.
func (w *WALRelay) recoverySourceAllowed(ctx context.Context, id string) bool {
	w.recMu.Lock()
	exp, ok := w.recoverySources[id]
	if ok && time.Now().Before(exp) {
		w.recMu.Unlock()
		return true
	}
	if ok {
		delete(w.recoverySources, id)
	}
	w.recMu.Unlock()

	sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	rows, err := w.m.store.ListSandboxesForAgent(sctx, w.m.agentID)
	if err != nil {
		return false
	}
	for _, row := range rows {
		rmap, _ := row.(map[string]any)
		if rmap == nil {
			continue
		}
		if md, ok := rmap["metadata"].(map[string]string); ok && md["db.recover_from"] == id {
			w.AllowRecoverySource(id, time.Hour)
			return true
		}
	}
	return false
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
	return &WALRelay{m: m, log: log, bucket: bucket, addr: addr, port: port, key: key, spool: spool,
		genCache: map[string]relayGenEntry{}, walBytes: map[string]int64{}, baseSize: map[string]int64{},
		lastPartial: map[string]string{}, baseFails: map[string]int{}, baseParkedTo: map[string]time.Time{}}, nil
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
	go w.runPartialWAL(ctx)
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
		// Same gate as ingest — only databases with a durable volume on this
		// host may read the archive through this relay — EXCEPT clone/PITR
		// sources, whose volume lives on another host (or nowhere anymore):
		// those get a time-bounded grant when the clone is staged here,
		// re-derivable from the clone row after an agent restart.
		if _, err := os.Stat(w.m.dbVolumePath(id)); err != nil && !w.recoverySourceAllowed(r.Context(), id) {
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
				// Full segment not archived. Before declaring end
				// of recovery, look for a PARTIAL upload of this segment —
				// the flushed prefix of the WAL the primary was writing when
				// it died. Serving it (zero-padded to a full segment) lets
				// recovery replay those last seconds of commits; Postgres
				// stops cleanly at the end of the valid records.
				if w.servePartialSegment(rw, r.Context(), id, name) {
					return
				}
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

// parsePartialName decodes "<segment>.partial-<offset>-g<gen>" (gen suffix
// optional for forward-compat). ok=false for anything else.
func parsePartialName(n string) (seg string, off, gen int64, ok bool) {
	i := strings.Index(n, ".partial-")
	if i <= 0 {
		return "", 0, 0, false
	}
	seg = n[:i]
	rest := n[i+len(".partial-"):]
	if j := strings.Index(rest, "-g"); j >= 0 {
		g, gerr := strconv.ParseInt(rest[j+2:], 10, 64)
		if gerr != nil {
			return "", 0, 0, false
		}
		gen = g
		rest = rest[:j]
	}
	o, oerr := strconv.ParseInt(rest, 10, 64)
	if oerr != nil || o <= 0 {
		return "", 0, 0, false
	}
	return seg, o, gen, true
}

// bestPartial picks the winning partial among candidates for one segment:
// highest generation first (the current owner epoch), then largest offset
// (most WAL). Returns "" when none parse.
func bestPartial(names []string, seg string) string {
	best, bestOff, bestGen := "", int64(-1), int64(-1)
	for _, n := range names {
		s, off, gen, ok := parsePartialName(n)
		if !ok || s != seg {
			continue
		}
		if gen > bestGen || (gen == bestGen && off > bestOff) {
			best, bestOff, bestGen = n, off, gen
		}
	}
	return best
}

// servePartialSegment streams the best partial upload of segment `name`,
// zero-padded to a full 16 MiB segment, from spool or GCS. Returns false when
// no partial exists (caller falls through to 404 = genuine end of recovery).
func (w *WALRelay) servePartialSegment(rw http.ResponseWriter, ctx context.Context, id, name string) bool {
	// Spool first (a partial spooled seconds ago may not have hit GCS yet).
	dir := filepath.Join(w.spool, id, "wal")
	var cands []string
	if ents, err := os.ReadDir(dir); err == nil {
		for _, e := range ents {
			cands = append(cands, e.Name())
		}
	}
	if best := bestPartial(cands, name); best != "" {
		if w.streamPadded(rw, filepath.Join(dir, best)) {
			w.log.Info("wal relay: served partial segment from spool", "id", id, "partial", best)
			return true
		}
	}
	// GCS: list partials for this segment.
	lctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	pat := "gs://" + w.bucket + "/db/" + id + "/wal/" + name + ".partial-*"
	out, err := exec.CommandContext(lctx, "gsutil", "ls", pat).CombinedOutput()
	if err != nil {
		return false // no partials (or listing failed — 404 is the safe answer)
	}
	var gcsNames []string
	for _, ln := range strings.Split(string(out), "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		gcsNames = append(gcsNames, ln[strings.LastIndex(ln, "/")+1:])
	}
	best := bestPartial(gcsNames, name)
	if best == "" {
		return false
	}
	tmpf, err := os.CreateTemp(w.spool, ".walfetch-*")
	if err != nil {
		return false
	}
	tmp := tmpf.Name()
	_ = tmpf.Close()
	defer os.Remove(tmp)
	obj := "gs://" + w.bucket + "/db/" + id + "/wal/" + best
	if _, err := exec.CommandContext(lctx, "gsutil", "-q", "cp", obj, tmp).CombinedOutput(); err != nil {
		return false
	}
	if w.streamPadded(rw, tmp) {
		w.log.Info("wal relay: served partial segment from GCS", "id", id, "partial", best)
		return true
	}
	return false
}

// streamPadded streams path zero-padded to walSegmentBytes with the padded
// Content-Length — Postgres expects full-size segment files.
func (w *WALRelay) streamPadded(rw http.ResponseWriter, path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.IsDir() || st.Size() > walSegmentBytes {
		return false
	}
	rw.Header().Set("Content-Type", "application/octet-stream")
	rw.Header().Set("Content-Length", strconv.FormatInt(walSegmentBytes, 10))
	if _, err := io.Copy(rw, f); err != nil {
		return true // headers sent; nothing more we can do
	}
	if pad := walSegmentBytes - st.Size(); pad > 0 {
		_, _ = io.CopyN(rw, zeroReader{}, pad)
	}
	return true
}

// zeroReader yields an endless stream of zero bytes for segment padding.
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
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
			var size int64
			if st, serr := os.Stat(path); serr == nil {
				size = st.Size()
			}
			obj := "gs://" + w.bucket + "/db/" + filepath.ToSlash(rel)
			// CREATE-ONLY upload (x-goog-if-generation-match:0).
			// Archive objects are immutable by name; first-writer-wins means a
			// stale host re-archiving the same segment after a failover CANNOT
			// clobber what the new owner already uploaded. A 412 Precondition
			// Failed therefore means "already archived" — drop the local copy.
			uctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
			out, err := exec.CommandContext(uctx, "gsutil", "-q",
				"-h", "x-goog-if-generation-match:0", "cp", path, obj).CombinedOutput()
			cancel()
			if err != nil {
				s := string(out)
				if strings.Contains(s, "412") || strings.Contains(s, "Precondition") || strings.Contains(s, "conditionNotMet") {
					w.log.Warn("wal relay: object already archived (duplicate or fenced stale writer) — dropping local copy",
						"file", rel)
					_ = os.Remove(path)
					continue
				}
				w.log.Warn("wal relay: gcs upload failed (will retry)",
					"file", rel, "err", err, "out", strings.TrimSpace(s))
				continue
			}
			_ = os.Remove(path)
			// Feed the ratio trigger. Full WAL segments count toward
			// walBytes; base uploads record the new base size. Partials are
			// duplicative prefixes and do not count.
			parts := strings.SplitN(filepath.ToSlash(rel), "/", 3)
			if len(parts) == 3 {
				id, kind, name := parts[0], parts[1], parts[2]
				w.archMu.Lock()
				switch {
				case kind == "wal" && !strings.Contains(name, ".partial-"):
					w.walBytes[id] += size
				case kind == "base":
					w.baseSize[id] = size
				}
				w.archMu.Unlock()
			}
			w.log.Debug("wal relay: uploaded", "object", obj)
		}
	}
}

// runBaseBackups triggers a daily pg_basebackup inside each running managed
// database, streamed through the relay (so it lands in the same spool→GCS
// pipeline). In-memory schedule only: an agent restart causes one early
// re-backup per database, which is harmless.
func (w *WALRelay) runBaseBackups(ctx context.Context) {
	// lastSuccess/lastAttempt are tracked SEPARATELY: a successful backup
	// waits the full walBaseEvery, but a FAILED attempt retries after
	// walBaseRetry. The old single-timestamp scheme made the first failure
	// (e.g. postgres still bootstrapping when the sweep hit a brand-new
	// database) postpone the first real backup by a full day — a day in
	// which failover had nothing to restore from.
	lastSuccess := map[string]time.Time{}
	lastAttempt := map[string]time.Time{}
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
			// Per-DB circuit breaker. 5 consecutive failures park
			// this DB's backups for 24 h — a wedged guest must not be
			// pg_basebackup-hammered every 5 minutes forever.
			w.archMu.Lock()
			parkedTo := w.baseParkedTo[id]
			w.archMu.Unlock()
			if !parkedTo.IsZero() && time.Now().Before(parkedTo) {
				continue
			}
			// Ratio trigger. Due when the daily timer says so OR
			// when the WAL uploaded since the last base exceeds ratio × the
			// base's own size — bounding how much WAL any restore must replay
			// regardless of write rate. baseSize==0 (agent restart, no upload
			// seen yet) disables the ratio half; the daily timer still runs.
			w.archMu.Lock()
			ratioDue := w.baseSize[id] > 0 &&
				float64(w.walBytes[id]) >= walBaseRatio()*float64(w.baseSize[id])
			w.archMu.Unlock()
			timerDue := lastSuccess[id].IsZero() || time.Since(lastSuccess[id]) >= walBaseEvery
			if !timerDue && !ratioDue {
				continue
			}
			if at := lastAttempt[id]; !at.IsZero() && time.Since(at) < walBaseRetry {
				continue // recent failure — back off, don't hammer
			}
			// A database that is still RECOVERING (clone/PITR replaying WAL,
			// or a failover restore mid-replay) must not be base-backed-up:
			// the tarball would capture a mid-replay PGDATA with recovery
			// config inside, polluting the database's own archive. ready.json
			// only exists after autostart saw recovery finish, so gate on it.
			// Not-ready is NOT a failure — skip without stamping the retry
			// backoff, so the first real backup still lands within ~a minute
			// of readiness (P1's 2-minute first-backup promise).
			if !w.dbGuestReady(ctx, id) {
				w.log.Debug("wal relay: base backup deferred (guest not ready)", "id", id)
				continue
			}
			err := w.baseBackupOne(ctx, id)
			// Stamp lastAttempt AFTER the attempt returns: baseBackupOne can
			// legitimately run for many minutes (30-min ceiling), and a
			// start-stamped backoff would already be expired when a long
			// failed attempt returned — back-to-back pg_basebackup hammering
			// of an already-struggling guest.
			lastAttempt[id] = time.Now()
			if err != nil {
				w.archMu.Lock()
				w.baseFails[id]++
				fails := w.baseFails[id]
				if fails >= 5 {
					w.baseParkedTo[id] = time.Now().Add(24 * time.Hour)
				}
				w.archMu.Unlock()
				if fails >= 5 {
					w.log.Error("wal relay: BASE-BACKUP CIRCUIT BREAKER TRIPPED — parking this database's backups for 24h",
						"id", id, "consecutive_failures", fails)
				} else {
					w.log.Warn("wal relay: base backup failed (will retry)",
						"id", id, "retry_in", walBaseRetry.String(), "err", err)
				}
				continue
			}
			w.archMu.Lock()
			w.baseFails[id] = 0
			delete(w.baseParkedTo, id)
			// Reset the ratio counter: the WAL uploaded before this base is
			// now covered by it.
			w.walBytes[id] = 0
			w.archMu.Unlock()
			lastSuccess[id] = time.Now()
			w.log.Info("wal relay: base backup complete", "id", id)
		}
		for id := range lastAttempt {
			if _, err := os.Stat(w.m.dbVolumePath(id)); err != nil {
				delete(lastAttempt, id)
				delete(lastSuccess, id)
			}
		}
	}
}

// runPartialWAL periodically uploads the FLUSHED PREFIX of each
// database's current WAL segment, so the object-storage RPO is bounded by
// this cadence instead of a full 16 MiB segment / archive_timeout. Mirrors
// Neon's wal_backup_partial (whose own timeout is 15 MINUTES — this beats
// their archive pipeline at its own game; their RPO=0 story is quorum disks,
// not S3).
//
// Mechanics: ask Postgres for (walfile, offset) at the current flush LSN,
// then `head -c offset` of that segment POSTed through the normal ingest
// path under a self-describing name: <segment>.partial-<offset>-g<gen>.
// The spool→GCS uploader ships it create-only like everything else. A name
// encodes everything needed at restore time; superseded partials are cleaned
// locally here and in GCS by the control-plane janitor.
//
// Postgres in recovery (clone/PITR replaying) has no flush LSN — the psql
// call fails and the DB is skipped, which is exactly right: a replaying
// database must not archive.
func (w *WALRelay) runPartialWAL(ctx context.Context) {
	interval := walPartialInterval()
	if interval <= 0 {
		w.log.Info("wal relay: partial-WAL uploads disabled")
		return
	}
	t := time.NewTicker(interval)
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
			if ctx.Err() != nil {
				return
			}
			if _, err := os.Stat(w.m.dbVolumePath(id)); err != nil {
				continue // not a managed database
			}
			w.partialOne(ctx, id)
		}
	}
}

// partialOne uploads one partial segment for id if it moved since last time.
// All failures are silent skips — the next tick retries, and the full-segment
// archive_command remains the durability backstop.
func (w *WALRelay) partialOne(ctx context.Context, id string) {
	gctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	gc, err := w.m.Guest(id)
	if err != nil {
		return
	}
	alloc, err := w.m.netPool.Lookup(gctx, id)
	if err != nil || alloc.HostIP == "" {
		return
	}
	gen := w.archiveGenFor(gctx, id)
	// One guest round-trip: resolve (segment, flushed-offset, datadir), skip
	// when idle (offset 0), then POST the flushed prefix through ingest.
	// Exit 3 = "nothing to do" (idle, or in recovery where flush LSN errors).
	probe := "bash -c 'line=$(sudo -u postgres psql -Atc \"SELECT file_name, file_offset FROM pg_walfile_name_offset(pg_current_wal_flush_lsn())\" 2>/dev/null) || exit 3; " +
		"seg=${line%|*}; off=${line#*|}; [ -n \"$seg\" ] && [ \"$off\" -gt 0 ] 2>/dev/null || exit 3; echo \"$seg $off\"'"
	res, err := gc.Exec(gctx, probe)
	if err != nil || res.ExitCode != 0 {
		return
	}
	fields := strings.Fields(strings.TrimSpace(res.Stdout))
	if len(fields) != 2 {
		return
	}
	seg, off := fields[0], fields[1]
	if !walNameRe.MatchString(seg) {
		return
	}
	offN, err := strconv.ParseInt(off, 10, 64)
	if err != nil || offN <= 0 || offN > walSegmentBytes {
		return
	}
	key := seg + ":" + off
	w.archMu.Lock()
	if w.lastPartial[id] == key {
		w.archMu.Unlock()
		return // nothing new flushed since the last partial
	}
	w.archMu.Unlock()

	name := fmt.Sprintf("%s.partial-%d-g%s", seg, offN, gen)
	url := fmt.Sprintf("http://%s:%d/wal/%s/%s", alloc.HostIP, w.port, id, name)
	up := fmt.Sprintf(
		"bash -c 'set -o pipefail; d=$(sudo -u postgres psql -Atc \"SHOW data_directory\") || exit 1; "+
			"sudo head -c %d \"$d/pg_wal/%s\" | curl -fsS --max-time 60 -X POST -T - -H \"Authorization: Bearer %s\" \"%s\"'",
		offN, seg, w.Token(id), url)
	res, err = gc.Exec(gctx, up)
	if err != nil || res.ExitCode != 0 {
		w.log.Debug("wal relay: partial upload failed (will retry)", "id", id, "seg", seg, "err", err)
		return
	}
	w.archMu.Lock()
	w.lastPartial[id] = key
	w.archMu.Unlock()
	// Drop superseded spool partials for the same segment (smaller offsets).
	dir := filepath.Join(w.spool, id, "wal")
	if ents, derr := os.ReadDir(dir); derr == nil {
		for _, e := range ents {
			n := e.Name()
			if n != name && strings.HasPrefix(n, seg+".partial-") {
				_ = os.Remove(filepath.Join(dir, n))
			}
		}
	}
	w.log.Debug("wal relay: partial segment uploaded", "id", id, "seg", seg, "bytes", offN)
}

// baseBackupOne streams one gzipped pg_basebackup tarball from the guest
// through the relay. -X fetch makes the tar self-contained (includes the WAL
// needed to reach consistency) so a restore works even with archive gaps.
// dbGuestReady reports whether the guest finished its full boot pipeline
// (recovery complete + credentials rotated): autostart.sh writes
// /run/pandastack/ready.json only at the very end. Unreachable guests
// report not-ready (the sweep just tries again next minute).
func (w *WALRelay) dbGuestReady(ctx context.Context, id string) bool {
	gctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	gc, err := w.m.Guest(id)
	if err != nil {
		return false
	}
	res, err := gc.Exec(gctx, "test -f /run/pandastack/ready.json")
	return err == nil && res.ExitCode == 0
}

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
	// Generation-stamped name. Restore prefers the highest
	// generation, so a stale host's late uploads (lower gen) can never win.
	name := "base-" + time.Now().UTC().Format("20060102T150405Z") + "-g" + w.archiveGenFor(cctx, id) + ".tar.gz"
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

