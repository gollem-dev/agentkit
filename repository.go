package agentkit

import (
	"context"
	"time"
)

// Repository is the kernel's persistence contract. It is an SPI implemented and
// injected by the caller; the application never calls it directly (reads go
// through Kernel.GetProcess / ListAwaits / ListEvents). It requires no
// transaction mechanism — only atomic application of a change set and
// conditional writes (Rev-based optimistic concurrency). The realization (RDB
// TX / Firestore TX / conditional write / mutex) is the implementation's choice.
//
// Contract (also verified by repository/repotest):
//  1. Apply applies the whole ChangeSet atomically — all or nothing.
//  2. Apply checks each Processes row's stored Rev against the row's Rev (CAS);
//     a single mismatch writes nothing and returns ErrConflict. On success each
//     row's Rev is +1'd (a new insert of a Rev=0 row is stored as Rev=1). This
//     fences stale-worker commits.
//  3. Apply checks each Guards ProcessGuard's Rev (a read-only precondition; no
//     write, so Rev is not advanced) — used for WaitChildren check-then-act.
//  4. ClaimNextProcess never double-claims a Process across concurrent workers
//     (atomic claim, +1 Rev). It writes status=running / lease_owner=workerID /
//     lease_token=new-uuid-v7 / lease_until, and mints a fresh lease_token every
//     claim (even a re-claim by the same workerID) — the fence identity.
//     When the target was status=running (an expired or absent lease) — i.e. the
//     previous claim died mid-transition — it also increments unclean_reclaims.
//     A claim from pending or waiting leaves it unchanged, and ClaimNextProcess
//     never writes step_attempts. This is what bounds re-execution after a
//     crash; an implementation that skips it degrades to unbounded replay.
//  5. Uniqueness is maintained: idempotency_key / open Process subject /
//     (process_id, await_key). An insert violation writes nothing and returns
//     ErrConflict.
//  6. ListEvents preserves per-Process append order.
type Repository interface {
	// GetProcess returns the Process. Absent -> ErrProcessNotFound.
	GetProcess(ctx context.Context, pid ProcessID) (*Process, error)
	// FindProcessByIdempotencyKey finds a Process by its idempotency key. Absent -> ErrProcessNotFound.
	FindProcessByIdempotencyKey(ctx context.Context, key string) (*Process, error)
	// FindOpenProcessBySubject finds an open (pending/running/waiting) Process holding subject. Absent -> ErrProcessNotFound.
	FindOpenProcessBySubject(ctx context.Context, subject SubjectRef) (*Process, error)

	// ClaimNextProcess atomically claims one runnable Process. Targets:
	// status=pending, or status=waiting with wake_at<=now, or status=running
	// with lease_until<now (lease expired). No target -> (nil, nil) (not an error).
	// A claim from status=running also increments unclean_reclaims (contract 4).
	ClaimNextProcess(ctx context.Context, workerID string, leaseUntil time.Time, now time.Time) (*Process, error)

	// ListAwaits returns all awaits of a Process.
	ListAwaits(ctx context.Context, pid ProcessID) ([]*Await, error)
	// ListEvents returns a Process's events in append order.
	ListEvents(ctx context.Context, pid ProcessID) ([]*Event, error)

	// Apply applies a ChangeSet atomically (see the contract above). On any
	// precondition failure it writes nothing and returns ErrConflict.
	Apply(ctx context.Context, cs ChangeSet) error
}

// ProcessGuard is a write-free precondition on a Process row (a read-set Rev
// CAS). Its main use is making WaitChildren check-then-act atomic (the parent
// reads child states to decide elision, then guards on the children's Revs).
type ProcessGuard struct {
	ProcessID ProcessID
	Rev       int64 // must equal the stored Rev.
}

// ChangeSet is the unit of atomic persistence.
type ChangeSet struct {
	Guards    []ProcessGuard // write-free Process preconditions (read-set; for WaitChildren).
	Processes []*Process     // Rev-CAS upserts (write-set). May be several rows (child creation + parent wake).
	Awaits    []*Await       // upserts keyed by (ProcessID, Key).
	Events    []*Event       // appends (per-Process append order preserved).
}
