// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
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
func (m *Manager) RestoreDatabase(ctx context.Context, id string, metadata map[string]string) (*Sandbox, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, fmt.Errorf("db restore: invalid database id %q", id)
	}
	if m.driver(id) != nil {
		return nil, fmt.Errorf("db restore: database %s is already running on this host", id)
	}
	vol := m.dbVolumePath(id)
	if _, err := os.Stat(vol); err == nil {
		m.log.Info("db restore: reusing existing local volume", "id", id, "path", vol)
	} else {
		if err := m.buildDBVolumeFromArchive(ctx, id, vol); err != nil {
			return nil, err
		}
	}
	// Clear any stale sandbox row from the database's previous life (shared
	// Postgres store in multi-node: the dead agent's row still exists, and
	// InsertSandbox is a plain INSERT that would conflict on the pinned ID).
	if err := m.store.DeleteSandbox(ctx, id); err != nil {
		m.log.Warn("db restore: stale sandbox row cleanup failed (continuing)", "id", id, "err", err)
	}
	// Create on the normal path: ensureDBVolume sees the staged image and
	// reuses it; kickPGPhase2 injects wal.env + fresh credentials; the lease
	// upsert retargets routing to this agent.
	sb, err := m.Create(ctx, CreateRequest{
		ID:         id,
		Template:   pgManagedTemplate,
		Persistent: true,
		Metadata:   metadata,
	})
	if err != nil {
		return nil, fmt.Errorf("db restore: boot restored database: %w", err)
	}
	m.log.Info("db restore: database restored and booting", "id", id)
	return sb, nil
}

// buildDBVolumeFromArchive stages a fresh durable volume for id at vol from
// gs://$PANDASTACK_SNAPSHOT_BUCKET/db/{id}/: latest base backup extracted
// into /pgdata plus recovery.signal + restore_command so the guest replays
// archived WAL on first boot. The image is built at a temp path and only
// renamed into place when complete, so a crashed restore never leaves a
// half-staged volume that the corruption guard in autostart.sh would trust.
func (m *Manager) buildDBVolumeFromArchive(ctx context.Context, id, vol string) (err error) {
	bucket := strings.TrimSpace(os.Getenv("PANDASTACK_SNAPSHOT_BUCKET"))
	if bucket == "" {
		return errors.New("db restore: PANDASTACK_SNAPSHOT_BUCKET not set — no archive to restore from")
	}
	prefix := "gs://" + bucket + "/db/" + id

	// Latest base backup: names embed a UTC timestamp (base-<ts>.tar.gz) so
	// the lexicographic max is the newest.
	lctx, lcancel := context.WithTimeout(ctx, 2*time.Minute)
	out, lerr := exec.CommandContext(lctx, "gsutil", "ls", prefix+"/base/").CombinedOutput()
	lcancel()
	if lerr != nil {
		return fmt.Errorf("db restore: list base backups for %s: %v: %s", id, lerr, strings.TrimSpace(string(out)))
	}
	var bases []string
	for _, ln := range strings.Split(string(out), "\n") {
		ln = strings.TrimSpace(ln)
		name := ln[strings.LastIndex(ln, "/")+1:]
		if strings.HasPrefix(name, "base-") && strings.HasSuffix(name, ".tar.gz") {
			bases = append(bases, ln)
		}
	}
	if len(bases) == 0 {
		return fmt.Errorf("db restore: no base backups under %s/base/", prefix)
	}
	sort.Strings(bases)
	baseObj := bases[len(bases)-1]

	work, err := os.MkdirTemp(m.cfg.DataDir, "db-restore-")
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

	m.log.Info("db restore: downloading base backup", "id", id, "object", baseObj)
	tarPath := filepath.Join(work, "base.tar.gz")
	dctx, dcancel := context.WithTimeout(ctx, 30*time.Minute)
	out, err = exec.CommandContext(dctx, "gsutil", "-q", "cp", baseObj, tarPath).CombinedOutput()
	dcancel()
	if err != nil {
		return fmt.Errorf("db restore: download %s: %v: %s", baseObj, err, strings.TrimSpace(string(out)))
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
	// promote on a new timeline.
	if err := os.WriteFile(filepath.Join(pgdata, "recovery.signal"), nil, 0o600); err != nil {
		return fmt.Errorf("db restore: recovery.signal: %w", err)
	}
	confPath := filepath.Join(pgdata, "postgresql.conf")
	conf, err := os.ReadFile(confPath)
	if err != nil {
		return fmt.Errorf("db restore: read postgresql.conf: %w", err)
	}
	if !strings.Contains(string(conf), "pandastack-wal-restore") {
		if err := os.WriteFile(confPath, append(conf, []byte(dbRestoreWALRestoreConf)...), 0o600); err != nil {
			return fmt.Errorf("db restore: append restore_command: %w", err)
		}
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
		"id", id, "base", baseObj, "size_bytes", sizeBytes, "path", vol)
	return nil
}
