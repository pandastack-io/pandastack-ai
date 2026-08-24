// SPDX-License-Identifier: Apache-2.0
//
// db_archive.go — managed-database archive hygiene.
//
// The backup engine (agent/internal/sandbox/wal_relay.go) writes periodic
// pg_basebackups and continuous WAL to gs://$SNAPSHOT_BUCKET/db/{id}/{base,wal}/.
// Nothing else prunes them, so without this an archive grows forever and a
// deleted database keeps costing storage. A 6h janitor loop walks every archive
// prefix and does two things:
//
//   - live database  -> prune to the configured retention window, replay-safely
//     (never delete WAL a retained base still needs to roll forward from).
//   - orphan (no row) -> purge the whole archive, so a deleted database leaves
//     nothing behind even when the delete-time cleanup failed.
//
// The window is a single operator setting (PANDASTACK_BACKUP_RETENTION_DAYS),
// not a per-tenant policy: this build has no tiers.
//
// This file also owns the small amount of shared schema the archive machinery
// needs — the fleet-wide janitor claim, and the generation/lease/health tables
// that db_fencing.go uses to keep two hosts from writing one archive chain.
package main

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// defaultRetentionDays is how far back the archive is kept by default.
	// Override with PANDASTACK_BACKUP_RETENTION_DAYS. Longer windows cost
	// object storage; shorter ones shrink how far a restore can roll back.
	defaultRetentionDays = 30

	// walPruneMargin keeps WAL a little older than the oldest retained base, so
	// archival lag can never make us delete a segment the anchor base still
	// needs to replay from.
	walPruneMargin = 6 * time.Hour

	// orphanPurgeGrace: an archive with no sandbox row is only purged once its
	// newest object is at least this old. A failover/restore deletes and
	// recreates the DB's row (agent/internal/sandbox/db_restore.go), leaving it
	// transiently — or, on an agent crash mid-restore, indistinguishably —
	// rowless; that db_restore.go comment is explicit that a rowless DB "is
	// exactly the state archive-cleanup logic treats as purgeable." A failover
	// is seconds-to-minutes and a failed restore re-inserts a `failed` row
	// within that window, so a multi-day grace makes purging a still-recoverable
	// archive practically impossible while still reclaiming truly-deleted ones.
	// (The synchronous cleanupArchive on delete is the fast path; this janitor
	// only mops up archives whose synchronous purge failed.)
	orphanPurgeGrace = 7 * 24 * time.Hour
)

func envDaysOr(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

func retentionEnabled() bool    { return os.Getenv("PANDASTACK_BACKUP_RETENTION") != "0" }
func janitorPurgeEnabled() bool { return os.Getenv("PANDASTACK_BACKUP_JANITOR_PURGE") != "0" }

// backupObj is one base backup: its full gs:// URL and the timestamp parsed
// from its name (base-<YYYYMMDDTHHMMSSZ>.tar.gz).
type backupObj struct {
	URL       string
	Timestamp time.Time
	SizeBytes int64
}

// walObj is one archived WAL segment: URL + its GCS upload time.
type walObj struct {
	URL   string
	Mtime time.Time
}

// baseNameRe matches both name generations: base-<stamp>.tar.gz (pre-fencing)
// and base-<stamp>-g<gen>.tar.gz (TUSK T1.1 generation-stamped uploads).
var baseNameRe = regexp.MustCompile(`base-(\d{8}T\d{6}Z)(?:-g(\d+))?\.tar\.gz$`)

// parseBaseTimestamp extracts the time from a base backup object name/URL.
// Returns ok=false for anything that isn't a base backup object.
func parseBaseTimestamp(nameOrURL string) (time.Time, bool) {
	m := baseNameRe.FindStringSubmatch(nameOrURL)
	if m == nil {
		return time.Time{}, false
	}
	t, err := time.Parse("20060102T150405Z", m[1])
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}

// parseBaseGeneration extracts the archive generation from a base backup
// name/URL. Pre-fencing names (no -g suffix) report generation 0, which sorts
// below every stamped generation — exactly the precedence we want.
func parseBaseGeneration(nameOrURL string) int64 {
	m := baseNameRe.FindStringSubmatch(nameOrURL)
	if m == nil || m[2] == "" {
		return 0
	}
	g, err := strconv.ParseInt(m[2], 10, 64)
	if err != nil {
		return 0
	}
	return g
}

// selectBasesToDelete is the PITR-safe retention decision for base backups.
//
// Keep every base newer than (now − window), PLUS the newest base older than
// the window — the "anchor" that lets a restore land at the far edge of the
// window. Never delete the single newest base (a database must always have at
// least one recovery point). Everything else is returned for deletion, along
// with the anchor timestamp (the oldest base we keep) so WAL can be pruned to
// match.
func selectBasesToDelete(bases []backupObj, window time.Duration, now time.Time) (toDelete []backupObj, anchor time.Time) {
	if len(bases) <= 1 {
		return nil, oldestTS(bases)
	}
	sorted := append([]backupObj(nil), bases...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Timestamp.Before(sorted[j].Timestamp) })
	cutoff := now.Add(-window)

	keep := make(map[string]bool)
	// Always keep the newest.
	keep[sorted[len(sorted)-1].URL] = true
	// Keep everything within the window.
	for _, b := range sorted {
		if !b.Timestamp.Before(cutoff) {
			keep[b.URL] = true
		}
	}
	// Keep the anchor: the newest base at or before the cutoff.
	for i := len(sorted) - 1; i >= 0; i-- {
		if sorted[i].Timestamp.Before(cutoff) {
			keep[sorted[i].URL] = true
			break
		}
	}
	anchor = now // if nothing is kept below "now", anchor stays now; corrected below
	first := true
	for _, b := range sorted {
		if keep[b.URL] {
			if first || b.Timestamp.Before(anchor) {
				anchor = b.Timestamp
				first = false
			}
			continue
		}
		toDelete = append(toDelete, b)
	}
	return toDelete, anchor
}

func oldestTS(bases []backupObj) time.Time {
	if len(bases) == 0 {
		return time.Time{}
	}
	min := bases[0].Timestamp
	for _, b := range bases[1:] {
		if b.Timestamp.Before(min) {
			min = b.Timestamp
		}
	}
	return min
}

// selectWALToDelete returns WAL segments safe to delete: those uploaded before
// the anchor base could possibly need them (anchor − margin). A zero anchor
// (no retained base to anchor on) deletes nothing.
func selectWALToDelete(wals []walObj, anchor time.Time, margin time.Duration) []walObj {
	if anchor.IsZero() {
		return nil
	}
	floor := anchor.Add(-margin)
	var out []walObj
	for _, w := range wals {
		if w.Mtime.Before(floor) {
			out = append(out, w)
		}
	}
	return out
}

// --- GCS listing / deletion (gsutil, same access the janitor already uses) ---

// listBaseBackups returns the base backups for a database, newest first.
func listBaseBackups(ctx context.Context, bucket, id string) ([]backupObj, error) {
	lctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	out, err := exec.CommandContext(lctx, gsutilPath(), "ls", "-l", "gs://"+bucket+"/db/"+id+"/base/").Output()
	if err != nil {
		if isGsutilNotFound(err, out) {
			return nil, nil
		}
		return nil, err
	}
	var res []backupObj
	for _, line := range strings.Split(string(out), "\n") {
		size, mtime, url, ok := parseGsutilLongLine(line)
		if !ok {
			continue
		}
		ts, ok := parseBaseTimestamp(url)
		if !ok {
			ts = mtime // fall back to upload time if the name is unexpected
		}
		res = append(res, backupObj{URL: url, Timestamp: ts, SizeBytes: size})
	}
	sort.Slice(res, func(i, j int) bool { return res[i].Timestamp.After(res[j].Timestamp) })
	return res, nil
}

// listWALObjects returns archived WAL segments with their upload times.
func listWALObjects(ctx context.Context, bucket, id string) ([]walObj, error) {
	lctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	out, err := exec.CommandContext(lctx, gsutilPath(), "ls", "-l", "gs://"+bucket+"/db/"+id+"/wal/").Output()
	if err != nil {
		if isGsutilNotFound(err, out) {
			return nil, nil
		}
		return nil, err
	}
	var res []walObj
	for _, line := range strings.Split(string(out), "\n") {
		_, mtime, url, ok := parseGsutilLongLine(line)
		if !ok {
			continue
		}
		res = append(res, walObj{URL: url, Mtime: mtime})
	}
	return res, nil
}

// parseGsutilLongLine parses one `gsutil ls -l` data row:
//
//	"    1234567  2026-08-20T08:30:00Z  gs://bucket/db/id/base/base-….tar.gz"
//
// Summary lines ("TOTAL:", "gs://…/" directory rows) and blanks return ok=false.
func parseGsutilLongLine(line string) (size int64, mtime time.Time, url string, ok bool) {
	f := strings.Fields(strings.TrimSpace(line))
	if len(f) < 3 {
		return 0, time.Time{}, "", false
	}
	url = f[len(f)-1]
	if !strings.HasPrefix(url, "gs://") || strings.HasSuffix(url, "/") {
		return 0, time.Time{}, "", false
	}
	sz, err := strconv.ParseInt(f[0], 10, 64)
	if err != nil {
		return 0, time.Time{}, "", false
	}
	t, err := time.Parse(time.RFC3339, f[1])
	if err != nil {
		return 0, time.Time{}, "", false
	}
	return sz, t.UTC(), url, true
}

func isGsutilNotFound(err error, out []byte) bool {
	s := string(out) + errString(err)
	return strings.Contains(s, "matched no objects") ||
		strings.Contains(s, "No URLs matched") ||
		strings.Contains(s, "NotFoundException")
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return string(ee.Stderr)
	}
	return err.Error()
}

// gsutilRm deletes the given gs:// URLs in one batched call. No-op on empty.
func gsutilRm(ctx context.Context, urls []string) error {
	if len(urls) == 0 {
		return nil
	}
	dctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	args := append([]string{"-m", "rm"}, urls...)
	return exec.CommandContext(dctx, gsutilPath(), args...).Run()
}

// --- the per-archive engine (called from the janitor loop) ---
func (d *databasesAPI) pruneArchiveToRetention(ctx context.Context, bucket, id string) {
	// TUSK T1.4: an in-flight failover or restore holds a lease on this
	// archive chain — do not prune under it (a pruned anchor mid-replay
	// truncates the recovery). The lease is minutes-long; next sweep prunes.
	if d.archiveLeaseActive(ctx, id) {
		d.log.Info("retention: archive leased (clone/restore in flight) — skipping prune", "id", id)
		return
	}
	// Belt-and-suspenders for the clone case (H3): the clone lease is a fixed
	// 45-min grant with no renewal, so a large-DB / old-target replay that runs
	// longer than the lease would otherwise become prunable while the clone is
	// still reading this SOURCE chain. cloneInFlightFrom tracks the clone's
	// actual provisioning window (creating, or running within the stage timeout)
	// and fails CLOSED, so it covers the tail the lease can't. Purge already
	// consults it; prune must too.
	if inflight, cloneID := d.cloneInFlightFrom(ctx, id); inflight {
		d.log.Info("retention: clone in flight from this source — skipping prune", "id", id, "clone", cloneID)
		return
	}
	window := time.Duration(envDaysOr("PANDASTACK_BACKUP_RETENTION_DAYS", defaultRetentionDays)) * 24 * time.Hour

	bases, err := listBaseBackups(ctx, bucket, id)
	if err != nil {
		d.log.Warn("retention: list base backups failed", "id", id, "err", err)
		return
	}
	delBases, anchor := selectBasesToDelete(bases, window, time.Now().UTC())

	wals, werr := listWALObjects(ctx, bucket, id)
	if werr != nil {
		d.log.Warn("retention: list WAL failed (pruning base only)", "id", id, "err", werr)
		wals = nil
	}
	delWALs := selectWALToDelete(wals, anchor, walPruneMargin)

	urls := make([]string, 0, len(delBases)+len(delWALs))
	for _, b := range delBases {
		urls = append(urls, b.URL)
	}
	// walDelSet tracks WAL objects being deleted this pass, so the catalog count
	// below reflects them (L1: partial deletions were previously not subtracted).
	walDelSet := make(map[string]bool, len(delWALs))
	for _, w := range delWALs {
		urls = append(urls, w.URL)
		walDelSet[w.URL] = true
	}
	// TUSK T1.2 housekeeping: superseded partial-WAL uploads (full segment
	// archived, or a bigger/newer partial exists). The agent never deletes
	// from GCS — reclaim is the janitor's job, matching the fencing rule that
	// only generation-validated control-plane paths destroy.
	walNames := make([]string, 0, len(wals))
	nameToURL := make(map[string]string, len(wals))
	for _, wo := range wals {
		n := lastPathSeg(wo.URL)
		walNames = append(walNames, n)
		nameToURL[n] = wo.URL
	}
	for _, n := range selectPartialsToDelete(walNames) {
		if u, ok := nameToURL[n]; ok {
			urls = append(urls, u)
			walDelSet[u] = true
		}
	}

	// TUSK T1.5: the listings this pass already paid for double as a scrub —
	// WAL-continuity + base sanity — persisted for the dashboard. Run it on
	// the KEPT view (post-prune plan) so a planned deletion isn't a "gap".
	scrub := func() {
		delSet := map[string]bool{}
		for _, u := range urls {
			delSet[u] = true
		}
		keptBases := make([]backupObj, 0, len(bases))
		for _, b := range bases {
			if !delSet[b.URL] {
				keptBases = append(keptBases, b)
			}
		}
		keptWALs := make([]walObj, 0, len(wals))
		for _, w := range wals {
			if !delSet[w.URL] {
				keptWALs = append(keptWALs, w)
			}
		}
		d.scrubArchiveHealth(ctx, id, keptBases, keptWALs)
	}
	if len(urls) == 0 {
		scrub()
		return
	}
	if err := gsutilRm(ctx, urls); err != nil {
		d.log.Warn("retention: prune delete failed", "id", id, "err", err)
		return
	}
	scrub()
	d.log.Info("retention: pruned database archive to the retention window",
		"id", id, "window", window,
		"bases_deleted", len(delBases), "bases_kept", len(bases)-len(delBases),
		"wal_deleted", len(delWALs))
}

// purgeOrphanArchive removes the entire archive of a database whose row is gone,
// so a deleted database leaves nothing behind. Guarded so it can never race a
// clone still provisioning from the archive, nor a database mid-provision that
// has not written its row yet.
func (d *databasesAPI) purgeOrphanArchive(ctx context.Context, bucket, id string) {
	if !janitorPurgeEnabled() {
		d.log.Error("archive janitor: ORPHANED ARCHIVE (auto-purge disabled) — purge manually: "+
			"gsutil -m rm -r gs://"+bucket+"/db/"+id, "id", id)
		return
	}
	if inflight, cloneID := d.cloneInFlightFrom(ctx, id); inflight {
		d.log.Info("archive janitor: skip orphan purge — a clone is still provisioning from it",
			"id", id, "clone", cloneID)
		return
	}
	// TUSK T1.4: any unexpired archive lease means someone is reading this
	// chain right now — never purge under it.
	if d.archiveLeaseActive(ctx, id) {
		d.log.Info("archive janitor: skip orphan purge — archive leased", "id", id)
		return
	}
	// TUSK T1.1: snapshot the generation before the slow guards below.
	genBefore, _ := d.currentArchiveGeneration(ctx, id)
	if newest := d.newestArchiveMtime(ctx, bucket, id); newest.IsZero() || time.Since(newest) < orphanPurgeGrace {
		// Too fresh (or unreadable) — a recently-active archive with no row is
		// far more likely a failover/restore in flight than a deletion whose
		// purge failed. Leave it; a genuinely deleted archive ages past the
		// grace and is reclaimed on a later pass.
		d.log.Info("archive janitor: orphan archive within grace — not purging yet",
			"id", id, "grace", orphanPurgeGrace.String())
		return
	}
	// Final safety: re-read the row immediately before the irreversible delete.
	// A failover/restore that recreated the row while we ran the guards must
	// abort the purge — this closes the window the report-only design refused to
	// risk.
	if d.sandboxRowExists(ctx, id) {
		d.log.Info("archive janitor: skip orphan purge — sandbox row reappeared mid-check", "id", id)
		return
	}
	// TUSK T1.1 generation fence: if the archive generation moved while we ran
	// the guards above, a failover/restore claimed this database mid-decision —
	// abort. A stale purge decision must never outrun a live ownership change.
	if genAfter, ok := d.currentArchiveGeneration(ctx, id); ok && genAfter != genBefore {
		d.log.Info("archive janitor: skip orphan purge — archive generation moved mid-check",
			"id", id, "before", genBefore, "after", genAfter)
		return
	}
	d.log.Warn("archive janitor: purging orphaned archive (deleted database left backups behind)",
		"id", id, "orphaned_for", ">7d")
	_ = d.cleanupArchive(ctx, id) // gsutil -m rm -r, retried + loudly logged on failure
}

// newestArchiveMtime returns the most recent timestamp across a database's base
// backups and WAL (zero if the archive is empty/unreadable).
func (d *databasesAPI) newestArchiveMtime(ctx context.Context, bucket, id string) time.Time {
	var newest time.Time
	if bs, err := listBaseBackups(ctx, bucket, id); err == nil {
		for _, b := range bs {
			if b.Timestamp.After(newest) {
				newest = b.Timestamp
			}
		}
	}
	if ws, err := listWALObjects(ctx, bucket, id); err == nil {
		for _, w := range ws {
			if w.Mtime.After(newest) {
				newest = w.Mtime
			}
		}
	}
	return newest
}

// SetupArchiveSchema creates the shared state the archive machinery coordinates
// through. Idempotent; safe to run on every boot.
//
// db_janitor_state    — fleet-wide claim so exactly one edge sweeps per window.
// db_archive_generations / db_archive_leases — fencing (see db_fencing.go), so a
//
//	restored or failed-over database can never have two hosts writing one chain.
//
// db_backup_health    — last observed archive health per database.
func (d *databasesAPI) SetupArchiveSchema(ctx context.Context) error {
	if d.db == nil {
		return nil
	}
	_, err := d.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS db_janitor_state (
	id            INTEGER PRIMARY KEY CHECK (id = 1),
	last_sweep_at TIMESTAMPTZ NOT NULL
);
CREATE TABLE IF NOT EXISTS db_archive_generations (
	db_id      TEXT PRIMARY KEY,
	generation BIGINT NOT NULL DEFAULT 1,
	holder     TEXT NOT NULL DEFAULT '',
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS db_archive_leases (
	db_id      TEXT NOT NULL,
	holder     TEXT NOT NULL,
	purpose    TEXT NOT NULL DEFAULT '',
	expires_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS db_archive_leases_idx ON db_archive_leases (db_id, expires_at);
CREATE TABLE IF NOT EXISTS db_backup_health (
	db_id      TEXT PRIMARY KEY,
	status     TEXT NOT NULL,
	detail     TEXT NOT NULL DEFAULT '',
	checked_at TIMESTAMPTZ NOT NULL
);`)
	return err
}

func (d *databasesAPI) claimJanitorSweep(ctx context.Context, minInterval time.Duration) bool {
	if d.db == nil {
		return true // single-node/dev: no shared state to coordinate through
	}
	var one int
	err := d.db.QueryRowContext(ctx, `
		INSERT INTO db_janitor_state (id, last_sweep_at) VALUES (1, now())
		ON CONFLICT (id) DO UPDATE SET last_sweep_at = now()
		WHERE db_janitor_state.last_sweep_at < now() - make_interval(secs => $1)
		RETURNING 1`, minInterval.Seconds()).Scan(&one)
	if err == nil {
		return true
	}
	if errors.Is(err, sql.ErrNoRows) {
		return false // someone swept (or is sweeping) recently
	}
	// Table missing / transient DB error: err on the side of NOT sweeping —
	// a skipped janitor pass is harmless (next tick retries), a stampede is not.
	d.log.Warn("archive janitor: sweep claim failed — skipping this pass", "err", err)
	return false
}
func lastPathSeg(u string) string {
	if i := strings.LastIndex(u, "/"); i >= 0 {
		return u[i+1:]
	}
	return u
}

// sandboxRowExists reports whether a sandbox row is currently present. Used as
// the final TOCTOU guard before an irreversible orphan-archive purge.
func (d *databasesAPI) sandboxRowExists(ctx context.Context, id string) bool {
	if d.db == nil {
		return false
	}
	var x int
	return d.db.QueryRowContext(ctx, `SELECT 1 FROM sandboxes WHERE id = $1`, id).Scan(&x) == nil
}

// archiveHasRow reports whether a sandbox row still exists for this archive
// prefix. ok=false means the lookup itself failed — callers MUST treat that as
// "unknown" and skip, never as "gone", or a transient database hiccup would
// purge a live customer's backups.
func (d *databasesAPI) archiveHasRow(ctx context.Context, id string) (hasRow, ok bool) {
	if d.db == nil {
		return false, false
	}
	var n int
	if err := d.db.QueryRowContext(ctx,
		`SELECT count(1) FROM sandboxes WHERE id = $1`, id).Scan(&n); err != nil {
		d.log.Warn("archive janitor: row lookup failed", "id", id, "err", err)
		return false, false
	}
	return n > 0, true
}
