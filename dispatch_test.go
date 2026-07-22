package agentkit_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gollem-dev/agentkit"
	"github.com/gollem-dev/agentkit/repository/memory"
	"github.com/m-mizutani/gt"
)

const hugePoll = time.Hour

// pollSyncRepo wraps a Repository to give eager-dispatch tests a deterministic
// "Serve is ready" signal instead of sleeping: firstEmptyPoll closes after the
// first ClaimNextProcess that finds nothing, which happens only once Serve has
// installed the dispatcher and started its poll loop. beforeApply is an optional
// hook (used to inject a fault) run before each Apply.
type pollSyncRepo struct {
	agentkit.Repository
	firstEmptyPoll chan struct{}
	closeOnce      sync.Once
	beforeApply    func(agentkit.ChangeSet) // set before Serve starts; read-only after.
}

func newPollSyncRepo() *pollSyncRepo {
	return &pollSyncRepo{Repository: memory.New(), firstEmptyPoll: make(chan struct{})}
}

func (r *pollSyncRepo) ClaimNextProcess(ctx context.Context, w string, leaseUntil, now time.Time) (*agentkit.Process, error) {
	p, err := r.Repository.ClaimNextProcess(ctx, w, leaseUntil, now)
	if p == nil && err == nil {
		r.closeOnce.Do(func() { close(r.firstEmptyPoll) })
	}
	return p, err
}

func (r *pollSyncRepo) Apply(ctx context.Context, cs agentkit.ChangeSet) error {
	if r.beforeApply != nil {
		r.beforeApply(cs)
	}
	return r.Repository.Apply(ctx, cs)
}

// startServe runs Serve in the background and blocks until the dispatcher is
// installed and the first poll has completed (via repo.firstEmptyPoll), so a
// subsequent Spawn is deterministically handled by eager dispatch — no sleep.
func startServe(t *testing.T, k *agentkit.Kernel, repo *pollSyncRepo, opts ...agentkit.ServeOption) (context.CancelFunc, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = k.Serve(ctx, opts...)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	select {
	case <-repo.firstEmptyPoll:
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not install the dispatcher / complete its first poll")
	}
	return cancel, done
}

func waitFor(t *testing.T, repo agentkit.Repository, pid agentkit.ProcessID, timeout time.Duration, want func(*agentkit.Process) bool) *agentkit.Process {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if p, err := repo.GetProcess(context.Background(), pid); err == nil && want(p) {
			return p
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
	return nil
}

func immediateDoneStep(_ context.Context, _ agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
	return st, agentkit.Done([]byte("ok")), nil
}

// setupEager builds a kernel over a pollSyncRepo with the single "main" agent.
func setupEager(t *testing.T, step stepFn) (*agentkit.Kernel, *pollSyncRepo, agentkit.Agent[scriptInput]) {
	t.Helper()
	model, _ := mockLLM(textResponse("x"))
	repo := newPollSyncRepo()
	reg := agentkit.NewRegistry()
	ag, err := agentkit.Register(reg, "main", 1, &scriptStrategy{step: step})
	gt.NoError(t, err)
	k, err := agentkit.New(repo, model, reg)
	gt.NoError(t, err)
	return k, repo, ag
}

// A spawned Process runs to completion via eager dispatch even though the poll
// loop is asleep for an hour.
func TestEagerDispatch_RunsWithoutPolling(t *testing.T) {
	k, repo, ag := setupEager(t, immediateDoneStep)
	startServe(t, k, repo, agentkit.WithPollInterval(hugePoll), agentkit.WithLease(2*time.Second))

	pid, err := ag.Spawn(context.Background(), k, scriptInput{Seed: "s"})
	gt.NoError(t, err)

	p := waitFor(t, repo, pid, 2*time.Second, isTerminal)
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)
}

// Respond resumes a waiting Process via eager dispatch, again without polling.
func TestEagerDispatch_RespondResumesWithoutPolling(t *testing.T) {
	step := func(_ context.Context, _ agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		if st.N == 0 {
			st.N = 1
			return st, agentkit.Suspend[[]byte](agentkit.Question("q", []byte("ok?"))), nil
		}
		return st, agentkit.Done([]byte("resumed")), nil
	}
	k, repo, ag := setupEager(t, step)
	startServe(t, k, repo, agentkit.WithPollInterval(hugePoll), agentkit.WithLease(2*time.Second))

	pid, err := ag.Spawn(context.Background(), k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	waitFor(t, repo, pid, 2*time.Second, func(p *agentkit.Process) bool { return p.Status == agentkit.ProcessWaiting })

	gt.NoError(t, k.Respond(context.Background(), pid, "q", []byte("yes")))
	p := waitFor(t, repo, pid, 2*time.Second, isTerminal)
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)
	gt.Value(t, string(p.Output)).Equal("resumed")
}

// A parent that spawns children and waits completes its whole tree via eager
// dispatch: children are dispatched on the parent's commit, and the last child's
// terminal commit dispatches the parent — no poll in between.
func TestEagerDispatch_ChildrenTreeCompletes(t *testing.T) {
	ctx := context.Background()
	model, _ := mockLLM(textResponse("x"))
	repo := newPollSyncRepo()
	reg := agentkit.NewRegistry()

	child, err := agentkit.Register(reg, "child", 1, &scriptStrategy{
		step: func(_ context.Context, _ agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
			return st, agentkit.Done([]byte(st.Seed)), nil
		},
	})
	gt.NoError(t, err)

	parentStep := func(c context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		if st.N == 0 {
			id1, e1 := child.SpawnChild(c, sys, scriptInput{Seed: "r1"})
			if e1 != nil {
				return st, agentkit.Decision[[]byte]{}, e1
			}
			id2, e2 := child.SpawnChild(c, sys, scriptInput{Seed: "r2"})
			if e2 != nil {
				return st, agentkit.Decision[[]byte]{}, e2
			}
			st.N = 1
			return st, agentkit.Suspend[[]byte](agentkit.WaitChildren("kids", id1, id2)), nil
		}
		aw, ok := sys.Await("kids")
		if !ok || aw.Status != agentkit.AwaitResponded {
			return st, agentkit.Decision[[]byte]{}, gollemErr("children not ready")
		}
		succeeded := 0
		for _, r := range aw.Results {
			if r.Status == agentkit.ProcessSucceeded {
				succeeded++
			}
		}
		return st, agentkit.Done([]byte(itoa(succeeded))), nil
	}
	parent, err := agentkit.Register(reg, "parent", 1, &scriptStrategy{step: parentStep})
	gt.NoError(t, err)

	k, err := agentkit.New(repo, model, reg)
	gt.NoError(t, err)
	startServe(t, k, repo, agentkit.WithPollInterval(hugePoll), agentkit.WithLease(2*time.Second))

	pid, err := parent.Spawn(ctx, k, scriptInput{Seed: "p"})
	gt.NoError(t, err)
	p := waitFor(t, repo, pid, 3*time.Second, isTerminal)
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)
	gt.Value(t, string(p.Output)).Equal("2")
}

// The hard limit bounds total concurrent claims (eager + poll). With a short
// poll interval the overflow is still picked up, and observed concurrency never
// exceeds the hard limit.
func TestEagerDispatch_HardLimitBoundsConcurrency(t *testing.T) {
	var active, maxSeen atomic.Int32
	step := func(_ context.Context, _ agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		n := active.Add(1)
		for {
			m := maxSeen.Load()
			if n <= m || maxSeen.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(40 * time.Millisecond)
		active.Add(-1)
		return st, agentkit.Done([]byte("ok")), nil
	}
	k, repo, ag := setupEager(t, step)
	startServe(t, k, repo, agentkit.WithPollInterval(2*time.Millisecond), agentkit.WithLease(2*time.Second),
		agentkit.WithMaxConcurrent(2))

	const n = 6
	pids := make([]agentkit.ProcessID, n)
	for i := 0; i < n; i++ {
		pid, err := ag.Spawn(context.Background(), k, scriptInput{Seed: "s"})
		gt.NoError(t, err)
		pids[i] = pid
	}
	for _, pid := range pids {
		p := waitFor(t, repo, pid, 5*time.Second, isTerminal)
		gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)
	}
	gt.Bool(t, maxSeen.Load() <= 2).True()
}

// A Process longer than maxStepsPerClaim keeps progressing under eager dispatch:
// runClaim releases at the step budget and the dispatcher re-submits, so it never
// falls back to the (sleeping) poll loop.
func TestEagerDispatch_LongProcessResubmits(t *testing.T) {
	step := func(_ context.Context, _ agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		if st.N >= 5 {
			return st, agentkit.Done([]byte("ok")), nil
		}
		st.N++
		return st, agentkit.Continue[[]byte](), nil
	}
	k, repo, ag := setupEager(t, step)
	startServe(t, k, repo, agentkit.WithPollInterval(hugePoll), agentkit.WithLease(2*time.Second),
		agentkit.WithMaxStepsPerClaim(2))

	pid, err := ag.Spawn(context.Background(), k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	p := waitFor(t, repo, pid, 2*time.Second, isTerminal)
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)
}

// A second Serve on the same Kernel is refused with ErrServeActive.
func TestServe_TwiceReturnsErrServeActive(t *testing.T) {
	k, repo, _ := setupEager(t, immediateDoneStep)
	startServe(t, k, repo, agentkit.WithPollInterval(hugePoll)) // waits until the first Serve owns the dispatcher.

	err := k.Serve(context.Background()) // returns immediately (does not block).
	gt.Bool(t, errors.Is(err, agentkit.ErrServeActive)).True()
}

// Without a running Serve there is no dispatcher, so Spawn leaves the row pending
// (eager dispatch is a no-op) — the only case where eager does not happen.
func TestServe_NoServeLeavesPending(t *testing.T) {
	k, repo, ag := setupEager(t, immediateDoneStep)

	pid, err := ag.Spawn(context.Background(), k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	time.Sleep(60 * time.Millisecond)
	p, err := repo.GetProcess(context.Background(), pid)
	gt.NoError(t, err)
	gt.Value(t, p.Status).Equal(agentkit.ProcessPending)
}

// Cancelling the caller's request context does not stop an eager execution: the
// run uses the Serve context, so Step never observes the request cancellation.
func TestEagerDispatch_IgnoresRequestCancellation(t *testing.T) {
	started := make(chan struct{})
	proceed := make(chan struct{})
	var stepCtxErr atomic.Pointer[error]
	step := func(ctx context.Context, _ agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		close(started)
		<-proceed
		err := ctx.Err()
		stepCtxErr.Store(&err)
		return st, agentkit.Done([]byte("ok")), nil
	}
	k, repo, ag := setupEager(t, step)
	startServe(t, k, repo, agentkit.WithPollInterval(hugePoll), agentkit.WithLease(2*time.Second))

	reqCtx, reqCancel := context.WithCancel(context.Background())
	pid, err := ag.Spawn(reqCtx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)

	<-started
	reqCancel()    // cancel the request context while Step is mid-flight.
	close(proceed) // let Step observe its own context.

	p := waitFor(t, repo, pid, 2*time.Second, isTerminal)
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)
	ep := stepCtxErr.Load()
	gt.Value(t, ep != nil).Equal(true)
	gt.NoError(t, *ep) // Step's ctx was NOT cancelled by reqCancel (it is the Serve ctx).
}

// Serve shutdown cancels an in-flight eager run: a Step blocked on ctx.Done()
// returns promptly, so Serve drains quickly rather than waiting out the step.
func TestServeShutdown_CancelsInFlightEager(t *testing.T) {
	running := make(chan struct{})
	var closeOnce atomic.Bool
	step := func(ctx context.Context, _ agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		if closeOnce.CompareAndSwap(false, true) {
			close(running)
		}
		select {
		case <-ctx.Done():
			return st, agentkit.Decision[[]byte]{}, ctx.Err()
		case <-time.After(5 * time.Second):
			return st, agentkit.Done([]byte("ok")), nil
		}
	}
	k, repo, ag := setupEager(t, step)
	cancel, done := startServe(t, k, repo, agentkit.WithPollInterval(hugePoll), agentkit.WithLease(10*time.Second))

	_, err := ag.Spawn(context.Background(), k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	<-running // the eager Step is now blocked on its context.

	start := time.Now()
	cancel()
	<-done
	gt.Bool(t, time.Since(start) < 3*time.Second).True() // cancelled, not waited out.
}

// A panic outside the transition boundary (here: a repository fault during the
// eager claim) is recovered by the dispatcher — the process is not crashed, and
// polling still completes the work.
func TestEagerDispatch_PanicRecovered(t *testing.T) {
	model, _ := mockLLM(textResponse("x"))
	repo := newPollSyncRepo()
	reg := agentkit.NewRegistry()
	ag, err := agentkit.Register(reg, "main", 1, &scriptStrategy{step: immediateDoneStep})
	gt.NoError(t, err)
	k, err := agentkit.New(repo, model, reg)
	gt.NoError(t, err)

	var panicked atomic.Bool
	// Panic once, on the eager claim Apply (a lone process flipped to running with
	// no awaits/events/guards). The poll path claims via ClaimNextProcess, not
	// Apply, so it is unaffected and completes the process.
	repo.beforeApply = func(cs agentkit.ChangeSet) {
		if len(cs.Processes) == 1 && cs.Processes[0].Status == agentkit.ProcessRunning &&
			len(cs.Awaits) == 0 && len(cs.Events) == 0 && len(cs.Guards) == 0 {
			if panicked.CompareAndSwap(false, true) {
				panic("boom during eager claim")
			}
		}
	}

	// Short poll so the poller recovers the row after the eager claim panics.
	startServe(t, k, repo, agentkit.WithPollInterval(2*time.Millisecond), agentkit.WithLease(2*time.Second))
	pid, err := ag.Spawn(context.Background(), k, scriptInput{Seed: "s"})
	gt.NoError(t, err)

	p := waitFor(t, repo, pid, 3*time.Second, isTerminal)
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)
	gt.Bool(t, panicked.Load()).True() // the panic path was actually exercised.
}

func TestServeConfig_Clamp(t *testing.T) {
	cases := []struct {
		name             string
		opts             []agentkit.ServeOption
		wantPoll, wantMx int
	}{
		{"defaults", nil, 1, 64},
		{"hard zero clamps to default", []agentkit.ServeOption{agentkit.WithMaxConcurrent(0)}, 1, 64},
		{"hard negative clamps to default", []agentkit.ServeOption{agentkit.WithMaxConcurrent(-5)}, 1, 64},
		{"soft zero clamps to one", []agentkit.ServeOption{agentkit.WithPollConcurrency(0)}, 1, 64},
		{"both set", []agentkit.ServeOption{agentkit.WithPollConcurrency(10), agentkit.WithMaxConcurrent(100)}, 10, 100},
		{"soft over hard clamps to hard", []agentkit.ServeOption{agentkit.WithPollConcurrency(200), agentkit.WithMaxConcurrent(100)}, 100, 100},
		{"soft over small hard", []agentkit.ServeOption{agentkit.WithPollConcurrency(5), agentkit.WithMaxConcurrent(3)}, 3, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			poll, mx := agentkit.ServeConfigForTest(tc.opts...)
			gt.Value(t, poll).Equal(tc.wantPoll)
			gt.Value(t, mx).Equal(tc.wantMx)
		})
	}
}

// maxStepsPerClaim <= 0 must be clamped to the default: 0 would run no transition
// and release, which under eager dispatch would re-submit in a tight loop.
func TestServeConfig_MaxStepsPerClaimClamped(t *testing.T) {
	gt.Value(t, agentkit.MaxStepsPerClaimForTest(agentkit.WithMaxStepsPerClaim(0))).Equal(16)
	gt.Value(t, agentkit.MaxStepsPerClaimForTest(agentkit.WithMaxStepsPerClaim(-3))).Equal(16)
	gt.Value(t, agentkit.MaxStepsPerClaimForTest(agentkit.WithMaxStepsPerClaim(4))).Equal(4)
}

// claimSpecific claims a pending row (and never increments UncleanReclaims), and
// refuses a row that is no longer pending.
func TestClaimSpecific_PendingOnly(t *testing.T) {
	ctx := context.Background()
	k, repo, ag := setupEager(t, immediateDoneStep)

	// Spawn without a Serve so the row stays pending (no eager dispatch).
	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)

	claimed, ok := k.ClaimSpecificForTest(ctx, pid)
	gt.Bool(t, ok).True()
	gt.Value(t, claimed.Status).Equal(agentkit.ProcessRunning)
	gt.Bool(t, claimed.LeaseToken != "").True()
	gt.Value(t, claimed.UncleanReclaims).Equal(0) // eager never charges an unclean reclaim.

	// The row is now running; a second eager claim refuses it (poller's job).
	_, ok2 := k.ClaimSpecificForTest(ctx, pid)
	gt.Bool(t, ok2).False()

	stored, err := repo.GetProcess(ctx, pid)
	gt.NoError(t, err)
	gt.Value(t, stored.UncleanReclaims).Equal(0)
}
