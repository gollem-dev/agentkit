// Package memory is an in-process, non-persistent reference implementation of
// agentkit.HistoryStore. It is intended for tests, development, and
// persistence-free one-shot runs. All state lives in memory guarded by a
// single mutex, and every value crossing the boundary is deep-copied via
// (*gollem.History).Clone so neither side can reach the other's storage.
//
// Versions are immutable, as the port requires: Save always mints a fresh ref
// and never touches a version it handed out earlier. Discard removes one
// immediately — there is no reclamation to defer to in an in-memory map.
package memory

import (
	"context"
	"sync"

	"github.com/gollem-dev/agentkit"
	"github.com/gollem-dev/gollem"
	"github.com/google/uuid"
	"github.com/m-mizutani/goerr/v2"
)

// Store is the in-memory agentkit.HistoryStore.
type Store struct {
	mu sync.Mutex
	m  map[agentkit.ProcessID]map[agentkit.HistoryRef]*gollem.History
}

// New returns an empty in-memory Store.
func New() *Store {
	return &Store{m: make(map[agentkit.ProcessID]map[agentkit.HistoryRef]*gollem.History)}
}

var _ agentkit.HistoryStore = (*Store)(nil)

// Save stores a clone of h under a fresh ref and returns it. The ref is a UUIDv7
// so refs sort by creation time, which the port prefers; nothing depends on it.
func (s *Store) Save(ctx context.Context, pid agentkit.ProcessID, h *gollem.History) (agentkit.HistoryRef, error) {
	ref := agentkit.HistoryRef(uuid.Must(uuid.NewV7()).String())

	s.mu.Lock()
	defer s.mu.Unlock()
	versions, ok := s.m[pid]
	if !ok {
		versions = make(map[agentkit.HistoryRef]*gollem.History)
		s.m[pid] = versions
	}
	versions[ref] = h.Clone()
	return ref, nil
}

// Load returns a clone of the version named by ref, or ErrHistoryVersionMissing
// when it is not there — a referenced version that is gone is data loss, not an
// empty conversation.
func (s *Store) Load(ctx context.Context, pid agentkit.ProcessID, ref agentkit.HistoryRef) (*gollem.History, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.m[pid][ref]
	if !ok {
		return nil, goerr.Wrap(agentkit.ErrHistoryVersionMissing, "no such history version",
			goerr.V("process", pid), goerr.V("ref", ref))
	}
	return h.Clone(), nil
}

// Discard drops the version. An unknown or already-discarded ref is not a
// failure, per the port contract.
func (s *Store) Discard(ctx context.Context, pid agentkit.ProcessID, ref agentkit.HistoryRef) {
	s.mu.Lock()
	defer s.mu.Unlock()
	versions, ok := s.m[pid]
	if !ok {
		return
	}
	delete(versions, ref)
	if len(versions) == 0 {
		delete(s.m, pid)
	}
}
