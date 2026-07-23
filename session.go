package agentkit

import (
	"context"

	"github.com/gollem-dev/gollem"
	"github.com/m-mizutani/goerr/v2"
)

// historyState is the claim-scoped holder of a Process's committed conversation
// History. There is one per claim (not per transition): a claim runs several
// transitions in a loop, and re-loading the (potentially large) History blob on
// every transition would be wasteful, so the committed baseline is kept in
// memory and advanced only when a transition commits.
//
// baseline is the LAST COMMITTED history. A transition works on a separate copy
// held by its Session; that copy becomes the new baseline only after the
// transition's Apply succeeds (see syscalls.commitHistory). This is what keeps a
// same-lease conflict retry (worker.go: i--; continue) re-seeding from committed
// state rather than from the abandoned attempt's history — the in-claim retry
// never re-reads the repo, so a Save that ran before a failed Apply cannot poison
// the next attempt. A crash between that Save and the Apply is the tolerated
// duplication window (ADR-0017, best-effort).
type historyState struct {
	repo     gollem.HistoryRepository // nil = History persistence not opted in (Session is claim-local only).
	pid      ProcessID
	baseline *gollem.History // last committed history; nil until loaded, or when none is stored.
	loaded   bool
}

// ensureLoaded fetches the committed history from the repository once per claim,
// on first use. With no repository (opt-out) it is a no-op and baseline stays
// nil, so the Session runs with an empty, claim-local history. A load error
// (including gollem's History version-gate mismatch) is propagated, never
// silently swallowed into an empty history.
func (h *historyState) ensureLoaded(ctx context.Context) error {
	if h.loaded {
		return nil
	}
	if h.repo == nil {
		h.loaded = true
		return nil
	}
	hist, err := h.repo.Load(ctx, string(h.pid))
	if err != nil {
		return goerr.Wrap(err, "load history", goerr.V("process", h.pid))
	}
	h.baseline = hist
	h.loaded = true
	return nil
}

// Session is a conversation handle that carries History across Generate calls.
// It is obtained from Syscalls.Session() and is scoped to one transition; the
// committed history it starts from and advances lives in the claim-scoped
// historyState. Session.Generate injects the carried History and the claim's
// tools automatically, so a strategy does not thread either by hand.
//
// Tools are bound from sys.Tools() at Generate time (gollem fixes tools at
// session construction, and a stable tool set is also what prompt caching
// wants). Callers may pass extra GenerateOption values; they are applied after
// the injected History/Tools and so can override them.
type Session struct {
	sys     *syscalls
	working *gollem.History // this transition's history; seeded from the committed baseline on first Generate.
	started bool            // whether working has been seeded from the baseline yet.
	dirty   bool            // whether a Generate advanced working this transition (⇒ save before commit).
}

// Generate runs one LLM turn with the carried History and the claim's tools
// injected, then folds the resulting History back into the session. It goes
// through the same middleware / Limiter / Metrics path as Syscalls.Generate.
func (s *Session) Generate(ctx context.Context, input []gollem.Input, opts ...GenerateOption) (*GenerateResult, error) {
	if err := s.sys.hist.ensureLoaded(ctx); err != nil {
		return nil, err
	}
	if !s.started {
		s.working = s.sys.hist.baseline
		s.started = true
	}
	full := make([]GenerateOption, 0, len(opts)+2)
	full = append(full, WithHistory(s.working), WithTools(s.sys.Tools()...))
	full = append(full, opts...)
	res, err := s.sys.Generate(ctx, input, full...)
	if err != nil {
		return nil, err
	}
	s.working = res.History
	s.dirty = true
	return res, nil
}

// History returns the session's current history: the working copy once a
// Generate has run this transition, otherwise the committed baseline. It loads
// the baseline from the repository on first use (like Generate), so it reflects
// stored History even before the first Generate of a fresh claim; a load error
// (including a gollem version mismatch) is returned, never swallowed.
func (s *Session) History(ctx context.Context) (*gollem.History, error) {
	if err := s.sys.hist.ensureLoaded(ctx); err != nil {
		return nil, err
	}
	if !s.started {
		return s.sys.hist.baseline, nil
	}
	return s.working, nil
}

// Session returns the conversation Session for this transition, creating it on
// first use. The same handle is returned for the rest of the transition.
func (s *syscalls) Session() *Session {
	if s.session == nil {
		s.session = &Session{sys: s}
	}
	return s.session
}

// historyPending reports whether this transition has an opted-in History write
// waiting (a used session on an agent with a repository). The worker checks it
// before saving so it can fence the blob write against a lost lease.
func (s *syscalls) historyPending() bool {
	return s.session != nil && s.session.dirty && s.hist.repo != nil
}

// saveHistory persists the session's working history to the agent's
// HistoryRepository BEFORE the transition commits (ADR-0017: commit is the
// completion marker, so durable work precedes it). It is a no-op when the agent
// did not opt in (repo == nil) or the session was not used this transition
// (not dirty). It does NOT advance the committed baseline — commitHistory does
// that, and only after Apply succeeds.
func (s *syscalls) saveHistory(ctx context.Context) error {
	if s.session == nil || !s.session.dirty || s.hist.repo == nil {
		return nil
	}
	if err := s.hist.repo.Save(ctx, string(s.hist.pid), s.session.working); err != nil {
		return goerr.Wrap(err, "save history", goerr.V("process", s.proc.ID))
	}
	return nil
}

// commitHistory advances the claim's committed baseline to this transition's
// working history. The worker calls it only after a successful Apply, so a
// conflicted/abandoned attempt leaves the baseline untouched and the next
// attempt re-seeds from committed state.
func (s *syscalls) commitHistory() {
	if s.session != nil && s.session.dirty {
		s.hist.baseline = s.session.working
	}
}
