// SPDX-License-Identifier: Apache-2.0
// Package events provides a per-sandbox append-only JSONL event log with
// live subscribers (for SSE streaming).
package events

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Event struct {
	Time      time.Time      `json:"time"`
	SandboxID string         `json:"sandbox_id"`
	Type      string         `json:"type"`
	Payload   map[string]any `json:"payload,omitempty"`
}

type Bus struct {
	dataDir string

	mu   sync.Mutex
	subs map[string]map[chan Event]struct{} // sandboxID -> set of subscriber chans
}

func NewBus(dataDir string) *Bus {
	return &Bus{dataDir: dataDir, subs: make(map[string]map[chan Event]struct{})}
}

// LogPath returns the per-sandbox JSONL log file path.
func (b *Bus) LogPath(sandboxID string) string {
	return filepath.Join(b.dataDir, "vms", sandboxID, "events.jsonl")
}

// Emit appends an event to disk and fans it out to live subscribers.
func (b *Bus) Emit(sandboxID, typ string, payload map[string]any) {
	ev := Event{
		Time:      time.Now().UTC(),
		SandboxID: sandboxID,
		Type:      typ,
		Payload:   payload,
	}
	path := b.LogPath(sandboxID)
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	if f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
		_ = json.NewEncoder(f).Encode(ev)
		_ = f.Close()
	}
	b.mu.Lock()
	for ch := range b.subs[sandboxID] {
		select {
		case ch <- ev:
		default:
		}
	}
	b.mu.Unlock()
}

// Subscribe registers a channel that receives all subsequent events for the
// sandbox until ctx is done.
func (b *Bus) Subscribe(ctx context.Context, sandboxID string) <-chan Event {
	ch := make(chan Event, 64)
	b.mu.Lock()
	if b.subs[sandboxID] == nil {
		b.subs[sandboxID] = make(map[chan Event]struct{})
	}
	b.subs[sandboxID][ch] = struct{}{}
	b.mu.Unlock()
	go func() {
		<-ctx.Done()
		b.mu.Lock()
		delete(b.subs[sandboxID], ch)
		if len(b.subs[sandboxID]) == 0 {
			delete(b.subs, sandboxID)
		}
		b.mu.Unlock()
		close(ch)
	}()
	return ch
}

// Tail returns the most recent n events from disk (n<=0 returns all).
func (b *Bus) Tail(sandboxID string, n int) ([]Event, error) {
	path := b.LogPath(sandboxID)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Event
	dec := json.NewDecoder(bytes.NewReader(data))
	for {
		var ev Event
		if err := dec.Decode(&ev); err != nil {
			if err == io.EOF {
				break
			}
			break
		}
		out = append(out, ev)
	}
	if n > 0 && len(out) > n {
		out = out[len(out)-n:]
	}
	return out, nil
}
