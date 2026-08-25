// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Database failover restore (managed-DB roadmap item 4).
//
// When the agent hosting a managed database dies, the control plane picks a
// healthy agent and calls POST /db/{id}/restore on it. This rebuilds the
// database's durable volume from its GCS archive (latest base backup +
// archived WAL, both written by the WAL relay) and boots it under the SAME
// sandbox ID, so the lease upsert atomically retargets <id>.db.pandastack.ai
// and the GCS archive prefix keeps accumulating under one key.
//
// Recovery mechanics: the volume is staged with recovery.signal +
// restore_command so postgres replays archived WAL past the base backup and
// then promotes on a new timeline. Credentials need no special handling —
// every restore rotates the password (kickPGPhase2) and the control plane
// reads it live from the guest.

// dbRestoreWALRestoreConf is appended to the restored cluster's
// postgresql.conf. pandastack-wal-restore reads /etc/pandastack/wal.env
// (injected by kickPGPhase2 before postgres starts) and fetches segments
// back through the host relay's GET /wal endpoint.
const dbRestoreWALRestoreConf = "\n# PandaStack failover recovery\n" +
	"restore_command = '/usr/local/bin/pandastack-wal-restore %f %p'\n" +
	"recovery_target_timeline = 'latest'\n"

// RestoreDatabase rebuilds managed database id on THIS host and starts it
// under its original sandbox ID. Idempotent inputs: if the durable volume is
// already present locally (e.g. failing back to a host that still has it),
// the archive download is skipped and the existing volume boots as-is.
//
// template preserves the database's RAM tier across failover: explicit arg >
// the stale row's template > pgManagedTemplate. Anything outside the managed
// family is refused — a failover must never move a database onto an
// unmanaged template.
// RestoreOptions distinguishes an in-place point-in-time restore of a HEALTHY
// database (InPlace) from a failover rebuild of a dead one (the zero value).
type RestoreOptions struct {
	// PreserveCreds re-injects the database's existing password + broker token
	// so its connection string is unchanged. nil rotates to fresh credentials.
	PreserveCreds *DBCreds
	// InPlace means the database is running on THIS host and the caller wants to
	// overwrite it: take a pre-restore safety backup, stop it, and rebuild the
	// volume from the archive. The zero value is the failover path (db is dead
	// elsewhere; never pre-backs up, reuses a local volume if present).
	InPlace bool
}

func (m *Manager) RestoreDatabase(ctx context.Context, id, template string, metadata map[string]string, opts *RestoreOptions) (*Sandbox, error) {
	if opts == nil {
		opts = &RestoreOptions{}
	}
	if _, err := uuid.Parse(id); err != nil {
		return nil, fmt.Errorf("db restore: invalid database id %q", id)
	}
	// Serialize this destructive rebuild against every other lifecycle op for
	// this id — a second restore dispatched by a DIFFERENT API replica (the
	// control-plane failoverInFlight guard is per-replica, and this feature is
	// multi-node), an auto-suspend hibernate, or an auto-wake. Without an
	// agent-owned lock two concurrent in-place restores would race baseBackupOne,
	// teardown, os.RemoveAll(vol), the volume rename, and two Creates for one
	// pinned id. Held for the whole restore; the driver checks below must be
	// inside it so a hibernate can't null the driver between check and use.
	unlock := m.lifecycleLock(id)
	defer unlock()
	if !opts.InPlace && m.driver(id) != nil {
		return nil, fmt.Errorf("db restore: database %s is already running on this host", id)
	}
	// Backfill identity from the existing row when the caller under-specifies it
	// (defense-in-depth for a direct agent call). The template pins the RAM tier;
	// metadata.workspace drives tenancy (every id-scoped route 404s without it)
	// AND the backup-retention tier (a lost workspace would let the janitor prune
	// a paid database's backups as free-tier). The control-plane restore/failover
	// handlers already send both — this keeps a bare agent call from rebuilding
	// the row stripped of its workspace.
	needTemplate := template == ""
	needWorkspace := metadata == nil || metadata["workspace"] == ""
	if needTemplate || needWorkspace {
		if row, err := m.store.GetSandbox(ctx, id); err == nil {
			if rmap, _ := row.(map[string]any); rmap != nil {
				if needTemplate {
					if tpl, _ := rmap["template"].(string); isPGManagedTemplate(tpl) {
						template = tpl
					}
				}
				if needWorkspace {
					if md, _ := rmap["metadata"].(map[string]string); md != nil && md["workspace"] != "" {
						if metadata == nil {
							metadata = map[string]string{}
						}
						metadata["workspace"] = md["workspace"]
					}
				}
			}
		}
	}
	if template == "" {
		template = pgManagedTemplate
	}
	if !isPGManagedTemplate(template) {
		return nil, fmt.Errorf("db restore: %q is not a managed database template", template)
	}
	// Both restore paths stage from the database's OWN archive — clone-era
	// recovery context must not survive (a stale db.recover_from would otherwise
	// ride along in the row forever). Restore always rolls forward to the latest
	// archived WAL, so any recorded recovery target is cleared too.
	if metadata != nil {
		delete(metadata, "db.recover_from")
		delete(metadata, "db.recovery_target_time")
	}

	vol := m.dbVolumePath(id)
	if opts.InPlace {
		// In-place restore OVERWRITES the live volume, so it is only safe on the
		// host that currently RUNS the database — the one place we can take the
		// mandatory pre-restore safety backup. If there is no active driver the
		// database is not live here (wrong host from a stale lease cache,
		// hibernated by auto-suspend, or crashed): ABORT rather than destroy a
		// volume we cannot back up. This is the guard that makes destruction
		// strictly conditional on a successful backup — and it also closes the
		// split-brain window, since a restore landing on a non-owner host (whose
		// driver is nil) can no longer retarget the lease out from under the real
		// primary.
		if m.driver(id) == nil {
			return nil, fmt.Errorf("db restore: in-place restore requires the database running on this host (no active driver for %s); refusing to destroy data with no safety backup", id)
		}
		if m.walRelay == nil {
			return nil, errors.New("db restore: in-place restore needs the WAL relay (backups) enabled")
		}
		// Fail CLEAN before any destruction: resolve the base backup (read-only)
		// up front, so an empty or unreachable archive aborts here instead of
		// after the volume is already gone (destroy-then-fail). Mirrors
		// failover's preflight-then-act.
		if _, verr := m.resolveBaseForTarget(ctx, id); verr != nil {
			return nil, verr
		}
		// Take a fresh base backup of the CURRENT state FIRST, so this restore is
		// itself undoable. Abort if it fails — never destroy data we could not
		// back up. (Note: baseBackupOne is durable to the WAL relay spool on
		// return; the async uploader flushes it to GCS shortly after.)
		if err := m.walRelay.baseBackupOne(ctx, id); err != nil {
			return nil, fmt.Errorf("db restore: pre-restore safety backup failed, aborting to protect current data: %w", err)
		}
		m.log.Info("db restore: pre-restore safety backup taken; tearing down current database", "id", id)
		// Stop the running VM and drop its volume so we rebuild from archive.
		m.teardownResourcesOpts(ctx, id, false /*keepDurableVolume*/)
		_ = os.RemoveAll(vol)
		if err := m.buildDBVolumeFromSource(ctx, id, vol, ""); err != nil {
			// The volume is already gone. Mark the still-present row FAILED so the
			// database stays visible + retryable (via a force failover) rather
			// than stuck at a stale "running" status with no VM. Its GCS archive
			// (including the safety backup just taken) keeps the data recoverable.
			if serr := m.store.SetStatus(ctx, id, string(StatusFailed)); serr != nil {
				m.log.Error("db restore: mark failed after volume-build error", "id", id, "err", serr)
			}
			return nil, err
		}
	} else if _, err := os.Stat(vol); err == nil {
		m.log.Info("db restore: reusing existing local volume", "id", id, "path", vol)
	} else {
		if err := m.buildDBVolumeFromSource(ctx, id, vol, ""); err != nil {
			return nil, err
		}
	}

	// Re-inject the existing credentials on the coming boot (in-place restore),
	// so the connection string is unchanged. kickPGPhase2 consumes this once.
	if opts.PreserveCreds != nil {
		m.setPendingPGCreds(id, opts.PreserveCreds)
	}
	// Clear any stale sandbox row from the database's previous life (shared
	// Postgres store in multi-node: the dead agent's row still exists, and
	// InsertSandbox is a plain INSERT that would conflict on the pinned ID).
	if err := m.store.DeleteSandbox(ctx, id); err != nil {
		m.log.Warn("db restore: stale sandbox row cleanup failed (continuing)", "id", id, "err", err)
	}
	// This host is starting a NEW serving epoch for id (failover or in-place
	// restore). The control plane bumped db.archive_gen and stamped it into
	// `metadata` above; re-pin the relay's cached generation from it so every
	// base/WAL this epoch uploads carries the new generation. Without this the
	// relay would keep stamping the pre-restore generation for the life of the
	// process (pinned-at-epoch), and a same-host in-place restore (M1) would
	// silently keep writing the old generation. A zombie old host never runs
	// this path, so it keeps its stale generation and loses base selection (C1).
	if m.walRelay != nil {
		m.walRelay.InvalidateGen(id)
	}
	// Create on the normal path: ensureDBVolume sees the staged image and
	// reuses it; kickPGPhase2 injects wal.env + fresh credentials; the lease
	// upsert retargets routing to this agent.
	sb, err := m.Create(ctx, CreateRequest{
		ID:         id,
		Template:   template,
		Persistent: true,
		Metadata:   metadata,
	})
	if err != nil {
		// Drain any preserved credentials we stashed for kickPGPhase2 but that
		// a failed boot never consumed. Left behind, they would leak into the
		// NEXT boot of this id (e.g. a later failover, which must ROTATE the
		// password) and silently re-inject the stale credential. takePendingPGCreds
		// is a get-and-delete, so this is a safe no-op when already consumed.
		if opts.PreserveCreds != nil {
			m.takePendingPGCreds(id)
		}
		// Re-insert a FAILED row: the stale row was deleted above and Create's
		// own cleanup removes any partial row, so without this a failed
		// restore leaves the database with NO row at all — invisible to
		// GET/failover retries, and (worse) indistinguishable from a deleted
		// database, which is exactly the state archive-cleanup logic treats
		// as purgeable. A failed row keeps the database visible, retryable,
		// and its GCS archive protected.
		failedRow := &Sandbox{
			ID:        id,
			Template:  template,
			Status:    StatusFailed,
			Metadata:  metadata,
			CreatedAt: time.Now().UTC(),
		}
		if ierr := m.store.InsertSandbox(ctx, failedRow); ierr != nil {
			m.log.Error("db restore: failed-row re-insert errored — database row is MISSING",
				"id", id, "err", ierr)
		}
		return nil, fmt.Errorf("db restore: boot restored database: %w", err)
	}
	m.log.Info("db restore: database restored and booting", "id", id)
	return sb, nil
}

// dbBaseNameTimeLayout is the UTC timestamp embedded in base backup object
// names (base-<ts>.tar.gz) by baseBackupOne. Lexicographic order == time
// order, and base selection parses it back out.
const dbBaseNameTimeLayout = "20060102T150405Z"

// stripPandaStackRecoveryConf removes every previously-appended PandaStack
// recovery line from a postgresql.conf. Base backups capture the whole
// PGDATA — including conf blocks appended by PAST restores/clones — so a
// staged volume must never trust inherited recovery config: a stale
// recovery_target_time would either silently pin recovery to an ancient
// instant or FATAL postgres ("stop point before consistent recovery point"),
// and a stale restore_command would mask the fresh one below it. The review
// found both failure modes live; sanitize-then-append is the fix.
func stripPandaStackRecoveryConf(conf []byte) []byte {
	var out []string
	for _, ln := range strings.Split(string(conf), "\n") {
		t := strings.TrimSpace(ln)
		if t == "# PandaStack failover recovery" ||
			strings.HasPrefix(t, "restore_command") ||
			strings.HasPrefix(t, "recovery_target_timeline") ||
			strings.HasPrefix(t, "recovery_target_time") ||
			strings.HasPrefix(t, "recovery_target_action") {
			continue
		}
		out = append(out, ln)
	}
	return []byte(strings.Join(out, "\n"))
}

// buildDBVolumeFromSource stages a fresh durable volume at vol from
// gs://$PANDASTACK_SNAPSHOT_BUCKET/db/{srcID}/: a base backup extracted into
// /pgdata plus recovery.signal + restore_command so the guest replays
// archived WAL on first boot. The image is built at a temp path and only
// renamed into place when complete, so a crashed restore never leaves a
// half-staged volume that the corruption guard in autostart.sh would trust.
//
// Restore always rolls forward to the newest archived WAL: srcID is the
// database's own id and cloneSrcToken is "", so the guest replays ALL archived
// WAL via the baked pandastack-wal-restore script (wal.env carries the DB's own
// id). The cloneSrcToken parameter stays in the signature for the alternate
// shape where the WAL of a DIFFERENT source is replayed through an inline
// restore_command; nothing in this build passes it.
//
// resolveBaseForTarget returns the single BEST base object to restore from —
// the head of resolveBaseCandidates. Kept as the read-only preflight the
// destructive in-place path runs to VALIDATE the archive before tearing
// anything down (it only needs to know a usable base EXISTS). The actual build uses
// resolveBaseCandidates so it can fall past a corrupt/truncated head base.
func (m *Manager) resolveBaseForTarget(ctx context.Context, srcID string) (string, error) {
	cands, err := m.resolveBaseCandidates(ctx, srcID)
	if err != nil {
		return "", err
	}
	return cands[0], nil
}

// resolveBaseCandidates lists the base backups under db/{srcID}/base/ and returns
// the GCS objects to restore from IN PRIORITY ORDER: every base sorted
// (generation DESC, timestamp DESC), so recovery always rolls forward from the
// newest base of the current epoch. The caller tries them head-first and falls
// to the next when one is unreadable/truncated, so a single corrupt base object
// cannot permanently shadow the good older ones. Strictly READ-ONLY. A non-nil
// error means nothing can be restored: no bucket configured, or no base backups.
func (m *Manager) resolveBaseCandidates(ctx context.Context, srcID string) ([]string, error) {
	bucket := strings.TrimSpace(os.Getenv("PANDASTACK_SNAPSHOT_BUCKET"))
	if bucket == "" {
		return nil, errors.New("db restore: PANDASTACK_SNAPSHOT_BUCKET not set — no archive to restore from")
	}
	prefix := "gs://" + bucket + "/db/" + srcID

	// Base backups: names embed a UTC timestamp and an
	// archive generation: base-<ts>[-g<gen>].tar.gz. Selection is by
	// (generation DESC, timestamp DESC): the highest generation is the chain
	// written by the CURRENT owner epoch — a stale host's base with a newer
	// wall-clock timestamp but an older generation belongs to an abandoned
	// timeline and must lose. Pre-fencing names parse as generation 0.
	lctx, lcancel := context.WithTimeout(ctx, 2*time.Minute)
	out, lerr := exec.CommandContext(lctx, "gsutil", "ls", prefix+"/base/").CombinedOutput()
	lcancel()
	if lerr != nil {
		return nil, fmt.Errorf("db restore: list base backups for %s: %v: %s", srcID, lerr, strings.TrimSpace(string(out)))
	}
	type baseCand struct {
		url string
		gen int64
		ts  time.Time
	}
	var bases []baseCand
	for _, ln := range strings.Split(string(out), "\n") {
		ln = strings.TrimSpace(ln)
		name := ln[strings.LastIndex(ln, "/")+1:]
		m := agentBaseNameRe.FindStringSubmatch(name)
		if m == nil {
			continue
		}
		bt, perr := time.Parse(dbBaseNameTimeLayout, m[1])
		if perr != nil {
			continue
		}
		var gen int64
		if m[2] != "" {
			gen, _ = strconv.ParseInt(m[2], 10, 64)
		}
		bases = append(bases, baseCand{url: ln, gen: gen, ts: bt})
	}
	if len(bases) == 0 {
		return nil, fmt.Errorf("db restore: no base backups under %s/base/", prefix)
	}
	sort.Slice(bases, func(i, j int) bool {
		if bases[i].gen != bases[j].gen {
			return bases[i].gen > bases[j].gen // highest generation first
		}
		return bases[i].ts.After(bases[j].ts) // then newest
	})
	urls := make([]string, len(bases))
	for i, b := range bases {
		urls[i] = b.url
	}
	return urls, nil
}

// agentBaseNameRe accepts both name generations: base-<stamp>.tar.gz and
// base-<stamp>-g<gen>.tar.gz (mirrors the API's baseNameRe).
var agentBaseNameRe = regexp.MustCompile(`^base-(\d{8}T\d{6}Z)(?:-g(\d+))?\.tar\.gz$`)

func (m *Manager) buildDBVolumeFromSource(ctx context.Context, srcID, vol, cloneSrcToken string) (err error) {
	// Resolve (read-only) the base backups to restore from, best first. Callers
	// on the destructive in-place path run the single-best preflight up front to
	// fail clean before touching the live volume, so an unsatisfiable target
	// never reaches here after a teardown. Here we keep the FULL candidate list
	// so a corrupt/truncated head base can be skipped for the next good one
	// — otherwise one bad object (e.g. a base whose pg_basebackup died mid-stream
	// and shipped a truncated tarball to an immutable, uniquely-named object)
	// would permanently shadow every good older base and block recovery.
	candidates, err := m.resolveBaseCandidates(ctx, srcID)
	if err != nil {
		return err
	}
	var out []byte // reused by the exec calls below

	// Stage the rebuilt volume on the SAME filesystem as its final location so
	// the os.Rename below is an atomic same-fs move. volumes/ is frequently a
	// SEPARATE mount (e.g. a dedicated data disk) from DataDir — staging under
	// DataDir would make the final rename a cross-device EXDEV failure
	// ("invalid cross-device link"), which is exactly the bug this guards
	// against. filepath.Dir(vol) is volumes/db/, on the volume disk.
	volParent := filepath.Dir(vol)
	if err := os.MkdirAll(volParent, 0o755); err != nil {
		return fmt.Errorf("db restore: volume dir: %w", err)
	}
	work, err := os.MkdirTemp(volParent, ".db-restore-")
	if err != nil {
		return fmt.Errorf("db restore: workdir: %w", err)
	}
	mnt := filepath.Join(work, "mnt")
	img := filepath.Join(work, "volume.ext4")
	mounted := false
	defer func() {
		if mounted {
			_ = exec.Command("umount", mnt).Run()
		}
		_ = os.RemoveAll(work)
	}()

	// Download candidates head-first; the first that passes an end-to-end gzip
	// integrity check wins. `gzip -t` decompresses the whole stream and verifies
	// the trailing CRC/length, so a tarball truncated by a died-mid-stream
	// pg_basebackup (the H2 hazard) fails here and we fall to the next base
	// instead of tarring garbage into the volume. A base that lists but 404s on
	// download is likewise skipped.
	tarPath := filepath.Join(work, "base.tar.gz")
	var baseObj string
	var lastErr error
	for _, cand := range candidates {
		_ = os.Remove(tarPath) // clear any partial from a previous failed candidate
		m.log.Info("db restore: downloading base backup", "src", srcID, "object", cand)
		dctx, dcancel := context.WithTimeout(ctx, 30*time.Minute)
		out, err = exec.CommandContext(dctx, "gsutil", "-q", "cp", cand, tarPath).CombinedOutput()
		dcancel()
		if err != nil {
			lastErr = fmt.Errorf("db restore: download %s: %v: %s", cand, err, strings.TrimSpace(string(out)))
			m.log.Warn("db restore: base download failed, trying older base", "src", srcID, "object", cand, "err", lastErr)
			continue
		}
		gctx, gcancel := context.WithTimeout(ctx, 10*time.Minute)
		out, err = exec.CommandContext(gctx, "gzip", "-t", tarPath).CombinedOutput()
		gcancel()
		if err != nil {
			lastErr = fmt.Errorf("db restore: base %s failed integrity check: %v: %s", cand, err, strings.TrimSpace(string(out)))
			m.log.Warn("db restore: base is corrupt/truncated, trying older base", "src", srcID, "object", cand, "err", lastErr)
			continue
		}
		baseObj = cand
		break
	}
	if baseObj == "" {
		if lastErr != nil {
			return fmt.Errorf("db restore: no usable base backup for %s among %d candidate(s): %w", srcID, len(candidates), lastErr)
		}
		return fmt.Errorf("db restore: no usable base backup for %s", srcID)
	}
	st, err := os.Stat(tarPath)
	if err != nil {
		return fmt.Errorf("db restore: stat base backup: %w", err)
	}

	// Size like ensureDBVolume's placeholder, but leave real headroom over
	// the (compressed) backup; the volume auto-grow sweep takes it from there.
	sizeBytes := int64(pgDataPlaceholderGB) << 30
	if want := st.Size()*4 + (1 << 30); want > sizeBytes {
		sizeBytes = want
	}
	if err := makeSparseImage(img, sizeBytes); err != nil {
		return fmt.Errorf("db restore: create volume image: %w", err)
	}
	// Same filesystem/label autostart.sh would create on a blank device.
	fctx, fcancel := context.WithTimeout(ctx, 5*time.Minute)
	out, err = exec.CommandContext(fctx, "mkfs.ext4", "-F", "-q", "-L", "pgdata", img).CombinedOutput()
	fcancel()
	if err != nil {
		return fmt.Errorf("db restore: mkfs.ext4: %v: %s", err, strings.TrimSpace(string(out)))
	}
	if err := os.MkdirAll(mnt, 0o700); err != nil {
		return fmt.Errorf("db restore: mountpoint: %w", err)
	}
	if out, err = exec.CommandContext(ctx, "mount", "-o", "loop", img, mnt).CombinedOutput(); err != nil {
		return fmt.Errorf("db restore: loop mount: %v: %s", err, strings.TrimSpace(string(out)))
	}
	mounted = true

	// pg_basebackup -Ft -X fetch tar = the full PGDATA contents (including
	// pg_wal and backup_label, which recovery requires; postmaster.pid is
	// excluded by postgres, removed again below just in case). Ownership is
	// normalised by autostart.sh (chown -R postgres) inside the guest.
	pgdata := filepath.Join(mnt, "pgdata")
	if err := os.MkdirAll(pgdata, 0o700); err != nil {
		return fmt.Errorf("db restore: pgdata dir: %w", err)
	}
	xctx, xcancel := context.WithTimeout(ctx, 30*time.Minute)
	out, err = exec.CommandContext(xctx, "tar", "--numeric-owner", "-xzf", tarPath, "-C", pgdata).CombinedOutput()
	xcancel()
	if err != nil {
		return fmt.Errorf("db restore: extract base backup: %v: %s", err, strings.TrimSpace(string(out)))
	}
	_ = os.Remove(filepath.Join(pgdata, "postmaster.pid"))

	// Arm archive recovery: replay archived WAL past the base backup, then
	// promote on a new timeline. For PITR, stop replay at the target instead
	// — recovery_target_action must be 'promote' (the default 'pause' would
	// park the clone in recovery forever, and autostart's readiness wait
	// would never see pg_is_in_recovery()=f).
	if err := os.WriteFile(filepath.Join(pgdata, "recovery.signal"), nil, 0o600); err != nil {
		return fmt.Errorf("db restore: recovery.signal: %w", err)
	}
	confPath := filepath.Join(pgdata, "postgresql.conf")
	conf, err := os.ReadFile(confPath)
	if err != nil {
		return fmt.Errorf("db restore: read postgresql.conf: %w", err)
	}
	// SANITIZE then append — never skip. The base backup's conf can carry
	// recovery blocks from past restores/clones of the source lineage;
	// trusting them silently ignores the PITR target or replays with a
	// stale one (see stripPandaStackRecoveryConf).
	conf = stripPandaStackRecoveryConf(conf)
	recoveryConf := dbRestoreWALRestoreConf
	if cloneSrcToken != "" {
		// Clone: inline fetch of the SOURCE's archive through this host's
		// relay. wal.env (injected at phase 2) supplies only the relay URL;
		// the source id + per-id token are baked here (hex/base64url —
		// conf-quote safe). The leading wait closes a real race: autostart
		// gives up waiting for creds after 60s and starts postgres anyway,
		// and a restore_command that ran before wal.env landed would exit
		// nonzero — which postgres reads as END OF ARCHIVE, silently
		// truncating the clone at base-backup state. Blocking here until
		// phase 2 delivers the env (bounded 120s) makes that impossible.
		recoveryConf = "\n# PandaStack failover recovery\n" +
			fmt.Sprintf("restore_command = 'for i in $(seq 1 120); do [ -f /etc/pandastack/wal.env ] && break; sleep 1; done; "+
				". /etc/pandastack/wal.env && curl -fsS --max-time 120 --retry 5 --retry-connrefused"+
				" -H \"Authorization: Bearer %s\" -o \"%%p\" \"$PANDASTACK_WAL_URL/wal/%s/%%f\"'\n", cloneSrcToken, srcID) +
			"recovery_target_timeline = 'latest'\n"
	}
	if err := os.WriteFile(confPath, append(conf, []byte(recoveryConf)...), 0o600); err != nil {
		return fmt.Errorf("db restore: append restore_command: %w", err)
	}

	if out, err = exec.Command("umount", mnt).CombinedOutput(); err != nil {
		return fmt.Errorf("db restore: umount: %v: %s", err, strings.TrimSpace(string(out)))
	}
	mounted = false

	if err := os.MkdirAll(filepath.Dir(vol), 0o755); err != nil {
		return fmt.Errorf("db restore: volume dir: %w", err)
	}
	if err := os.Rename(img, vol); err != nil {
		return fmt.Errorf("db restore: move volume into place: %w", err)
	}
	m.log.Info("db restore: volume staged from archive",
		"src", srcID, "base", baseObj, "size_bytes", sizeBytes, "path", vol)
	return nil
}
