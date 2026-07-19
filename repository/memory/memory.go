// Package memory is an in-process, non-persistent reference implementation of
// agentkit.Repository. It is intended for tests, development, and
// persistence-free one-shot runs; the kernel's own tests use it. All state
// lives in memory guarded by a single mutex, and every value crossing the
// boundary is deep-copied.
package memory

import (
	"context"
	"sync"
	"time"

	"github.com/gollem-dev/agentkit"
	"github.com/gollem-dev/agentkit/repository/internal/store"
)

// Repository is the in-memory agentkit.Repository.
type Repository struct {
	mu    sync.Mutex
	state *store.State
}

// New returns an empty in-memory Repository.
func New() *Repository {
	return &Repository{state: store.NewState()}
}

var _ agentkit.Repository = (*Repository)(nil)

// GetProcess returns the Process, or agentkit.ErrProcessNotFound.
func (r *Repository) GetProcess(ctx context.Context, pid agentkit.ProcessID) (*agentkit.Process, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state.GetProcess(pid)
}

// FindProcessByIdempotencyKey finds a Process by idempotency key.
func (r *Repository) FindProcessByIdempotencyKey(ctx context.Context, key string) (*agentkit.Process, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state.FindByIdempotencyKey(key)
}

// FindOpenProcessBySubject finds an open Process holding the subject.
func (r *Repository) FindOpenProcessBySubject(ctx context.Context, subject agentkit.SubjectRef) (*agentkit.Process, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state.FindOpenBySubject(subject)
}

// ClaimNextProcess atomically claims one runnable Process, minting a fresh
// LeaseToken. No target -> (nil, nil).
func (r *Repository) ClaimNextProcess(ctx context.Context, workerID string, leaseUntil time.Time, now time.Time) (*agentkit.Process, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	claimed, next, err := r.state.ClaimNext(workerID, leaseUntil, now)
	if err != nil {
		return nil, err
	}
	if claimed == nil {
		return nil, nil
	}
	r.state = next
	return claimed, nil
}

// ListAwaits returns all awaits of a Process.
func (r *Repository) ListAwaits(ctx context.Context, pid agentkit.ProcessID) ([]*agentkit.Await, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state.ListAwaits(pid), nil
}

// ListEvents returns a Process's events in append order.
func (r *Repository) ListEvents(ctx context.Context, pid agentkit.ProcessID) ([]*agentkit.Event, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state.ListEvents(pid), nil
}

// Apply applies a ChangeSet atomically. On any precondition failure it writes
// nothing and returns agentkit.ErrConflict.
func (r *Repository) Apply(ctx context.Context, cs agentkit.ChangeSet) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	next, err := r.state.After(cs)
	if err != nil {
		return err
	}
	r.state = next
	return nil
}
