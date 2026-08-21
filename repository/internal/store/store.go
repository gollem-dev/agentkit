// Package store holds the shared in-memory state machine behind the memory and
// filesystem reference Repository implementations. It centralizes the contract
// logic (Rev CAS, Guards, uniqueness, atomic ChangeSet application, and
// ClaimNextProcess) so both implementations satisfy an identical contract and
// pass repository/repotest. The package is internal: the boundary genuinely
// spans two sibling packages, which is why it is a package rather than
// unexported helpers.
package store

import (
	"encoding/json"
	"maps"
	"time"

	"github.com/gollem-dev/agentkit"
	"github.com/google/uuid"
	"github.com/m-mizutani/goerr/v2"
)

// State is an immutable-by-convention snapshot of the whole Repository. Writes
// never mutate an existing State: After and ClaimNext build and return a fresh
// State (copy-on-write on the touched keys), so the caller can persist it before
// swapping it in. Reads deep-copy on the way out so a caller mutating a returned
// value cannot reach into stored state.
type State struct {
	procs  map[agentkit.ProcessID]*agentkit.Process
	awaits map[agentkit.ProcessID]map[agentkit.AwaitKey]*agentkit.Await
	events map[agentkit.ProcessID][]*agentkit.Event

	// Derived indexes, rebuilt from procs whenever a next State is produced.
	idem map[string]agentkit.ProcessID              // idempotency_key -> pid (non-empty keys only).
	subj map[agentkit.SubjectRef]agentkit.ProcessID // subject -> pid (open processes only).
	sem  map[semKey]*semHolders                     // (key, value) -> who holds it (held, non-terminal rows only).
}

// semKey is the unit a semaphore counts: one instance of one declared key. Slots
// is deliberately NOT part of it — it is the limit ON this unit, not part of its
// identity.
type semKey struct {
	key   string
	value string
}

// semHolders is the derived index for one semKey: which trees hold it, the
// smallest Slots any holder carries, and which row carried it. Rows sharing a
// semKey normally agree, because Slots comes from the agent definition and
// Register rejects two agents declaring one key with different slot counts —
// they can differ only while two deployments overlap. The smallest then binds,
// through both the claim predicate and the Apply check.
type semHolders struct {
	roots     map[agentkit.ProcessID]struct{}
	minSlots  int
	strictest agentkit.ProcessID // diagnostic: the row minSlots came from.
}

// NewState returns an empty State.
func NewState() *State {
	return &State{
		procs:  map[agentkit.ProcessID]*agentkit.Process{},
		awaits: map[agentkit.ProcessID]map[agentkit.AwaitKey]*agentkit.Await{},
		events: map[agentkit.ProcessID][]*agentkit.Event{},
		idem:   map[string]agentkit.ProcessID{},
		subj:   map[agentkit.SubjectRef]agentkit.ProcessID{},
		sem:    map[semKey]*semHolders{},
	}
}

// snapshot is the on-disk JSON shape used by the filesystem implementation. The
// choice of encoding/json is a storage-format decision of the reference impl.
type snapshot struct {
	Processes map[agentkit.ProcessID]*agentkit.Process                     `json:"processes"`
	Awaits    map[agentkit.ProcessID]map[agentkit.AwaitKey]*agentkit.Await `json:"awaits"`
	Events    map[agentkit.ProcessID][]*agentkit.Event                     `json:"events"`
}

// Marshal serializes the full state to JSON.
func (s *State) Marshal() ([]byte, error) {
	data, err := json.MarshalIndent(snapshot{
		Processes: s.procs,
		Awaits:    s.awaits,
		Events:    s.events,
	}, "", "  ")
	if err != nil {
		return nil, goerr.Wrap(err, "marshal state snapshot")
	}
	return data, nil
}

// Load reconstructs a State from a JSON snapshot and rebuilds its indexes.
func Load(data []byte) (*State, error) {
	var snap snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, goerr.Wrap(err, "unmarshal state snapshot", goerr.V("size", len(data)))
	}
	s := NewState()
	if snap.Processes != nil {
		s.procs = snap.Processes
	}
	if snap.Awaits != nil {
		s.awaits = snap.Awaits
	}
	if snap.Events != nil {
		s.events = snap.Events
	}
	if err := s.rebuildIndexes(); err != nil {
		return nil, goerr.Wrap(err, "loaded state violates uniqueness")
	}
	return s, nil
}

// GetProcess returns a deep copy of the Process, or ErrProcessNotFound.
func (s *State) GetProcess(pid agentkit.ProcessID) (*agentkit.Process, error) {
	p, ok := s.procs[pid]
	if !ok {
		return nil, goerr.Wrap(agentkit.ErrProcessNotFound, "process not found", goerr.V("process_id", pid))
	}
	return cloneProcess(p), nil
}

// FindByIdempotencyKey returns the Process with the given idempotency key, or
// ErrProcessNotFound.
func (s *State) FindByIdempotencyKey(key string) (*agentkit.Process, error) {
	if key == "" {
		return nil, goerr.Wrap(agentkit.ErrProcessNotFound, "empty idempotency key")
	}
	pid, ok := s.idem[key]
	if !ok {
		return nil, goerr.Wrap(agentkit.ErrProcessNotFound, "process not found by idempotency key", goerr.V("key", key))
	}
	return cloneProcess(s.procs[pid]), nil
}

// FindOpenBySubject returns the open (pending/running/waiting) Process holding
// the subject, or ErrProcessNotFound.
func (s *State) FindOpenBySubject(subject agentkit.SubjectRef) (*agentkit.Process, error) {
	pid, ok := s.subj[subject]
	if !ok {
		return nil, goerr.Wrap(agentkit.ErrProcessNotFound, "open process not found by subject", goerr.V("subject", subject))
	}
	return cloneProcess(s.procs[pid]), nil
}

// ListAwaits returns deep copies of all awaits of a Process.
func (s *State) ListAwaits(pid agentkit.ProcessID) []*agentkit.Await {
	inner := s.awaits[pid]
	out := make([]*agentkit.Await, 0, len(inner))
	for _, a := range inner {
		out = append(out, cloneAwait(a))
	}
	return out
}

// ListEvents returns deep copies of a Process's events in append order, starting
// after the cursor and capped at limit. See the Repository contract for the
// cursor semantics; an unknown after is agentkit.ErrEventNotFound.
func (s *State) ListEvents(pid agentkit.ProcessID, q agentkit.EventQuery) ([]*agentkit.Event, error) {
	es := s.events[pid]
	start := 0
	if q.After != "" {
		found := false
		for i, e := range es {
			if e.ID == q.After {
				start = i + 1
				found = true
				break
			}
		}
		if !found {
			return nil, goerr.Wrap(agentkit.ErrEventNotFound, "unknown ListEvents cursor",
				goerr.V("process", pid), goerr.V("after", q.After))
		}
	}
	es = es[start:]
	if q.Limit > 0 && len(es) > q.Limit {
		es = es[:q.Limit]
	}
	out := make([]*agentkit.Event, len(es))
	for i, e := range es {
		out[i] = cloneEvent(e)
	}
	return out, nil
}

// checkPreconditions verifies Guards (read-only Rev CAS) and Processes (Rev CAS)
// against the current state. A single mismatch returns ErrConflict.
func (s *State) checkPreconditions(cs agentkit.ChangeSet) error {
	for _, g := range cs.Guards {
		st, ok := s.procs[g.ProcessID]
		if !ok || st.Rev != g.Rev {
			return goerr.Wrap(agentkit.ErrConflict, "guard rev mismatch",
				goerr.V("process_id", g.ProcessID), goerr.V("guard_rev", g.Rev), goerr.V("stored_present", ok))
		}
	}
	for _, p := range cs.Processes {
		st, ok := s.procs[p.ID]
		if !ok {
			// Insert: the row must declare Rev 0.
			if p.Rev != 0 {
				return goerr.Wrap(agentkit.ErrConflict, "insert rev must be zero",
					goerr.V("process_id", p.ID), goerr.V("row_rev", p.Rev))
			}
			continue
		}
		if st.Rev != p.Rev {
			return goerr.Wrap(agentkit.ErrConflict, "process rev mismatch",
				goerr.V("process_id", p.ID), goerr.V("row_rev", p.Rev), goerr.V("stored_rev", st.Rev))
		}
		// An existing row's Semaphore is fixed and its SemaphoreHeld is monotone.
		// Every kernel path builds its update from a clone of the row, so this only
		// fires on a write that reached in and changed one of them — which would
		// hand a Process a free run outside the limit the store is maintaining.
		if !sameSemaphore(st.Semaphore, p.Semaphore) {
			return goerr.Wrap(agentkit.ErrConflict, "semaphore is immutable",
				goerr.V("process_id", p.ID), goerr.V("stored", st.Semaphore), goerr.V("row", p.Semaphore))
		}
		if st.SemaphoreHeld && !p.SemaphoreHeld {
			return goerr.Wrap(agentkit.ErrConflict, "semaphore_held cannot go back to false",
				goerr.V("process_id", p.ID))
		}
	}
	return nil
}

// sameSemaphore compares two Semaphore pointers by value, treating nil as a
// value of its own so that "declared none" and "declared one" are distinguished.
func sameSemaphore(a, b *agentkit.ProcessSemaphore) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// After validates cs against the current state and, on success, returns a fresh
// next State with the ChangeSet applied (Processes' Rev +1'd, Awaits upserted,
// Events appended). On any precondition or uniqueness failure it returns
// ErrConflict and no State (the caller writes nothing).
func (s *State) After(cs agentkit.ChangeSet) (*State, error) {
	if err := s.checkPreconditions(cs); err != nil {
		return nil, err
	}

	next := s.shallowClone()

	for _, p := range cs.Processes {
		np := cloneProcess(p)
		np.Rev = p.Rev + 1 // insert (Rev 0) -> 1; update (Rev == stored) -> stored+1.
		next.procs[p.ID] = np
	}

	touchedAwaits := map[agentkit.ProcessID]bool{}
	for _, a := range cs.Awaits {
		if !touchedAwaits[a.ProcessID] {
			// Copy-on-write the inner map so we never mutate the shared one.
			cp := make(map[agentkit.AwaitKey]*agentkit.Await, len(next.awaits[a.ProcessID])+1)
			maps.Copy(cp, next.awaits[a.ProcessID])
			next.awaits[a.ProcessID] = cp
			touchedAwaits[a.ProcessID] = true
		}
		next.awaits[a.ProcessID][a.Key] = cloneAwait(a) // upsert by (ProcessID, Key).
	}

	touchedEvents := map[agentkit.ProcessID]bool{}
	for _, e := range cs.Events {
		if !touchedEvents[e.ProcessID] {
			// Detach from the shared backing array before appending.
			next.events[e.ProcessID] = append([]*agentkit.Event(nil), next.events[e.ProcessID]...)
			touchedEvents[e.ProcessID] = true
		}
		next.events[e.ProcessID] = append(next.events[e.ProcessID], cloneEvent(e))
	}

	if err := next.rebuildIndexes(); err != nil {
		return nil, err
	}
	if err := s.semaphoreRegression(next); err != nil {
		return nil, err
	}
	return next, nil
}

// ClaimNext atomically claims the runnable Process with the smallest CreatedAt.
// It returns a deep copy of the claimed Process and the next State to persist,
// or (nil, nil, nil) when there is no target.
func (s *State) ClaimNext(workerID string, leaseUntil, now time.Time) (*agentkit.Process, *State, error) {
	var target *agentkit.Process
	var kind claimKind
	for _, p := range s.procs {
		k := claimKindOf(p, now)
		if k == notClaimable {
			continue
		}
		if !s.semaphoreAdmits(p) {
			// The pair is full. Leave the row where it is — a pending row waiting for
			// a slot needs no state of its own, and a later poll picks it up once a
			// holder terminates.
			continue
		}
		// kind is assigned with target, not per candidate: only the winner's
		// reason may survive the loop.
		if target == nil || p.CreatedAt.Before(target.CreatedAt) {
			target, kind = p, k
		}
	}
	if target == nil {
		return nil, nil, nil
	}

	next := s.shallowClone()
	np := cloneProcess(target)
	np.Status = agentkit.ProcessRunning
	np.LeaseOwner = workerID
	np.LeaseToken = uuid.Must(uuid.NewV7()).String() // fresh fence identity every claim.
	if np.Semaphore != nil {
		np.SemaphoreHeld = true // taking the slot is part of the atomic claim.
	}
	if kind == uncleanClaim {
		np.UncleanReclaims++
	}
	lu := leaseUntil
	np.LeaseUntil = &lu
	np.Rev++
	np.UpdatedAt = now
	next.procs[np.ID] = np
	if err := next.rebuildIndexes(); err != nil {
		return nil, nil, err
	}
	return cloneProcess(np), next, nil
}

// claimKind reports why a Process is claimable. The reason is named where it is
// decided so that a future claimable status cannot silently be counted as clean.
type claimKind int

const (
	notClaimable claimKind = iota
	cleanClaim             // pending, or waiting whose WakeAt is due.
	uncleanClaim           // running: the previous claim died mid-transition.
)

// claimKindOf classifies p as a ClaimNext target at now.
func claimKindOf(p *agentkit.Process, now time.Time) claimKind {
	switch p.Status {
	case agentkit.ProcessPending:
		// A pending row may carry a WakeAt: the worker's requeue writes one as the
		// retry backoff. Without this gate the backoff is inert and a Process that
		// keeps failing is re-claimed as fast as the poll loop can go.
		if p.WakeAt == nil || !p.WakeAt.After(now) {
			return cleanClaim
		}
		return notClaimable
	case agentkit.ProcessWaiting:
		// Not the same condition as pending: a waiting row with no deadline is
		// waiting on a response and must never wake on its own.
		if p.WakeAt != nil && !p.WakeAt.After(now) {
			return cleanClaim
		}
		return notClaimable
	case agentkit.ProcessRunning:
		// A live lease fences the row; nil lease means unfenced. Every orderly
		// exit from a claim (suspend, terminate, requeue, release) clears the
		// lease as it moves the Process off running, so reaching here at all
		// means the previous claim vanished mid-transition.
		if p.LeaseUntil == nil || p.LeaseUntil.Before(now) {
			return uncleanClaim
		}
		return notClaimable
	default:
		return notClaimable
	}
}

// shallowClone copies the top-level maps but shares the entry pointers. Callers
// replace touched keys with fresh values (copy-on-write), so the original State
// is never mutated. Indexes are rebuilt by the caller.
func (s *State) shallowClone() *State {
	n := &State{
		procs:  make(map[agentkit.ProcessID]*agentkit.Process, len(s.procs)),
		awaits: make(map[agentkit.ProcessID]map[agentkit.AwaitKey]*agentkit.Await, len(s.awaits)),
		events: make(map[agentkit.ProcessID][]*agentkit.Event, len(s.events)),
		idem:   map[string]agentkit.ProcessID{},
		subj:   map[agentkit.SubjectRef]agentkit.ProcessID{},
		sem:    map[semKey]*semHolders{},
	}
	maps.Copy(n.procs, s.procs)
	maps.Copy(n.awaits, s.awaits)
	maps.Copy(n.events, s.events)
	return n
}

// rebuildIndexes recomputes the idempotency, open-subject and semaphore indexes
// from procs, returning ErrConflict on a uniqueness violation.
//
// It does NOT check the semaphore slot count. Whether a resulting occupancy is
// acceptable depends on the state it came from (see semaphoreRegression), which
// this method does not have; it only builds the index.
func (s *State) rebuildIndexes() error {
	s.idem = make(map[string]agentkit.ProcessID, len(s.procs))
	s.subj = make(map[agentkit.SubjectRef]agentkit.ProcessID, len(s.procs))
	s.sem = make(map[semKey]*semHolders)
	for id, p := range s.procs {
		// Only held, non-terminal rows hold a slot. A terminal row dropping out of
		// the set is what releases its slot without anyone writing a release.
		if p.Semaphore != nil && p.SemaphoreHeld && !p.Status.Terminal() {
			k := semKey{key: p.Semaphore.Key, value: p.Semaphore.Value}
			h, ok := s.sem[k]
			if !ok {
				h = &semHolders{
					roots:     map[agentkit.ProcessID]struct{}{},
					minSlots:  p.Semaphore.Slots,
					strictest: id,
				}
				s.sem[k] = h
			} else if p.Semaphore.Slots < h.minSlots {
				h.minSlots, h.strictest = p.Semaphore.Slots, id
			}
			h.roots[p.RootID] = struct{}{}
		}
		if p.IdempotencyKey != "" {
			if other, ok := s.idem[p.IdempotencyKey]; ok && other != id {
				return goerr.Wrap(agentkit.ErrConflict, "duplicate idempotency key",
					goerr.V("key", p.IdempotencyKey), goerr.V("process_id", id), goerr.V("existing", other))
			}
			s.idem[p.IdempotencyKey] = id
		}
		if !p.Status.Terminal() && p.Subject != nil {
			if other, ok := s.subj[*p.Subject]; ok && other != id {
				return goerr.Wrap(agentkit.ErrConflict, "duplicate open subject",
					goerr.V("subject", *p.Subject), goerr.V("process_id", id), goerr.V("existing", other))
			}
			s.subj[*p.Subject] = id
		}
	}
	return nil
}

// semaphoreAdmits reports whether p may be claimed under its semaphore. It is
// evaluated against the CURRENT state, so it and the claim write are one atomic
// step.
//
// It tests the PROSPECTIVE state — p added to the holder set — against the
// PROSPECTIVE limit, which is the smallest Slots among the holders p would join,
// p's own included. Both halves matter:
//
//   - p's own Slots must be in the min. A holder declaring 1 while p declares 2
//     means the limit is 1; reading p's figure alone would admit a second holder.
//   - "p's tree already holds it" is NOT sufficient on its own. With two trees
//     holding under Slots 2, a child of one of them declaring Slots 1 would be
//     waved through by an ancestor check, and the resulting state (2 holders,
//     limit 1) is one the Apply check refuses — which would make ClaimNext itself
//     fail on every poll.
//
// Only a row that ALREADY holds its slot short-circuits: it is in the index, so
// claiming it again changes neither the set nor the min.
func (s *State) semaphoreAdmits(p *agentkit.Process) bool {
	if p.Semaphore == nil || p.SemaphoreHeld {
		return true // no limit, or this row already holds its slot.
	}
	h, ok := s.sem[semKey{key: p.Semaphore.Key, value: p.Semaphore.Value}]
	if !ok {
		return true // nothing holds it; Slots >= 1 is enforced at Register.
	}
	roots := len(h.roots)
	if _, held := h.roots[p.RootID]; !held {
		roots++ // p's tree would be a new holder.
	}
	return roots <= min(h.minSlots, p.Semaphore.Slots)
}

// semaphoreRegression reports the first (key, value) whose occupancy next would
// make worse than it is in s. "Worse" is: over its limit AND either more holders
// than before, or the same holders under a smaller limit.
//
// This is a MONOTONICITY check, not an absolute one. An absolute "never over the
// limit" rule cannot be satisfied by the very writes that would fix an
// over-limit state — a terminal commit that drops one of three holders under a
// limit of one still leaves two — so a single over-limit key would reject every
// Apply on every Process, permanently, since a ChangeSet may touch several rows.
func (s *State) semaphoreRegression(next *State) error {
	for k, h := range next.sem {
		if len(h.roots) <= h.minSlots {
			continue // within the limit: nothing to object to.
		}
		before, existed := s.sem[k]
		if existed && len(h.roots) <= len(before.roots) && h.minSlots >= before.minSlots {
			continue // already over the limit and not made worse: let the write through.
		}
		return goerr.Wrap(agentkit.ErrConflict, "semaphore slots exceeded",
			goerr.V("key", k.key), goerr.V("value", k.value), goerr.V("slots", h.minSlots),
			goerr.V("holders", len(h.roots)), goerr.V("process_id", h.strictest))
	}
	return nil
}

// SemaphoreStatus reports the occupancy of one (key, value) pair. A pair nothing
// references returns a zero-valued status rather than an error.
//
// Held and Slots come from the derived index; Waiting and OldestWaiting need a
// scan of procs, because waiting rows are deliberately not indexed — they churn
// on every claim, and rebuildIndexes runs on every Apply, which is too much
// upkeep for one diagnostic read.
func (s *State) SemaphoreStatus(key, value string) *agentkit.SemaphoreStatus {
	out := &agentkit.SemaphoreStatus{Key: key, Value: value}
	if h, ok := s.sem[semKey{key: key, value: value}]; ok {
		out.Held = len(h.roots)
		out.Slots = h.minSlots
	}
	for _, p := range s.procs {
		if p.Semaphore == nil || p.SemaphoreHeld || p.Status.Terminal() {
			continue
		}
		if p.Semaphore.Key != key || p.Semaphore.Value != value {
			continue
		}
		out.Waiting++
		if out.OldestWaiting == nil || p.CreatedAt.Before(*out.OldestWaiting) {
			t := p.CreatedAt
			out.OldestWaiting = &t
		}
	}
	return out
}

// cloneProcess deep-copies a Process (mirrors the root package's clone).
func cloneProcess(p *agentkit.Process) *agentkit.Process {
	if p == nil {
		return nil
	}
	cp := *p
	if p.Metadata != nil {
		cp.Metadata = maps.Clone(p.Metadata)
	}
	if p.Output != nil {
		cp.Output = append([]byte(nil), p.Output...)
	}
	if p.State != nil {
		cp.State = append([]byte(nil), p.State...)
	}
	if p.Failure != nil {
		f := *p.Failure
		cp.Failure = &f
	}
	if p.Subject != nil {
		sub := *p.Subject
		cp.Subject = &sub
	}
	if p.Semaphore != nil {
		sem := *p.Semaphore
		cp.Semaphore = &sem
	}
	if p.InheritedHistory != nil {
		ih := *p.InheritedHistory
		cp.InheritedHistory = &ih
	}
	if p.ParentID != nil {
		id := *p.ParentID
		cp.ParentID = &id
	}
	if p.WakeAt != nil {
		t := *p.WakeAt
		cp.WakeAt = &t
	}
	if p.LeaseUntil != nil {
		t := *p.LeaseUntil
		cp.LeaseUntil = &t
	}
	return &cp
}

// cloneAwait deep-copies an Await.
func cloneAwait(a *agentkit.Await) *agentkit.Await {
	if a == nil {
		return nil
	}
	cp := *a
	if a.Deadline != nil {
		t := *a.Deadline
		cp.Deadline = &t
	}
	if a.Question != nil {
		cp.Question = append([]byte(nil), a.Question...)
	}
	if a.Response != nil {
		cp.Response = append([]byte(nil), a.Response...)
	}
	if a.Children != nil {
		cp.Children = append([]agentkit.ProcessID(nil), a.Children...)
	}
	if a.Results != nil {
		cp.Results = cloneChildResults(a.Results)
	}
	if a.RespondedAt != nil {
		t := *a.RespondedAt
		cp.RespondedAt = &t
	}
	return &cp
}

// cloneChildResults deep-copies a ChildResult slice.
func cloneChildResults(rs []agentkit.ChildResult) []agentkit.ChildResult {
	out := make([]agentkit.ChildResult, len(rs))
	for i, r := range rs {
		out[i] = r
		if r.Output != nil {
			out[i].Output = append([]byte(nil), r.Output...)
		}
		if r.Failure != nil {
			f := *r.Failure
			out[i].Failure = &f
		}
		// Metrics needs no copy: it is a struct of scalars, copied by out[i] = r.
	}
	return out
}

// cloneEvent deep-copies an Event.
func cloneEvent(e *agentkit.Event) *agentkit.Event {
	if e == nil {
		return nil
	}
	cp := *e
	if e.Payload != nil {
		cp.Payload = append([]byte(nil), e.Payload...)
	}
	return &cp
}
