// SPDX-License-Identifier: Apache-2.0
//
// db_fencing.go — TUSK T1: archive generations, GC leases, backup-health
// scrubbing, and circuit breakers for the managed-DB archive plane.
//
// The problem class (two real incidents: cross-agent sandbox murder 2026-07,
// edge-janitor stampede 2026-08-20): during failover/restore/redeploy windows,
// more than one actor can briefly believe it owns a database's GCS archive.
// A stale actor that WRITES must never clobber the new owner's data, and a
// stale actor must never DELETE anything. Neon's RFC 025 shape, adapted:
//
//   - db_archive_generations: one monotonic counter per database, bumped
//     transactionally at every ownership change (create / failover / in-place
//     restore / clone-target). The current generation rides the sandbox row
//     metadata (db.archive_gen), so the agent's WAL relay stamps base backups
//     with it (base-<ts>-g<gen>.tar.gz) and restore prefers the highest
//     generation. Writes from a stale generation become ignorable litter.
//   - Uploads are create-only (x-goog-if-generation-match:0 on the agent
//     side), so identical object names are first-writer-wins — a stale host
//     can never overwrite an object the new owner already archived.
//   - db_archive_leases: an in-flight clone/PITR/failover takes a lease on
//     the SOURCE archive; the retention pruner and orphan purge skip leased
//     archives. Closes the "janitor GCs the chain mid-clone" race.
//   - Deletions re-validate: purgeOrphanArchive re-reads the generation right
//     before the irreversible rm — a bump after the purge decision aborts it
//     (someone is restoring the database right now).
//   - db_backup_health: the janitor's listings double as a scrub pass — WAL
//     segment continuity (name math, not mtimes) + base-name sanity — and the
//     verdict is persisted for the dashboard. Integrity as product surface.
package main

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Archive generations
// ---------------------------------------------------------------------------

// seedArchiveGeneration records generation 1 for a fresh database. Idempotent;
// never lowers an existing generation. Fresh-create only — every OTHER caller
// wants bumpArchiveGeneration.
func (d *databasesAPI) seedArchiveGeneration(ctx context.Context, id string) error {
	if d.db == nil {
		return nil
	}
	_, err := d.db.ExecContext(ctx, `
		INSERT INTO db_archive_generations (db_id, generation, holder, updated_at)
		VALUES ($1, 1, 'create', now())
		ON CONFLICT (db_id) DO NOTHING`, id)
	return err
}

// bumpArchiveGeneration atomically increments (creating if absent) the archive
// generation for a database and records who now holds it. Returns the NEW
// generation. Called at every ownership change; the caller must place the
// returned generation into the sandbox row metadata as db.archive_gen so the
// agent-side WAL relay stamps its uploads with it.
//
// The no-row INSERT starts at 2, not 1: a legacy database created before this
// table existed has no row, and its (possibly stale) original host implicitly
// holds the relay default of generation 1 — the first ownership change must
// beat that default, so it lands at 2 whether or not the row was ever seeded.
func (d *databasesAPI) bumpArchiveGeneration(ctx context.Context, id, holder string) (int64, error) {
	if d.db == nil {
		return 1, nil
	}
	var gen int64
	err := d.db.QueryRowContext(ctx, `
		INSERT INTO db_archive_generations (db_id, generation, holder, updated_at)
		VALUES ($1, 2, $2, now())
		ON CONFLICT (db_id) DO UPDATE
		  SET generation = db_archive_generations.generation + 1,
		      holder = $2, updated_at = now()
		RETURNING generation`, id, holder).Scan(&gen)
	if err != nil {
		return 0, err
	}
	return gen, nil
}

// bumpArchiveGenerationRetry is bumpArchiveGeneration with a bounded retry, for
// the destructive ownership-change paths (failover / in-place restore) where a
// transient control-plane blip must NOT be allowed to proceed "unfenced". An
// unfenced ownership change reopens the split-brain window the fence exists to
// close: both the new owner and a still-alive old host default to generation 1,
// base selection falls to timestamp order, and a zombie's later base can win —
// silent restore onto an abandoned timeline. So the caller treats a persistent
// failure as fatal to the operation (which stays retryable) rather than racing
// on without a fence. Each attempt gets its own short timeout so one wedged
// connection can't consume the whole budget.
func (d *databasesAPI) bumpArchiveGenerationRetry(ctx context.Context, id, holder string) (int64, error) {
	if d.db == nil {
		return 1, nil
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(time.Duration(attempt) * 500 * time.Millisecond):
			case <-ctx.Done():
				return 0, ctx.Err()
			}
		}
		actx, acancel := context.WithTimeout(ctx, 5*time.Second)
		gen, err := d.bumpArchiveGeneration(actx, id, holder)
		acancel()
		if err == nil {
			return gen, nil
		}
		lastErr = err
	}
	return 0, lastErr
}

// currentArchiveGeneration reads the recorded generation (0, false = no row).
func (d *databasesAPI) currentArchiveGeneration(ctx context.Context, id string) (int64, bool) {
	if d.db == nil {
		return 0, false
	}
	var gen int64
	if err := d.db.QueryRowContext(ctx,
		`SELECT generation FROM db_archive_generations WHERE db_id = $1`, id).Scan(&gen); err != nil {
		return 0, false
	}
	return gen, true
}

// ---------------------------------------------------------------------------
// Archive GC leases
// ---------------------------------------------------------------------------

// acquireArchiveLease records that holder is reading id's archive chain for
// purpose (clone/pitr/failover/restore) for ttl. Multiple concurrent leases
// are fine — the pruner skips while ANY unexpired lease exists. Errors are
// returned but callers may treat them as non-fatal (a missing lease degrades
// to the pre-T1.4 behavior, it does not block the user's operation).
func (d *databasesAPI) acquireArchiveLease(ctx context.Context, id, holder, purpose string, ttl time.Duration) error {
	if d.db == nil {
		return nil
	}
	_, err := d.db.ExecContext(ctx, `
		INSERT INTO db_archive_leases (db_id, holder, purpose, expires_at)
		VALUES ($1, $2, $3, now() + make_interval(secs => $4))`,
		id, holder, purpose, ttl.Seconds())
	return err
}

// releaseArchiveLease drops holder's leases on id. Best-effort — an unreleased
// lease simply expires.
func (d *databasesAPI) releaseArchiveLease(ctx context.Context, id, holder string) {
	if d.db == nil {
		return
	}
	_, _ = d.db.ExecContext(ctx,
		`DELETE FROM db_archive_leases WHERE db_id = $1 AND holder = $2`, id, holder)
}

// archiveLeaseActive reports whether any unexpired lease exists for id.
// Fails CLOSED for destructive callers: a transient DB error reads as "leased"
// so the pruner skips this pass rather than risk GC'ing a chain mid-clone.
func (d *databasesAPI) archiveLeaseActive(ctx context.Context, id string) bool {
	if d.db == nil {
		return false
	}
	var one int
	err := d.db.QueryRowContext(ctx,
		`SELECT 1 FROM db_archive_leases WHERE db_id = $1 AND expires_at > now() LIMIT 1`, id).Scan(&one)
	if err == nil {
		return true
	}
	if strings.Contains(err.Error(), "no rows") {
		return false
	}
	d.log.Warn("archive lease check failed (failing closed: treating as leased)", "id", id, "err", err)
	return true
}

// expireArchiveLeases GCs long-expired lease rows (janitor housekeeping).
func (d *databasesAPI) expireArchiveLeases(ctx context.Context) {
	if d.db == nil {
		return
	}
	_, _ = d.db.ExecContext(ctx,
		`DELETE FROM db_archive_leases WHERE expires_at < now() - interval '1 hour'`)
}

// ---------------------------------------------------------------------------
// Backup-health scrub (T1.5)
// ---------------------------------------------------------------------------

// countWALGaps parses WAL object names and reports (gaps, segments) within the
// HIGHEST timeline present. Only full segments participate; .partial uploads,
// .history files and .backup labels are skipped. Gap math follows Postgres
// segment numbering for 16 MiB segments: within a log, seg runs 0x00..0xFF;
// after (log, 0xFF) comes (log+1, 0x00). Lower timelines are ignored — after a
// promote the new TLI is the live chain, and retention legitimately erodes the
// old one.
// isFullWALSegment reports whether name is a complete 16 MiB WAL segment object
// (a 24-char all-uppercase-hex name: TLI+log+seg). Excludes .partial uploads,
// .history files and .backup labels — the objects that are NOT replayable
// segments and must not inflate the catalog's wal_count.
func isFullWALSegment(name string) bool {
	name = strings.TrimSpace(name)
	if len(name) != 24 {
		return false
	}
	for _, c := range name {
		if !strings.ContainsRune("0123456789ABCDEF", c) {
			return false
		}
	}
	return true
}

func countWALGaps(names []string) (gaps, segments int) {
	type segID struct{ log, seg uint64 }
	byTLI := map[uint64][]segID{}
	for _, n := range names {
		n = strings.TrimSpace(n)
		if len(n) != 24 {
			continue
		}
		ok := true
		for _, c := range n {
			if !strings.ContainsRune("0123456789ABCDEF", c) {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		tli, err1 := strconv.ParseUint(n[0:8], 16, 64)
		lg, err2 := strconv.ParseUint(n[8:16], 16, 64)
		sg, err3 := strconv.ParseUint(n[16:24], 16, 64)
		if err1 != nil || err2 != nil || err3 != nil {
			continue
		}
		byTLI[tli] = append(byTLI[tli], segID{lg, sg})
	}
	if len(byTLI) == 0 {
		return 0, 0
	}
	var maxTLI uint64
	for tli := range byTLI {
		if tli > maxTLI {
			maxTLI = tli
		}
	}
	segs := byTLI[maxTLI]
	sort.Slice(segs, func(i, j int) bool {
		if segs[i].log != segs[j].log {
			return segs[i].log < segs[j].log
		}
		return segs[i].seg < segs[j].seg
	})
	const segsPerLog = 0x100 // 16 MiB wal_segment_size
	for i := 1; i < len(segs); i++ {
		prev := segs[i-1].log*segsPerLog + segs[i-1].seg
		cur := segs[i].log*segsPerLog + segs[i].seg
		if cur == prev {
			continue // duplicate listing
		}
		if cur != prev+1 {
			gaps += int(cur - prev - 1)
		}
	}
	return gaps, len(segs)
}

// selectPartialsToDelete returns the WAL-prefix partial uploads that are now
// superseded and safe to delete: (a) every partial whose FULL segment exists
// in the archive (the prefix is a strict subset of it), and (b) all but the
// best partial per segment (highest generation, then largest offset — the
// same precedence the relay serves with). Names arrive as bare object names.
func selectPartialsToDelete(names []string) []string {
	type pinfo struct {
		name string
		off  int64
		gen  int64
	}
	full := map[string]bool{}
	partials := map[string][]pinfo{}
	for _, n := range names {
		if i := strings.Index(n, ".partial-"); i > 0 {
			seg := n[:i]
			rest := n[i+len(".partial-"):]
			var gen int64
			if j := strings.Index(rest, "-g"); j >= 0 {
				gen, _ = strconv.ParseInt(rest[j+2:], 10, 64)
				rest = rest[:j]
			}
			off, err := strconv.ParseInt(rest, 10, 64)
			if err != nil || off <= 0 {
				continue
			}
			partials[seg] = append(partials[seg], pinfo{name: n, off: off, gen: gen})
			continue
		}
		if len(n) == 24 {
			full[n] = true
		}
	}
	var del []string
	for seg, ps := range partials {
		if full[seg] {
			for _, p := range ps {
				del = append(del, p.name)
			}
			continue
		}
		best := 0
		for i := 1; i < len(ps); i++ {
			if ps[i].gen > ps[best].gen || (ps[i].gen == ps[best].gen && ps[i].off > ps[best].off) {
				best = i
			}
		}
		for i, p := range ps {
			if i != best {
				del = append(del, p.name)
			}
		}
	}
	sort.Strings(del)
	return del
}

// scrubArchiveHealth turns the janitor's existing listings into a persisted
// per-DB integrity verdict. Zero extra GCS calls — it runs on the same
// bases/wals the retention pass already fetched.
func (d *databasesAPI) scrubArchiveHealth(ctx context.Context, id string, bases []backupObj, wals []walObj) {
	if d.db == nil {
		return
	}
	status, detail := "ok", ""
	names := make([]string, 0, len(wals))
	for _, w := range wals {
		names = append(names, lastPathSeg(w.URL))
	}
	gaps, segs := countWALGaps(names)
	switch {
	case len(bases) == 0:
		status, detail = "error", "no base backups in archive"
	case gaps > 0:
		status = "error"
		detail = fmt.Sprintf("WAL gap detected: %d missing segment(s) among %d on the live timeline — PITR may fail across the gap", gaps, segs)
	default:
		zero := 0
		for _, b := range bases {
			if b.SizeBytes == 0 {
				zero++
			}
		}
		if zero > 0 {
			status = "warn"
			detail = fmt.Sprintf("%d base backup(s) have zero size", zero)
		}
	}
	if _, err := d.db.ExecContext(ctx, `
		INSERT INTO db_backup_health (db_id, status, detail, checked_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (db_id) DO UPDATE
		  SET status = $2, detail = $3, checked_at = now()`,
		id, status, detail); err != nil {
		d.log.Warn("backup health persist failed", "id", id, "err", err)
	}
	if status != "ok" {
		d.log.Warn("backup health scrub flagged archive", "id", id, "status", status, "detail", detail)
	}
}

// readBackupHealth returns the persisted verdict for the backups endpoint.
func (d *databasesAPI) readBackupHealth(ctx context.Context, id string) (status, detail string, checkedAt time.Time, ok bool) {
	if d.db == nil {
		return "", "", time.Time{}, false
	}
	if err := d.db.QueryRowContext(ctx,
		`SELECT status, detail, checked_at FROM db_backup_health WHERE db_id = $1`, id).
		Scan(&status, &detail, &checkedAt); err != nil {
		return "", "", time.Time{}, false
	}
	return status, detail, checkedAt, true
}

// ---------------------------------------------------------------------------
// Circuit breakers (T1.6)
// ---------------------------------------------------------------------------

// loopBreaker is a tiny consecutive-failure circuit breaker for platform
// background loops (janitor sweep, catalog refresh, scrub). After `trip`
// consecutive failures the loop is parked for `park` and an ERROR is logged
// once; any success resets. Modeled on Neon's compaction circuit breaker —
// the point is to stop a wedged loop from hammering shared infrastructure
// (the 2026-08-20 stampede class) while staying loud about it.
type loopBreaker struct {
	name string
	trip int
	park time.Duration

	mu          sync.Mutex
	fails       int
	parkedUntil time.Time
	tripped     bool
}

func newLoopBreaker(name string, trip int, park time.Duration) *loopBreaker {
	return &loopBreaker{name: name, trip: trip, park: park}
}

// Allow reports whether the loop may run now.
func (b *loopBreaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return time.Now().After(b.parkedUntil)
}

// Success resets the failure streak, un-trips, and un-parks.
func (b *loopBreaker) Success() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.fails = 0
	b.tripped = false
	b.parkedUntil = time.Time{}
}

// Fail records a failure; on the trip'th consecutive failure the breaker parks
// the loop and returns true exactly once (the caller logs the alert).
func (b *loopBreaker) Fail() (justTripped bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.fails++
	if b.fails >= b.trip && !b.tripped {
		b.tripped = true
		b.parkedUntil = time.Now().Add(b.park)
		return true
	}
	if b.tripped {
		b.parkedUntil = time.Now().Add(b.park)
	}
	return false
}
