// SPDX-License-Identifier: Apache-2.0
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ForkTreeResult is the response shape for a tree-fork: one snapshot of the
// parent, then N children booted from that snapshot via the time-travel
// (NATID snapshot-restore) path. Each child has the parent's MEMORY +
// FILESYSTEM state, but a fresh network identity.
//
// Use case: parallel agent exploration — fan out N approaches from the same
// state, then promote() the winner and delete siblings. Sub-second per child
// vs ~3s for cold rootfs-clone fork; child boot times scale with snapshot
// restore (~200-500ms each, parallel-friendly).
type ForkTreeResult struct {
	TreeID   string         `json:"tree_id"`
	Parent   string         `json:"parent_id"`
	Snapshot string         `json:"snapshot_id"`
	Children []ForkTreeKid  `json:"children"`
	At       time.Time      `json:"at"`
	Errors   []ForkTreeFail `json:"errors,omitempty"`
}

type ForkTreeKid struct {
	ID       string `json:"id"`
	GuestIP  string `json:"guest_ip"`
	BootMS   int64  `json:"boot_ms"`
	BootMode string `json:"boot_mode"`
}

type ForkTreeFail struct {
	Index int    `json:"index"`
	Err   string `json:"err"`
}

const (
	// metadata keys threaded into every child sandbox so promote() can find
	// the siblings without a dedicated DB table.
	MetaForkTreeID   = "fork_tree_id"
	MetaForkTreeRole = "fork_tree_role" // "child"
	MetaForkParentID = "fork_parent_id"

	maxTreeChildren = 16
)

// ForkTree snapshots the parent ONCE and spawns `count` children in parallel
// from that snapshot. Children inherit the parent's memory+disk state. The
// parent is NOT modified (snapshot is non-destructive: pause→snap→resume).
func (m *Manager) ForkTree(ctx context.Context, parentID string, count int, extraMeta map[string]string) (*ForkTreeResult, error) {
	if count <= 0 {
		count = 1
	}
	if count > maxTreeChildren {
		return nil, fmt.Errorf("fork-tree count capped at %d", maxTreeChildren)
	}
	if m.driver(parentID) == nil {
		return nil, errors.New("parent sandbox not found or not running")
	}

	// 1. Snapshot parent (mem+state, uploads to GCS synchronously).
	snap, err := m.Snapshot(ctx, parentID)
	if err != nil {
		return nil, fmt.Errorf("snapshot parent: %w", err)
	}

	// Inherit parent's resources for children.
	parentAny, _ := m.store.GetSandbox(ctx, parentID)
	row, _ := parentAny.(map[string]any)
	cpu := asInt(row["cpu"])
	mem := asInt(row["memory_mb"])
	tpl, _ := row["template"].(string)
	// Inherit parent's workspace tag so the children are visible to the
	// same caller's auth scope (lease + multi-node routing + dashboard).
	parentWorkspace := ""
	if md, ok := row["metadata"].(map[string]string); ok {
		parentWorkspace = md["workspace"]
	}

	treeID := uuid.NewString()
	res := &ForkTreeResult{
		TreeID:   treeID,
		Parent:   parentID,
		Snapshot: snap.ID,
		At:       time.Now().UTC(),
	}

	var (
		mu   sync.Mutex
		wg   sync.WaitGroup
		kids = make([]ForkTreeKid, 0, count)
		errs = make([]ForkTreeFail, 0)
	)
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			md := map[string]string{
				MetaForkTreeID:   treeID,
				MetaForkTreeRole: "child",
				MetaForkParentID: parentID,
				"fork_index":     fmt.Sprintf("%d", i),
			}
			if parentWorkspace != "" {
				md["workspace"] = parentWorkspace
			}
			for k, v := range extraMeta {
				md[k] = v
			}
			req := CreateRequest{
				Template:     tpl,
				CPU:          cpu,
				MemoryMB:     mem,
				FromSnapshot: snap.ID,
				Metadata:     md,
			}
			sb, err := m.Create(ctx, req)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, ForkTreeFail{Index: i, Err: err.Error()})
				return
			}
			kids = append(kids, ForkTreeKid{
				ID:       sb.ID,
				GuestIP:  sb.GuestIP,
				BootMS:   sb.BootMS,
				BootMode: sb.BootMode,
			})
		}(i)
	}
	wg.Wait()
	res.Children = kids
	res.Errors = errs
	if m.bus != nil {
		m.bus.Emit(parentID, "fork_tree.completed", map[string]any{
			"tree_id":   treeID,
			"snapshot":  snap.ID,
			"requested": count,
			"succeeded": len(kids),
			"failed":    len(errs),
		})
	}
	return res, nil
}

// PromoteTreeWinner deletes all siblings in a fork-tree EXCEPT winnerID.
// Returns the IDs that were deleted. Best-effort: deletion failures are
// collected but do not abort the operation.
func (m *Manager) PromoteTreeWinner(ctx context.Context, treeID, winnerID string) (deleted []string, failures map[string]string, err error) {
	if treeID == "" {
		return nil, nil, errors.New("tree_id required")
	}
	if winnerID == "" {
		return nil, nil, errors.New("winner_id required")
	}
	listAny, err := m.store.ListSandboxes(ctx)
	if err != nil {
		return nil, nil, err
	}
	failures = map[string]string{}
	for _, sbAny := range listAny {
		row, _ := sbAny.(map[string]any)
		mdAny := row["metadata"]
		md, _ := mdAny.(map[string]string)
		if md == nil {
			// metadata may be JSON-string in some store impls; ignore.
			continue
		}
		if md[MetaForkTreeID] != treeID {
			continue
		}
		id, _ := row["id"].(string)
		if id == "" || id == winnerID {
			continue
		}
		if delErr := m.Delete(ctx, id); delErr != nil {
			failures[id] = delErr.Error()
			continue
		}
		deleted = append(deleted, id)
	}
	if m.bus != nil {
		m.bus.Emit(winnerID, "fork_tree.promoted", map[string]any{
			"tree_id": treeID,
			"winner":  winnerID,
			"deleted": deleted,
			"failed":  failures,
		})
	}
	return deleted, failures, nil
}
