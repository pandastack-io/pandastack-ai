// SPDX-License-Identifier: Apache-2.0
package sandbox

import (
	"sync"
	"time"
)

// lifecycleState is the in-memory runtime view and the source of truth while the
// agent is up. The persistent flag + ttl are also written to the sandboxes table
// (see Store.SetSandboxLifecycle) so an agent restart can rehydrate them via
// Manager.rehydrateLifecycle instead of downgrading to the default TTL.
type lifecycleState struct {
	ttl        time.Duration
	persistent bool
	createdAt  time.Time
}

type lifecycleStore struct {
	mu         sync.RWMutex
	m          map[string]lifecycleState
	defaultTTL time.Duration
}

func newLifecycleStore(defaultTTL time.Duration) *lifecycleStore {
	return &lifecycleStore{
		m:          make(map[string]lifecycleState),
		defaultTTL: defaultTTL,
	}
}

func (s *lifecycleStore) Set(id string, ttl time.Duration, persistent bool) {
	s.mu.Lock()
	s.m[id] = lifecycleState{ttl: ttl, persistent: persistent, createdAt: time.Now().UTC()}
	s.mu.Unlock()
}

func (s *lifecycleStore) Get(id string) (lifecycleState, bool) {
	s.mu.RLock()
	st, ok := s.m[id]
	s.mu.RUnlock()
	return st, ok
}

func (s *lifecycleStore) Delete(id string) {
	s.mu.Lock()
	delete(s.m, id)
	s.mu.Unlock()
}

func (s *lifecycleStore) SetTTL(id string, ttl time.Duration) {
	s.mu.Lock()
	st, ok := s.m[id]
	if ok {
		st.ttl = ttl
		s.m[id] = st
	}
	s.mu.Unlock()
}

func (s *lifecycleStore) SetPersistent(id string, persistent bool) {
	s.mu.Lock()
	st, ok := s.m[id]
	if ok {
		st.persistent = persistent
		s.m[id] = st
	}
	s.mu.Unlock()
}

func (s *lifecycleStore) List() map[string]lifecycleState {
	s.mu.RLock()
	out := make(map[string]lifecycleState, len(s.m))
	for id, st := range s.m {
		out[id] = st
	}
	s.mu.RUnlock()
	return out
}
