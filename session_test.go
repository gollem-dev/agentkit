package agentkit_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gollem-dev/agentkit"
	histmem "github.com/gollem-dev/agentkit/historystore/memory"
	"github.com/gollem-dev/agentkit/repository/memory"
	"github.com/gollem-dev/gollem"
	"github.com/gollem-dev/gollem/mock"
	"github.com/m-mizutani/gt"
)

// growingLLM returns a client whose History() is the seeded history plus one
// message. Session().Generate seeds each call with the carried history, so across
// transitions the message count grows by one per Generate — a distinguishable
// signal for asserting that History was carried and (with a store) persisted.
func growingLLM() gollem.LLMClient {
	return &mock.LLMClientMock{
		NewSessionFunc: func(_ context.Context, opts ...gollem.SessionOption) (gollem.Session, error) {
			cfg := gollem.NewSessionConfig(opts...)
			var seeded []gollem.Message
			if h := cfg.History(); h != nil {
				seeded = h.Messages
			}
			return &mock.SessionMock{
				GenerateFunc: func(_ context.Context, _ []gollem.Input, _ ...gollem.GenerateOption) (*gollem.Response, error) {
					return &gollem.Response{Texts: []string{"ok"}, InputToken: 1, OutputToken: 1}, nil
				},
				HistoryFunc: func() (*gollem.History, error) {
					grown := make([]gollem.Message, len(seeded)+1)
					copy(grown, seeded)
					return &gollem.History{LLType: gollem.LLMTypeClaude, Version: gollem.HistoryVersion, Messages: grown}, nil
				},
			}, nil
		},
	}
}

func histLen(h *gollem.History) int {
	if h == nil {
		return 0
	}
	return len(h.Messages)
}

// committedHistory loads the version the Process record names. It is how a test
// reads "the History this Process actually committed", as opposed to whatever
// versions happen to be lying in the store.
func committedHistory(t *testing.T, store agentkit.HistoryStore, p *agentkit.Process) *gollem.History {
	t.Helper()
	if p.HistoryRef == "" {
		return nil
	}
	h, err := store.Load(context.Background(), p.ID, p.HistoryRef)
	gt.NoError(t, err)
	return h
}

// sessionStep builds a step that records the carried-in History length (before
// Generate), runs one Session().Generate, and Continues until `turns` generates
// then Dones. seen captures the carried-in lengths across transitions/attempts.
func sessionStep(seen *[]int, mu *sync.Mutex, turns int) stepFn {
	return func(ctx context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		h, _ := sys.Session().History(ctx)
		mu.Lock()
		*seen = append(*seen, histLen(h))
		mu.Unlock()
		if _, err := sys.Session().Generate(ctx, []gollem.Input{gollem.Text("hi")}); err != nil {
			return st, agentkit.Decision[[]byte]{}, err
		}
		st.N++
		if st.N >= turns {
			return st, agentkit.Done([]byte("done")), nil
		}
		return st, agentkit.Continue[[]byte](), nil
	}
}

func registerWithHistory(t *testing.T, step stepFn, model gollem.LLMClient, hs agentkit.HistoryStore, kopts ...agentkit.KernelOption) (*agentkit.Kernel, agentkit.Repository, agentkit.Agent[scriptInput]) {
	t.Helper()
	repo := memory.New()
	reg := agentkit.NewRegistry()
	ag, err := agentkit.Register(reg, "main", 1, &scriptStrategy{step: step}, agentkit.WithHistoryStore[[]byte](hs))
	gt.NoError(t, err)
	k, err := agentkit.New(repo, model, reg, kopts...)
	gt.NoError(t, err)
	return k, repo, ag
}

// histCall is one (pid, ref) pair a probeStore was asked about. The pid matters
// once a Process can start from a version another one committed: "which key
// space was read" and "whose version was released" are then different questions
// from "how many times".
type histCall struct {
	pid agentkit.ProcessID
	ref agentkit.HistoryRef
}

// probeStore wraps a HistoryStore, recording every Save (message length and the
// ref handed back), every Load and every Discard, in order. It can inject
// Load/Save failures or an empty ref.
type probeStore struct {
	inner     agentkit.HistoryStore
	mu        sync.Mutex
	saves     []int
	savedRefs []agentkit.HistoryRef
	discards  []agentkit.HistoryRef
	loadCalls []histCall
	discCalls []histCall
	loads     int
	loadErr   error
	saveErr   error
	emptyRef  bool
}

func (s *probeStore) Save(ctx context.Context, pid agentkit.ProcessID, h *gollem.History) (agentkit.HistoryRef, error) {
	s.mu.Lock()
	s.saves = append(s.saves, histLen(h))
	se, empty := s.saveErr, s.emptyRef
	s.mu.Unlock()
	if se != nil {
		return "", se
	}
	ref, err := s.inner.Save(ctx, pid, h)
	if err == nil {
		s.mu.Lock()
		s.savedRefs = append(s.savedRefs, ref)
		s.mu.Unlock()
	}
	if empty {
		return "", nil
	}
	return ref, err
}

func (s *probeStore) Load(ctx context.Context, pid agentkit.ProcessID, ref agentkit.HistoryRef) (*gollem.History, error) {
	s.mu.Lock()
	s.loads++
	s.loadCalls = append(s.loadCalls, histCall{pid: pid, ref: ref})
	le := s.loadErr
	s.mu.Unlock()
	if le != nil {
		return nil, le
	}
	return s.inner.Load(ctx, pid, ref)
}

func (s *probeStore) Discard(ctx context.Context, pid agentkit.ProcessID, ref agentkit.HistoryRef) {
	s.mu.Lock()
	s.discards = append(s.discards, ref)
	s.discCalls = append(s.discCalls, histCall{pid: pid, ref: ref})
	s.mu.Unlock()
	s.inner.Discard(ctx, pid, ref)
}

func (s *probeStore) setLoadErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadErr = err
}

func (s *probeStore) saveCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.saves)
}

func (s *probeStore) discarded() []agentkit.HistoryRef {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]agentkit.HistoryRef(nil), s.discards...)
}

func (s *probeStore) saved() []agentkit.HistoryRef {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]agentkit.HistoryRef(nil), s.savedRefs...)
}

// loaded returns the (pid, ref) pairs Load was called with, in order.
func (s *probeStore) loaded() []histCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]histCall(nil), s.loadCalls...)
}

// discardedPairs returns the (pid, ref) pairs Discard was called with, in order.
func (s *probeStore) discardedPairs() []histCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]histCall(nil), s.discCalls...)
}

// fragileRepo wraps a Repository and injects `err` into the first Apply that
// runs while armed (one-shot). Used to simulate a same-lease conflict
// (ErrConflict) or a crash at commit (a generic error).
type fragileRepo struct {
	agentkit.Repository
	armed atomic.Bool
	fired atomic.Bool
	err   error
}

func (r *fragileRepo) Apply(ctx context.Context, cs agentkit.ChangeSet) error {
	if r.armed.Load() && r.fired.CompareAndSwap(false, true) {
		return r.err
	}
	return r.Repository.Apply(ctx, cs)
}

// ---- persistence & carry ----

func TestSession_PersistsAcrossTransitionsAndTerminal(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	var seen []int
	hs := histmem.New()
	k, repo, ag := registerWithHistory(t, sessionStep(&seen, &mu, 3), growingLLM(), hs)

	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	p := serveUntil(t, k, repo, pid, 5*time.Second, isTerminal)
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)

	mu.Lock()
	got := append([]int(nil), seen...)
	mu.Unlock()
	gt.Value(t, got).Equal([]int{0, 1, 2})

	gt.Value(t, histLen(committedHistory(t, hs, p))).Equal(3) // terminal transition saved too (D-D).
}

// TestSession_PersistsAcrossClaims forces one transition per claim so History
// load/save genuinely crosses claim boundaries (not just in-memory carry).
func TestSession_PersistsAcrossClaims(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	var seen []int
	hs := histmem.New()
	k, repo, ag := registerWithHistory(t, sessionStep(&seen, &mu, 3), growingLLM(), hs)

	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	p := serveUntil(t, k, repo, pid, 5*time.Second, isTerminal, agentkit.WithMaxStepsPerClaim(1))
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)

	mu.Lock()
	got := append([]int(nil), seen...)
	mu.Unlock()
	gt.Value(t, got).Equal([]int{0, 1, 2}) // each new claim loaded the version the record named.

	gt.Value(t, histLen(committedHistory(t, hs, p))).Equal(3)
}

// TestSession_WithoutStoreErrors verifies that using the managed conversation on
// an agent NOT registered with WithHistoryStore fails loudly
// (ErrHistoryNotConfigured) instead of silently running without persistence.
// Session() itself still hands back a usable handle — the error belongs at the
// point of use, not to a nil check the caller can forget.
func TestSession_WithoutStoreErrors(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	var genErr, histErr, toolErr error
	var handleNil bool
	step := func(ctx context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		sess := sys.Session()
		_, he := sess.History(ctx)
		_, te := sess.CallTool(ctx, gollem.FunctionCall{ID: "c1", Name: "t"})
		_, ge := sess.Generate(ctx, []gollem.Input{gollem.Text("hi")})
		mu.Lock()
		handleNil, histErr, toolErr, genErr = sess == nil, he, te, ge
		mu.Unlock()
		return st, agentkit.Decision[[]byte]{}, ge
	}
	repo := memory.New()
	reg := agentkit.NewRegistry()
	ag, err := agentkit.Register(reg, "main", 1, &scriptStrategy{step: step}) // no WithHistoryStore.
	gt.NoError(t, err)
	k, err := agentkit.New(repo, growingLLM(), reg)
	gt.NoError(t, err)

	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	p := serveUntil(t, k, repo, pid, 5*time.Second, isTerminal, agentkit.WithMaxStepAttempts(1))
	gt.Value(t, p.Status).Equal(agentkit.ProcessFailed)

	mu.Lock()
	defer mu.Unlock()
	gt.Value(t, handleNil).Equal(false)
	gt.Value(t, errors.Is(genErr, agentkit.ErrHistoryNotConfigured)).Equal(true)
	gt.Value(t, errors.Is(histErr, agentkit.ErrHistoryNotConfigured)).Equal(true)
	gt.Value(t, errors.Is(toolErr, agentkit.ErrHistoryNotConfigured)).Equal(true)
}

// ---- error paths ----

// A fresh Process names no version, so its first transition loads nothing. The
// load only happens once a ref is committed, which is what this arms for.
func TestSession_LoadErrorFailsTransition(t *testing.T) {
	ctx := context.Background()
	store := &probeStore{inner: histmem.New()}
	step := func(ctx context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		if _, err := sys.Session().Generate(ctx, []gollem.Input{gollem.Text("hi")}); err != nil {
			return st, agentkit.Decision[[]byte]{}, err
		}
		store.setLoadErr(gollemErr("load boom")) // breaks the NEXT claim's load.
		st.N++
		if st.N >= 3 {
			return st, agentkit.Done([]byte("done")), nil
		}
		return st, agentkit.Continue[[]byte](), nil
	}
	k, repo, ag := registerWithHistory(t, step, growingLLM(), store)

	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	p := serveUntil(t, k, repo, pid, 5*time.Second, isTerminal,
		agentkit.WithMaxStepsPerClaim(1), agentkit.WithMaxStepAttempts(1))
	gt.Value(t, p.Status).Equal(agentkit.ProcessFailed)
	gt.Value(t, p.Failure.Code).Equal(agentkit.FailureRetryExhausted)
}

// TestSession_HistoryMethodSurfacesLoadError checks that Session().History
// propagates a load failure to the strategy rather than returning nil silently.
func TestSession_HistoryMethodSurfacesLoadError(t *testing.T) {
	ctx := context.Background()
	store := &probeStore{inner: histmem.New()}
	step := func(ctx context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		if st.N == 0 {
			// Commit a version first: there is nothing to fail to load until then.
			if _, err := sys.Session().Generate(ctx, []gollem.Input{gollem.Text("hi")}); err != nil {
				return st, agentkit.Decision[[]byte]{}, err
			}
			store.setLoadErr(gollemErr("load boom"))
			st.N = 1
			return st, agentkit.Continue[[]byte](), nil
		}
		if _, herr := sys.Session().History(ctx); herr != nil {
			return st, agentkit.Decision[[]byte]{}, herr
		}
		return st, agentkit.Done([]byte("x")), nil
	}
	k, repo, ag := registerWithHistory(t, step, growingLLM(), store)

	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	p := serveUntil(t, k, repo, pid, 5*time.Second, isTerminal,
		agentkit.WithMaxStepsPerClaim(1), agentkit.WithMaxStepAttempts(1))
	gt.Value(t, p.Status).Equal(agentkit.ProcessFailed)
	gt.Value(t, p.Failure.Code).Equal(agentkit.FailureRetryExhausted)
}

func TestSession_SaveErrorRequeues(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	var seen []int
	store := &probeStore{inner: histmem.New(), saveErr: gollemErr("save boom")}
	k, repo, ag := registerWithHistory(t, sessionStep(&seen, &mu, 3), growingLLM(), store)

	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	p := serveUntil(t, k, repo, pid, 5*time.Second, isTerminal, agentkit.WithMaxStepAttempts(1))
	gt.Value(t, p.Status).Equal(agentkit.ProcessFailed)
	gt.Value(t, p.Failure.Code).Equal(agentkit.FailureRetryExhausted)
	gt.Value(t, p.HistoryRef).Equal(agentkit.HistoryRef("")) // nothing was committed.
}

// The save runs before the terminal commit too, and its failure has to stop that
// commit. turns=1 makes the very first decision a Done, so this exercises the
// terminal path rather than the Continue one.
func TestSession_SaveErrorOnTerminalDoesNotCommit(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	var seen []int
	store := &probeStore{inner: histmem.New(), saveErr: gollemErr("save boom")}
	k, repo, ag := registerWithHistory(t, sessionStep(&seen, &mu, 1), growingLLM(), store)

	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	p := serveUntil(t, k, repo, pid, 5*time.Second, isTerminal, agentkit.WithMaxStepAttempts(1))

	// Terminal by exhaustion, never by the Done the strategy returned.
	gt.Value(t, p.Status).Equal(agentkit.ProcessFailed)
	gt.Value(t, p.Failure.Code).Equal(agentkit.FailureRetryExhausted)
	gt.Value(t, p.HistoryRef).Equal(agentkit.HistoryRef(""))
	gt.Array(t, store.discarded()).Length(0)
}

// A terminal Apply that fails with anything other than a conflict leaves the
// outcome unknown. Nothing may be released: neither the version this attempt
// saved nor the one the record still names.
func TestSession_TerminalApplyErrorReleasesNothing(t *testing.T) {
	ctx := context.Background()
	inner := memory.New()
	repo := &fragileRepo{Repository: inner, err: gollemErr("disk gone at terminal commit")}
	store := &probeStore{inner: histmem.New()}
	reg := agentkit.NewRegistry()
	step := func(ctx context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		if _, err := sys.Session().Generate(ctx, []gollem.Input{gollem.Text("hi")}); err != nil {
			return st, agentkit.Decision[[]byte]{}, err
		}
		if st.N == 0 {
			st.N = 1
			return st, agentkit.Continue[[]byte](), nil
		}
		repo.armed.Store(true) // break the terminal Apply, after this transition's save.
		return st, agentkit.Done([]byte("done")), nil
	}
	ag, err := agentkit.Register(reg, "main", 1, &scriptStrategy{step: step}, agentkit.WithHistoryStore[[]byte](store))
	gt.NoError(t, err)
	k, err := agentkit.New(repo, growingLLM(), reg)
	gt.NoError(t, err)

	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	// The abandoned claim leaves a running row; reclaiming it counts as unclean,
	// and a budget of 0 turns that into a terminal failure instead of a retry, so
	// the state right after the broken Apply is what the test observes.
	p := serveUntil(t, k, repo, pid, 5*time.Second, isTerminal,
		agentkit.WithLease(80*time.Millisecond), agentkit.WithMaxUncleanReclaims(0))
	gt.Value(t, p.Status).Equal(agentkit.ProcessFailed)
	gt.Value(t, p.Failure.Code).Equal(agentkit.FailureUncleanReclaim)

	saved := store.saved()
	gt.Array(t, saved).Length(2) // the Continue transition, then the terminal attempt.
	// The record still names the first version, and neither it nor the terminal
	// attempt's version was released.
	gt.Value(t, p.HistoryRef).Equal(saved[0])
	gt.Array(t, store.discarded()).Length(0)
	gt.Value(t, histLen(committedHistory(t, store, p))).Equal(1)
}

// An empty ref is the record's way of saying "nothing committed yet", so a store
// that returns one would make the next Load be skipped and silently truncate the
// conversation. The transition must fail instead.
func TestSession_SaveEmptyRefFailsTransition(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	var seen []int
	store := &probeStore{inner: histmem.New(), emptyRef: true}
	k, repo, ag := registerWithHistory(t, sessionStep(&seen, &mu, 3), growingLLM(), store)

	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	p := serveUntil(t, k, repo, pid, 5*time.Second, isTerminal, agentkit.WithMaxStepAttempts(1))
	gt.Value(t, p.Status).Equal(agentkit.ProcessFailed)
	gt.Value(t, p.Failure.Code).Equal(agentkit.FailureRetryExhausted)
	gt.Value(t, p.HistoryRef).Equal(agentkit.HistoryRef(""))
}

// ---- ordering ----

// orderStore records, at Save time, the process's currently-committed StateSeq
// (read from the process repo). save-before-commit means the value observed is
// the pre-commit StateSeq.
type orderStore struct {
	proc      agentkit.Repository
	inner     agentkit.HistoryStore
	mu        sync.Mutex
	seqOnSave []int
}

func (s *orderStore) Load(ctx context.Context, pid agentkit.ProcessID, ref agentkit.HistoryRef) (*gollem.History, error) {
	return s.inner.Load(ctx, pid, ref)
}

func (s *orderStore) Save(ctx context.Context, pid agentkit.ProcessID, h *gollem.History) (agentkit.HistoryRef, error) {
	if p, err := s.proc.GetProcess(ctx, pid); err == nil {
		s.mu.Lock()
		s.seqOnSave = append(s.seqOnSave, p.StateSeq)
		s.mu.Unlock()
	}
	return s.inner.Save(ctx, pid, h)
}

func (s *orderStore) Discard(ctx context.Context, pid agentkit.ProcessID, ref agentkit.HistoryRef) {
	s.inner.Discard(ctx, pid, ref)
}

func TestSession_SaveHappensBeforeCommit(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	var seen []int
	repo := memory.New()
	hs := &orderStore{proc: repo, inner: histmem.New()}
	reg := agentkit.NewRegistry()
	ag, err := agentkit.Register(reg, "main", 1, &scriptStrategy{step: sessionStep(&seen, &mu, 1)}, agentkit.WithHistoryStore[[]byte](hs))
	gt.NoError(t, err)
	k, err := agentkit.New(repo, growingLLM(), reg)
	gt.NoError(t, err)

	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	p := serveUntil(t, k, repo, pid, 5*time.Second, isTerminal)
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)
	gt.Value(t, p.StateSeq).Equal(1)

	hs.mu.Lock()
	seqs := append([]int(nil), hs.seqOnSave...)
	hs.mu.Unlock()
	gt.Value(t, len(seqs)).Equal(1)
	gt.Value(t, seqs[0]).Equal(0) // saved before the commit that bumped StateSeq to 1.
}

// ---- conflict / re-seed (same lease) ----

// TestSession_SameLeaseConflictReseeds injects one ErrConflict on the second
// transition's Apply. The same-lease retry must re-seed from the committed
// version, NOT from the aborted attempt's history, so no turn is duplicated —
// and the version that attempt saved must be released, since a conflict proves
// nothing committed.
func TestSession_SameLeaseConflictReseeds(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	var seen []int
	inner := memory.New()
	repo := &fragileRepo{Repository: inner, err: agentkit.ErrConflict}
	store := &probeStore{inner: histmem.New()}
	reg := agentkit.NewRegistry()
	step := func(ctx context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		h, _ := sys.Session().History(ctx)
		mu.Lock()
		seen = append(seen, histLen(h))
		mu.Unlock()
		if st.N == 1 {
			repo.armed.Store(true) // arm the one-shot conflict on entering the 2nd transition.
		}
		if _, err := sys.Session().Generate(ctx, []gollem.Input{gollem.Text("hi")}); err != nil {
			return st, agentkit.Decision[[]byte]{}, err
		}
		st.N++
		if st.N >= 3 {
			return st, agentkit.Done([]byte("done")), nil
		}
		return st, agentkit.Continue[[]byte](), nil
	}
	ag, err := agentkit.Register(reg, "main", 1, &scriptStrategy{step: step}, agentkit.WithHistoryStore[[]byte](store))
	gt.NoError(t, err)
	k, err := agentkit.New(repo, growingLLM(), reg)
	gt.NoError(t, err)

	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	p := serveUntil(t, k, repo, pid, 5*time.Second, isTerminal)
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)

	mu.Lock()
	got := append([]int(nil), seen...)
	mu.Unlock()
	// T1=0, T2-attempt1=1, T2-attempt2=1 (re-seeded from the committed version, not 2), T3=2.
	gt.Value(t, got).Equal([]int{0, 1, 1, 2})

	gt.Value(t, histLen(committedHistory(t, store, p))).Equal(3) // no duplication: 3 turns, not 4.

	// One save per attempt: T1, T2-attempt1 (conflicted), T2-attempt2, T3.
	saved := store.saved()
	gt.Array(t, saved).Length(4)
	// The conflicted attempt's version is released — a conflict proves nothing
	// committed — and it never became the committed one.
	gt.Value(t, slices.Contains(store.discarded(), saved[1])).Equal(true)
	gt.Value(t, saved[1]).NotEqual(p.HistoryRef)
	// The committed version itself is not released.
	gt.Value(t, slices.Contains(store.discarded(), p.HistoryRef)).Equal(false)
}

// ---- crash between save and commit ----

// TestSession_CrashBetweenSaveAndCommitRollsBack injects a non-conflict Apply
// error on the second transition (a crash at commit, after the History save
// already ran). The claim abandons; a re-claim re-runs from committed State and
// loads the version the record still names — the saved-but-uncommitted one is
// never reachable, so the conversation carries no duplicated turn.
func TestSession_CrashBetweenSaveAndCommitRollsBack(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	var seen []int
	inner := memory.New()
	repo := &fragileRepo{Repository: inner, err: gollemErr("disk gone at commit")}
	store := &probeStore{inner: histmem.New()}
	reg := agentkit.NewRegistry()
	step := func(ctx context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		h, _ := sys.Session().History(ctx)
		mu.Lock()
		seen = append(seen, histLen(h))
		mu.Unlock()
		if st.N == 1 {
			repo.armed.Store(true) // crash the 2nd transition's commit (after its History save).
		}
		if _, err := sys.Session().Generate(ctx, []gollem.Input{gollem.Text("hi")}); err != nil {
			return st, agentkit.Decision[[]byte]{}, err
		}
		st.N++
		if st.N >= 3 {
			return st, agentkit.Done([]byte("done")), nil
		}
		return st, agentkit.Continue[[]byte](), nil
	}
	ag, err := agentkit.Register(reg, "main", 1, &scriptStrategy{step: step}, agentkit.WithHistoryStore[[]byte](store))
	gt.NoError(t, err)
	k, err := agentkit.New(repo, growingLLM(), reg)
	gt.NoError(t, err)

	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	p := serveUntil(t, k, repo, pid, 5*time.Second, isTerminal,
		agentkit.WithLease(80*time.Millisecond), agentkit.WithMaxUncleanReclaims(10))
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)

	// A clean run is 3 turns; the aborted attempt's version was saved but never
	// named by the record, so the transcript is still 3 and not 4.
	gt.Value(t, histLen(committedHistory(t, store, p))).Equal(3)
}

// An Apply failure that is not a conflict leaves the commit outcome unknown, so
// the version this attempt saved must be kept: discarding it could release the
// one the record now names.
func TestSession_UnknownApplyErrorKeepsVersion(t *testing.T) {
	ctx := context.Background()
	inner := memory.New()
	repo := &fragileRepo{Repository: inner, err: gollemErr("disk gone at commit")}
	store := &probeStore{inner: histmem.New()}
	reg := agentkit.NewRegistry()
	step := func(ctx context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		if st.N == 0 {
			repo.armed.Store(true)
		}
		if _, err := sys.Session().Generate(ctx, []gollem.Input{gollem.Text("hi")}); err != nil {
			return st, agentkit.Decision[[]byte]{}, err
		}
		st.N++
		if st.N >= 2 {
			return st, agentkit.Done([]byte("done")), nil
		}
		return st, agentkit.Continue[[]byte](), nil
	}
	ag, err := agentkit.Register(reg, "main", 1, &scriptStrategy{step: step}, agentkit.WithHistoryStore[[]byte](store))
	gt.NoError(t, err)
	k, err := agentkit.New(repo, growingLLM(), reg)
	gt.NoError(t, err)

	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	p := serveUntil(t, k, repo, pid, 5*time.Second, isTerminal,
		agentkit.WithLease(80*time.Millisecond), agentkit.WithMaxUncleanReclaims(10))
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)

	// The first attempt is the one whose Apply failed opaquely. Its version is
	// still in the store: no Discard names it, and it never became the committed
	// one either.
	saved := store.saved()
	gt.Value(t, len(saved) > 1).Equal(true)
	gt.Value(t, slices.Contains(store.discarded(), saved[0])).Equal(false)
	gt.Value(t, saved[0]).NotEqual(p.HistoryRef)
}

// ---- a ref the record still names is never released ----

// contentAddressedStore names a version by its content (here, its message
// count), so saving the same conversation twice hands back the same ref. The
// HistoryStore contract allows this explicitly, and it is what makes "release
// the version this attempt saved" dangerous: that ref can be the committed one.
type contentAddressedStore struct {
	mu       sync.Mutex
	m        map[agentkit.HistoryRef]*gollem.History
	discards []agentkit.HistoryRef
}

func newContentAddressedStore() *contentAddressedStore {
	return &contentAddressedStore{m: make(map[agentkit.HistoryRef]*gollem.History)}
}

func (s *contentAddressedStore) Save(_ context.Context, _ agentkit.ProcessID, h *gollem.History) (agentkit.HistoryRef, error) {
	ref := agentkit.HistoryRef(fmt.Sprintf("len-%d", histLen(h)))
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[ref] = h.Clone()
	return ref, nil
}

func (s *contentAddressedStore) Load(_ context.Context, _ agentkit.ProcessID, ref agentkit.HistoryRef) (*gollem.History, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.m[ref]
	if !ok {
		return nil, agentkit.ErrHistoryVersionMissing
	}
	return h.Clone(), nil
}

func (s *contentAddressedStore) Discard(_ context.Context, _ agentkit.ProcessID, ref agentkit.HistoryRef) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.discards = append(s.discards, ref)
	delete(s.m, ref)
}

func (s *contentAddressedStore) discarded() []agentkit.HistoryRef {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]agentkit.HistoryRef(nil), s.discards...)
}

// stableLLM answers with a history that never grows, so two Generates over the
// same conversation produce identical content — and a content-addressed store
// returns the same ref for both.
func stableLLM() gollem.LLMClient {
	return &mock.LLMClientMock{
		NewSessionFunc: func(_ context.Context, _ ...gollem.SessionOption) (gollem.Session, error) {
			return &mock.SessionMock{
				GenerateFunc: func(_ context.Context, _ []gollem.Input, _ ...gollem.GenerateOption) (*gollem.Response, error) {
					return &gollem.Response{Texts: []string{"ok"}, InputToken: 1, OutputToken: 1}, nil
				},
				HistoryFunc: func() (*gollem.History, error) {
					return &gollem.History{
						LLType:   gollem.LLMTypeClaude,
						Version:  gollem.HistoryVersion,
						Messages: make([]gollem.Message, 1),
					}, nil
				},
			}, nil
		},
	}
}

// A conflicted attempt releases the version it saved — unless the store handed
// back the ref the record already names, in which case releasing it would
// destroy the committed conversation.
func TestSession_ConflictKeepsRefTheRecordStillNames(t *testing.T) {
	ctx := context.Background()
	inner := memory.New()
	repo := &fragileRepo{Repository: inner, err: agentkit.ErrConflict}
	store := newContentAddressedStore()
	reg := agentkit.NewRegistry()
	step := func(ctx context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		if st.N == 1 {
			repo.armed.Store(true) // conflict the 2nd transition's Apply, after its save.
		}
		if _, err := sys.Session().Generate(ctx, []gollem.Input{gollem.Text("hi")}); err != nil {
			return st, agentkit.Decision[[]byte]{}, err
		}
		st.N++
		if st.N >= 3 {
			return st, agentkit.Done([]byte("done")), nil
		}
		return st, agentkit.Continue[[]byte](), nil
	}
	ag, err := agentkit.Register(reg, "main", 1, &scriptStrategy{step: step}, agentkit.WithHistoryStore[[]byte](store))
	gt.NoError(t, err)
	k, err := agentkit.New(repo, stableLLM(), reg)
	gt.NoError(t, err)

	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	p := serveUntil(t, k, repo, pid, 5*time.Second, isTerminal)
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)

	// Every save returned the same ref, so nothing was ever superseded and the
	// conflicted attempt must not have released it.
	gt.Value(t, p.HistoryRef).Equal(agentkit.HistoryRef("len-1"))
	gt.Array(t, store.discarded()).Length(0)
	got, lerr := store.Load(ctx, pid, p.HistoryRef)
	gt.NoError(t, lerr)
	gt.Value(t, histLen(got)).Equal(1)
}

// When another path finishes the Process first, this worker's terminal Apply
// conflicts and the record keeps whatever that path wrote — including its
// HistoryRef. Treating the "already terminal" outcome as its own success would
// release the version the record still names.
func TestSession_TerminalLostToAnotherPathKeepsVersion(t *testing.T) {
	ctx := context.Background()
	repo := memory.New()
	store := &probeStore{inner: histmem.New()}
	reg := agentkit.NewRegistry()
	var raced atomic.Bool
	step := func(ctx context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		if _, err := sys.Session().Generate(ctx, []gollem.Input{gollem.Text("hi")}); err != nil {
			return st, agentkit.Decision[[]byte]{}, err
		}
		if st.N == 0 {
			st.N = 1
			return st, agentkit.Continue[[]byte](), nil
		}
		// Another path terminates the Process before this transition commits,
		// leaving HistoryRef at the version the first transition committed.
		if raced.CompareAndSwap(false, true) {
			if cur, gerr := repo.GetProcess(ctx, sys.ProcessID()); gerr == nil {
				np := agentkit.CloneProcess(cur)
				np.Status = agentkit.ProcessSucceeded
				np.Output = []byte("finished-by-another-path")
				_ = repo.Apply(ctx, agentkit.ChangeSet{Processes: []*agentkit.Process{np}})
			}
		}
		return st, agentkit.Done([]byte("done")), nil
	}
	ag, err := agentkit.Register(reg, "main", 1, &scriptStrategy{step: step}, agentkit.WithHistoryStore[[]byte](store))
	gt.NoError(t, err)
	k, err := agentkit.New(repo, growingLLM(), reg)
	gt.NoError(t, err)

	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	p := serveUntil(t, k, repo, pid, 5*time.Second, isTerminal)
	gt.Value(t, raced.Load()).Equal(true)
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)
	gt.Value(t, string(p.Output)).Equal("finished-by-another-path")

	// The record names the first transition's version, and it is still there.
	gt.Value(t, slices.Contains(store.discarded(), p.HistoryRef)).Equal(false)
	gt.Value(t, histLen(committedHistory(t, store, p))).Equal(1)
}

// Session().History hands out a copy. Mutating it must not reach the committed
// baseline, which is what a same-lease retry re-seeds from.
func TestSession_HistoryReturnsACopy(t *testing.T) {
	ctx := context.Background()
	hs := histmem.New()
	step := func(ctx context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		if st.N == 1 {
			// Second transition: the baseline is non-nil here. Mutating what
			// History() returned must not affect what Generate seeds from.
			h, herr := sys.Session().History(ctx)
			if herr != nil {
				return st, agentkit.Decision[[]byte]{}, herr
			}
			h.Messages = append(h.Messages, gollem.Message{Role: gollem.RoleUser})
		}
		if _, err := sys.Session().Generate(ctx, []gollem.Input{gollem.Text("hi")}); err != nil {
			return st, agentkit.Decision[[]byte]{}, err
		}
		st.N++
		if st.N >= 2 {
			return st, agentkit.Done([]byte("done")), nil
		}
		return st, agentkit.Continue[[]byte](), nil
	}
	k, repo, ag := registerWithHistory(t, step, growingLLM(), hs)

	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	p := serveUntil(t, k, repo, pid, 5*time.Second, isTerminal)
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)

	// growingLLM adds one message per Generate: two transitions make 2. A leaked
	// baseline would have carried the strategy's extra message into the second
	// Generate's seed and committed 3.
	gt.Value(t, histLen(committedHistory(t, hs, p))).Equal(2)
}

// ---- superseded versions ----

// Once a transition's version is committed, the one it replaced is no longer
// referenced and the store is told so — that is what keeps the steady state at a
// couple of versions instead of one per Step.
func TestSession_SupersededVersionDiscarded(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	var seen []int
	store := &probeStore{inner: histmem.New()}
	k, repo, ag := registerWithHistory(t, sessionStep(&seen, &mu, 3), growingLLM(), store)

	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	p := serveUntil(t, k, repo, pid, 5*time.Second, isTerminal)
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)

	// Three saves, and the two superseded ones released. The committed one is not
	// among them — it is still loadable.
	gt.Value(t, store.saveCount()).Equal(3)
	gt.Array(t, store.discarded()).Length(2)
	gt.Value(t, slices.Contains(store.discarded(), p.HistoryRef)).Equal(false)
	gt.Value(t, histLen(committedHistory(t, store, p))).Equal(3)
}

// A transition that does not touch the managed conversation leaves the record's
// ref alone and releases nothing.
func TestSession_UntouchedConversationKeepsRef(t *testing.T) {
	ctx := context.Background()
	store := &probeStore{inner: histmem.New()}
	step := func(ctx context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		if st.N == 0 {
			if _, err := sys.Session().Generate(ctx, []gollem.Input{gollem.Text("hi")}); err != nil {
				return st, agentkit.Decision[[]byte]{}, err
			}
			st.N = 1
			return st, agentkit.Continue[[]byte](), nil
		}
		return st, agentkit.Done([]byte("done")), nil // no Session use at all.
	}
	k, repo, ag := registerWithHistory(t, step, growingLLM(), store)

	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	p := serveUntil(t, k, repo, pid, 5*time.Second, isTerminal)
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)

	gt.Value(t, store.saveCount()).Equal(1)
	gt.Array(t, store.discarded()).Length(0)
	gt.Value(t, histLen(committedHistory(t, store, p))).Equal(1)
}

// ---- stale worker ----

// TestSession_StaleWorkerSaveDoesNotAffectCommitted simulates a newer worker
// reclaiming the Process mid-transition (the lease token changes). The stale
// worker's Save still runs — there is no fence any more, and none is needed:
// a version is never rewritten, so all it can do is add one nobody references.
// What matters is that the record ends up naming the version of the attempt that
// actually committed.
func TestSession_StaleWorkerSaveDoesNotAffectCommitted(t *testing.T) {
	ctx := context.Background()
	repo := memory.New()
	store := &probeStore{inner: histmem.New()}
	reg := agentkit.NewRegistry()
	var stole atomic.Bool
	step := func(ctx context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		if _, err := sys.Session().Generate(ctx, []gollem.Input{gollem.Text("hi")}); err != nil {
			return st, agentkit.Decision[[]byte]{}, err
		}
		// On the first attempt, simulate a newer worker reclaiming this Process by
		// rewriting its lease token, so this attempt's commit is fenced out.
		if stole.CompareAndSwap(false, true) {
			if cur, gerr := repo.GetProcess(ctx, sys.ProcessID()); gerr == nil {
				np := agentkit.CloneProcess(cur)
				np.LeaseToken = "reclaimed-by-another-worker"
				_ = repo.Apply(ctx, agentkit.ChangeSet{Processes: []*agentkit.Process{np}})
			}
		}
		return st, agentkit.Done([]byte("done")), nil
	}
	ag, err := agentkit.Register(reg, "main", 1, &scriptStrategy{step: step}, agentkit.WithHistoryStore[[]byte](store))
	gt.NoError(t, err)
	k, err := agentkit.New(repo, growingLLM(), reg)
	gt.NoError(t, err)

	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	p := serveUntil(t, k, repo, pid, 5*time.Second, isTerminal,
		agentkit.WithLease(80*time.Millisecond), agentkit.WithMaxUncleanReclaims(10))
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)
	gt.Value(t, stole.Load()).Equal(true) // the steal actually happened.

	// The stale attempt did write a version (2 saves for a one-transition process),
	// and it cost nothing: the record names the committing attempt's version and it
	// still resolves.
	gt.Value(t, store.saveCount()).Equal(2)
	gt.Value(t, histLen(committedHistory(t, store, p))).Equal(1)
}

// ---- terminal variants ----

func TestSession_FailTerminalSavesHistory(t *testing.T) {
	ctx := context.Background()
	hs := histmem.New()
	step := func(ctx context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		if _, err := sys.Session().Generate(ctx, []gollem.Input{gollem.Text("hi")}); err != nil {
			return st, agentkit.Decision[[]byte]{}, err
		}
		return st, agentkit.Fail[[]byte](agentkit.FailureStrategyError, "boom"), nil
	}
	k, repo, ag := registerWithHistory(t, step, growingLLM(), hs)

	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	p := serveUntil(t, k, repo, pid, 5*time.Second, isTerminal)
	gt.Value(t, p.Status).Equal(agentkit.ProcessFailed)

	gt.Value(t, histLen(committedHistory(t, hs, p))).Equal(1) // Fail terminal saved History too (D-D).
}

// ---- inherited history ----

// runIssuer serves one Process to completion: the "has already committed a
// conversation" side of an inheritance test. The returned record's HistoryRef
// names the version an heir inherits.
func runIssuer(t *testing.T, k *agentkit.Kernel, repo agentkit.Repository, ag agentkit.Agent[scriptInput],
	extra ...agentkit.ServeOption) *agentkit.Process {
	t.Helper()
	pid, err := ag.Spawn(context.Background(), k, scriptInput{Seed: "issuer"})
	gt.NoError(t, err)
	p := serveUntil(t, k, repo, pid, 5*time.Second, isTerminal, extra...)
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)
	gt.Value(t, p.HistoryRef).NotEqual(agentkit.HistoryRef(""))
	return p
}

// A Process spawned with WithInheritedHistory starts its first turn from the
// conversation another Process committed, read under THAT Process's id.
func TestSession_InheritedHistorySeedsTheConversation(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	var seen []int
	store := &probeStore{inner: histmem.New()}
	k, repo, ag := registerWithHistory(t, sessionStep(&seen, &mu, 2), growingLLM(), store)

	issuer := runIssuer(t, k, repo, ag)
	gt.Value(t, histLen(committedHistory(t, store, issuer))).Equal(2)

	heirID, err := ag.Spawn(ctx, k, scriptInput{Seed: "heir"}, agentkit.WithInheritedHistory(issuer.ID))
	gt.NoError(t, err)

	// The version was resolved at Spawn and recorded; the heir's own ref is still
	// empty, because it has committed nothing.
	spawned, err := repo.GetProcess(ctx, heirID)
	gt.NoError(t, err)
	gt.NotNil(t, spawned.InheritedHistory)
	gt.Value(t, *spawned.InheritedHistory).
		Equal(agentkit.InheritedHistory{Process: issuer.ID, Ref: issuer.HistoryRef})
	gt.Value(t, spawned.HistoryRef).Equal(agentkit.HistoryRef(""))

	mu.Lock()
	issuerTurns := len(seen)
	mu.Unlock()
	// committedHistory reads through the probe too, so the heir's loads are the
	// ones recorded from here on.
	before := len(store.loaded())

	heir := serveUntil(t, k, repo, heirID, 5*time.Second, isTerminal)
	gt.Value(t, heir.Status).Equal(agentkit.ProcessSucceeded)
	byHeir := store.loaded()[before:]

	mu.Lock()
	got := append([]int(nil), seen[issuerTurns:]...)
	mu.Unlock()
	// The heir's first turn carried the issuer's two messages, not zero.
	gt.Value(t, got).Equal([]int{2, 3})
	gt.Value(t, histLen(committedHistory(t, store, heir))).Equal(4)
	// One load for the whole run, and it named the ISSUER's key space. The issuer's
	// own run loaded nothing: it started from an empty conversation in one claim.
	gt.Value(t, byHeir).Equal([]histCall{{pid: issuer.ID, ref: issuer.HistoryRef}})
}

// The inherited version must survive the heir's commits. It is the one thing a
// Discard keyed on the heir's own id would destroy: the issuing Process's record
// still names it, so releasing it is data loss for a Process nobody touched.
func TestSession_InheritedVersionIsNeverDiscarded(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	var seen []int
	store := &probeStore{inner: histmem.New()}
	k, repo, ag := registerWithHistory(t, sessionStep(&seen, &mu, 2), growingLLM(), store)

	issuer := runIssuer(t, k, repo, ag)
	inherited := histCall{pid: issuer.ID, ref: issuer.HistoryRef}

	heirID, err := ag.Spawn(ctx, k, scriptInput{Seed: "heir"}, agentkit.WithInheritedHistory(issuer.ID))
	gt.NoError(t, err)
	heir := serveUntil(t, k, repo, heirID, 5*time.Second, isTerminal)
	gt.Value(t, heir.Status).Equal(agentkit.ProcessSucceeded)

	// Never announced as superseded...
	gt.Value(t, slices.Contains(store.discardedPairs(), inherited)).Equal(false)
	// ...and still resolvable, which is what the issuer's record needs.
	gt.Value(t, histLen(committedHistory(t, store, issuer))).Equal(2)

	// The ordinary release did happen, under the heir's own id: its first version
	// was superseded by its second. saved() is in call order — the issuer's two
	// saves, then the heir's two.
	refs := store.saved()
	gt.Array(t, refs).Length(4)
	gt.Value(t, slices.Contains(store.discardedPairs(), histCall{pid: heirID, ref: refs[2]})).Equal(true)
}

// Once the heir has committed a version of its own, that is what later
// transitions read. The inherited version is never consulted again.
func TestSession_InheritedRefIsNotReadAfterOwnCommit(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	var seen []int
	store := &probeStore{inner: histmem.New()}
	k, repo, ag := registerWithHistory(t, sessionStep(&seen, &mu, 2), growingLLM(), store)

	issuer := runIssuer(t, k, repo, ag)

	heirID, err := ag.Spawn(ctx, k, scriptInput{Seed: "heir"}, agentkit.WithInheritedHistory(issuer.ID))
	gt.NoError(t, err)
	// One transition per claim, so the second transition genuinely re-loads from
	// the store instead of reusing the claim's in-memory baseline.
	heir := serveUntil(t, k, repo, heirID, 5*time.Second, isTerminal, agentkit.WithMaxStepsPerClaim(1))
	gt.Value(t, heir.Status).Equal(agentkit.ProcessSucceeded)

	loads := store.loaded()
	gt.Array(t, loads).Length(2)
	gt.Value(t, loads[0]).Equal(histCall{pid: issuer.ID, ref: issuer.HistoryRef})
	// The second load is the heir's own first version, under the heir's own id.
	gt.Value(t, loads[1].pid).Equal(heirID)
	gt.Value(t, loads[1].ref).Equal(store.saved()[2])
	gt.Value(t, histLen(committedHistory(t, store, heir))).Equal(4)
}

// The version is pinned at Spawn, not resolved at first use: a turn the issuing
// Process commits afterwards does not change what the heir starts from.
func TestSession_InheritedVersionIsPinnedAtSpawn(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	var seen []int
	store := &probeStore{inner: histmem.New()}
	k, repo, ag := registerWithHistory(t, sessionStep(&seen, &mu, 1), growingLLM(), store)

	issuer := runIssuer(t, k, repo, ag)
	pinned := issuer.HistoryRef

	heirID, err := ag.Spawn(ctx, k, scriptInput{Seed: "heir"}, agentkit.WithInheritedHistory(issuer.ID))
	gt.NoError(t, err)

	// The issuer moves on: a new, much longer version becomes what its record
	// names. Written directly, because a terminal Process runs no more transitions.
	longer := &gollem.History{
		LLType:   gollem.LLMTypeClaude,
		Version:  gollem.HistoryVersion,
		Messages: make([]gollem.Message, 9),
	}
	moved, err := store.Save(ctx, issuer.ID, longer)
	gt.NoError(t, err)
	advanced := agentkit.CloneProcess(issuer)
	advanced.HistoryRef = moved
	gt.NoError(t, repo.Apply(ctx, agentkit.ChangeSet{Processes: []*agentkit.Process{advanced}}))

	mu.Lock()
	issuerTurns := len(seen)
	mu.Unlock()

	heir := serveUntil(t, k, repo, heirID, 5*time.Second, isTerminal)
	gt.Value(t, heir.Status).Equal(agentkit.ProcessSucceeded)

	mu.Lock()
	got := append([]int(nil), seen[issuerTurns:]...)
	mu.Unlock()
	gt.Value(t, got).Equal([]int{1}) // the pinned version's length, not the 9 above.
	gt.Value(t, store.loaded()).Equal([]histCall{{pid: issuer.ID, ref: pinned}})
}

// The kernel does not verify at Spawn that the inherited version is still in the
// store (D-6): it cannot promise it either way, because the issuer's next commit
// can release it. What it must do is fail loudly rather than start the
// conversation over from empty.
func TestSession_InheritedVersionMissingFailsTheProcess(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	var seen []int
	store := &probeStore{inner: histmem.New()}
	k, repo, ag := registerWithHistory(t, sessionStep(&seen, &mu, 1), growingLLM(), store)

	issuer := runIssuer(t, k, repo, ag)
	heirID, err := ag.Spawn(ctx, k, scriptInput{Seed: "heir"}, agentkit.WithInheritedHistory(issuer.ID))
	gt.NoError(t, err)

	// The version goes away between Spawn and the first transition.
	store.Discard(ctx, issuer.ID, issuer.HistoryRef)

	heir := serveUntil(t, k, repo, heirID, 5*time.Second, isTerminal, agentkit.WithMaxStepAttempts(1))
	gt.Value(t, heir.Status).Equal(agentkit.ProcessFailed)
	gt.NotNil(t, heir.Failure)
	gt.Value(t, heir.Failure.Code).Equal(agentkit.FailureRetryExhausted)
	gt.Value(t, heir.HistoryRef).Equal(agentkit.HistoryRef("")) // nothing was committed.
}

// Re-runnability: an attempt that did not commit re-runs from the SAME inherited
// baseline, so the conversation carries no duplicated turn.
func TestSession_InheritedHistoryRerunDoesNotDouble(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	var seen []int
	inner := memory.New()
	repo := &fragileRepo{Repository: inner, err: gollemErr("disk gone at commit")}
	store := &probeStore{inner: histmem.New()}
	reg := agentkit.NewRegistry()
	ag, err := agentkit.Register(reg, "main", 1,
		&scriptStrategy{step: sessionStep(&seen, &mu, 2)}, agentkit.WithHistoryStore[[]byte](store))
	gt.NoError(t, err)
	k, err := agentkit.New(repo, growingLLM(), reg)
	gt.NoError(t, err)

	issuer := runIssuer(t, k, repo, ag)

	heirID, err := ag.Spawn(ctx, k, scriptInput{Seed: "heir"}, agentkit.WithInheritedHistory(issuer.ID))
	gt.NoError(t, err)
	// Armed after the insert, so the next Apply — the heir's FIRST commit, whose
	// History save has already run — is the one that fails.
	repo.armed.Store(true)

	heir := serveUntil(t, k, repo, heirID, 5*time.Second, isTerminal,
		agentkit.WithLease(80*time.Millisecond), agentkit.WithMaxUncleanReclaims(10))
	gt.Value(t, heir.Status).Equal(agentkit.ProcessSucceeded)
	gt.Value(t, repo.fired.Load()).Equal(true) // the commit really did fail once.

	// A clean run of two turns on top of the issuer's two messages is 4. The
	// aborted attempt's version was saved but never named, and the re-run started
	// from the inherited version again — not from the abandoned attempt.
	gt.Value(t, histLen(committedHistory(t, store, heir))).Equal(4)
	// And the inherited version is still intact after all of it.
	gt.Value(t, histLen(committedHistory(t, store, issuer))).Equal(2)
}

// ---- tools ----

func TestSession_InjectsClaimTools(t *testing.T) {
	ctx := context.Background()
	var sawTool int32
	model := &mock.LLMClientMock{
		NewSessionFunc: func(_ context.Context, opts ...gollem.SessionOption) (gollem.Session, error) {
			cfg := gollem.NewSessionConfig(opts...)
			if len(cfg.Tools()) > 0 {
				atomic.StoreInt32(&sawTool, 1)
			}
			return &mock.SessionMock{
				GenerateFunc: func(_ context.Context, _ []gollem.Input, _ ...gollem.GenerateOption) (*gollem.Response, error) {
					return &gollem.Response{Texts: []string{"ok"}, InputToken: 1, OutputToken: 1}, nil
				},
				HistoryFunc: func() (*gollem.History, error) {
					return &gollem.History{LLType: gollem.LLMTypeClaude, Version: gollem.HistoryVersion}, nil
				},
			}, nil
		},
	}
	step := func(ctx context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		if _, err := sys.Session().Generate(ctx, []gollem.Input{gollem.Text("hi")}); err != nil {
			return st, agentkit.Decision[[]byte]{}, err
		}
		return st, agentkit.Done([]byte("done")), nil
	}
	factory := func(_ context.Context, _ *agentkit.Process) ([]gollem.Tool, error) {
		return []gollem.Tool{mockTool("t", map[string]any{})}, nil
	}
	k, repo, ag := registerWithHistory(t, step, model, histmem.New(), agentkit.WithToolFactory(factory))

	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	p := serveUntil(t, k, repo, pid, 5*time.Second, isTerminal)
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)
	gt.Value(t, atomic.LoadInt32(&sawTool)).Equal(int32(1))
}

// lastToolResponse returns the ToolResponseContent of a History's last message,
// failing the test unless that message is a single tool response.
func lastToolResponse(t *testing.T, h *gollem.History) *gollem.ToolResponseContent {
	t.Helper()
	gt.NotNil(t, h)
	gt.Value(t, len(h.Messages) > 0).Equal(true)
	last := h.Messages[len(h.Messages)-1]
	gt.Value(t, last.Role).Equal(gollem.RoleTool)
	gt.Array(t, last.Contents).Length(1)
	tr, err := last.Contents[0].GetToolResponseContent()
	gt.NoError(t, err)
	return tr
}

func TestSession_CallToolAppendsResult(t *testing.T) {
	ctx := context.Background()
	hs := histmem.New()
	var mu sync.Mutex
	var afterLen int
	var toolResp *gollem.ToolResponseContent
	step := func(ctx context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		if _, err := sys.Session().Generate(ctx, []gollem.Input{gollem.Text("hi")}); err != nil {
			return st, agentkit.Decision[[]byte]{}, err
		}
		out, err := sys.Session().CallTool(ctx, gollem.FunctionCall{ID: "call-1", Name: "t"})
		if err != nil {
			return st, agentkit.Decision[[]byte]{}, err
		}
		if out["ok"] != true {
			return st, agentkit.Decision[[]byte]{}, gollemErr("unexpected tool output")
		}
		h, herr := sys.Session().History(ctx)
		if herr != nil {
			return st, agentkit.Decision[[]byte]{}, herr
		}
		mu.Lock()
		afterLen = histLen(h)
		toolResp = lastToolResponse(t, h)
		mu.Unlock()
		return st, agentkit.Done([]byte("done")), nil
	}
	factory := func(_ context.Context, _ *agentkit.Process) ([]gollem.Tool, error) {
		return []gollem.Tool{mockTool("t", map[string]any{"ok": true})}, nil
	}
	k, repo, ag := registerWithHistory(t, step, growingLLM(), hs, agentkit.WithToolFactory(factory))

	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	p := serveUntil(t, k, repo, pid, 5*time.Second, isTerminal)
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)

	mu.Lock()
	defer mu.Unlock()
	gt.Value(t, afterLen).Equal(2) // the generate's message plus the appended result.
	gt.Value(t, toolResp.ToolCallID).Equal("call-1")
	gt.Value(t, toolResp.Name).Equal("t")
	gt.Value(t, toolResp.IsError).Equal(false)
	// And it is what got committed, not just what the strategy saw.
	gt.Value(t, histLen(committedHistory(t, hs, p))).Equal(2)
}

// A failing tool still has to close the pair: the model asked for the call, so
// the next request must answer it. The error reaches the strategy as well.
func TestSession_CallToolAppendsErrorResult(t *testing.T) {
	ctx := context.Background()
	hs := histmem.New()
	var mu sync.Mutex
	var callErr error
	var toolResp *gollem.ToolResponseContent
	step := func(ctx context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		if _, err := sys.Session().Generate(ctx, []gollem.Input{gollem.Text("hi")}); err != nil {
			return st, agentkit.Decision[[]byte]{}, err
		}
		// No tool named "missing" exists, so the call fails before any Run.
		_, cerr := sys.Session().CallTool(ctx, gollem.FunctionCall{ID: "call-1", Name: "missing"})
		h, herr := sys.Session().History(ctx)
		if herr != nil {
			return st, agentkit.Decision[[]byte]{}, herr
		}
		mu.Lock()
		callErr = cerr
		toolResp = lastToolResponse(t, h)
		mu.Unlock()
		return st, agentkit.Done([]byte("done")), nil
	}
	k, repo, ag := registerWithHistory(t, step, growingLLM(), hs)

	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	p := serveUntil(t, k, repo, pid, 5*time.Second, isTerminal)
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)

	mu.Lock()
	defer mu.Unlock()
	gt.Value(t, errors.Is(callErr, agentkit.ErrToolNotFound)).Equal(true)
	gt.Value(t, toolResp.ToolCallID).Equal("call-1")
	gt.Value(t, toolResp.IsError).Equal(true)
	gt.Value(t, toolResp.Response["error"] != nil).Equal(true)
}

// A tool result that cannot be encoded into a conversation message leaves the
// pair open, so the transition has to fail rather than commit a History the next
// request cannot answer. The tool has already run by then — that is unavoidable
// — but nothing is appended.
func TestSession_CallToolEncodeFailureAppendsNothing(t *testing.T) {
	ctx := context.Background()
	hs := histmem.New()
	var ran atomic.Bool
	var mu sync.Mutex
	var callErr error
	var lenAfterCall int
	tool := &mock.ToolMock{
		SpecFunc: func() gollem.ToolSpec { return gollem.ToolSpec{Name: "t"} },
		RunFunc: func(_ context.Context, _ map[string]any) (map[string]any, error) {
			ran.Store(true)
			// encoding/json cannot marshal a func, so building the tool_response
			// content fails after the tool itself succeeded.
			return map[string]any{"callback": func() {}}, nil
		},
	}
	step := func(ctx context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		if _, err := sys.Session().Generate(ctx, []gollem.Input{gollem.Text("hi")}); err != nil {
			return st, agentkit.Decision[[]byte]{}, err
		}
		_, cerr := sys.Session().CallTool(ctx, gollem.FunctionCall{ID: "call-1", Name: "t"})
		h, herr := sys.Session().History(ctx)
		if herr != nil {
			return st, agentkit.Decision[[]byte]{}, herr
		}
		mu.Lock()
		callErr, lenAfterCall = cerr, histLen(h)
		mu.Unlock()
		return st, agentkit.Done([]byte("done")), nil
	}
	factory := func(_ context.Context, _ *agentkit.Process) ([]gollem.Tool, error) {
		return []gollem.Tool{tool}, nil
	}
	k, repo, ag := registerWithHistory(t, step, growingLLM(), hs, agentkit.WithToolFactory(factory))

	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	p := serveUntil(t, k, repo, pid, 5*time.Second, isTerminal)
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)

	mu.Lock()
	defer mu.Unlock()
	gt.Value(t, ran.Load()).Equal(true) // the tool did run,
	gt.Error(t, callErr)                // the failure reached the strategy,
	gt.Value(t, lenAfterCall).Equal(1)  // and nothing was appended.
	gt.Value(t, histLen(committedHistory(t, hs, p))).Equal(1)
}

// Appending a result to a conversation that does not exist yet would mean
// inventing the provider identity a History carries, so the call is refused
// before the tool runs.
func TestSession_CallToolWithoutHistoryRejected(t *testing.T) {
	ctx := context.Background()
	var ran atomic.Bool
	var mu sync.Mutex
	var callErr error
	tool := &mock.ToolMock{
		SpecFunc: func() gollem.ToolSpec { return gollem.ToolSpec{Name: "t"} },
		RunFunc: func(_ context.Context, _ map[string]any) (map[string]any, error) {
			ran.Store(true)
			return map[string]any{"ok": true}, nil
		},
	}
	step := func(ctx context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		_, cerr := sys.Session().CallTool(ctx, gollem.FunctionCall{ID: "call-1", Name: "t"})
		mu.Lock()
		callErr = cerr
		mu.Unlock()
		return st, agentkit.Done([]byte("done")), nil
	}
	factory := func(_ context.Context, _ *agentkit.Process) ([]gollem.Tool, error) {
		return []gollem.Tool{tool}, nil
	}
	k, repo, ag := registerWithHistory(t, step, growingLLM(), histmem.New(), agentkit.WithToolFactory(factory))

	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	p := serveUntil(t, k, repo, pid, 5*time.Second, isTerminal)
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)

	mu.Lock()
	defer mu.Unlock()
	gt.Value(t, errors.Is(callErr, agentkit.ErrInvalidRequest)).Equal(true)
	gt.Value(t, ran.Load()).Equal(false)
	gt.Value(t, p.HistoryRef).Equal(agentkit.HistoryRef("")) // nothing was appended, nothing saved.
}

// ---- tool round split across steps ----

// toolThenTextLLM answers the first Generate of each session with a tool call and
// any later one with text, and grows the history by one message per call. It is
// the shape a strategy hits when the model asks for a tool and then replies.
func toolThenTextLLM(callID, toolName string) gollem.LLMClient {
	return &mock.LLMClientMock{
		NewSessionFunc: func(_ context.Context, opts ...gollem.SessionOption) (gollem.Session, error) {
			cfg := gollem.NewSessionConfig(opts...)
			var seeded []gollem.Message
			if h := cfg.History(); h != nil {
				seeded = h.Messages
			}
			// The tool call comes only while the conversation is still empty; once
			// the seeded history carries the tool_use and its result, answer in text.
			wantTool := len(seeded) == 0
			return &mock.SessionMock{
				GenerateFunc: func(_ context.Context, _ []gollem.Input, _ ...gollem.GenerateOption) (*gollem.Response, error) {
					if wantTool {
						return &gollem.Response{
							FunctionCalls: []*gollem.FunctionCall{{ID: callID, Name: toolName}},
							InputToken:    1, OutputToken: 1,
						}, nil
					}
					return &gollem.Response{Texts: []string{"final"}, InputToken: 1, OutputToken: 1}, nil
				},
				HistoryFunc: func() (*gollem.History, error) {
					grown := make([]gollem.Message, len(seeded)+1)
					copy(grown, seeded)
					return &gollem.History{LLType: gollem.LLMTypeClaude, Version: gollem.HistoryVersion, Messages: grown}, nil
				},
			}, nil
		},
	}
}

// The obligation this change removes: a Step may commit with the conversation
// mid-round. Step 1 generates a tool_use and Continues without answering it;
// Step 2 (a different claim) runs the tool, appends the result, and generates
// again with no input at all.
func TestSession_ToolRoundSplitAcrossSteps(t *testing.T) {
	ctx := context.Background()
	hs := histmem.New()
	var mu sync.Mutex
	var pendingSeen int
	var finalTexts []string
	step := func(ctx context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		if st.N == 0 {
			res, err := sys.Session().Generate(ctx, []gollem.Input{gollem.Text("hi")})
			if err != nil {
				return st, agentkit.Decision[[]byte]{}, err
			}
			mu.Lock()
			pendingSeen = len(res.FunctionCalls)
			mu.Unlock()
			// Commit here, with the conversation ending on the unanswered call.
			st.N = 1
			return st, agentkit.Continue[[]byte](), nil
		}
		if _, err := sys.Session().CallTool(ctx, gollem.FunctionCall{ID: "call-1", Name: "t"}); err != nil {
			return st, agentkit.Decision[[]byte]{}, err
		}
		res, err := sys.Session().Generate(ctx, nil) // no input: the result is already in the History.
		if err != nil {
			return st, agentkit.Decision[[]byte]{}, err
		}
		mu.Lock()
		finalTexts = res.Texts
		mu.Unlock()
		return st, agentkit.Done([]byte("done")), nil
	}
	factory := func(_ context.Context, _ *agentkit.Process) ([]gollem.Tool, error) {
		return []gollem.Tool{mockTool("t", map[string]any{"ok": true})}, nil
	}
	k, repo, ag := registerWithHistory(t, step, toolThenTextLLM("call-1", "t"), hs, agentkit.WithToolFactory(factory))

	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	// One transition per claim, so the second Step genuinely reloads the committed
	// mid-round conversation instead of carrying it in memory.
	p := serveUntil(t, k, repo, pid, 5*time.Second, isTerminal, agentkit.WithMaxStepsPerClaim(1))
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)

	mu.Lock()
	defer mu.Unlock()
	gt.Value(t, pendingSeen).Equal(1)
	gt.Value(t, finalTexts).Equal([]string{"final"})
	// 1 (tool_use turn) + 1 (appended result) + 1 (final turn).
	gt.Value(t, histLen(committedHistory(t, hs, p))).Equal(3)
}

// WithLLMSessionOptions is appended last, so gollem.WithSessionHistory passed
// through it replaces the History the managed conversation is carrying. The
// override is not a detour around the managed conversation: what the model
// returns from the overridden session becomes the new baseline and is persisted
// like any other turn. Both halves are asserted, because the dangerous
// regression is the second one silently stopping.
func TestSession_LLMSessionOptionsOverrideManagedHistory(t *testing.T) {
	ctx := context.Background()
	injected := &gollem.History{
		LLType:   gollem.LLMTypeClaude,
		Version:  gollem.HistoryVersion,
		Messages: make([]gollem.Message, 7),
	}
	// Each session grows what it was seeded with by one, so a message count
	// identifies which History reached NewSession.
	var mu sync.Mutex
	var seen []int
	model := &mock.LLMClientMock{
		NewSessionFunc: func(_ context.Context, opts ...gollem.SessionOption) (gollem.Session, error) {
			cfg := gollem.NewSessionConfig(opts...)
			var seeded []gollem.Message
			if h := cfg.History(); h != nil {
				seeded = h.Messages
			}
			mu.Lock()
			seen = append(seen, len(seeded))
			mu.Unlock()
			return &mock.SessionMock{
				GenerateFunc: func(_ context.Context, _ []gollem.Input, _ ...gollem.GenerateOption) (*gollem.Response, error) {
					return &gollem.Response{Texts: []string{"ok"}, InputToken: 1, OutputToken: 1}, nil
				},
				HistoryFunc: func() (*gollem.History, error) {
					grown := make([]gollem.Message, len(seeded)+1)
					copy(grown, seeded)
					return &gollem.History{LLType: gollem.LLMTypeClaude, Version: gollem.HistoryVersion, Messages: grown}, nil
				},
			}, nil
		},
	}
	// Transition 1 overrides the History; transition 2 uses the managed one, so
	// the second NewSession reveals what the override left behind as baseline.
	step := func(ctx context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		var opts []agentkit.GenerateOption
		if st.N == 0 {
			opts = append(opts, agentkit.WithLLMSessionOptions(gollem.WithSessionHistory(injected)))
		}
		if _, err := sys.Session().Generate(ctx, []gollem.Input{gollem.Text("hi")}, opts...); err != nil {
			return st, agentkit.Decision[[]byte]{}, err
		}
		if st.N == 0 {
			st.N = 1
			return st, agentkit.Continue[[]byte](), nil
		}
		return st, agentkit.Done([]byte("done")), nil
	}
	hs := histmem.New()
	k, repo, ag := registerWithHistory(t, step, model, hs)

	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	p := serveUntil(t, k, repo, pid, 5*time.Second, isTerminal)
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)

	mu.Lock()
	defer mu.Unlock()
	gt.Array(t, seen).Length(2)
	gt.Value(t, seen[0]).Equal(7) // the override replaced the empty managed History,
	gt.Value(t, seen[1]).Equal(8) // and its result carried on as the baseline.

	gt.Value(t, histLen(committedHistory(t, hs, p))).Equal(9) // persisted, not just carried in memory.
}
