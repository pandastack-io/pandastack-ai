// SPDX-License-Identifier: Apache-2.0
package sandbox

import (
	"context"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/pandastack/agent/internal/network"
)

// natidPrewarmTarget is the desired free-list depth per template identity.
// Tuneable via PANDASTACK_NATID_POOL_SIZE (default 4).
func natidPrewarmTarget() int {
	if v := os.Getenv("PANDASTACK_NATID_POOL_SIZE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return 4
}

// startNATIDPrewarmer pre-builds netns + veth + tap + iptables slots for each
// ready template snapshot. After every Claim the pool refills itself in the
// background so subsequent boots stay O(1).
func (m *Manager) startNATIDPrewarmer() {
	if os.Getenv("PANDASTACK_NATID") != "1" {
		return
	}
	target := natidPrewarmTarget()
	if target == 0 {
		return
	}
	// Map identityKey -> (tapHostIP, guestIP, mac, ports) so the refill hook
	// can rebuild after Claim.
	type ident struct {
		tapHostIP, guestIP, mac string
		ports                   map[int]int
	}
	var (
		mu     sync.Mutex
		idents = map[string]ident{}
	)

	// Refill hook fires after every Claim. Best-effort; one slot per call.
	m.netPool.SetNATIDRefill(func(key string) {
		mu.Lock()
		id, ok := idents[key]
		mu.Unlock()
		if !ok {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := m.netPool.PrebuildNATID(ctx, id.tapHostIP, id.guestIP, id.mac, id.ports); err != nil {
			m.log.Warn("NATID refill failed", "err", err)
		}
	})

	// prewarmTemplate registers a template's NATID identity and pre-builds
	// `target` free-list slots. Safe to call more than once for the same
	// template — AllocateNATID is idempotent on the pre-built pool.
	prewarmTemplate := func(t string) {
		snapDir := templateSnapDir(m.cfg.DataDir, t)
		tapHostIP, guestIP, mac, err := loadTemplateIdentity(snapDir)
		if err != nil {
			m.log.Warn("NATID prewarm: identity load failed", "template", t, "err", err)
			return
		}
		ports := map[int]int{22: 22}
		key := network.NATIDIdentityKey(tapHostIP, guestIP, mac, ports)
		mu.Lock()
		idents[key] = ident{tapHostIP, guestIP, mac, ports}
		mu.Unlock()

		for i := 0; i < target; i++ {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			n, err := m.netPool.PrebuildNATID(ctx, tapHostIP, guestIP, mac, ports)
			cancel()
			if err != nil {
				m.log.Warn("NATID prewarm: build failed", "template", t, "err", err)
				break
			}
			m.log.Info("NATID prewarmed", "template", t, "free", n)
		}
	}

	go func() {
		seen := map[string]bool{}
		tick := time.NewTicker(30 * time.Second)
		defer tick.Stop()
		for {
			tmpls, _ := listReadyTemplates(m.cfg.DataDir)
			for _, t := range tmpls {
				if seen[t] {
					continue
				}
				seen[t] = true
				prewarmTemplate(t)
			}
			<-tick.C
		}
	}()
}
