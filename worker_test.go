package agentkit_test

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gollem-dev/agentkit"
	"github.com/gollem-dev/agentkit/repository/memory"
	"github.com/gollem-dev/gollem"
	"github.com/gollem-dev/gollem/mock"
	"github.com/m-mizutani/gt"
)

// --- scriptable fake strategy (drives the worker in tests) ---

type scriptInput struct {
	Seed string `json:"seed"`
}

type scriptState struct {
	Seed string `json:"seed"`
	N    int    `json:"n"`
}

// scriptStrategy is a Strategy whose behavior is supplied by closures. Init
// rejects an empty Seed (to exercise the Init-error path).
type scriptStrategy struct {
	version int
	step    func(ctx context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error)
}

func (s *scriptStrategy) Version() int {
	if s.version == 0 {
		return 1
	}
	return s.version
}

func (s *scriptStrategy) Init(in scriptInput) (scriptState, error) {
	if in.Seed == "" {
		return scriptState{}, gollemErr("seed required")
	}
	return scriptState{Seed: in.Seed}, nil
}

func (s *scriptStrategy) Step(ctx context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
	return s.step(ctx, sys, st)
}

func (s *scriptStrategy) EncodeOutput(out []byte) ([]byte, error) { return out, nil }

func (s *scriptStrategy) EncodeState(st scriptState) ([]byte, error) { return json.Marshal(st) }

func (s *scriptStrategy) DecodeState(_ int, raw []byte) (scriptState, error) {
	var st scriptState
	err := json.Unmarshal(raw, &st)
	return st, err
}

func gollemErr(msg string) error { return &simpleErr{msg} }

type simpleErr struct{ msg string }

func (e *simpleErr) Error() string { return e.msg }

// --- gollem mock helpers ---

// mockLLM returns an LLMClient whose sessions yield the given responses in
// order (cycling on the last one), and whose History() returns an empty
// gollem-v3 history. callCount tracks how many Generate calls happened.
func mockLLM(responses ...*gollem.Response) (gollem.LLMClient, *int) {
	var mu sync.Mutex
	count := 0
	idx := 0
	hist := &gollem.History{LLType: gollem.LLMTypeClaude, Version: gollem.HistoryVersion}
	client := &mock.LLMClientMock{
		NewSessionFunc: func(ctx context.Context, _ ...gollem.SessionOption) (gollem.Session, error) {
			return &mock.SessionMock{
				GenerateFunc: func(_ context.Context, _ []gollem.Input, _ ...gollem.GenerateOption) (*gollem.Response, error) {
					mu.Lock()
					defer mu.Unlock()
					count++
					r := responses[idx]
					if idx < len(responses)-1 {
						idx++
					}
					return r, nil
				},
				HistoryFunc: func() (*gollem.History, error) { return hist, nil },
			}, nil
		},
	}
	return client, &count
}

// textResponse is a plain-text LLM response with token usage.
func textResponse(text string) *gollem.Response {
	return &gollem.Response{Texts: []string{text}, InputToken: 5, OutputToken: 7}
}

func mockTool(name string, result map[string]any) gollem.Tool {
	return &mock.ToolMock{
		SpecFunc: func() gollem.ToolSpec { return gollem.ToolSpec{Name: name} },
		RunFunc:  func(_ context.Context, _ map[string]any) (map[string]any, error) { return result, nil },
	}
}

// --- serve helper ---

// serveUntil runs the kernel in a background goroutine and polls until want is
// satisfied (or a timeout), then stops the worker and returns the final Process.
func serveUntil(t *testing.T, k *agentkit.Kernel, repo agentkit.Repository, pid agentkit.ProcessID, timeout time.Duration, want func(*agentkit.Process) bool, extra ...agentkit.ServeOption) *agentkit.Process {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	opts := append([]agentkit.ServeOption{
		agentkit.WithPollInterval(2 * time.Millisecond),
		agentkit.WithLease(2 * time.Second),
	}, extra...)
	go func() {
		_ = k.Serve(ctx, opts...)
		close(done)
	}()
	defer func() { cancel(); <-done }()

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

func isTerminal(p *agentkit.Process) bool { return p.Status.Terminal() }

// setup builds a kernel over memory with the given options and registers the
// script strategy "main" with the given step. Returns kernel, repo, handle.
func setupScript(t *testing.T, step stepFn, model gollem.LLMClient, opts ...agentkit.KernelOption) (*agentkit.Kernel, agentkit.Repository, agentkit.Agent[scriptInput]) {
	t.Helper()
	repo := memory.New()
	reg := agentkit.NewRegistry()
	ag, err := agentkit.Register(reg, "main", 1, &scriptStrategy{step: step})
	gt.NoError(t, err)
	k, err := agentkit.New(repo, model, reg, opts...)
	gt.NoError(t, err)
	return k, repo, ag
}

type stepFn = func(context.Context, agentkit.Syscalls, scriptState) (scriptState, agentkit.Decision[[]byte], error)

func TestUC1_GenerateThenDone(t *testing.T) {
	ctx := context.Background()
	model, count := mockLLM(textResponse("the answer"))
	step := func(c context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		res, err := sys.Generate(c, []gollem.Input{gollem.Text(st.Seed)})
		if err != nil {
			return st, agentkit.Decision[[]byte]{}, err
		}
		return st, agentkit.Done([]byte(res.Texts[0])), nil
	}
	k, repo, ag := setupScript(t, step, model)
	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "hello"})
	gt.NoError(t, err)

	p := serveUntil(t, k, repo, pid, 3*time.Second, isTerminal)
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)
	gt.Value(t, string(p.Output)).Equal("the answer")
	gt.Value(t, p.Metrics[agentkit.MetricLLMCalls]).Equal(int64(1))
	gt.Value(t, p.Metrics[agentkit.MetricInputTokens]).Equal(int64(5))
	gt.Value(t, p.Metrics[agentkit.MetricSteps]).Equal(int64(1))
	gt.Value(t, *count).Equal(1)
	events, _ := repo.ListEvents(ctx, pid)
	gt.Bool(t, hasEvent(events, agentkit.EventProcessCreated)).True()
	gt.Bool(t, hasEvent(events, agentkit.EventProcessFinished)).True()
}

func TestE6_RetryExhausted(t *testing.T) {
	ctx := context.Background()
	model, _ := mockLLM(textResponse("x"))
	step := func(c context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		_, _ = sys.Generate(c, []gollem.Input{gollem.Text("go")})
		return st, agentkit.Decision[[]byte]{}, gollemErr("boom")
	}
	// maxStepAttempts=0 -> fail on the first error (no backoff wait in the test).
	k, repo, ag := setupScript(t, step, model)
	pid, _ := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	p := serveUntil(t, k, repo, pid, 3*time.Second, isTerminal, agentkit.WithMaxStepAttempts(0))
	gt.Value(t, p.Status).Equal(agentkit.ProcessFailed)
	gt.Value(t, p.Failure.Code).Equal(agentkit.FailureRetryExhausted)
	// The consumed generate metric was folded onto the terminal Process (#5).
	gt.Value(t, p.Metrics[agentkit.MetricLLMCalls]).Equal(int64(1))
}

func TestE7_LimiterMetricsFoldNoBypass(t *testing.T) {
	ctx := context.Background()
	model, count := mockLLM(textResponse("x"))
	// A strategy that would loop forever generating.
	step := func(c context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		if _, err := sys.Generate(c, []gollem.Input{gollem.Text("go")}); err != nil {
			return st, agentkit.Decision[[]byte]{}, err
		}
		return st, agentkit.Continue[[]byte](), nil
	}
	limiter := func(_ context.Context, _ *agentkit.Process, m agentkit.Metrics) error {
		if m[agentkit.MetricLLMCalls] >= 1 {
			return gollemErr("llm cap reached")
		}
		return nil
	}
	k, repo, ag := setupScript(t, step, model, agentkit.WithLimiter(limiter))
	pid, _ := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	p := serveUntil(t, k, repo, pid, 3*time.Second, isTerminal)
	gt.Value(t, p.Status).Equal(agentkit.ProcessFailed)
	gt.Value(t, p.Failure.Code).Equal(agentkit.FailureLimitExceeded)
	// #5: the one Generate's metric was folded (committed), so the limiter sees
	// llm_calls==1 at the next boundary and stops. No bypass, no re-call.
	gt.Value(t, p.Metrics[agentkit.MetricLLMCalls]).Equal(int64(1))
	gt.Value(t, *count).Equal(1)
}

func TestE8_SuspendWithoutAwait(t *testing.T) {
	ctx := context.Background()
	model, _ := mockLLM(textResponse("x"))
	step := func(_ context.Context, _ agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		return st, agentkit.Suspend[[]byte](), nil // no awaits, none pre-open -> transition error
	}
	k, repo, ag := setupScript(t, step, model)
	pid, _ := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	p := serveUntil(t, k, repo, pid, 3*time.Second, isTerminal, agentkit.WithMaxStepAttempts(0))
	gt.Value(t, p.Status).Equal(agentkit.ProcessFailed)
	gt.Value(t, p.Failure.Code).Equal(agentkit.FailureRetryExhausted)
}

func TestE9_DoneNilOutput(t *testing.T) {
	ctx := context.Background()
	model, _ := mockLLM(textResponse("x"))
	step := func(_ context.Context, _ agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		return st, agentkit.Done[[]byte](nil), nil // nil output -> transition error
	}
	k, repo, ag := setupScript(t, step, model)
	pid, _ := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	p := serveUntil(t, k, repo, pid, 3*time.Second, isTerminal, agentkit.WithMaxStepAttempts(0))
	gt.Value(t, p.Status).Equal(agentkit.ProcessFailed)
	gt.Value(t, p.Failure.Code).Equal(agentkit.FailureRetryExhausted)
}

func TestTimerFires(t *testing.T) {
	ctx := context.Background()
	model, _ := mockLLM(textResponse("x"))
	step := func(c context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		if st.N == 0 {
			st.N = 1
			return st, agentkit.Suspend[[]byte](agentkit.Timer("t:1", sys.Now().Add(10*time.Millisecond))), nil
		}
		aw, ok := sys.Await("t:1")
		if !ok || !aw.Fired {
			return st, agentkit.Decision[[]byte]{}, gollemErr("timer not fired")
		}
		return st, agentkit.Done([]byte("fired")), nil
	}
	k, repo, ag := setupScript(t, step, model)
	pid, _ := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	p := serveUntil(t, k, repo, pid, 3*time.Second, isTerminal)
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)
	gt.Value(t, string(p.Output)).Equal("fired")
}

func TestQuestionRoundtrip(t *testing.T) {
	ctx := context.Background()
	model, _ := mockLLM(textResponse("x"))
	step := func(c context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		if st.N == 0 {
			st.N = 1
			return st, agentkit.Suspend[[]byte](agentkit.Question("q:1", []byte("confirm?"))), nil
		}
		aw, ok := sys.Await("q:1")
		if !ok || aw.Status != agentkit.AwaitResponded {
			return st, agentkit.Decision[[]byte]{}, gollemErr("no answer")
		}
		return st, agentkit.Done(aw.Response), nil
	}
	k, repo, ag := setupScript(t, step, model)
	pid, _ := ag.Spawn(ctx, k, scriptInput{Seed: "s"})

	// Run until the Process is waiting on the question.
	serveUntil(t, k, repo, pid, 3*time.Second, func(p *agentkit.Process) bool { return p.Status == agentkit.ProcessWaiting })
	events, _ := repo.ListEvents(ctx, pid)
	gt.Bool(t, hasEvent(events, agentkit.EventAwaitCreated)).True()

	gt.NoError(t, k.Respond(ctx, pid, "q:1", []byte("yes"), agentkit.WithRespondedBy("slack:U1")))

	p := serveUntil(t, k, repo, pid, 3*time.Second, isTerminal)
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)
	gt.Value(t, string(p.Output)).Equal("yes")
}

func TestChildrenWakeup(t *testing.T) {
	ctx := context.Background()
	model, _ := mockLLM(textResponse("x"))
	repo := memory.New()
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
	pid, err := parent.Spawn(ctx, k, scriptInput{Seed: "p"})
	gt.NoError(t, err)

	// WithConcurrency(4) so the two children run in parallel, exercising the
	// #3 sibling-finalize serialization and #4 buffered-child overlay.
	p := serveUntil(t, k, repo, pid, 5*time.Second, isTerminal, agentkit.WithConcurrency(4))
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)
	gt.Value(t, string(p.Output)).Equal("2") // both children succeeded and were collected.
}

// hookRepo wraps a Repository so a test can force a precise interleaving: onApply
// fires just before each Apply; failApply can reject one outright; onGet can fail
// a GetProcess.
type hookRepo struct {
	agentkit.Repository
	mu        sync.Mutex
	onApply   func(cs agentkit.ChangeSet)
	failApply func(cs agentkit.ChangeSet) error
	onGet     func(pid agentkit.ProcessID) error
}

func (h *hookRepo) Apply(ctx context.Context, cs agentkit.ChangeSet) error {
	h.mu.Lock()
	fn, fail := h.onApply, h.failApply
	h.mu.Unlock()
	if fn != nil {
		fn(cs)
	}
	if fail != nil {
		if err := fail(cs); err != nil {
			return err
		}
	}
	return h.Repository.Apply(ctx, cs)
}

func (h *hookRepo) GetProcess(ctx context.Context, pid agentkit.ProcessID) (*agentkit.Process, error) {
	h.mu.Lock()
	fn := h.onGet
	h.mu.Unlock()
	if fn != nil {
		if err := fn(pid); err != nil {
			return nil, err
		}
	}
	return h.Repository.GetProcess(ctx, pid)
}

func (h *hookRepo) setApply(fn func(agentkit.ChangeSet)) { h.mu.Lock(); h.onApply = fn; h.mu.Unlock() }

func (h *hookRepo) setFailApply(fn func(agentkit.ChangeSet) error) {
	h.mu.Lock()
	h.failApply = fn
	h.mu.Unlock()
}

// #1: a Cancel racing a claim must NOT be lost. Between Cancel's read (pending)
// and its finalize Apply, a worker claims the Process (running, new LeaseToken).
// The fix makes Cancel re-read and set CancelRequested instead of abandoning.
func TestCancelClaimRaceNotLost(t *testing.T) {
	ctx := context.Background()
	base := memory.New()
	hr := &hookRepo{Repository: base}
	reg := agentkit.NewRegistry()
	ag, _ := agentkit.Register(reg, "a", 1, &scriptStrategy{step: doneStep()})
	model, _ := mockLLM(textResponse("x"))
	k, _ := agentkit.New(hr, model, reg)
	pid, _ := ag.Spawn(ctx, k, scriptInput{Seed: "s"})

	var once sync.Once
	hr.setApply(func(cs agentkit.ChangeSet) {
		for _, p := range cs.Processes {
			if p.ID == pid && p.Status == agentkit.ProcessCancelled {
				// Simulate a worker claiming P between Cancel's read and its Apply.
				once.Do(func() {
					_, _ = base.ClaimNextProcess(ctx, "worker-B", time.Now().Add(time.Hour), time.Now())
				})
			}
		}
	})

	gt.NoError(t, k.Cancel(ctx, pid, "abort"))

	p, err := base.GetProcess(ctx, pid)
	gt.NoError(t, err)
	// The cancel was not lost: it landed as CancelRequested on the now-running Process.
	gt.Bool(t, p.CancelRequested).True()
	gt.Value(t, p.Status).Equal(agentkit.ProcessRunning)
}

// #3: after MaxStepsPerClaim is consumed, release must re-read so the Process is
// actually returned to pending (a stale-Rev release would leave it running with
// a live lease, unclaimable until the lease expired).
func TestReleaseAfterMaxStepsReclaims(t *testing.T) {
	ctx := context.Background()
	repo := memory.New()
	reg := agentkit.NewRegistry()
	step := func(_ context.Context, _ agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		if st.N == 0 {
			st.N = 1
			return st, agentkit.Continue[[]byte](), nil
		}
		return st, agentkit.Done([]byte("done")), nil
	}
	ag, _ := agentkit.Register(reg, "a", 1, &scriptStrategy{step: step})
	model, _ := mockLLM(textResponse("x"))
	k, _ := agentkit.New(repo, model, reg)
	pid, _ := ag.Spawn(ctx, k, scriptInput{Seed: "s"})

	// MaxStepsPerClaim=1 forces a release after the first Continue commit. A long
	// lease means the stale-Rev bug would strand the Process (running, leased) and
	// time out; the fix re-reads and releases to pending so it re-claims and finishes.
	p := serveUntil(t, k, repo, pid, 3*time.Second, isTerminal,
		agentkit.WithMaxStepsPerClaim(1), agentkit.WithLease(30*time.Second))
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)
}

// #4: WaitChildren must reject an id that is not a direct child.
func TestWaitChildrenRejectsNonChild(t *testing.T) {
	ctx := context.Background()
	repo := memory.New()
	reg := agentkit.NewRegistry()

	// An unrelated existing Process (no ParentID).
	stranger := agentkit.ProcessID("stranger-" + randSuffix())
	gt.NoError(t, repo.Apply(ctx, agentkit.ChangeSet{
		Processes: []*agentkit.Process{{ID: stranger, Agent: "x", Status: agentkit.ProcessSucceeded, RootID: stranger, Output: []byte("secret")}},
	}))

	step := func(_ context.Context, _ agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		return st, agentkit.Suspend[[]byte](agentkit.WaitChildren("k", stranger)), nil
	}
	ag, _ := agentkit.Register(reg, "a", 1, &scriptStrategy{step: step})
	model, _ := mockLLM(textResponse("x"))
	k, _ := agentkit.New(repo, model, reg)
	pid, _ := ag.Spawn(ctx, k, scriptInput{Seed: "s"})

	// The transition errors (ErrInvalidRequest); with MaxStepAttempts=0 it fails fast.
	p := serveUntil(t, k, repo, pid, 3*time.Second, isTerminal, agentkit.WithMaxStepAttempts(0))
	gt.Value(t, p.Status).Equal(agentkit.ProcessFailed)
	gt.Value(t, p.Failure.Code).Equal(agentkit.FailureRetryExhausted)
	// The stranger must not have been waited on / read into a ChildResult.
	aws, _ := repo.ListAwaits(ctx, pid)
	gt.Array(t, aws).Length(0)
}

// #2: a transient sibling-read failure during a child's finalize must NOT let
// the child commit terminal and lose the parent wakeup. With the fix the child's
// finalize aborts and retries (via lease expiry) until the read recovers, so the
// parent still wakes and succeeds; the swallowing bug would hang the parent.
func TestParentWakeupSurvivesTransientReadError(t *testing.T) {
	ctx := context.Background()
	base := memory.New()
	hr := &hookRepo{Repository: base}
	reg := agentkit.NewRegistry()

	child, _ := agentkit.Register(reg, "child", 1, &scriptStrategy{
		step: func(_ context.Context, _ agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
			return st, agentkit.Done([]byte(st.Seed)), nil
		},
	})
	parentStep := func(c context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		if st.N == 0 {
			id1, e := child.SpawnChild(c, sys, scriptInput{Seed: "r1"})
			if e != nil {
				return st, agentkit.Decision[[]byte]{}, e
			}
			id2, e := child.SpawnChild(c, sys, scriptInput{Seed: "r2"})
			if e != nil {
				return st, agentkit.Decision[[]byte]{}, e
			}
			st.N = 1
			return st, agentkit.Suspend[[]byte](agentkit.WaitChildren("kids", id1, id2)), nil
		}
		aw, ok := sys.Await("kids")
		if !ok || aw.Status != agentkit.AwaitResponded {
			return st, agentkit.Decision[[]byte]{}, gollemErr("children not ready")
		}
		return st, agentkit.Done([]byte("ok")), nil
	}
	parent, _ := agentkit.Register(reg, "parent", 1, &scriptStrategy{step: parentStep})
	model, _ := mockLLM(textResponse("x"))
	k, _ := agentkit.New(hr, model, reg)
	pid, _ := parent.Spawn(ctx, k, scriptInput{Seed: "p"})

	// Fail reads of an already-terminal child (i.e. a sibling read during the
	// other child's finalize) a couple of times, then let it succeed.
	var failsLeft int32 = 2
	hr.mu.Lock()
	hr.onGet = func(id agentkit.ProcessID) error {
		if p, err := base.GetProcess(ctx, id); err == nil && p.ParentID != nil && p.Status.Terminal() {
			if atomic.AddInt32(&failsLeft, -1) >= 0 {
				return gollemErr("transient read failure")
			}
		}
		return nil
	}
	hr.mu.Unlock()

	// Short lease so an aborted finalize retries quickly.
	p := serveUntil(t, k, hr, pid, 5*time.Second, isTerminal,
		agentkit.WithLease(30*time.Millisecond), agentkit.WithConcurrency(2))
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)
	gt.Value(t, string(p.Output)).Equal("ok")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// --- completion handler (fireFinish) ---

type finishOut struct {
	Text string `json:"text"`
}

// finishStrategy's EncodeOutput deliberately discards Text, so the bytes stored
// on the Process cannot reconstruct the value. A handler that nonetheless sees
// Text must have received what Done was given, not a decode of Process.Output.
type finishStrategy struct {
	step      func(ctx context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[finishOut], error)
	encodeErr error
}

func (*finishStrategy) Version() int { return 1 }

func (*finishStrategy) Init(in scriptInput) (scriptState, error) {
	if in.Seed == "" {
		return scriptState{}, gollemErr("seed required")
	}
	return scriptState{Seed: in.Seed}, nil
}

func (s *finishStrategy) Step(ctx context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[finishOut], error) {
	return s.step(ctx, sys, st)
}

func (s *finishStrategy) EncodeOutput(finishOut) ([]byte, error) {
	if s.encodeErr != nil {
		return nil, s.encodeErr
	}
	return []byte("opaque"), nil
}

func (*finishStrategy) EncodeState(st scriptState) ([]byte, error) { return json.Marshal(st) }

func (*finishStrategy) DecodeState(_ int, raw []byte) (scriptState, error) {
	var st scriptState
	err := json.Unmarshal(raw, &st)
	return st, err
}

type finishStepFn = func(context.Context, agentkit.Syscalls, scriptState) (scriptState, agentkit.Decision[finishOut], error)

// finishRecorder collects what the completion handler was given.
type finishRecorder struct {
	mu      sync.Mutex
	calls   int
	results []agentkit.FinishResult[finishOut]
}

func (r *finishRecorder) handler(_ context.Context, _ agentkit.ProcessID, res agentkit.FinishResult[finishOut]) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.results = append(r.results, res)
	return nil
}

func (r *finishRecorder) snapshot() (int, []agentkit.FinishResult[finishOut]) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls, append([]agentkit.FinishResult[finishOut](nil), r.results...)
}

func setupFinish(t *testing.T, step finishStepFn, h agentkit.FinishHandler[finishOut], opts ...agentkit.KernelOption) (*agentkit.Kernel, agentkit.Repository, agentkit.Agent[scriptInput]) {
	t.Helper()
	repo := memory.New()
	reg := agentkit.NewRegistry()
	ag, err := agentkit.Register(reg, "main", 1, &finishStrategy{step: step}, agentkit.WithOnFinish(h))
	gt.NoError(t, err)
	model, _ := mockLLM(textResponse("x"))
	k, err := agentkit.New(repo, model, reg, opts...)
	gt.NoError(t, err)
	return k, repo, ag
}

func finishDoneStep(text string) finishStepFn {
	return func(_ context.Context, _ agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[finishOut], error) {
		return st, agentkit.Done(finishOut{Text: text}), nil
	}
}

func TestFinishHandlerOnDone(t *testing.T) {
	ctx := context.Background()
	var rec finishRecorder
	k, repo, ag := setupFinish(t, finishDoneStep("the answer"), rec.handler)
	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)

	p := serveUntil(t, k, repo, pid, 3*time.Second, isTerminal)
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)

	calls, results := rec.snapshot()
	gt.Value(t, calls).Equal(1)
	gt.Value(t, results[0].Status).Equal(agentkit.ProcessSucceeded)
	gt.NotNil(t, results[0].Output)
	gt.Value(t, results[0].Output.Text).Equal("the answer")
	gt.Nil(t, results[0].Failure)

	// The persisted bytes cannot produce Text, so the handler's value came
	// straight from Done — no encode/decode round trip.
	gt.Value(t, string(p.Output)).Equal("opaque")
}

func TestFinishHandlerOnFail(t *testing.T) {
	ctx := context.Background()
	var rec finishRecorder
	step := func(_ context.Context, _ agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[finishOut], error) {
		return st, agentkit.Fail[finishOut](agentkit.FailureStrategyError, "nope"), nil
	}
	k, repo, ag := setupFinish(t, step, rec.handler)
	pid, _ := ag.Spawn(ctx, k, scriptInput{Seed: "s"})

	p := serveUntil(t, k, repo, pid, 3*time.Second, isTerminal)
	gt.Value(t, p.Status).Equal(agentkit.ProcessFailed)

	calls, results := rec.snapshot()
	gt.Value(t, calls).Equal(1)
	gt.Value(t, results[0].Status).Equal(agentkit.ProcessFailed)
	gt.Nil(t, results[0].Output)
	gt.NotNil(t, results[0].Failure)
	gt.Value(t, results[0].Failure.Code).Equal(agentkit.FailureStrategyError)
	gt.Value(t, results[0].Failure.Message).Equal("nope")
}

// A limit-exceeded termination goes through finalize, not commitTerminal, and
// must still reach the handler.
func TestFinishHandlerOnLimitExceeded(t *testing.T) {
	ctx := context.Background()
	var rec finishRecorder
	step := func(c context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[finishOut], error) {
		if _, err := sys.Generate(c, []gollem.Input{gollem.Text(st.Seed)}); err != nil {
			return st, agentkit.Decision[finishOut]{}, err
		}
		return st, agentkit.Continue[finishOut](), nil
	}
	limiter := func(_ context.Context, _ *agentkit.Process, m agentkit.Metrics) error {
		if m[agentkit.MetricLLMCalls] >= 1 {
			return gollemErr("llm cap reached")
		}
		return nil
	}
	k, repo, ag := setupFinish(t, step, rec.handler, agentkit.WithLimiter(limiter))
	pid, _ := ag.Spawn(ctx, k, scriptInput{Seed: "s"})

	p := serveUntil(t, k, repo, pid, 3*time.Second, isTerminal)
	gt.Value(t, p.Failure.Code).Equal(agentkit.FailureLimitExceeded)

	calls, results := rec.snapshot()
	gt.Value(t, calls).Equal(1)
	gt.Value(t, results[0].Status).Equal(agentkit.ProcessFailed)
	gt.Nil(t, results[0].Output)
	gt.Value(t, results[0].Failure.Code).Equal(agentkit.FailureLimitExceeded)
}

func TestFinishHandlerErrorLeavesProcessCommitted(t *testing.T) {
	ctx := context.Background()
	var calls atomic.Int32
	h := func(_ context.Context, _ agentkit.ProcessID, _ agentkit.FinishResult[finishOut]) error {
		calls.Add(1)
		return gollemErr("handler blew up")
	}
	k, repo, ag := setupFinish(t, finishDoneStep("ok"), h)
	pid, _ := ag.Spawn(ctx, k, scriptInput{Seed: "s"})

	p := serveUntil(t, k, repo, pid, 3*time.Second, isTerminal)
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)
	gt.Value(t, calls.Load()).Equal(int32(1))
	gt.Nil(t, p.Failure)
}

func TestFinishHandlerPanicDoesNotKillWorker(t *testing.T) {
	ctx := context.Background()
	var calls atomic.Int32
	h := func(_ context.Context, _ agentkit.ProcessID, _ agentkit.FinishResult[finishOut]) error {
		calls.Add(1)
		panic("handler panic")
	}
	k, repo, ag := setupFinish(t, finishDoneStep("ok"), h)

	first, _ := ag.Spawn(ctx, k, scriptInput{Seed: "a"})
	second, _ := ag.Spawn(ctx, k, scriptInput{Seed: "b"})

	// The worker must survive the first panic and go on to claim the second.
	p2 := serveUntil(t, k, repo, second, 5*time.Second, isTerminal)
	gt.Value(t, p2.Status).Equal(agentkit.ProcessSucceeded)
	p1, _ := repo.GetProcess(ctx, first)
	gt.Value(t, p1.Status).Equal(agentkit.ProcessSucceeded)
	gt.Value(t, calls.Load()).Equal(int32(2))
}

// A worker draining on a cancelled context must still deliver the notification:
// fireFinish detaches cancellation from the handler's context.
func TestFinishHandlerRunsWhenServeContextIsCancelled(t *testing.T) {
	ctx := context.Background()
	released := make(chan struct{})
	var sawErr atomic.Bool
	h := func(hctx context.Context, _ agentkit.ProcessID, _ agentkit.FinishResult[finishOut]) error {
		if hctx.Err() != nil {
			sawErr.Store(true)
		}
		close(released)
		return nil
	}
	k, repo, ag := setupFinish(t, finishDoneStep("ok"), h)
	pid, _ := ag.Spawn(ctx, k, scriptInput{Seed: "s"})

	serveCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = k.Serve(serveCtx, agentkit.WithPollInterval(2*time.Millisecond), agentkit.WithLease(2*time.Second))
		close(done)
	}()
	select {
	case <-released:
	case <-time.After(3 * time.Second):
		t.Fatal("handler was not called")
	}
	cancel()
	<-done

	gt.Bool(t, sawErr.Load()).False()
	p, _ := repo.GetProcess(ctx, pid)
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)
}

// The handler runs synchronously inside commitFinal, so a blocked handler must
// hold the terminal commit open rather than letting the worker move on.
func TestFinishHandlerIsSynchronous(t *testing.T) {
	ctx := context.Background()
	entered := make(chan struct{})
	release := make(chan struct{})
	h := func(_ context.Context, _ agentkit.ProcessID, _ agentkit.FinishResult[finishOut]) error {
		close(entered)
		<-release
		return nil
	}
	k, repo, ag := setupFinish(t, finishDoneStep("ok"), h)
	pid, _ := ag.Spawn(ctx, k, scriptInput{Seed: "s"})

	serveCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = k.Serve(serveCtx, agentkit.WithPollInterval(2*time.Millisecond), agentkit.WithLease(2*time.Second))
		close(done)
	}()
	defer func() { cancel(); <-done }()

	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("handler was not called")
	}
	// While the handler blocks, the terminal Apply has landed but the worker is
	// still inside commitFinal.
	p, err := repo.GetProcess(ctx, pid)
	gt.NoError(t, err)
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)
	close(release)
}

// An unknown agent terminates through finalize with no binding to look up. It
// must fail cleanly rather than panic on the nil finish closure.
func TestFinishHandlerAbsentForUnknownAgent(t *testing.T) {
	ctx := context.Background()
	repo := memory.New()
	reg := agentkit.NewRegistry()
	var rec finishRecorder
	ag, err := agentkit.Register(reg, "main", 1, &finishStrategy{step: finishDoneStep("ok")},
		agentkit.WithOnFinish(rec.handler))
	gt.NoError(t, err)
	model, _ := mockLLM(textResponse("x"))
	k, err := agentkit.New(repo, model, reg)
	gt.NoError(t, err)

	pid, _ := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	// Rewrite the row to name an agent nobody registered.
	p, _ := repo.GetProcess(ctx, pid)
	p.Agent = "ghost"
	gt.NoError(t, repo.Apply(ctx, agentkit.ChangeSet{Processes: []*agentkit.Process{p}}))

	got := serveUntil(t, k, repo, pid, 3*time.Second, isTerminal)
	gt.Value(t, got.Status).Equal(agentkit.ProcessFailed)
	calls, _ := rec.snapshot()
	gt.Value(t, calls).Equal(0)
}

// Concurrent workers race for every claim, but only the one whose terminal
// Apply lands reaches fireFinish. Each Process must notify exactly once.
func TestFinishHandlerFiresOncePerProcessUnderConcurrency(t *testing.T) {
	ctx := context.Background()
	const n = 12
	var rec finishRecorder
	k, repo, ag := setupFinish(t, finishDoneStep("ok"), rec.handler)

	pids := make([]agentkit.ProcessID, 0, n)
	for i := 0; i < n; i++ {
		pid, err := ag.Spawn(ctx, k, scriptInput{Seed: itoa(i)})
		gt.NoError(t, err)
		pids = append(pids, pid)
	}

	serveCtx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = k.Serve(serveCtx,
				agentkit.WithPollInterval(2*time.Millisecond),
				agentkit.WithLease(2*time.Second),
				agentkit.WithConcurrency(2))
		}()
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		allDone := true
		for _, pid := range pids {
			p, err := repo.GetProcess(ctx, pid)
			if err != nil || !p.Status.Terminal() {
				allDone = false
				break
			}
		}
		if allDone {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			wg.Wait()
			t.Fatal("processes did not all finish")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	wg.Wait()

	calls, results := rec.snapshot()
	gt.Value(t, calls).Equal(n)
	for _, res := range results {
		gt.Value(t, res.Status).Equal(agentkit.ProcessSucceeded)
		gt.NotNil(t, res.Output)
	}
}

// EncodeOutput runs in the worker after the Step middleware chain, so its
// failure is a transition error: the Process never reaches a terminal state on
// that attempt and the handler is not called.
func TestEncodeOutputErrorFailsTheTransition(t *testing.T) {
	ctx := context.Background()
	var rec finishRecorder

	repo := memory.New()
	reg := agentkit.NewRegistry()
	ag, err := agentkit.Register(reg, "main", 1,
		&finishStrategy{step: finishDoneStep("ok"), encodeErr: gollemErr("cannot encode")},
		agentkit.WithOnFinish(rec.handler))
	gt.NoError(t, err)
	model, _ := mockLLM(textResponse("x"))
	k, err := agentkit.New(repo, model, reg)
	gt.NoError(t, err)

	pid, _ := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	p := serveUntil(t, k, repo, pid, 3*time.Second, isTerminal, agentkit.WithMaxStepAttempts(0))

	// Retries are exhausted rather than the Process succeeding with no output.
	gt.Value(t, p.Status).Equal(agentkit.ProcessFailed)
	gt.Value(t, p.Failure.Code).Equal(agentkit.FailureRetryExhausted)
	gt.Nil(t, p.Output)

	// The handler still fires for the failed terminal, but never for a success.
	_, results := rec.snapshot()
	for _, res := range results {
		gt.Value(t, res.Status).NotEqual(agentkit.ProcessSucceeded)
	}
}
