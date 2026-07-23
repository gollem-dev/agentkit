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
// (syscalls.sessWorking); that copy becomes the new baseline only after the
// transition's Apply succeeds (commitHistory). This keeps a same-lease conflict
// retry (worker.go: i--; continue) re-seeding from committed state rather than
// from the abandoned attempt's history — the in-claim retry never re-reads the
// repo, so a Save that ran before a failed Apply cannot poison the next attempt.
// A crash between that Save and the Apply is the tolerated duplication window
// (ADR-0017, best-effort).
type historyState struct {
	repo     gollem.HistoryRepository // nil when the agent did not opt into WithHistoryRepository.
	pid      ProcessID
	baseline *gollem.History // last committed history; nil until loaded, or when none is stored.
	loaded   bool
}

// ensureLoaded fetches the committed history from the repository once per claim,
// on first use. It is only reached with a non-nil repo (SessionGenerate /
// SessionHistory reject a nil repo before calling it). A load error (including
// gollem's History version-gate mismatch) is propagated, never swallowed.
func (h *historyState) ensureLoaded(ctx context.Context) error {
	if h.loaded {
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

// SessionGenerate runs one LLM turn as part of the Process's managed
// conversation: the runtime carries History across calls (and, once committed,
// across steps and workers) and injects the claim's tools, so the strategy
// threads neither History nor tools by hand. History is persisted by the worker
// before the next commit (ADR-0017).
//
// It requires WithHistoryRepository; without it, it returns
// ErrHistoryNotConfigured rather than silently running without persistence. For
// a strategy that manages History itself (e.g. one that splits a tool round
// across steps), use the primitive Generate with WithHistory instead.
//
// Tools are bound from Tools() (gollem fixes tools at session construction, and
// a stable tool set is also what prompt caching wants). Extra GenerateOption
// values are applied after the injected History/Tools and so can override them.
func (s *syscalls) SessionGenerate(ctx context.Context, input []gollem.Input, opts ...GenerateOption) (*GenerateResult, error) {
	if s.hist == nil || s.hist.repo == nil {
		return nil, goerr.Wrap(ErrHistoryNotConfigured, "SessionGenerate requires WithHistoryRepository", goerr.V("agent", s.proc.Agent))
	}
	if err := s.hist.ensureLoaded(ctx); err != nil {
		return nil, err
	}
	if !s.sessStarted {
		s.sessWorking = s.hist.baseline
		s.sessStarted = true
	}
	full := make([]GenerateOption, 0, len(opts)+2)
	full = append(full, WithHistory(s.sessWorking), WithTools(s.tools...))
	full = append(full, opts...)
	res, err := s.Generate(ctx, input, full...)
	if err != nil {
		return nil, err
	}
	s.sessWorking = res.History
	s.sessDirty = true
	return res, nil
}

// SessionHistory returns the managed conversation's current history: the working
// copy once SessionGenerate has run this transition, otherwise the committed
// baseline (loaded from the repository on first use, so it reflects stored
// History even before the first SessionGenerate of a fresh claim). Like
// SessionGenerate it requires WithHistoryRepository (ErrHistoryNotConfigured
// otherwise); a load error is returned, never swallowed.
func (s *syscalls) SessionHistory(ctx context.Context) (*gollem.History, error) {
	if s.hist == nil || s.hist.repo == nil {
		return nil, goerr.Wrap(ErrHistoryNotConfigured, "SessionHistory requires WithHistoryRepository", goerr.V("agent", s.proc.Agent))
	}
	if err := s.hist.ensureLoaded(ctx); err != nil {
		return nil, err
	}
	if !s.sessStarted {
		return s.hist.baseline, nil
	}
	return s.sessWorking, nil
}

// historyPending reports whether this transition advanced the managed
// conversation, so the worker must save before committing. It is only ever true
// on an opted-in agent, since SessionGenerate rejects a nil repo before it can
// set sessDirty.
func (s *syscalls) historyPending() bool {
	return s.sessDirty && s.hist != nil && s.hist.repo != nil
}

// saveHistory persists the working history BEFORE the transition commits
// (ADR-0017: commit is the completion marker, so durable work precedes it). It
// is a no-op when the managed conversation was not used this transition. It does
// NOT advance the committed baseline — commitHistory does that, and only after
// Apply succeeds.
func (s *syscalls) saveHistory(ctx context.Context) error {
	if !s.historyPending() {
		return nil
	}
	if err := s.hist.repo.Save(ctx, string(s.hist.pid), s.sessWorking); err != nil {
		return goerr.Wrap(err, "save history", goerr.V("process", s.proc.ID))
	}
	return nil
}

// commitHistory advances the claim's committed baseline to this transition's
// working history, after a successful Apply, so a conflicted/abandoned attempt
// leaves the baseline untouched and the next attempt re-seeds from committed
// state.
func (s *syscalls) commitHistory() {
	if s.sessDirty {
		s.hist.baseline = s.sessWorking
	}
}
