package agentkit

import (
	"context"

	"github.com/gollem-dev/gollem"
)

// HistoryRef names one stored version of a Process's conversation History. It is
// opaque to the kernel: the store mints it in Save, and the kernel only records
// it on the Process record and hands it back to Load. The zero value means "no
// version has been committed yet", so a store must never return it.
type HistoryRef string

// HistoryStore persists a Process's conversation History as immutable versions.
//
// Which version is current is decided by Process.HistoryRef, which commits in the
// same Apply as State, so a save whose transition never committed is simply never
// referenced and the next attempt re-seeds from the committed one. That is what
// makes History roll back together with State, and it is why a stored version
// must never be rewritten in place (ADR-0017).
//
// It is a port separate from Repository on purpose: History grows unbounded and
// does not belong in the transactional store, which carries only the ref. The
// implementation serializes *gollem.History; the kernel marshals nothing
// (ADR-0007).
type HistoryStore interface {
	// Save stores h as a NEW version and returns the ref naming it. It must not
	// modify or replace any previously returned version. Returning the same ref
	// for byte-identical content is allowed (a content-addressed implementation);
	// returning a ref that later names DIFFERENT content is a contract violation,
	// and it is the one that breaks the rollback guarantee. The returned ref must
	// not be empty.
	//
	// A ref that sorts by creation time is preferred, so an operator can read the
	// order of versions off the store, but it is not required.
	Save(ctx context.Context, pid ProcessID, h *gollem.History) (HistoryRef, error)

	// Load returns the version named by ref. The kernel never calls it with an
	// empty ref. An unknown ref must be an error (ErrHistoryVersionMissing), not a
	// nil result: a referenced version that is gone is data loss, not "nothing
	// saved yet".
	Load(ctx context.Context, pid ProcessID, ref HistoryRef) (*gollem.History, error)

	// Discard reports that ref is no longer referenced. It is a NOTIFICATION, not
	// a deletion order: reclaiming immediately, deferring to a sweep, or ignoring
	// it are all conforming, and an unknown or already-discarded ref is not a
	// failure. It returns nothing on purpose — the kernel would only log and carry
	// on, so reporting a failed reclaim is the implementation's own business.
	//
	// Versions leak without it too: a crash between Save and the commit leaves one
	// nobody will ever discard. A store is expected to have its own reclamation
	// policy for those.
	Discard(ctx context.Context, pid ProcessID, ref HistoryRef)
}
