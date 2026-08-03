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
// store, so a Save that ran before a failed Apply cannot poison the next attempt.
// A crash in that window leaves an unreferenced version instead, because the ref
// only becomes current when the commit records it (ADR-0017).
type historyState struct {
	store    HistoryStore // nil when the agent did not opt into WithHistoryStore.
	pid      ProcessID
	baseline *gollem.History // last committed history; nil until loaded, or when none is stored.
	loaded   bool
}

// ensureLoaded fetches the committed version from the store once per claim, on
// first use. Both ref and inherited come from the Process record read at the
// start of this transition: the record is the only source of "which version is
// current", so a retry can never disagree with the commit that set it.
//
// An empty ref means this Process has committed nothing yet. That is not an
// error — the conversation starts empty, unless the record also carries an
// InheritedHistory, in which case it starts from the version another Process
// committed, read under THAT Process's id (a store addresses a version by
// (pid, ref)). Once this Process commits a version of its own, ref names it and
// the inherited one is never read again.
//
// A load error (including gollem's History version-gate mismatch) is
// propagated, never swallowed.
func (h *historyState) ensureLoaded(ctx context.Context, ref HistoryRef, inherited *InheritedHistory) error {
	if h.loaded {
		return nil
	}
	pid, load := h.pid, ref
	if load == "" && inherited != nil {
		pid, load = inherited.Process, inherited.Ref
	}
	if load == "" {
		h.loaded = true
		return nil
	}
	hist, err := h.store.Load(ctx, pid, load)
	if err != nil {
		// pid, not h.pid: on an inherited load the two differ, and which one was
		// asked for is the first thing a reader of this error needs.
		return goerr.Wrap(err, "load history",
			goerr.V("process", pid), goerr.V("ref", load))
	}
	h.baseline = hist
	h.loaded = true
	return nil
}

// Session is the Process's managed conversation: the runtime carries History
// across calls and, once committed, across steps and workers, so a strategy
// threads neither History nor tools by hand. History is persisted by the worker
// before the next commit (ADR-0017).
//
// It is scoped to one transition, like the Syscalls it came from. Do not keep it
// in strategy state.
//
// Every method requires the agent to have been registered with WithHistoryStore
// and returns ErrHistoryNotConfigured otherwise, so the managed conversation is
// never silently run without persistence. For a strategy that manages History
// itself, the primitive Syscalls.Generate with WithHistory is still there.
type Session interface {
	// Generate runs one LLM turn in the managed conversation. Tools are bound
	// from Syscalls.Tools() (gollem fixes tools at session construction, and a
	// stable tool set is also what prompt caching wants). Extra GenerateOption
	// values are applied after the injected History/Tools and so can override
	// them.
	//
	// Passing no input at all is meaningful: it continues from the History as it
	// stands, which is what follows a CallTool that already appended its result.
	Generate(ctx context.Context, input []gollem.Input, opts ...GenerateOption) (*GenerateResult, error)

	// CallTool runs a tool call like Syscalls.CallTool and appends its result to
	// the conversation, so the History a Step boundary commits can end on a closed
	// tool_use/tool_result pair without spending another LLM turn.
	//
	// Use it for a call the MODEL asked for. For a call the strategy makes on its
	// own initiative, use Syscalls.CallTool: appending a tool_response with no
	// matching tool_use would corrupt the conversation, and the kernel cannot tell
	// the two apart.
	//
	// Exactly one tool_response is appended per call, whatever the outcome. On an
	// error (unknown tool, invalid arguments, a failing Run, or a middleware that
	// refused) the appended result carries IsError with the error text, and the
	// error is ALSO returned. Leaving the pair open is not an option — the model
	// asked for this call and the next request has to answer it.
	//
	// It needs the conversation to already hold a History (a Generate earlier in
	// this transition, or a committed one), because a History carries the provider
	// identity and the kernel cannot invent it; otherwise ErrInvalidRequest.
	CallTool(ctx context.Context, call gollem.FunctionCall) (map[string]any, error)

	// History returns the conversation's current history: the working copy once
	// this transition has advanced it, otherwise the committed version (loaded on
	// first use, so it reflects stored History even before the first Generate of a
	// fresh claim).
	History(ctx context.Context) (*gollem.History, error)

	session() // sealed, like the unexported spawn on Syscalls.
}

// managedSession is the Session implementation. It is a separate type rather
// than syscalls itself because syscalls already carries Generate and CallTool
// with different semantics (no managed History, no tool injection).
type managedSession struct{ s *syscalls }

func (managedSession) session() {}

// Session returns this Process's managed conversation. It always returns a
// usable handle; a missing store is reported by the handle's methods, so a
// misconfiguration surfaces as ErrHistoryNotConfigured at the point of use
// rather than as a nil check the caller can forget.
func (s *syscalls) Session() Session { return managedSession{s} }

// ready rejects a managed-conversation call on an agent without a store, then
// loads the committed baseline. The store check comes first so a
// misconfiguration surfaces as ErrHistoryNotConfigured rather than as a nil
// dereference inside ensureLoaded.
func (s *syscalls) ready(ctx context.Context) error {
	if s.hist == nil || s.hist.store == nil {
		return goerr.Wrap(ErrHistoryNotConfigured, "the managed conversation requires WithHistoryStore",
			goerr.V("agent", s.proc.Agent))
	}
	return s.hist.ensureLoaded(ctx, s.proc.HistoryRef, s.proc.InheritedHistory)
}

// start seeds this transition's working copy from the committed baseline on
// first use.
func (s *syscalls) start() {
	if !s.sessStarted {
		s.sessWorking = s.hist.baseline
		s.sessStarted = true
	}
}

func (c managedSession) Generate(ctx context.Context, input []gollem.Input, opts ...GenerateOption) (*GenerateResult, error) {
	s := c.s
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	s.start()
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

func (c managedSession) CallTool(ctx context.Context, call gollem.FunctionCall) (map[string]any, error) {
	s := c.s
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	s.start()
	if s.sessWorking == nil {
		return nil, goerr.Wrap(ErrInvalidRequest, "Session().CallTool before any Session().Generate",
			goerr.V("tool", call.Name))
	}

	out, terr := s.CallTool(ctx, call)
	resp, isErr := out, false
	if terr != nil {
		resp, isErr = map[string]any{"error": terr.Error()}, true
	}
	content, cerr := gollem.NewToolResponseContent(call.ID, call.Name, resp, isErr)
	if cerr != nil {
		// The tool already ran, but the conversation cannot be closed on it. Fail
		// the transition rather than commit a History with an open pair.
		return out, goerr.Wrap(cerr, "build tool response content", goerr.V("tool", call.Name))
	}

	// Clone before appending: sessWorking may still be the claim's committed
	// baseline, which a same-lease retry re-seeds from and must not observe this
	// transition's writes.
	next := s.sessWorking.Clone()
	next.Messages = append(next.Messages, gollem.Message{
		Role:     gollem.RoleTool,
		Contents: []gollem.MessageContent{content},
	})
	s.sessWorking = next
	s.sessDirty = true
	return out, terr
}

func (c managedSession) History(ctx context.Context) (*gollem.History, error) {
	s := c.s
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	// A copy, not the live value. *gollem.History is mutable, and the committed
	// baseline is what a same-lease retry re-seeds from — handing it out would let
	// a strategy edit the state this transition is supposed to be able to roll
	// back to. Advancing the conversation goes through Generate and CallTool.
	if !s.sessStarted {
		return s.hist.baseline.Clone(), nil
	}
	return s.sessWorking.Clone(), nil
}

// historyPending reports whether this transition advanced the managed
// conversation, so the worker must save before committing. It is only ever true
// on an opted-in agent, since every Session method rejects a nil store before it
// can set sessDirty.
func (s *syscalls) historyPending() bool {
	return s.sessDirty && s.hist != nil && s.hist.store != nil
}

// saveHistory persists the working history as a NEW version BEFORE the
// transition commits (ADR-0017: commit is the completion marker, so durable work
// precedes it), and records the ref the commit will publish. It is a no-op when
// the managed conversation was not used this transition. It does NOT advance the
// committed baseline — commitHistory does that, and only after Apply succeeds.
func (s *syscalls) saveHistory(ctx context.Context) error {
	if !s.historyPending() {
		return nil
	}
	ref, err := s.hist.store.Save(ctx, s.hist.pid, s.sessWorking)
	if err != nil {
		return goerr.Wrap(err, "save history", goerr.V("process", s.proc.ID))
	}
	if ref == "" {
		// "" is the record's way of saying "nothing committed yet", so writing it
		// would skip the next Load and silently truncate the conversation.
		return goerr.Wrap(ErrInvalidConfig, "history store returned an empty ref",
			goerr.V("process", s.proc.ID), goerr.V("agent", s.proc.Agent))
	}
	s.sessRef = ref
	return nil
}

// nextHistoryRef is the ref this transition's commit records: the version
// saveHistory just wrote, or the committed one unchanged when the managed
// conversation did not advance. History and State do not have to move in step,
// only to agree.
func (s *syscalls) nextHistoryRef() HistoryRef {
	if s.sessRef == "" {
		return s.proc.HistoryRef
	}
	return s.sessRef
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

// discardHistory tells the store a version is no longer referenced. There is no
// outcome to check — Discard is a notification (ADR-0017) — so the guard is only
// for an agent without a store and for the "nothing committed yet" ref.
func discardHistory(ctx context.Context, store HistoryStore, pid ProcessID, ref HistoryRef) {
	if store == nil || ref == "" {
		return
	}
	store.Discard(ctx, pid, ref)
}

// discardSuperseded releases the version a committed transition replaced. It is
// a no-op when the transition wrote none, and when the store handed back the ref
// it already had (a content-addressed store re-saving identical content), where
// discarding would release the version the record still names.
func discardSuperseded(ctx context.Context, store HistoryStore, pid ProcessID, prev, next HistoryRef) {
	if next == "" || next == prev {
		return
	}
	discardHistory(ctx, store, pid, prev)
}

// discardUncommitted releases the version an attempt saved but did not commit.
// Callers must be certain the commit did not happen; an unknown outcome keeps
// the version. The same-ref guard as discardSuperseded applies for the same
// reason: a content-addressed store may hand back the committed ref for
// identical content, and that one is still named by the record.
func discardUncommitted(ctx context.Context, store HistoryStore, pid ProcessID, committed, saved HistoryRef) {
	if saved == "" || saved == committed {
		return
	}
	discardHistory(ctx, store, pid, saved)
}
