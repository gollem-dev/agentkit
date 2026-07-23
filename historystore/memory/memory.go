// Package memory is an in-process, non-persistent reference implementation of
// gollem.HistoryRepository. It is intended for tests, development, and
// persistence-free one-shot runs. All state lives in memory guarded by a
// single mutex, and every value crossing the boundary is deep-copied via
// (*gollem.History).Clone so neither side can reach the other's storage.
package memory

import (
	"context"
	"sync"

	"github.com/gollem-dev/gollem"
)

// Repository is the in-memory gollem.HistoryRepository.
type Repository struct {
	mu sync.Mutex
	m  map[string]*gollem.History
}

// New returns an empty in-memory Repository.
func New() *Repository {
	return &Repository{m: make(map[string]*gollem.History)}
}

var _ gollem.HistoryRepository = (*Repository)(nil)

// Load returns a clone of the stored History for sessionID, or (nil, nil) if
// nothing has been saved under that key.
func (r *Repository) Load(ctx context.Context, sessionID string) (*gollem.History, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	h, ok := r.m[sessionID]
	if !ok {
		return nil, nil
	}
	return h.Clone(), nil
}

// Save stores a clone of history under sessionID, overwriting any previous
// value (last-writer-wins).
func (r *Repository) Save(ctx context.Context, sessionID string, history *gollem.History) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m[sessionID] = history.Clone()
	return nil
}
