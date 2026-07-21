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
}

// NewState returns an empty State.
func NewState() *State {
	return &State{
		procs:  map[agentkit.ProcessID]*agentkit.Process{},
		awaits: map[agentkit.ProcessID]map[agentkit.AwaitKey]*agentkit.Await{},
		events: map[agentkit.ProcessID][]*agentkit.Event{},
		idem:   map[string]agentkit.ProcessID{},
		subj:   map[agentkit.SubjectRef]agentkit.ProcessID{},
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

// ListEvents returns deep copies of a Process's events in append order.
func (s *State) ListEvents(pid agentkit.ProcessID) []*agentkit.Event {
	es := s.events[pid]
	out := make([]*agentkit.Event, len(es))
	for i, e := range es {
		out[i] = cloneEvent(e)
	}
	return out
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
	}
	return nil
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
		return cleanClaim
	case agentkit.ProcessWaiting:
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
	}
	maps.Copy(n.procs, s.procs)
	maps.Copy(n.awaits, s.awaits)
	maps.Copy(n.events, s.events)
	return n
}

// rebuildIndexes recomputes the idempotency and open-subject indexes from procs,
// returning ErrConflict on a uniqueness violation.
func (s *State) rebuildIndexes() error {
	s.idem = make(map[string]agentkit.ProcessID, len(s.procs))
	s.subj = make(map[agentkit.SubjectRef]agentkit.ProcessID, len(s.procs))
	for id, p := range s.procs {
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
	if p.Metrics != nil {
		cp.Metrics = maps.Clone(p.Metrics)
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
