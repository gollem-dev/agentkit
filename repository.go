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
//     A pending row whose wake_at is still in the future is NOT a target: that
//     is the retry backoff the worker writes when it puts a failed transition
//     back. An implementation that claims it anyway still runs correctly, but
//     retries as fast as it polls.
//     ClaimNextProcess and Apply are mutually linearizable on the same Process
//     row: a claim and a Rev-CAS Apply that both read the row at Rev N cannot
//     both succeed — exactly one advances it to N+1 and the other observes the
//     new Rev (a claim finds nothing to claim; an Apply returns ErrConflict).
//     This is what lets eager dispatch claim a specific pending row via Apply
//     without a dedicated SPI method, racing a poller's ClaimNextProcess safely.
//     A row carrying a Semaphore is additionally gated by the slot count, and a
//     claim takes the slot in the same atomic step (see items 7 and 8).
//  5. Uniqueness is maintained: idempotency_key / open Process subject /
//     (process_id, await_key). An insert violation writes nothing and returns
//     ErrConflict.
//  6. ListEvents preserves per-Process append order, and round-trips each
//     Event's kernel-assigned ID verbatim. An implementation never mints or
//     rewrites one: the ID is what a caller holds as a cursor, so a value that
//     changed between write and read would resume from the wrong place.
//  7. Semaphore terms, used by item 4 and item 8 (ADR-0021):
//     - the HOLDER SET of a (Key, Value) pair is the set of RootIDs of the rows
//     carrying that pair with SemaphoreHeld true and a non-terminal status. A
//     terminal row drops out of it, which is what releases its slot without
//     anyone writing a release.
//     - the EFFECTIVE LIMIT of that pair is the smallest Slots among those rows.
//     Rows sharing a pair normally agree, because Slots comes from the agent
//     definition; they can differ only while two deployments disagree, and the
//     smallest then binds.
//     A row with SemaphoreHeld false is a claim target only when adding its
//     RootID to the holder set keeps the size within min(effective limit, its own
//     Slots), and a claim sets SemaphoreHeld in the same atomic write.
//  8. Apply never makes a pair's occupancy worse: it is refused when the result
//     is over the effective limit AND worse than before — more holders, or the
//     same holders under a smaller limit. An existing row's Semaphore never
//     changes, and SemaphoreHeld never goes back to false. A violation writes
//     nothing and returns ErrConflict.
//     THE CLAIM PREDICATE (item 7) AND THIS ITEM MUST AGREE: an implementation
//     that admits a claim this item then refuses makes ClaimNextProcess itself
//     return an error, and the same row is re-picked on every poll, so the worker
//     stops making progress.
//     This is deliberately a MONOTONICITY rule rather than "the state is never
//     over the limit": the writes that would fix an over-limit pair (a holder
//     reaching a terminal status) still leave it over the limit, so an absolute
//     rule would reject them forever — and with them every other row in the same
//     ChangeSet.
type Repository interface {
	// GetProcess returns the Process. Absent -> ErrProcessNotFound.
	GetProcess(ctx context.Context, pid ProcessID) (*Process, error)
	// FindProcessByIdempotencyKey finds a Process by its idempotency key. Absent -> ErrProcessNotFound.
	FindProcessByIdempotencyKey(ctx context.Context, key string) (*Process, error)
	// FindOpenProcessBySubject finds an open (pending/running/waiting) Process holding subject. Absent -> ErrProcessNotFound.
	FindOpenProcessBySubject(ctx context.Context, subject SubjectRef) (*Process, error)
	// GetSemaphoreStatus reports the occupancy of one (key, value) pair: how many
	// process trees hold it, the effective limit, how many rows are queued behind
	// it, and how old the oldest of those is. A row whose tree already holds the
	// pair is not queued — it shares that slot — and is excluded from the count.
	//
	// A pair nothing references is NOT an error. It returns a zero-valued status
	// with key and value echoed back, because "nothing is using it" is a
	// legitimate answer to the question.
	//
	// This is the only way a caller can see the backlog behind a semaphore: Spawn
	// always succeeds and the number of waiting rows is unbounded, so throttling
	// is the caller's job and this is the figure to throttle on.
	GetSemaphoreStatus(ctx context.Context, key, value string) (*SemaphoreStatus, error)

	// ClaimNextProcess atomically claims one runnable Process. Targets:
	// status=pending with wake_at unset or <=now, or status=waiting with
	// wake_at<=now, or status=running with lease_until<now (lease expired). No
	// target -> (nil, nil) (not an error). A claim from status=running also
	// increments unclean_reclaims (contract 4).
	//
	// The two wake_at conditions differ on purpose: a pending row without one is
	// runnable now, whereas a waiting row without one is waiting for a response
	// and must never wake by itself.
	ClaimNextProcess(ctx context.Context, workerID string, leaseUntil time.Time, now time.Time) (*Process, error)

	// ListAwaits returns all awaits of a Process.
	ListAwaits(ctx context.Context, pid ProcessID) ([]*Await, error)
	// ListEvents returns a Process's events in append order, narrowed by q. A
	// zero EventQuery returns all of them.
	ListEvents(ctx context.Context, pid ProcessID, q EventQuery) ([]*Event, error)

	// Apply applies a ChangeSet atomically (see the contract above). On any
	// precondition failure it writes nothing and returns ErrConflict.
	Apply(ctx context.Context, cs ChangeSet) error
}

// EventQuery narrows a ListEvents read. Every field is optional, so the zero
// value means "all of this Process's events" — it is a struct rather than
// options so an implementation reads fields instead of resolving closures
// (ADR-0019).
type EventQuery struct {
	// After is a cursor. "" starts from the first event; otherwise the result is
	// the events appended strictly after the one with this ID. An ID this Process
	// has no event for is ErrEventNotFound and returns no events — returning the
	// whole list instead would reach the caller as a burst of new events, which
	// it has no way to tell from the real thing.
	After EventID
	// Limit caps how many events are returned. <= 0 means no cap.
	Limit int
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
