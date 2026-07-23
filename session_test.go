package agentkit_test

import (
	"context"
	"errors"
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
// message. Session.Generate seeds each call with the carried history, so across
// transitions the message count grows by one per Generate — a distinguishable
// signal for asserting that History was carried and (with a repo) persisted.
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

// sessionStep builds a step that records the carried-in History length (before
// Generate), runs one Session.Generate, and Continues until `turns` generates
// then Dones. seen captures the carried-in lengths across transitions/attempts.
func sessionStep(seen *[]int, mu *sync.Mutex, turns int) stepFn {
	return func(ctx context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		h, _ := sys.SessionHistory(ctx)
		mu.Lock()
		*seen = append(*seen, histLen(h))
		mu.Unlock()
		if _, err := sys.SessionGenerate(ctx, []gollem.Input{gollem.Text("hi")}); err != nil {
			return st, agentkit.Decision[[]byte]{}, err
		}
		st.N++
		if st.N >= turns {
			return st, agentkit.Done([]byte("done")), nil
		}
		return st, agentkit.Continue[[]byte](), nil
	}
}

func registerWithHistory(t *testing.T, step stepFn, model gollem.LLMClient, hr gollem.HistoryRepository, kopts ...agentkit.KernelOption) (*agentkit.Kernel, agentkit.Repository, agentkit.Agent[scriptInput]) {
	t.Helper()
	repo := memory.New()
	reg := agentkit.NewRegistry()
	ag, err := agentkit.Register(reg, "main", 1, &scriptStrategy{step: step}, agentkit.WithHistoryRepository[[]byte](hr))
	gt.NoError(t, err)
	k, err := agentkit.New(repo, model, reg, kopts...)
	gt.NoError(t, err)
	return k, repo, ag
}

// probeHistoryRepo wraps a HistoryRepository, recording every Save's message
// length (in order) and the load count, and can inject Load/Save errors.
type probeHistoryRepo struct {
	inner   gollem.HistoryRepository
	mu      sync.Mutex
	saves   []int
	loads   int
	loadErr error
	saveErr error
}

func (r *probeHistoryRepo) Load(ctx context.Context, id string) (*gollem.History, error) {
	r.mu.Lock()
	r.loads++
	le := r.loadErr
	r.mu.Unlock()
	if le != nil {
		return nil, le
	}
	return r.inner.Load(ctx, id)
}

func (r *probeHistoryRepo) Save(ctx context.Context, id string, h *gollem.History) error {
	r.mu.Lock()
	r.saves = append(r.saves, histLen(h))
	se := r.saveErr
	r.mu.Unlock()
	if se != nil {
		return se
	}
	return r.inner.Save(ctx, id, h)
}

func (r *probeHistoryRepo) saveCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.saves)
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
	hr := histmem.New()
	k, repo, ag := registerWithHistory(t, sessionStep(&seen, &mu, 3), growingLLM(), hr)

	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	p := serveUntil(t, k, repo, pid, 5*time.Second, isTerminal)
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)

	mu.Lock()
	got := append([]int(nil), seen...)
	mu.Unlock()
	gt.Value(t, got).Equal([]int{0, 1, 2})

	stored, err := hr.Load(ctx, string(pid))
	gt.NoError(t, err)
	gt.Value(t, histLen(stored)).Equal(3) // terminal transition saved too (D-D).
}

// TestSession_PersistsAcrossClaims forces one transition per claim so History
// load/save genuinely crosses claim boundaries (not just in-memory carry).
func TestSession_PersistsAcrossClaims(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	var seen []int
	hr := histmem.New()
	k, repo, ag := registerWithHistory(t, sessionStep(&seen, &mu, 3), growingLLM(), hr)

	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	p := serveUntil(t, k, repo, pid, 5*time.Second, isTerminal, agentkit.WithMaxStepsPerClaim(1))
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)

	mu.Lock()
	got := append([]int(nil), seen...)
	mu.Unlock()
	gt.Value(t, got).Equal([]int{0, 1, 2}) // each new claim loaded the prior claim's saved history.

	stored, err := hr.Load(ctx, string(pid))
	gt.NoError(t, err)
	gt.Value(t, histLen(stored)).Equal(3)
}

// TestSession_WithoutRepositoryErrors verifies that using the managed
// conversation on an agent NOT registered with WithHistoryRepository fails
// loudly (ErrHistoryNotConfigured) instead of silently running without
// persistence — for both SessionGenerate and SessionHistory.
func TestSession_WithoutRepositoryErrors(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	var genErr, histErr error
	step := func(ctx context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		_, he := sys.SessionHistory(ctx)
		_, ge := sys.SessionGenerate(ctx, []gollem.Input{gollem.Text("hi")})
		mu.Lock()
		histErr, genErr = he, ge
		mu.Unlock()
		return st, agentkit.Decision[[]byte]{}, ge
	}
	repo := memory.New()
	reg := agentkit.NewRegistry()
	ag, err := agentkit.Register(reg, "main", 1, &scriptStrategy{step: step}) // no WithHistoryRepository.
	gt.NoError(t, err)
	k, err := agentkit.New(repo, growingLLM(), reg)
	gt.NoError(t, err)

	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	p := serveUntil(t, k, repo, pid, 5*time.Second, isTerminal, agentkit.WithMaxStepAttempts(1))
	gt.Value(t, p.Status).Equal(agentkit.ProcessFailed)

	mu.Lock()
	ge, he := genErr, histErr
	mu.Unlock()
	gt.Value(t, errors.Is(ge, agentkit.ErrHistoryNotConfigured)).Equal(true)
	gt.Value(t, errors.Is(he, agentkit.ErrHistoryNotConfigured)).Equal(true)
}

// ---- error paths ----

func TestSession_LoadErrorFailsTransition(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	var seen []int
	hr := &probeHistoryRepo{inner: histmem.New(), loadErr: gollemErr("load boom")}
	k, repo, ag := registerWithHistory(t, sessionStep(&seen, &mu, 3), growingLLM(), hr)

	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	p := serveUntil(t, k, repo, pid, 5*time.Second, isTerminal, agentkit.WithMaxStepAttempts(1))
	gt.Value(t, p.Status).Equal(agentkit.ProcessFailed)
	gt.Value(t, p.Failure.Code).Equal(agentkit.FailureRetryExhausted)
}

// TestSession_HistoryMethodSurfacesLoadError checks that Session.History(ctx)
// propagates a load failure to the strategy rather than returning nil silently.
func TestSession_HistoryMethodSurfacesLoadError(t *testing.T) {
	ctx := context.Background()
	hr := &probeHistoryRepo{inner: histmem.New(), loadErr: gollemErr("load boom")}
	step := func(ctx context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		if _, herr := sys.SessionHistory(ctx); herr != nil {
			return st, agentkit.Decision[[]byte]{}, herr
		}
		return st, agentkit.Done([]byte("x")), nil
	}
	k, repo, ag := registerWithHistory(t, step, growingLLM(), hr)

	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	p := serveUntil(t, k, repo, pid, 5*time.Second, isTerminal, agentkit.WithMaxStepAttempts(1))
	gt.Value(t, p.Status).Equal(agentkit.ProcessFailed)
	gt.Value(t, p.Failure.Code).Equal(agentkit.FailureRetryExhausted)
}

func TestSession_SaveErrorRequeues(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	var seen []int
	hr := &probeHistoryRepo{inner: histmem.New(), saveErr: gollemErr("save boom")}
	k, repo, ag := registerWithHistory(t, sessionStep(&seen, &mu, 3), growingLLM(), hr)

	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	p := serveUntil(t, k, repo, pid, 5*time.Second, isTerminal, agentkit.WithMaxStepAttempts(1))
	gt.Value(t, p.Status).Equal(agentkit.ProcessFailed)
	gt.Value(t, p.Failure.Code).Equal(agentkit.FailureRetryExhausted)
}

// ---- ordering ----

// orderHistoryRepo records, at Save time, the process's currently-committed
// StateSeq (read from the process repo). save-before-commit means the value
// observed is the pre-commit StateSeq.
type orderHistoryRepo struct {
	proc      agentkit.Repository
	inner     gollem.HistoryRepository
	mu        sync.Mutex
	seqOnSave []int
}

func (r *orderHistoryRepo) Load(ctx context.Context, id string) (*gollem.History, error) {
	return r.inner.Load(ctx, id)
}
func (r *orderHistoryRepo) Save(ctx context.Context, id string, h *gollem.History) error {
	if p, err := r.proc.GetProcess(ctx, agentkit.ProcessID(id)); err == nil {
		r.mu.Lock()
		r.seqOnSave = append(r.seqOnSave, p.StateSeq)
		r.mu.Unlock()
	}
	return r.inner.Save(ctx, id, h)
}

func TestSession_SaveHappensBeforeCommit(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	var seen []int
	repo := memory.New()
	hr := &orderHistoryRepo{proc: repo, inner: histmem.New()}
	reg := agentkit.NewRegistry()
	ag, err := agentkit.Register(reg, "main", 1, &scriptStrategy{step: sessionStep(&seen, &mu, 1)}, agentkit.WithHistoryRepository[[]byte](hr))
	gt.NoError(t, err)
	k, err := agentkit.New(repo, growingLLM(), reg)
	gt.NoError(t, err)

	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	p := serveUntil(t, k, repo, pid, 5*time.Second, isTerminal)
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)
	gt.Value(t, p.StateSeq).Equal(1)

	hr.mu.Lock()
	seqs := append([]int(nil), hr.seqOnSave...)
	hr.mu.Unlock()
	gt.Value(t, len(seqs)).Equal(1)
	gt.Value(t, seqs[0]).Equal(0) // saved before the commit that bumped StateSeq to 1.
}

// ---- conflict / re-seed (same lease) ----

// TestSession_SameLeaseConflictReseeds injects one ErrConflict on the second
// transition's Apply. The same-lease retry must re-seed from the committed
// baseline, NOT from the aborted attempt's history, so no turn is duplicated.
func TestSession_SameLeaseConflictReseeds(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	var seen []int
	inner := memory.New()
	repo := &fragileRepo{Repository: inner, err: agentkit.ErrConflict}
	hr := histmem.New()
	reg := agentkit.NewRegistry()
	step := func(ctx context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		h, _ := sys.SessionHistory(ctx)
		mu.Lock()
		seen = append(seen, histLen(h))
		mu.Unlock()
		if st.N == 1 {
			repo.armed.Store(true) // arm the one-shot conflict on entering the 2nd transition.
		}
		if _, err := sys.SessionGenerate(ctx, []gollem.Input{gollem.Text("hi")}); err != nil {
			return st, agentkit.Decision[[]byte]{}, err
		}
		st.N++
		if st.N >= 3 {
			return st, agentkit.Done([]byte("done")), nil
		}
		return st, agentkit.Continue[[]byte](), nil
	}
	ag, err := agentkit.Register(reg, "main", 1, &scriptStrategy{step: step}, agentkit.WithHistoryRepository[[]byte](hr))
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
	// T1=0, T2-attempt1=1, T2-attempt2=1 (re-seeded from committed baseline, not 2), T3=2.
	gt.Value(t, got).Equal([]int{0, 1, 1, 2})

	stored, err := hr.Load(ctx, string(pid))
	gt.NoError(t, err)
	gt.Value(t, histLen(stored)).Equal(3) // no duplication: 3 turns, not 4.
}

// ---- crash between save and commit (tolerated duplication) ----

// TestSession_CrashBetweenSaveAndCommitDuplicates injects a non-conflict Apply
// error on the second transition (a crash at commit, after the History save
// already ran). The claim abandons; a re-claim re-runs from committed State but
// loads the ahead-of-State saved history, so the conversation carries a
// duplicated turn — the accepted best-effort behavior (ADR-0017).
func TestSession_CrashBetweenSaveAndCommitDuplicates(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	var seen []int
	inner := memory.New()
	repo := &fragileRepo{Repository: inner, err: gollemErr("disk gone at commit")}
	hr := histmem.New()
	reg := agentkit.NewRegistry()
	step := func(ctx context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		h, _ := sys.SessionHistory(ctx)
		mu.Lock()
		seen = append(seen, histLen(h))
		mu.Unlock()
		if st.N == 1 {
			repo.armed.Store(true) // crash the 2nd transition's commit (after its History save).
		}
		if _, err := sys.SessionGenerate(ctx, []gollem.Input{gollem.Text("hi")}); err != nil {
			return st, agentkit.Decision[[]byte]{}, err
		}
		st.N++
		if st.N >= 3 {
			return st, agentkit.Done([]byte("done")), nil
		}
		return st, agentkit.Continue[[]byte](), nil
	}
	ag, err := agentkit.Register(reg, "main", 1, &scriptStrategy{step: step}, agentkit.WithHistoryRepository[[]byte](hr))
	gt.NoError(t, err)
	k, err := agentkit.New(repo, growingLLM(), reg)
	gt.NoError(t, err)

	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	p := serveUntil(t, k, repo, pid, 5*time.Second, isTerminal,
		agentkit.WithLease(80*time.Millisecond), agentkit.WithMaxUncleanReclaims(10))
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)

	// The aborted attempt saved a len-2 history that its State never committed; the
	// re-claim loads it and appends, so the final transcript is one turn longer
	// than the clean run's 3 — duplication is tolerated, and the process still
	// completes well-formed.
	stored, err := hr.Load(ctx, string(pid))
	gt.NoError(t, err)
	gt.Value(t, histLen(stored)).Equal(4)
}

// ---- stale-worker fence (the #1 fix) ----

// TestSession_StaleWorkerSaveSkipped simulates a newer worker reclaiming the
// Process mid-transition (the lease token changes). The pre-save fence must skip
// the stale worker's History write entirely, so its Save never reaches the
// store; the Process then recovers on a clean re-claim. Without the fence the
// stale Save would run (two Saves for a one-transition process); with it there
// is exactly one.
func TestSession_StaleWorkerSaveSkipped(t *testing.T) {
	ctx := context.Background()
	repo := memory.New()
	hr := &probeHistoryRepo{inner: histmem.New()}
	reg := agentkit.NewRegistry()
	var stole atomic.Bool
	step := func(ctx context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		if _, err := sys.SessionGenerate(ctx, []gollem.Input{gollem.Text("hi")}); err != nil {
			return st, agentkit.Decision[[]byte]{}, err
		}
		// On the first attempt, simulate a newer worker reclaiming this Process by
		// rewriting its lease token. The pre-save fence must then skip this (now
		// stale) worker's History write.
		if stole.CompareAndSwap(false, true) {
			if cur, gerr := repo.GetProcess(ctx, sys.ProcessID()); gerr == nil {
				np := agentkit.CloneProcess(cur)
				np.LeaseToken = "reclaimed-by-another-worker"
				_ = repo.Apply(ctx, agentkit.ChangeSet{Processes: []*agentkit.Process{np}})
			}
		}
		return st, agentkit.Done([]byte("done")), nil
	}
	ag, err := agentkit.Register(reg, "main", 1, &scriptStrategy{step: step}, agentkit.WithHistoryRepository[[]byte](hr))
	gt.NoError(t, err)
	k, err := agentkit.New(repo, growingLLM(), reg)
	gt.NoError(t, err)

	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	p := serveUntil(t, k, repo, pid, 5*time.Second, isTerminal,
		agentkit.WithLease(80*time.Millisecond), agentkit.WithMaxUncleanReclaims(10))
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)
	gt.Value(t, stole.Load()).Equal(true) // the steal actually happened.
	gt.Value(t, hr.saveCount()).Equal(1)  // the stale attempt's Save was fenced out.
}

// ---- terminal variants ----

func TestSession_FailTerminalSavesHistory(t *testing.T) {
	ctx := context.Background()
	hr := histmem.New()
	step := func(ctx context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		if _, err := sys.SessionGenerate(ctx, []gollem.Input{gollem.Text("hi")}); err != nil {
			return st, agentkit.Decision[[]byte]{}, err
		}
		return st, agentkit.Fail[[]byte](agentkit.FailureStrategyError, "boom"), nil
	}
	k, repo, ag := registerWithHistory(t, step, growingLLM(), hr)

	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	p := serveUntil(t, k, repo, pid, 5*time.Second, isTerminal)
	gt.Value(t, p.Status).Equal(agentkit.ProcessFailed)

	stored, err := hr.Load(ctx, string(pid)) // Fail terminal saved History too (D-D).
	gt.NoError(t, err)
	gt.Value(t, histLen(stored)).Equal(1)
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
		if _, err := sys.SessionGenerate(ctx, []gollem.Input{gollem.Text("hi")}); err != nil {
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
