// SPDX-License-Identifier: Apache-2.0
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"time"

	"github.com/pandastack/agent/internal/guest"
	"github.com/pandastack/agent/internal/seed"
)

// runSeedSync implements the `pandastack-agent seed-sync` subcommand. It is a
// one-shot, best-effort maintenance step invoked by cloud-init at agent boot
// (BEFORE the agent service starts) to pull fleet-shared template snapshots
// from GCS into <data-dir>/template-snaps/. It always exits 0: seeding is an
// optimization, and any failure simply leaves the agent to cold-bake on first
// use (today's behaviour). Exiting non-zero would needlessly fail boot.
func runSeedSync(args []string) int {
	fs := flag.NewFlagSet("seed-sync", flag.ContinueOnError)
	dataDir := fs.String("data-dir", "/var/lib/pandastack", "Agent data dir")
	timeout := fs.Duration("timeout", 20*time.Minute, "Overall deadline for the sync")
	if err := fs.Parse(args); err != nil {
		return 0
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	store := seed.NewFromEnv()
	if !store.Enabled() {
		log.Info("seed-sync: PANDASTACK_GCS_BUCKET unset; nothing to do")
		return 0
	}

	// Resolve the agent's ssh key fingerprint exactly as the running agent
	// will: NewKeyStore loads (or generates) the key under <data-dir>/keys/.
	// In the seeding flow cloud-init has already placed the fleet-wide SHARED
	// key there, so this fingerprint matches every published seed.
	ks, err := guest.NewKeyStore(*dataDir)
	if err != nil {
		log.Warn("seed-sync: keystore load failed; skipping", "err", err)
		return 0
	}

	// Flavor is derived identically to the agent (PANDASTACK_NATID == "1").
	flavor := "legacy"
	if os.Getenv("PANDASTACK_NATID") == "1" {
		flavor = "natid"
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	t0 := time.Now()
	store.Sync(ctx, *dataDir, ks.Fingerprint(), flavor, log)
	log.Info("seed-sync: done", "elapsed_ms", time.Since(t0).Milliseconds())
	return 0
}
