package agentkit_test

import (
	"context"
	"encoding/json"
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
	limit   agentkit.Limiter // nil = unlimited.
}

func (s *scriptStrategy) Limit(ctx context.Context, proc *agentkit.Process, m agentkit.Metrics) agentkit.LimitDecision {
	if s.limit == nil {
		return agentkit.LimitPass()
	}
	return s.limit(ctx, proc, m)
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
		// The default curve spends 2+4+8 seconds to exhaust a three-attempt budget.
		// Tests here are about what the retry does, not how long it waits; a test
		// that cares about the wait passes its own curve in extra.
		agentkit.WithRetryBackoff(func(int) time.Duration { return time.Millisecond }),
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
	return setupScriptLimited(t, step, model, nil, opts...)
}

// setupScriptLimited is setupScript with a budget. The limiter goes on the
// strategy, not the Kernel, so a test that wants one has to register with it.
func setupScriptLimited(t *testing.T, step stepFn, model gollem.LLMClient, limit agentkit.Limiter,
	opts ...agentkit.KernelOption) (*agentkit.Kernel, agentkit.Repository, agentkit.Agent[scriptInput]) {
	t.Helper()
	repo := memory.New()
	reg := agentkit.NewRegistry()
	ag, err := agentkit.Register(reg, "main", 1, &scriptStrategy{step: step, limit: limit})
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
	gt.Value(t, p.Metrics.LLMCalls).Equal(int64(1))
	gt.Value(t, p.Metrics.InputTokens).Equal(int64(5))
	gt.Value(t, p.Metrics.Steps).Equal(int64(1))
	// textResponse sets no cache fields, so this is the "no caching" case
	// (indistinguishable from a non-Claude provider) — the breakdown stays 0.
	gt.Value(t, p.Metrics.CacheReadInputTokens).Equal(int64(0))
	gt.Value(t, p.Metrics.CacheCreationInputTokens).Equal(int64(0))
	gt.Value(t, *count).Equal(1)
	events, _ := repo.ListEvents(ctx, pid, agentkit.EventQuery{})
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
	gt.Value(t, p.Metrics.LLMCalls).Equal(int64(1))
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
	limiter := func(_ context.Context, _ *agentkit.Process, m agentkit.Metrics) agentkit.LimitDecision {
		if m.LLMCalls >= 1 {
			return agentkit.LimitStop("llm cap reached")
		}
		return agentkit.LimitPass()
	}
	k, repo, ag := setupScriptLimited(t, step, model, limiter)
	pid, _ := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	p := serveUntil(t, k, repo, pid, 3*time.Second, isTerminal)
	gt.Value(t, p.Status).Equal(agentkit.ProcessFailed)
	gt.Value(t, p.Failure.Code).Equal(agentkit.FailureLimitExceeded)
	// The reason reaches the row verbatim: the Limiter hands over a string and
	// the kernel is the only thing that ever makes it an error.
	gt.Value(t, p.Failure.Message).Equal("llm cap reached")
	// #5: the one Generate's metric was folded (committed), so the limiter sees
	// llm_calls==1 at the next boundary and stops. No bypass, no re-call.
	gt.Value(t, p.Metrics.LLMCalls).Equal(int64(1))
	gt.Value(t, *count).Equal(1)
}

// Limit runs at the transition boundary, which is outside runTransition's
// recover. A panic there must become a transition error like any other piece of
// strategy code that blew up -- never a dead worker.
func TestLimitPanicAtBoundaryIsATransitionError(t *testing.T) {
	ctx := context.Background()
	model, count := mockLLM(textResponse("x"))
	step := func(_ context.Context, _ agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		return st, agentkit.Done([]byte("unreachable")), nil
	}
	limiter := func(_ context.Context, _ *agentkit.Process, _ agentkit.Metrics) agentkit.LimitDecision {
		panic("budget lookup exploded")
	}
	k, repo, ag := setupScriptLimited(t, step, model, limiter)
	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)

	// maxStepAttempts=0 turns the first failure into the last one, so this also
	// proves the panic went down the ordinary retry path rather than some
	// separate handling.
	p := serveUntil(t, k, repo, pid, 3*time.Second, isTerminal, agentkit.WithMaxStepAttempts(0))
	gt.Value(t, p.Status).Equal(agentkit.ProcessFailed)
	gt.Value(t, p.Failure.Code).Equal(agentkit.FailureRetryExhausted)
	// Step never ran: the boundary refused before the strategy was reached.
	gt.Value(t, *count).Equal(0)
	gt.Value(t, p.Metrics.LLMCalls).Equal(int64(0))
}

// One Serve, two Processes: the panicking one must not stop the loop from
// driving the other. Both are waited for inside a single serveUntil, because
// serveUntil cancels its Serve on return — two calls would prove only that a
// fresh worker works, which is not the claim.
func TestLimitPanicDoesNotKillTheWorker(t *testing.T) {
	ctx := context.Background()
	model, _ := mockLLM(textResponse("x"))
	repo := memory.New()
	reg := agentkit.NewRegistry()

	doneStep := func(_ context.Context, _ agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		return st, agentkit.Done([]byte("fine")), nil
	}
	boom, err := agentkit.Register(reg, "boom", 1, &scriptStrategy{
		step: doneStep,
		limit: func(_ context.Context, _ *agentkit.Process, _ agentkit.Metrics) agentkit.LimitDecision {
			panic("budget lookup exploded")
		},
	})
	gt.NoError(t, err)
	healthy, err := agentkit.Register(reg, "healthy", 1, &scriptStrategy{step: doneStep})
	gt.NoError(t, err)

	k, err := agentkit.New(repo, model, reg)
	gt.NoError(t, err)

	badPID, err := boom.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	goodPID, err := healthy.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)

	good := serveUntil(t, k, repo, goodPID, 3*time.Second, func(p *agentkit.Process) bool {
		if !p.Status.Terminal() {
			return false
		}
		bad, err := repo.GetProcess(ctx, badPID)
		return err == nil && bad.Status.Terminal()
	}, agentkit.WithMaxStepAttempts(0))
	gt.Value(t, good.Status).Equal(agentkit.ProcessSucceeded)
	gt.Value(t, string(good.Output)).Equal("fine")

	bad := gt.R1(repo.GetProcess(ctx, badPID)).NoError(t)
	gt.Value(t, bad.Status).Equal(agentkit.ProcessFailed)
	gt.Value(t, bad.Failure.Code).Equal(agentkit.FailureRetryExhausted)
}

// The other two call sites run inside runTransition, so their panics are the
// recover there rather than callLimit. These assert that the asymmetry actually
// holds -- and that a panic AFTER an effect still folds that effect's metrics in.
func TestLimitPanicInsideAnEffect(t *testing.T) {
	step := func(c context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		if _, err := sys.Generate(c, []gollem.Input{gollem.Text("go")}); err != nil {
			return st, agentkit.Decision[[]byte]{}, err
		}
		return st, agentkit.Done([]byte("ok")), nil
	}
	// Call 1 is the transition boundary, 2 is checkLimit before Generate, 3 is
	// meter after it.
	panicOnCall := func(n int) agentkit.Limiter {
		var mu sync.Mutex
		calls := 0
		return func(_ context.Context, _ *agentkit.Process, _ agentkit.Metrics) agentkit.LimitDecision {
			mu.Lock()
			calls++
			c := calls
			mu.Unlock()
			if c == n {
				panic("budget lookup exploded")
			}
			return agentkit.LimitPass()
		}
	}

	t.Run("before the effect: nothing ran, nothing counted", func(t *testing.T) {
		ctx := context.Background()
		model, count := mockLLM(textResponse("x"))
		k, repo, ag := setupScriptLimited(t, step, model, panicOnCall(2))
		pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
		gt.NoError(t, err)

		p := serveUntil(t, k, repo, pid, 3*time.Second, isTerminal, agentkit.WithMaxStepAttempts(0))
		gt.Value(t, p.Status).Equal(agentkit.ProcessFailed)
		gt.Value(t, p.Failure.Code).Equal(agentkit.FailureRetryExhausted)
		gt.Value(t, *count).Equal(0)
		gt.Value(t, p.Metrics.LLMCalls).Equal(int64(0))
	})

	t.Run("after the effect: the Generate is counted anyway", func(t *testing.T) {
		ctx := context.Background()
		model, count := mockLLM(textResponse("x"))
		k, repo, ag := setupScriptLimited(t, step, model, panicOnCall(3))
		pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
		gt.NoError(t, err)

		p := serveUntil(t, k, repo, pid, 3*time.Second, isTerminal, agentkit.WithMaxStepAttempts(0))
		gt.Value(t, p.Status).Equal(agentkit.ProcessFailed)
		gt.Value(t, p.Failure.Code).Equal(agentkit.FailureRetryExhausted)
		// The call happened and cost real tokens, so it is on the row even though
		// the transition never committed a state change.
		gt.Value(t, *count).Equal(1)
		gt.Value(t, p.Metrics.LLMCalls).Equal(int64(1))
	})
}

// A notice reaches the strategy and does not end the Process. This is the whole
// point of the third verdict: a budget can warn without refusing.
func TestLimitNoticeReachesStrategyWithoutStopping(t *testing.T) {
	ctx := context.Background()
	model, _ := mockLLM(textResponse("x"))
	var seen []string
	step := func(c context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		d := sys.LimitStatus()
		seen = append(seen, string(d.Kind())+":"+d.Message())
		if _, err := sys.Generate(c, []gollem.Input{gollem.Text("go")}); err != nil {
			return st, agentkit.Decision[[]byte]{}, err
		}
		return st, agentkit.Done([]byte("ok")), nil
	}
	limiter := func(_ context.Context, _ *agentkit.Process, _ agentkit.Metrics) agentkit.LimitDecision {
		return agentkit.LimitNotice("careful")
	}
	k, repo, ag := setupScriptLimited(t, step, model, limiter)
	pid, _ := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	p := serveUntil(t, k, repo, pid, 3*time.Second, isTerminal)

	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)
	gt.Array(t, seen).Length(1)
	gt.Value(t, seen[0]).Equal("notice:careful")
}

// Without a Limiter the verdict is the zero value, which has to read as a pass
// rather than as an empty kind.
func TestLimitStatusWithoutLimiterIsPass(t *testing.T) {
	ctx := context.Background()
	model, _ := mockLLM(textResponse("x"))
	var kind agentkit.LimitKind
	var msg string
	step := func(_ context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		kind, msg = sys.LimitStatus().Kind(), sys.LimitStatus().Message()
		return st, agentkit.Done([]byte("ok")), nil
	}
	k, repo, ag := setupScript(t, step, model)
	pid, _ := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	serveUntil(t, k, repo, pid, 3*time.Second, isTerminal)

	gt.Value(t, kind).Equal(agentkit.LimitKindPass)
	gt.Value(t, msg).Equal("")
}

// The verdict has to advance with Metrics(), not lag a call behind it: a
// Limiter that only warns once the tokens are spent is useless if the strategy
// cannot see the warning until its next effect.
func TestLimitStatusRefreshesRightAfterGenerate(t *testing.T) {
	ctx := context.Background()
	model, _ := mockLLM(textResponse("x"))
	var before, after agentkit.LimitDecision
	step := func(c context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		before = sys.LimitStatus()
		if _, err := sys.Generate(c, []gollem.Input{gollem.Text("go")}); err != nil {
			return st, agentkit.Decision[[]byte]{}, err
		}
		after = sys.LimitStatus()
		return st, agentkit.Done([]byte("ok")), nil
	}
	limiter := func(_ context.Context, _ *agentkit.Process, m agentkit.Metrics) agentkit.LimitDecision {
		if m.LLMCalls >= 1 {
			return agentkit.LimitNotice("nearly out")
		}
		return agentkit.LimitPass()
	}
	k, repo, ag := setupScriptLimited(t, step, model, limiter)
	pid, _ := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	p := serveUntil(t, k, repo, pid, 3*time.Second, isTerminal)

	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)
	gt.Value(t, before.Kind()).Equal(agentkit.LimitKindPass)
	gt.Value(t, after.Kind()).Equal(agentkit.LimitKindNotice)
	gt.Value(t, after.Message()).Equal("nearly out")
}

// An effect that crosses the cap is not undone, but the strategy has to be able
// to see that it crossed it — that is the difference between finishing with the
// result in hand and discovering the refusal one effect later.
func TestLimitStopAfterEffectDoesNotFailTheEffect(t *testing.T) {
	ctx := context.Background()
	model, _ := mockLLM(textResponse("x"))
	var genErr error
	var after agentkit.LimitDecision
	step := func(c context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		_, genErr = sys.Generate(c, []gollem.Input{gollem.Text("go")})
		after = sys.LimitStatus()
		return st, agentkit.Done([]byte("ok")), nil
	}
	// Passes before the call (llm_calls == 0), refuses after it (llm_calls == 1).
	limiter := func(_ context.Context, _ *agentkit.Process, m agentkit.Metrics) agentkit.LimitDecision {
		if m.LLMCalls >= 1 {
			return agentkit.LimitStop("cap crossed")
		}
		return agentkit.LimitPass()
	}
	k, repo, ag := setupScriptLimited(t, step, model, limiter)
	pid, _ := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	p := serveUntil(t, k, repo, pid, 3*time.Second, isTerminal)

	// The Generate itself stands: it had already run when the Limiter refused.
	gt.NoError(t, genErr)
	// But the refusal is visible, so the strategy can act on it.
	gt.Value(t, after.Kind()).Equal(agentkit.LimitKindStop)
	gt.Value(t, after.Message()).Equal("cap crossed")
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)
}

// The point of making a post-effect refusal visible: a strategy can wrap up with
// the result it already paid for, instead of spending another effect to find out.
func TestStrategyCanWrapUpOnCrossingTheCap(t *testing.T) {
	ctx := context.Background()
	model, count := mockLLM(textResponse("first"), textResponse("second"))
	step := func(c context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		if _, err := sys.Generate(c, []gollem.Input{gollem.Text("go")}); err != nil {
			return st, agentkit.Decision[[]byte]{}, err
		}
		if sys.LimitStatus().Kind() == agentkit.LimitKindStop {
			return st, agentkit.Done([]byte("wrapped up")), nil
		}
		return st, agentkit.Continue[[]byte](), nil
	}
	limiter := func(_ context.Context, _ *agentkit.Process, m agentkit.Metrics) agentkit.LimitDecision {
		if m.LLMCalls >= 1 {
			return agentkit.LimitStop("cap crossed")
		}
		return agentkit.LimitPass()
	}
	k, repo, ag := setupScriptLimited(t, step, model, limiter)
	pid, _ := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	p := serveUntil(t, k, repo, pid, 3*time.Second, isTerminal)

	// Succeeded rather than failed(limit_exceeded): the strategy got to decide.
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)
	gt.String(t, string(p.Output)).Contains("wrapped up")
	gt.Value(t, *count).Equal(1) // no second Generate was spent finding out.
}

// A strategy is allowed to catch ErrLimitExceeded and carry on, so the refusal
// it caught has to remain readable without parsing the error's text.
func TestLimitStopBeforeEffectIsReadableAfterCatching(t *testing.T) {
	ctx := context.Background()
	model, _ := mockLLM(textResponse("x"))
	var genErr error
	var after agentkit.LimitDecision
	step := func(c context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		_, genErr = sys.Generate(c, []gollem.Input{gollem.Text("go")})
		after = sys.LimitStatus()
		return st, agentkit.Done([]byte("ok")), nil // swallow it, as a strategy may.
	}
	calls := &int32Box{}
	limiter := func(_ context.Context, _ *agentkit.Process, _ agentkit.Metrics) agentkit.LimitDecision {
		calls.inc()
		if calls.get() == 1 {
			return agentkit.LimitNotice("warned") // the transition boundary.
		}
		return agentkit.LimitStop("over")
	}
	k, repo, ag := setupScriptLimited(t, step, model, limiter)
	pid, _ := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	serveUntil(t, k, repo, pid, 3*time.Second, isTerminal)

	gt.Value(t, errors.Is(genErr, agentkit.ErrLimitExceeded)).Equal(true)
	gt.Value(t, after.Kind()).Equal(agentkit.LimitKindStop)
	gt.Value(t, after.Message()).Equal("over")
}

// A Limiter that stops warning has to be able to say so.
func TestLimitNoticeClears(t *testing.T) {
	ctx := context.Background()
	model, _ := mockLLM(textResponse("x"))
	var after agentkit.LimitDecision
	step := func(c context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		if _, err := sys.Generate(c, []gollem.Input{gollem.Text("go")}); err != nil {
			return st, agentkit.Decision[[]byte]{}, err
		}
		after = sys.LimitStatus()
		return st, agentkit.Done([]byte("ok")), nil
	}
	calls := &int32Box{}
	limiter := func(_ context.Context, _ *agentkit.Process, _ agentkit.Metrics) agentkit.LimitDecision {
		calls.inc()
		if calls.get() == 1 {
			return agentkit.LimitNotice("x")
		}
		return agentkit.LimitPass()
	}
	k, repo, ag := setupScriptLimited(t, step, model, limiter)
	pid, _ := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	serveUntil(t, k, repo, pid, 3*time.Second, isTerminal)

	gt.Value(t, after.Kind()).Equal(agentkit.LimitKindPass)
	gt.Value(t, after.Message()).Equal("")
}

// meter runs only where addMetrics used to, so a failed Generate charges
// nothing and refreshes nothing.
func TestFailedGenerateUpdatesNeitherMetricsNorLimitStatus(t *testing.T) {
	ctx := context.Background()
	model := &mock.LLMClientMock{
		NewSessionFunc: func(_ context.Context, _ ...gollem.SessionOption) (gollem.Session, error) {
			return nil, gollemErr("model down")
		},
	}
	var after agentkit.LimitDecision
	step := func(c context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		_, _ = sys.Generate(c, []gollem.Input{gollem.Text("go")})
		after = sys.LimitStatus()
		return st, agentkit.Done([]byte("ok")), nil
	}
	limiter := func(_ context.Context, _ *agentkit.Process, m agentkit.Metrics) agentkit.LimitDecision {
		if m.LLMCalls >= 1 {
			return agentkit.LimitNotice("spent")
		}
		return agentkit.LimitPass()
	}
	k, repo, ag := setupScriptLimited(t, step, model, limiter)
	pid, _ := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	p := serveUntil(t, k, repo, pid, 3*time.Second, isTerminal)

	gt.Value(t, after.Kind()).Equal(agentkit.LimitKindPass)
	gt.Value(t, p.Metrics.LLMCalls).Equal(int64(0))
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
	events, _ := repo.ListEvents(ctx, pid, agentkit.EventQuery{})
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

	// WithPollConcurrency(4) so the two children run in parallel, exercising the
	// #3 sibling-finalize serialization and #4 buffered-child overlay.
	p := serveUntil(t, k, repo, pid, 5*time.Second, isTerminal, agentkit.WithPollConcurrency(4))
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)
	gt.Value(t, string(p.Output)).Equal("2") // both children succeeded and were collected.
}

// TestChildMetricsRollUpToParent exercises worker.go:1122
// (pClone.Metrics = pClone.Metrics.add(child.Metrics)), the fold that runs on
// the child's own terminal transition. The parent itself never calls
// Generate, so every token counter on its final Metrics — cache breakdown
// included — has to have come from the child.
func TestChildMetricsRollUpToParent(t *testing.T) {
	ctx := context.Background()
	// Four distinct values so a fold that swaps two counters cannot pass by
	// coincidence.
	model, _ := mockLLM(&gollem.Response{
		Texts:                   []string{"child done"},
		InputToken:              21,
		OutputToken:             23,
		CacheCreationInputToken: 27,
		CacheReadInputToken:     29,
	})
	repo := memory.New()
	reg := agentkit.NewRegistry()

	child, err := agentkit.Register(reg, "child", 1, &scriptStrategy{
		step: func(c context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
			res, err := sys.Generate(c, []gollem.Input{gollem.Text(st.Seed)})
			if err != nil {
				return st, agentkit.Decision[[]byte]{}, err
			}
			return st, agentkit.Done([]byte(res.Texts[0])), nil
		},
	})
	gt.NoError(t, err)

	parentStep := func(c context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		if st.N == 0 {
			id, e := child.SpawnChild(c, sys, scriptInput{Seed: "r1"})
			if e != nil {
				return st, agentkit.Decision[[]byte]{}, e
			}
			st.N = 1
			return st, agentkit.Suspend[[]byte](agentkit.WaitChildren("kid", id)), nil
		}
		aw, ok := sys.Await("kid")
		if !ok || aw.Status != agentkit.AwaitResponded {
			return st, agentkit.Decision[[]byte]{}, gollemErr("child not ready")
		}
		return st, agentkit.Done([]byte("parent done")), nil
	}
	parent, err := agentkit.Register(reg, "parent", 1, &scriptStrategy{step: parentStep})
	gt.NoError(t, err)

	k, err := agentkit.New(repo, model, reg)
	gt.NoError(t, err)
	pid, err := parent.Spawn(ctx, k, scriptInput{Seed: "p"})
	gt.NoError(t, err)

	p := serveUntil(t, k, repo, pid, 5*time.Second, isTerminal)
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)

	// The parent never calls Generate itself, so every token counter here is
	// the child's own, folded in once at the child's terminal transition. The
	// parent commits two of its own transitions (spawn+suspend, then
	// collect+done), the child one (Generate+Done) — Steps rolls up the same
	// way the token counters do, so the parent's final Steps is its own 2 plus
	// the child's 1.
	gt.Value(t, p.Metrics).Equal(agentkit.Metrics{
		InputTokens: 21, OutputTokens: 23, CacheCreationInputTokens: 27, CacheReadInputTokens: 29,
		LLMCalls: 1, Spawns: 1, Steps: 3,
	})
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
		agentkit.WithLease(30*time.Millisecond), agentkit.WithPollConcurrency(2))
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
	limit     agentkit.Limiter // nil = unlimited.
}

func (*finishStrategy) Version() int { return 1 }

func (s *finishStrategy) Limit(ctx context.Context, proc *agentkit.Process, m agentkit.Metrics) agentkit.LimitDecision {
	if s.limit == nil {
		return agentkit.LimitPass()
	}
	return s.limit(ctx, proc, m)
}

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
	return setupFinishLimited(t, step, h, nil, opts...)
}

// setupFinishLimited is setupFinish with a budget on the strategy.
func setupFinishLimited(t *testing.T, step finishStepFn, h agentkit.FinishHandler[finishOut],
	limit agentkit.Limiter, opts ...agentkit.KernelOption) (*agentkit.Kernel, agentkit.Repository, agentkit.Agent[scriptInput]) {
	t.Helper()
	repo := memory.New()
	reg := agentkit.NewRegistry()
	ag, err := agentkit.Register(reg, "main", 1, &finishStrategy{step: step, limit: limit}, agentkit.WithOnFinish(h))
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
	limiter := func(_ context.Context, _ *agentkit.Process, m agentkit.Metrics) agentkit.LimitDecision {
		if m.LLMCalls >= 1 {
			return agentkit.LimitStop("llm cap reached")
		}
		return agentkit.LimitPass()
	}
	k, repo, ag := setupFinishLimited(t, step, rec.handler, limiter)
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

// Many processes drained by several workers yield exactly one notification
// each. A claim is exclusive, so this does NOT exercise two workers racing the
// same terminal Apply — see the deterministic tests below for that.
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
				agentkit.WithPollConcurrency(2))
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

// --- fireFinish's committed-once property, forced deterministically ---
//
// The concurrency test above shows N processes yield N notifications, but a
// claim is exclusive, so it does not actually make two workers race the same
// terminal Apply. These do, by driving the Repository directly.

func setupFinishOn(t *testing.T, repo agentkit.Repository, h agentkit.FinishHandler[finishOut]) (*agentkit.Kernel, agentkit.Agent[scriptInput]) {
	t.Helper()
	reg := agentkit.NewRegistry()
	ag, err := agentkit.Register(reg, "main", 1, &finishStrategy{step: finishDoneStep("ok")},
		agentkit.WithOnFinish(h))
	gt.NoError(t, err)
	model, _ := mockLLM(textResponse("x"))
	k, err := agentkit.New(repo, model, reg)
	gt.NoError(t, err)
	return k, ag
}

// A conflict the worker still owns the lease for is rebuilt and retried inside
// commitFinal. The retry must not notify twice.
func TestFinishHandlerFiresOnceAcrossACommitRetry(t *testing.T) {
	ctx := context.Background()
	base := memory.New()
	hr := &hookRepo{Repository: base}
	var rec finishRecorder
	k, ag := setupFinishOn(t, hr, rec.handler)
	pid, _ := ag.Spawn(ctx, k, scriptInput{Seed: "s"})

	var applies atomic.Int32
	var once sync.Once
	hr.setFailApply(func(cs agentkit.ChangeSet) error {
		for _, p := range cs.Processes {
			if p.ID == pid && p.Status == agentkit.ProcessSucceeded {
				applies.Add(1)
				var reject bool
				once.Do(func() { reject = true })
				if reject {
					return agentkit.ErrConflict
				}
			}
		}
		return nil
	})

	p := serveUntil(t, k, hr, pid, 3*time.Second, isTerminal)
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)
	// The terminal Apply really was attempted twice...
	gt.Number(t, applies.Load()).GreaterOrEqual(int32(2))
	// ...and the handler still fired exactly once.
	calls, results := rec.snapshot()
	gt.Value(t, calls).Equal(1)
	gt.Value(t, results[0].Status).Equal(agentkit.ProcessSucceeded)
}

// A worker that lost its lease abandons the finalize instead of rebasing, so it
// must not notify: the winner will.
func TestFinishHandlerDoesNotFireWhenTheLeaseWasLost(t *testing.T) {
	ctx := context.Background()
	base := memory.New()
	hr := &hookRepo{Repository: base}
	var rec finishRecorder
	k, ag := setupFinishOn(t, hr, rec.handler)
	pid, _ := ag.Spawn(ctx, k, scriptInput{Seed: "s"})

	var once sync.Once
	hr.setFailApply(func(cs agentkit.ChangeSet) error {
		var steal bool
		for _, p := range cs.Processes {
			if p.ID == pid && p.Status == agentkit.ProcessSucceeded {
				once.Do(func() { steal = true })
			}
		}
		if !steal {
			return nil
		}
		// Hand the lease to someone else behind the worker's back, then reject
		// its commit. On the re-read it finds a token that is not its own.
		cur, err := base.GetProcess(ctx, pid)
		if err != nil {
			return err
		}
		cur.LeaseToken = "another-worker"
		if aerr := base.Apply(ctx, agentkit.ChangeSet{Processes: []*agentkit.Process{cur}}); aerr != nil {
			return aerr
		}
		return agentkit.ErrConflict
	})

	serveCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = k.Serve(serveCtx, agentkit.WithPollInterval(2*time.Millisecond), agentkit.WithLease(2*time.Second))
		close(done)
	}()
	time.Sleep(200 * time.Millisecond)
	cancel()
	<-done

	calls, _ := rec.snapshot()
	gt.Value(t, calls).Equal(0)
	p, err := base.GetProcess(ctx, pid)
	gt.NoError(t, err)
	gt.Value(t, p.Status).NotEqual(agentkit.ProcessSucceeded)
}

// Cancel commits from the caller's own goroutine. When it races a claim it is
// converted into CancelRequested and the worker finalizes instead, so exactly
// one of the two paths notifies.
func TestFinishHandlerFiresOnceWhenCancelRacesAClaim(t *testing.T) {
	ctx := context.Background()
	base := memory.New()
	hr := &hookRepo{Repository: base}
	var rec finishRecorder
	k, ag := setupFinishOn(t, hr, rec.handler)
	pid, _ := ag.Spawn(ctx, k, scriptInput{Seed: "s"})

	var once sync.Once
	hr.setApply(func(cs agentkit.ChangeSet) {
		for _, p := range cs.Processes {
			if p.ID == pid && p.Status == agentkit.ProcessCancelled {
				// A short lease so this stand-in worker gets out of the way and a
				// real one can pick the Process up again.
				once.Do(func() {
					_, _ = base.ClaimNextProcess(ctx, "worker-B",
						time.Now().Add(20*time.Millisecond), time.Now())
				})
			}
		}
	})

	// Cancel loses the race and lands as CancelRequested: no terminal commit
	// happened here, so nothing fired.
	gt.NoError(t, k.Cancel(ctx, pid, "abort"))
	calls, _ := rec.snapshot()
	gt.Value(t, calls).Equal(0)

	// Once that lease lapses a worker observes CancelRequested and finalizes.
	hr.setApply(nil)
	p := serveUntil(t, k, hr, pid, 5*time.Second, isTerminal)
	gt.Value(t, p.Status).Equal(agentkit.ProcessCancelled)

	calls, results := rec.snapshot()
	gt.Value(t, calls).Equal(1)
	gt.Value(t, results[0].Status).Equal(agentkit.ProcessCancelled)
	gt.Nil(t, results[0].Output)
}

// The worker's context is already cancelled by the time fireFinish runs, which
// is the case context.WithoutCancel exists for.
func TestFinishHandlerRunsWithAContextCancelledBeforeItWasCalled(t *testing.T) {
	ctx := context.Background()
	base := memory.New()
	hr := &hookRepo{Repository: base}

	var handlerCtxErr atomic.Value
	handlerCtxErr.Store("not-called")
	released := make(chan struct{})
	k, ag := setupFinishOn(t, hr, func(hctx context.Context, _ agentkit.ProcessID, _ agentkit.FinishResult[finishOut]) error {
		if err := hctx.Err(); err != nil {
			handlerCtxErr.Store(err.Error())
		} else {
			handlerCtxErr.Store("")
		}
		close(released)
		return nil
	})
	pid, _ := ag.Spawn(ctx, k, scriptInput{Seed: "s"})

	serveCtx, cancel := context.WithCancel(context.Background())
	// Cancel the worker's context on the way into the terminal Apply, so it is
	// already cancelled when fireFinish is reached.
	var once sync.Once
	hr.setApply(func(cs agentkit.ChangeSet) {
		for _, p := range cs.Processes {
			if p.ID == pid && p.Status == agentkit.ProcessSucceeded {
				once.Do(cancel)
			}
		}
	})

	done := make(chan struct{})
	go func() {
		_ = k.Serve(serveCtx, agentkit.WithPollInterval(2*time.Millisecond), agentkit.WithLease(2*time.Second))
		close(done)
	}()
	select {
	case <-released:
	case <-time.After(3 * time.Second):
		cancel()
		<-done
		t.Fatal("handler was not called")
	}
	<-done

	// It ran, and it ran with cancellation detached.
	gt.Value(t, handlerCtxErr.Load()).Equal("")
	p, err := base.GetProcess(ctx, pid)
	gt.NoError(t, err)
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)
}

// --- unclean reclaims -------------------------------------------------------

// forceUncleanReclaimable rewrites pid's row into the state a worker leaves
// behind when it dies mid-transition: still running, with a lease nobody will
// ever renew. The next claim therefore takes it over as an unclean reclaim.
func forceUncleanReclaimable(t *testing.T, repo agentkit.Repository, pid agentkit.ProcessID) {
	t.Helper()
	p, err := repo.GetProcess(context.Background(), pid)
	gt.NoError(t, err)
	p.Status = agentkit.ProcessRunning
	p.LeaseOwner = "dead-worker"
	p.LeaseToken = "dead-token"
	past := time.Now().Add(-time.Hour)
	p.LeaseUntil = &past
	gt.NoError(t, repo.Apply(context.Background(), agentkit.ChangeSet{
		Processes: []*agentkit.Process{p},
	}))
}

// countingStep wraps a step and records how many times it ran.
func countingStep(calls *atomic.Int32, inner stepFn) stepFn {
	return func(c context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		calls.Add(1)
		return inner(c, sys, st)
	}
}

// With the bound at 0, taking over a dead claim must not re-run Step at all —
// the previous attempt may already have run every effect and died before its
// commit.
func TestUncleanReclaimZeroDoesNotRerunStep(t *testing.T) {
	ctx := context.Background()
	var calls atomic.Int32
	step := countingStep(&calls, func(_ context.Context, _ agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		return st, agentkit.Done([]byte("done")), nil
	})
	model, _ := mockLLM(textResponse("x"))
	k, repo, ag := setupScript(t, step, model)

	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	forceUncleanReclaimable(t, repo, pid)

	p := serveUntil(t, k, repo, pid, 3*time.Second, isTerminal, agentkit.WithMaxUncleanReclaims(0))
	gt.Value(t, p.Status).Equal(agentkit.ProcessFailed)
	gt.Value(t, p.Failure.Code).Equal(agentkit.FailureUncleanReclaim)
	gt.Value(t, calls.Load()).Equal(int32(0))
}

// The default posture is still "a crash resumes". Only the unbounded part goes.
func TestUncleanReclaimWithinBoundStillRuns(t *testing.T) {
	ctx := context.Background()
	var calls atomic.Int32
	step := countingStep(&calls, func(_ context.Context, _ agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		return st, agentkit.Done([]byte("done")), nil
	})
	model, _ := mockLLM(textResponse("x"))
	k, repo, ag := setupScript(t, step, model)

	pid, _ := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	forceUncleanReclaimable(t, repo, pid)

	p := serveUntil(t, k, repo, pid, 3*time.Second, isTerminal, agentkit.WithMaxUncleanReclaims(3))
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)
	gt.Value(t, string(p.Output)).Equal("done")
	gt.Value(t, calls.Load()).Equal(int32(1))
}

// The point of two counters: an error budget and a crash budget are independent.
// With crashes forbidden outright, error retries must still get their three.
func TestUncleanReclaimBoundIsIndependentOfStepAttempts(t *testing.T) {
	ctx := context.Background()
	var calls atomic.Int32
	step := countingStep(&calls, func(_ context.Context, _ agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		return st, agentkit.Decision[[]byte]{}, gollemErr("boom")
	})
	model, _ := mockLLM(textResponse("x"))
	k, repo, ag := setupScript(t, step, model)

	pid, _ := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	p := serveUntil(t, k, repo, pid, 10*time.Second, isTerminal,
		agentkit.WithMaxUncleanReclaims(0), agentkit.WithMaxStepAttempts(3))

	// Terminated by the error budget, not the crash budget.
	gt.Value(t, p.Status).Equal(agentkit.ProcessFailed)
	gt.Value(t, p.Failure.Code).Equal(agentkit.FailureRetryExhausted)
	// StepAttempts+1 > 3 finalizes on the fourth attempt.
	gt.Value(t, calls.Load()).Equal(int32(4))
	// requeue moves the Process to pending, so no claim here was unclean.
	gt.Value(t, p.UncleanReclaims).Equal(0)
}

// A committed transition clears both counters, so a later unclean reclaim
// starts from zero rather than inheriting the earlier one.
func TestUncleanReclaimResetOnCommit(t *testing.T) {
	ctx := context.Background()
	step := func(_ context.Context, _ agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		if st.N == 0 {
			st.N = 1
			return st, agentkit.Continue[[]byte](), nil
		}
		return st, agentkit.Done([]byte("done")), nil
	}
	model, _ := mockLLM(textResponse("x"))
	k, repo, ag := setupScript(t, step, model)

	pid, _ := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	forceUncleanReclaimable(t, repo, pid)

	// maxUncleanReclaims=0 would fire on the first iteration; 1 lets it through
	// so the commit can prove it clears the counter.
	p := serveUntil(t, k, repo, pid, 3*time.Second, isTerminal, agentkit.WithMaxUncleanReclaims(1))
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)
	gt.Value(t, p.UncleanReclaims).Equal(0)
	gt.Value(t, p.StepAttempts).Equal(0)
}

func TestAttemptInfoReportsBothOrigins(t *testing.T) {
	ctx := context.Background()

	t.Run("first attempt is not a replay", func(t *testing.T) {
		var got agentkit.AttemptInfo
		step := func(_ context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
			got = sys.Attempt()
			return st, agentkit.Done([]byte("done")), nil
		}
		model, _ := mockLLM(textResponse("x"))
		k, repo, ag := setupScript(t, step, model)
		pid, _ := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
		serveUntil(t, k, repo, pid, 3*time.Second, isTerminal)

		gt.Value(t, got).Equal(agentkit.AttemptInfo{})
		gt.Bool(t, got.IsReplay()).False()
	})

	t.Run("an error-driven retry reports Errors", func(t *testing.T) {
		var mu sync.Mutex
		var seen []agentkit.AttemptInfo
		step := func(_ context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
			mu.Lock()
			seen = append(seen, sys.Attempt())
			n := len(seen)
			mu.Unlock()
			if n == 1 {
				return st, agentkit.Decision[[]byte]{}, gollemErr("boom")
			}
			return st, agentkit.Done([]byte("done")), nil
		}
		model, _ := mockLLM(textResponse("x"))
		k, repo, ag := setupScript(t, step, model)
		pid, _ := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
		p := serveUntil(t, k, repo, pid, 10*time.Second, isTerminal)
		gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)

		mu.Lock()
		defer mu.Unlock()
		gt.Array(t, seen).Length(2)
		gt.Value(t, seen[0]).Equal(agentkit.AttemptInfo{})
		gt.Value(t, seen[1]).Equal(agentkit.AttemptInfo{Errors: 1})
		gt.Bool(t, seen[1].IsReplay()).True()
	})

	t.Run("a crash-driven reclaim reports UncleanReclaims", func(t *testing.T) {
		var got agentkit.AttemptInfo
		step := func(_ context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
			got = sys.Attempt()
			return st, agentkit.Done([]byte("done")), nil
		}
		model, _ := mockLLM(textResponse("x"))
		k, repo, ag := setupScript(t, step, model)
		pid, _ := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
		forceUncleanReclaimable(t, repo, pid)
		serveUntil(t, k, repo, pid, 3*time.Second, isTerminal, agentkit.WithMaxUncleanReclaims(3))

		gt.Value(t, got).Equal(agentkit.AttemptInfo{UncleanReclaims: 1})
		gt.Bool(t, got.IsReplay()).True()
	})
}

// An orderly exit whose Apply loses a CAS race must not be mistaken for a crash.
// Before the fix, requeue logged the conflict and returned, leaving the row
// running; the next claim then charged an unclean reclaim for an error the
// worker had actually observed.
func TestRequeueConflictIsRetriedNotCountedAsUnclean(t *testing.T) {
	ctx := context.Background()
	base := memory.New()
	hr := &hookRepo{Repository: base}
	reg := agentkit.NewRegistry()

	var seen []agentkit.AttemptInfo
	var mu sync.Mutex
	step := func(_ context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		mu.Lock()
		seen = append(seen, sys.Attempt())
		n := len(seen)
		mu.Unlock()
		if n == 1 {
			return st, agentkit.Decision[[]byte]{}, gollemErr("boom")
		}
		return st, agentkit.Done([]byte("done")), nil
	}
	ag, err := agentkit.Register(reg, "main", 1, &scriptStrategy{step: step})
	gt.NoError(t, err)
	model, _ := mockLLM(textResponse("x"))
	k, err := agentkit.New(hr, model, reg)
	gt.NoError(t, err)

	pid, _ := ag.Spawn(ctx, k, scriptInput{Seed: "s"})

	// Reject the first requeue Apply once, as a concurrent writer bumping Rev
	// between the worker's read and its write would.
	var once sync.Once
	hr.setFailApply(func(cs agentkit.ChangeSet) error {
		var reject bool
		for _, p := range cs.Processes {
			if p.ID == pid && p.Status == agentkit.ProcessPending {
				once.Do(func() { reject = true })
			}
		}
		if reject {
			return agentkit.ErrConflict
		}
		return nil
	})

	p := serveUntil(t, k, hr, pid, 10*time.Second, isTerminal)
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)

	mu.Lock()
	defer mu.Unlock()
	gt.Array(t, seen).Length(2)
	// The retry is charged to the error budget, exactly once...
	gt.Value(t, seen[1].Errors).Equal(1)
	// ...and NOT to the crash budget: the worker exited in an orderly way.
	gt.Value(t, seen[1].UncleanReclaims).Equal(0)
}

// A ToolFactory failure is not the strategy's fault, so it neither consumes a
// step attempt nor makes the next transition look like a replay.
func TestToolFactoryFailureDoesNotConsumeAnAttempt(t *testing.T) {
	ctx := context.Background()
	var factoryCalls atomic.Int32
	var seen []agentkit.AttemptInfo
	var mu sync.Mutex

	step := func(_ context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		mu.Lock()
		seen = append(seen, sys.Attempt())
		mu.Unlock()
		return st, agentkit.Done([]byte("done")), nil
	}
	// Fail the factory once, then let it through.
	tf := func(_ context.Context, _ *agentkit.Process) ([]gollem.Tool, error) {
		if factoryCalls.Add(1) == 1 {
			return nil, gollemErr("factory down")
		}
		return nil, nil
	}
	model, _ := mockLLM(textResponse("x"))
	k, repo, ag := setupScript(t, step, model, agentkit.WithToolFactory(tf))

	pid, _ := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	p := serveUntil(t, k, repo, pid, 10*time.Second, isTerminal)
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)
	gt.Number(t, factoryCalls.Load()).GreaterOrEqual(int32(2))

	mu.Lock()
	defer mu.Unlock()
	// Step ran once, and it was a first attempt: the infrastructure fault
	// happened before Step, so nothing about it says "an effect may have fired".
	gt.Array(t, seen).Length(1)
	gt.Value(t, seen[0]).Equal(agentkit.AttemptInfo{})
	gt.Value(t, seen[0].IsReplay()).Equal(false)
}

// The two counters must be able to be non-zero at the same time and be spent
// independently: a crash-driven reclaim followed by an error-driven retry.
func TestBothAttemptCountersCoexist(t *testing.T) {
	ctx := context.Background()
	var seen []agentkit.AttemptInfo
	var mu sync.Mutex
	step := func(_ context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		mu.Lock()
		seen = append(seen, sys.Attempt())
		n := len(seen)
		mu.Unlock()
		if n == 1 {
			return st, agentkit.Decision[[]byte]{}, gollemErr("boom")
		}
		return st, agentkit.Done([]byte("done")), nil
	}
	model, _ := mockLLM(textResponse("x"))
	k, repo, ag := setupScript(t, step, model)

	pid, _ := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	// Crash first: the row is taken over, counting one unclean reclaim.
	forceUncleanReclaimable(t, repo, pid)

	p := serveUntil(t, k, repo, pid, 10*time.Second, isTerminal,
		agentkit.WithMaxUncleanReclaims(3), agentkit.WithMaxStepAttempts(3))
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)

	mu.Lock()
	defer mu.Unlock()
	gt.Array(t, seen).Length(2)
	// First Step of the reclaimed claim: crash budget spent, error budget clean.
	gt.Value(t, seen[0]).Equal(agentkit.AttemptInfo{UncleanReclaims: 1})
	// That Step errored and was requeued. The retry carries BOTH: the error it
	// just made, and the reclaim it inherited. Neither consumed the other.
	gt.Value(t, seen[1]).Equal(agentkit.AttemptInfo{Errors: 1, UncleanReclaims: 1})
	gt.Value(t, seen[1].IsReplay()).Equal(true)
	// A successful commit clears both.
	gt.Value(t, p.StepAttempts).Equal(0)
	gt.Value(t, p.UncleanReclaims).Equal(0)
}

// setupMetricsTree registers a child that burns one LLM call and a parent that
// spawns nChildren, waits for them, and finishes. repo is wrapped so a test can
// intercept commits.
func setupMetricsTree(t *testing.T, repo agentkit.Repository, nChildren int) (*agentkit.Kernel, agentkit.Agent[scriptInput]) {
	t.Helper()
	reg := agentkit.NewRegistry()
	child, err := agentkit.Register(reg, "child", 1, &scriptStrategy{
		step: func(c context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
			if _, e := sys.Generate(c, []gollem.Input{gollem.Text("hi")}); e != nil {
				return st, agentkit.Decision[[]byte]{}, e
			}
			return st, agentkit.Done([]byte("kid")), nil
		},
	})
	gt.NoError(t, err)

	parentStep := func(c context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		if st.N == 0 {
			ids := make([]agentkit.ProcessID, 0, nChildren)
			for i := 0; i < nChildren; i++ {
				id, e := child.SpawnChild(c, sys, scriptInput{Seed: "kid"})
				if e != nil {
					return st, agentkit.Decision[[]byte]{}, e
				}
				ids = append(ids, id)
			}
			st.N = 1
			return st, agentkit.Suspend[[]byte](agentkit.WaitChildren("kids", ids...)), nil
		}
		return st, agentkit.Done([]byte("done")), nil
	}
	parent, err := agentkit.Register(reg, "parent", 1, &scriptStrategy{step: parentStep})
	gt.NoError(t, err)

	model, _ := mockLLM(textResponse("x"), textResponse("x"), textResponse("x"), textResponse("x"))
	k, err := agentkit.New(repo, model, reg)
	gt.NoError(t, err)
	return k, parent
}

// A Limiter on the parent has to see what the subtree spent, so a child's usage
// is folded into the parent when the children await resolves.
func TestChildMetricsFoldIntoParent(t *testing.T) {
	ctx := context.Background()
	repo := memory.New()
	k, parent := setupMetricsTree(t, repo, 3)

	pid, err := parent.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	p := serveUntil(t, k, repo, pid, 5*time.Second, func(p *agentkit.Process) bool {
		return p.Status == agentkit.ProcessSucceeded
	})

	// Three children, one Generate each (mockLLM reports 5 in / 7 out per call).
	gt.Value(t, p.Metrics.LLMCalls).Equal(int64(3))
	gt.Value(t, p.Metrics.InputTokens).Equal(int64(15))
	gt.Value(t, p.Metrics.OutputTokens).Equal(int64(21))
	// The parent's own counters are still its own: it spawned 3 and never
	// generated, so the LLM calls above came entirely from the fold.
	gt.Value(t, p.Metrics.Spawns).Equal(int64(3))
}

// Siblings finalize concurrently, but only the last one closes the await, so the
// fold must not run once per sibling.
func TestChildMetricsFoldOnlyOnce(t *testing.T) {
	ctx := context.Background()
	repo := memory.New()
	k, parent := setupMetricsTree(t, repo, 3)

	pid, err := parent.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	p := serveUntil(t, k, repo, pid, 5*time.Second, func(p *agentkit.Process) bool {
		return p.Status == agentkit.ProcessSucceeded
	}, agentkit.WithPollConcurrency(4))

	gt.Value(t, p.Metrics.LLMCalls).Equal(int64(3))
	gt.Value(t, p.Metrics.InputTokens).Equal(int64(15))
}

// A transition that does not commit leaves nothing behind, so the fold on the
// retry is still the only one.
func TestChildMetricsFoldSurvivesFailedCommit(t *testing.T) {
	ctx := context.Background()
	base := memory.New()
	repo := &hookRepo{Repository: base}
	k, parent := setupMetricsTree(t, repo, 2)

	// Reject the first Apply that carries the parent's resolved children await.
	var rejected bool
	repo.setFailApply(func(cs agentkit.ChangeSet) error {
		for _, aw := range cs.Awaits {
			if aw.Kind == agentkit.AwaitChildren && aw.Status == agentkit.AwaitResponded && !rejected {
				rejected = true
				return agentkit.ErrConflict
			}
		}
		return nil
	})

	pid, err := parent.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	p := serveUntil(t, k, repo, pid, 5*time.Second, func(p *agentkit.Process) bool {
		return p.Status == agentkit.ProcessSucceeded
	})

	gt.Value(t, rejected).Equal(true)
	gt.Value(t, p.Metrics.LLMCalls).Equal(int64(2))
	gt.Value(t, p.Metrics.InputTokens).Equal(int64(10))
}

// A children await with nothing in it resolves immediately and adds nothing.
func TestChildMetricsFoldWithNoChildren(t *testing.T) {
	ctx := context.Background()
	repo := memory.New()
	reg := agentkit.NewRegistry()
	step := func(_ context.Context, _ agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		if st.N == 0 {
			st.N = 1
			return st, agentkit.Suspend[[]byte](agentkit.WaitChildren("kids")), nil
		}
		return st, agentkit.Done([]byte("done")), nil
	}
	ag, err := agentkit.Register(reg, "solo", 1, &scriptStrategy{step: step})
	gt.NoError(t, err)
	model, _ := mockLLM(textResponse("x"))
	k, err := agentkit.New(repo, model, reg)
	gt.NoError(t, err)

	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	p := serveUntil(t, k, repo, pid, 5*time.Second, func(p *agentkit.Process) bool {
		return p.Status == agentkit.ProcessSucceeded
	})
	gt.Value(t, p.Metrics.LLMCalls).Equal(int64(0))
}

// When every waited child is already terminal at declaration time the await is
// resolved on the spot, without a later wakeParentIfComplete — a second place
// the fold has to happen, and exactly once there too. The timer is what makes
// the child terminal before the await is declared.
func TestChildMetricsFoldOnDeclarationTimeElision(t *testing.T) {
	ctx := context.Background()
	repo := memory.New()
	reg := agentkit.NewRegistry()
	child, err := agentkit.Register(reg, "child", 1, &scriptStrategy{
		step: func(c context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
			if _, e := sys.Generate(c, []gollem.Input{gollem.Text("hi")}); e != nil {
				return st, agentkit.Decision[[]byte]{}, e
			}
			return st, agentkit.Done([]byte("kid")), nil
		},
	})
	gt.NoError(t, err)

	var kidID agentkit.ProcessID
	var kidMu sync.Mutex
	parentStep := func(c context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		switch st.N {
		case 0:
			id, e := child.SpawnChild(c, sys, scriptInput{Seed: "kid"})
			if e != nil {
				return st, agentkit.Decision[[]byte]{}, e
			}
			kidMu.Lock()
			kidID = id
			kidMu.Unlock()
			st.N = 1
			// Park until the child has certainly finished, so the next transition
			// declares the children await over an already-terminal child.
			return st, agentkit.Suspend[[]byte](agentkit.Timer("settle", sys.Now().Add(150*time.Millisecond))), nil
		case 1:
			kidMu.Lock()
			id := kidID
			kidMu.Unlock()
			st.N = 2
			return st, agentkit.Suspend[[]byte](agentkit.WaitChildren("kids", id)), nil
		}
		return st, agentkit.Done([]byte("done")), nil
	}
	parent, err := agentkit.Register(reg, "parent", 1, &scriptStrategy{step: parentStep})
	gt.NoError(t, err)

	model, _ := mockLLM(textResponse("x"), textResponse("x"))
	k, err := agentkit.New(repo, model, reg)
	gt.NoError(t, err)

	pid, err := parent.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	p := serveUntil(t, k, repo, pid, 5*time.Second, func(p *agentkit.Process) bool {
		return p.Status == agentkit.ProcessSucceeded
	})

	kidMu.Lock()
	id := kidID
	kidMu.Unlock()
	kid, err := k.GetProcess(ctx, id)
	gt.NoError(t, err)
	gt.Value(t, kid.Status).Equal(agentkit.ProcessSucceeded)

	gt.Value(t, p.Metrics.LLMCalls).Equal(int64(1))
	gt.Value(t, p.Metrics.InputTokens).Equal(int64(5))
	gt.Value(t, p.Metrics.OutputTokens).Equal(int64(7))
}

// Folding is keyed to the child's own terminal transition, so naming the same
// child from two different await keys counts it once. Keying the fold to the
// await instead made this the case that double-counted.
func TestChildMetricsFoldOncePerChildAcrossAwaitKeys(t *testing.T) {
	ctx := context.Background()
	repo := memory.New()
	reg := agentkit.NewRegistry()
	child, err := agentkit.Register(reg, "child", 1, &scriptStrategy{
		step: func(c context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
			if _, e := sys.Generate(c, []gollem.Input{gollem.Text("hi")}); e != nil {
				return st, agentkit.Decision[[]byte]{}, e
			}
			return st, agentkit.Done([]byte("kid")), nil
		},
	})
	gt.NoError(t, err)

	var kidMu sync.Mutex
	var kidID agentkit.ProcessID
	parentStep := func(c context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		switch st.N {
		case 0:
			id, e := child.SpawnChild(c, sys, scriptInput{Seed: "kid"})
			if e != nil {
				return st, agentkit.Decision[[]byte]{}, e
			}
			kidMu.Lock()
			kidID = id
			kidMu.Unlock()
			st.N = 1
			return st, agentkit.Suspend[[]byte](agentkit.WaitChildren("first", id)), nil
		case 1:
			kidMu.Lock()
			id := kidID
			kidMu.Unlock()
			st.N = 2
			// A second await over the very same child. It resolves by elision, since
			// the child is long terminal.
			return st, agentkit.Suspend[[]byte](agentkit.WaitChildren("second", id)), nil
		}
		return st, agentkit.Done([]byte("done")), nil
	}
	parent, err := agentkit.Register(reg, "parent", 1, &scriptStrategy{step: parentStep})
	gt.NoError(t, err)

	model, _ := mockLLM(textResponse("x"), textResponse("x"))
	k, err := agentkit.New(repo, model, reg)
	gt.NoError(t, err)

	pid, err := parent.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	p := serveUntil(t, k, repo, pid, 5*time.Second, func(p *agentkit.Process) bool {
		return p.Status == agentkit.ProcessSucceeded
	})

	// One child, one Generate. Two awaits naming it must not make it two.
	gt.Value(t, p.Metrics.LLMCalls).Equal(int64(1))
	gt.Value(t, p.Metrics.InputTokens).Equal(int64(5))
	gt.Value(t, p.Metrics.OutputTokens).Equal(int64(7))
}

// Same reasoning for a duplicate inside one spec.
func TestChildMetricsFoldOnceWithDuplicateIDInOneAwait(t *testing.T) {
	ctx := context.Background()
	repo := memory.New()
	reg := agentkit.NewRegistry()
	child, err := agentkit.Register(reg, "child", 1, &scriptStrategy{
		step: func(c context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
			if _, e := sys.Generate(c, []gollem.Input{gollem.Text("hi")}); e != nil {
				return st, agentkit.Decision[[]byte]{}, e
			}
			return st, agentkit.Done([]byte("kid")), nil
		},
	})
	gt.NoError(t, err)

	parentStep := func(c context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		if st.N == 0 {
			id, e := child.SpawnChild(c, sys, scriptInput{Seed: "kid"})
			if e != nil {
				return st, agentkit.Decision[[]byte]{}, e
			}
			st.N = 1
			return st, agentkit.Suspend[[]byte](agentkit.WaitChildren("kids", id, id)), nil
		}
		return st, agentkit.Done([]byte("done")), nil
	}
	parent, err := agentkit.Register(reg, "parent", 1, &scriptStrategy{step: parentStep})
	gt.NoError(t, err)

	model, _ := mockLLM(textResponse("x"), textResponse("x"))
	k, err := agentkit.New(repo, model, reg)
	gt.NoError(t, err)

	pid, err := parent.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	p := serveUntil(t, k, repo, pid, 5*time.Second, func(p *agentkit.Process) bool {
		return p.Status == agentkit.ProcessSucceeded
	})
	gt.Value(t, p.Metrics.LLMCalls).Equal(int64(1))
}

// ADR-0009 permits spawning a child and never waiting on it. Its usage still has
// to reach the parent, or a tree total is not a tree total.
func TestChildMetricsFoldForAnUnwaitedChild(t *testing.T) {
	ctx := context.Background()
	repo := memory.New()
	reg := agentkit.NewRegistry()
	child, err := agentkit.Register(reg, "child", 1, &scriptStrategy{
		step: func(c context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
			if _, e := sys.Generate(c, []gollem.Input{gollem.Text("hi")}); e != nil {
				return st, agentkit.Decision[[]byte]{}, e
			}
			return st, agentkit.Done([]byte("kid")), nil
		},
	})
	gt.NoError(t, err)

	parentStep := func(c context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		switch st.N {
		case 0:
			if _, e := child.SpawnChild(c, sys, scriptInput{Seed: "kid"}); e != nil {
				return st, agentkit.Decision[[]byte]{}, e
			}
			st.N = 1
			// Park long enough for the child to finish and report, without ever
			// declaring a children await over it.
			return st, agentkit.Suspend[[]byte](agentkit.Timer("settle", sys.Now().Add(200*time.Millisecond))), nil
		}
		return st, agentkit.Done([]byte("done")), nil
	}
	parent, err := agentkit.Register(reg, "parent", 1, &scriptStrategy{step: parentStep})
	gt.NoError(t, err)

	model, _ := mockLLM(textResponse("x"), textResponse("x"))
	k, err := agentkit.New(repo, model, reg)
	gt.NoError(t, err)

	pid, err := parent.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	p := serveUntil(t, k, repo, pid, 5*time.Second, func(p *agentkit.Process) bool {
		return p.Status == agentkit.ProcessSucceeded
	})
	gt.Value(t, p.Metrics.LLMCalls).Equal(int64(1))
}

// The Limiter is the reason the fold exists, so assert it actually observes the
// folded total rather than only the row's own spend.
func TestLimiterObservesFoldedChildMetrics(t *testing.T) {
	ctx := context.Background()
	repo := memory.New()
	reg := agentkit.NewRegistry()
	child, err := agentkit.Register(reg, "child", 1, &scriptStrategy{
		step: func(c context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
			if _, e := sys.Generate(c, []gollem.Input{gollem.Text("hi")}); e != nil {
				return st, agentkit.Decision[[]byte]{}, e
			}
			return st, agentkit.Done([]byte("kid")), nil
		},
	})
	gt.NoError(t, err)

	parentStep := func(c context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		if st.N == 0 {
			id1, e := child.SpawnChild(c, sys, scriptInput{Seed: "a"})
			if e != nil {
				return st, agentkit.Decision[[]byte]{}, e
			}
			id2, e2 := child.SpawnChild(c, sys, scriptInput{Seed: "b"})
			if e2 != nil {
				return st, agentkit.Decision[[]byte]{}, e2
			}
			st.N = 1
			return st, agentkit.Suspend[[]byte](agentkit.WaitChildren("kids", id1, id2)), nil
		}
		return st, agentkit.Done([]byte("done")), nil
	}
	var mu sync.Mutex
	maxSeenOnParent := int64(-1)
	limiter := func(_ context.Context, proc *agentkit.Process, m agentkit.Metrics) agentkit.LimitDecision {
		if proc.Agent == "parent" {
			mu.Lock()
			if v := m.LLMCalls; v > maxSeenOnParent {
				maxSeenOnParent = v
			}
			mu.Unlock()
		}
		return agentkit.LimitPass()
	}
	parent, err := agentkit.Register(reg, "parent", 1, &scriptStrategy{step: parentStep, limit: limiter})
	gt.NoError(t, err)

	model, _ := mockLLM(textResponse("x"), textResponse("x"), textResponse("x"))
	k, err := agentkit.New(repo, model, reg)
	gt.NoError(t, err)

	pid, err := parent.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	serveUntil(t, k, repo, pid, 5*time.Second, func(p *agentkit.Process) bool {
		return p.Status == agentkit.ProcessSucceeded
	})

	mu.Lock()
	defer mu.Unlock()
	// The parent never generates; every call it sees belongs to its children.
	gt.Value(t, maxSeenOnParent).Equal(int64(2))
}

// A parent that also declared a timer suspends with that deadline as its
// WakeAt. When the children finish first, the wakeup has to clear it, or the
// parent would sit pending until the timer it never needed.
func TestChildrenWakeupClearsTheTimerWakeTime(t *testing.T) {
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
			id, e := child.SpawnChild(c, sys, scriptInput{Seed: "r1"})
			if e != nil {
				return st, agentkit.Decision[[]byte]{}, e
			}
			st.N = 1
			// The timer is far enough away that it can only fire if the test is
			// wrong about which await woke the parent.
			return st, agentkit.Suspend[[]byte](
				agentkit.WaitChildren("kids", id),
				agentkit.Timer("late", sys.Now().Add(time.Hour)),
			), nil
		}
		aw, ok := sys.Await("kids")
		if !ok || aw.Status != agentkit.AwaitResponded {
			return st, agentkit.Decision[[]byte]{}, gollemErr("children not ready")
		}
		return st, agentkit.Done([]byte("parent done")), nil
	}
	parent, err := agentkit.Register(reg, "parent", 1, &scriptStrategy{step: parentStep})
	gt.NoError(t, err)

	k, err := agentkit.New(repo, model, reg)
	gt.NoError(t, err)

	pid, err := parent.Spawn(ctx, k, scriptInput{Seed: "p"})
	gt.NoError(t, err)
	p := serveUntil(t, k, repo, pid, 5*time.Second, isTerminal)
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)
	gt.Value(t, string(p.Output)).Equal("parent done")
}

// The default curve, and what WithRetryBackoff is allowed to replace it with.
func TestRetryBackoffOption(t *testing.T) {
	t.Run("default doubles and caps at a minute", func(t *testing.T) {
		fn := agentkit.RetryBackoffForTest()
		gt.Value(t, fn(0)).Equal(time.Second)
		gt.Value(t, fn(1)).Equal(2 * time.Second)
		gt.Value(t, fn(3)).Equal(8 * time.Second)
		gt.Value(t, fn(6)).Equal(time.Minute)
		// Past the cap, and past the shift width, it stays at the cap rather than
		// overflowing.
		gt.Value(t, fn(30)).Equal(time.Minute)
		// A stored counter that went negative must not panic on the shift.
		gt.Value(t, fn(-1)).Equal(time.Second)
	})

	t.Run("a caller's curve replaces it", func(t *testing.T) {
		fn := agentkit.RetryBackoffForTest(
			agentkit.WithRetryBackoff(func(n int) time.Duration {
				return time.Duration(n) * time.Millisecond
			}))
		gt.Value(t, fn(5)).Equal(5 * time.Millisecond)
	})

	t.Run("nil restores the default", func(t *testing.T) {
		fn := agentkit.RetryBackoffForTest(agentkit.WithRetryBackoff(nil))
		gt.Value(t, fn(1)).Equal(2 * time.Second)
	})
}

// A negative duration would put the wake time in the past and quietly turn the
// backoff into no backoff at all, so the worker floors it at zero.
func TestRetryBackoffNegativeIsFlooredAtZero(t *testing.T) {
	ctx := context.Background()
	var calls atomic.Int32
	step := countingStep(&calls, func(_ context.Context, _ agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		return st, agentkit.Decision[[]byte]{}, gollemErr("boom")
	})
	model, _ := mockLLM(textResponse("x"))
	k, repo, ag := setupScript(t, step, model)

	pid, _ := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	p := serveUntil(t, k, repo, pid, 10*time.Second, isTerminal,
		agentkit.WithRetryBackoff(func(int) time.Duration { return -time.Hour }),
		agentkit.WithMaxStepAttempts(1))

	gt.Value(t, p.Status).Equal(agentkit.ProcessFailed)
	gt.Value(t, p.Failure.Code).Equal(agentkit.FailureRetryExhausted)
	// The retry still happened -- flooring the wait is not the same as skipping
	// the requeue.
	gt.Value(t, calls.Load()).Equal(int32(2))
}

// --- cancelling at a conversation boundary ---

// openToolCallLLM hands back a History that carries the tool_call content
// itself, so a committed conversation really does end on a call nothing has
// answered — the state the cancel branch has to recognise. toolThenTextLLM grows
// the message count without content, which is enough for the History tests but
// invisible to this check.
func openToolCallLLM(t *testing.T, callID, toolName string) gollem.LLMClient {
	t.Helper()
	call, err := gollem.NewToolCallContent(callID, toolName, map[string]any{})
	gt.NoError(t, err)
	return &mock.LLMClientMock{
		NewSessionFunc: func(_ context.Context, opts ...gollem.SessionOption) (gollem.Session, error) {
			cfg := gollem.NewSessionConfig(opts...)
			var seeded []gollem.Message
			if h := cfg.History(); h != nil {
				seeded = h.Messages
			}
			return &mock.SessionMock{
				GenerateFunc: func(_ context.Context, _ []gollem.Input, _ ...gollem.GenerateOption) (*gollem.Response, error) {
					return &gollem.Response{
						FunctionCalls: []*gollem.FunctionCall{{ID: callID, Name: toolName}},
						InputToken:    1, OutputToken: 1,
					}, nil
				},
				HistoryFunc: func() (*gollem.History, error) {
					grown := append(append([]gollem.Message{}, seeded...),
						gollem.Message{Role: gollem.RoleAssistant, Contents: []gollem.MessageContent{call}})
					return &gollem.History{LLType: gollem.LLMTypeClaude, Version: gollem.HistoryVersion, Messages: grown}, nil
				},
			}, nil
		},
	}
}

// boundaryGate stops the worker once at a transition boundary — the loop-top
// re-read — so a test can land a Cancel with nothing in flight. Cancelling
// mid-transition would conflict with that transition's own Apply and send the
// worker down the rebuild path, which is a different thing to test.
type boundaryGate struct {
	base    agentkit.Repository
	mu      sync.Mutex
	armed   bool
	reached chan struct{}
	release chan struct{}
}

func newBoundaryGate(base agentkit.Repository) *boundaryGate {
	return &boundaryGate{base: base, armed: true, reached: make(chan struct{}), release: make(chan struct{})}
}

// onGet fires on the first read that finds a committed transition, and disarms
// itself BEFORE blocking: Cancel reads the same row through the same repo, so an
// armed gate would hold the very call it is waiting for.
func (g *boundaryGate) onGet(pid agentkit.ProcessID) error {
	g.mu.Lock()
	if !g.armed {
		g.mu.Unlock()
		return nil
	}
	p, err := g.base.GetProcess(context.Background(), pid)
	if err != nil || p.StateSeq < 1 {
		g.mu.Unlock()
		return nil
	}
	g.armed = false
	g.mu.Unlock()
	close(g.reached)
	<-g.release
	return nil
}

// cancelAtBoundary spawns the Process, waits for the gate, cancels there, and
// serves until the Process is terminal.
func cancelAtBoundary(t *testing.T, k *agentkit.Kernel, base agentkit.Repository,
	ag agentkit.Agent[scriptInput], gate *boundaryGate, extra ...agentkit.ServeOption) *agentkit.Process {
	t.Helper()
	ctx := context.Background()
	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	go func() {
		<-gate.reached
		_ = k.Cancel(ctx, pid, "user stopped")
		close(gate.release)
	}()
	return serveUntil(t, k, base, pid, 5*time.Second, isTerminal, extra...)
}

// setupCancelScript wires the gated repo, the open-tool-call model and a tool
// the strategy can answer the call with. store may be nil, for an agent that
// opted into no managed conversation.
func setupCancelScript(t *testing.T, step stepFn, store agentkit.HistoryStore) (*agentkit.Kernel, agentkit.Repository, agentkit.Agent[scriptInput], *boundaryGate) {
	t.Helper()
	base := memory.New()
	gate := newBoundaryGate(base)
	hr := &hookRepo{Repository: base, onGet: gate.onGet}

	reg := agentkit.NewRegistry()
	var regOpts []agentkit.RegisterOption[[]byte]
	if store != nil {
		regOpts = append(regOpts, agentkit.WithHistoryStore[[]byte](store))
	}
	ag, err := agentkit.Register(reg, "main", 1, &scriptStrategy{step: step}, regOpts...)
	gt.NoError(t, err)

	k, err := agentkit.New(hr, openToolCallLLM(t, "c1", "probe"), reg,
		agentkit.WithToolFactory(func(context.Context, *agentkit.Process) ([]gollem.Tool, error) {
			return []gollem.Tool{mockTool("probe", map[string]any{"ok": true})}, nil
		}))
	gt.NoError(t, err)
	return k, base, ag, gate
}

// The transition that answers the call runs AFTER the cancel is observed: the
// worker does not stop on a conversation ending in an unanswered tool call.
func TestCancelWaitsForTheToolRoundToClose(t *testing.T) {
	hs := histmem.New()
	var steps atomic.Int32
	step := func(ctx context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		steps.Add(1)
		switch st.N {
		case 0:
			if _, err := sys.Session().Generate(ctx, []gollem.Input{gollem.Text("go")}); err != nil {
				return st, agentkit.Decision[[]byte]{}, err
			}
			st.N = 1
			return st, agentkit.Continue[[]byte](), nil
		case 1:
			if _, err := sys.Session().CallTool(ctx, gollem.FunctionCall{ID: "c1", Name: "probe"}); err != nil {
				return st, agentkit.Decision[[]byte]{}, err
			}
			st.N = 2
			return st, agentkit.Continue[[]byte](), nil
		}
		return st, agentkit.Done([]byte("done")), nil
	}

	k, base, ag, gate := setupCancelScript(t, step, hs)
	p := cancelAtBoundary(t, k, base, ag, gate)

	gt.Value(t, p.Status).Equal(agentkit.ProcessCancelled)
	gt.Value(t, steps.Load()).Equal(int32(2)) // the answering transition ran.
	gt.Bool(t, agentkit.HasOpenToolCallForTest(committedHistory(t, hs, p))).False()
}

// The wait is only for a round in progress. A strategy that closed its round
// inside the transition is stopped where it stands, with nothing extra run.
func TestCancelStopsAtOnceOnAClosedConversation(t *testing.T) {
	hs := histmem.New()
	var steps atomic.Int32
	step := func(ctx context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		steps.Add(1)
		if st.N == 0 {
			if _, err := sys.Session().Generate(ctx, []gollem.Input{gollem.Text("go")}); err != nil {
				return st, agentkit.Decision[[]byte]{}, err
			}
			if _, err := sys.Session().CallTool(ctx, gollem.FunctionCall{ID: "c1", Name: "probe"}); err != nil {
				return st, agentkit.Decision[[]byte]{}, err
			}
			st.N = 1
			return st, agentkit.Continue[[]byte](), nil
		}
		return st, agentkit.Done([]byte("done")), nil
	}

	k, base, ag, gate := setupCancelScript(t, step, hs)
	p := cancelAtBoundary(t, k, base, ag, gate)

	gt.Value(t, p.Status).Equal(agentkit.ProcessCancelled)
	gt.Value(t, steps.Load()).Equal(int32(1))
	gt.Bool(t, agentkit.HasOpenToolCallForTest(committedHistory(t, hs, p))).False()
}

// The deferred transition is an ordinary one, so it can finish the Process. A
// pending cancel does not override a terminal decision the strategy reached.
func TestCancelDoesNotOverrideATerminalTransition(t *testing.T) {
	hs := histmem.New()
	var steps atomic.Int32
	step := func(ctx context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		steps.Add(1)
		if st.N == 0 {
			if _, err := sys.Session().Generate(ctx, []gollem.Input{gollem.Text("go")}); err != nil {
				return st, agentkit.Decision[[]byte]{}, err
			}
			st.N = 1
			return st, agentkit.Continue[[]byte](), nil
		}
		if _, err := sys.Session().CallTool(ctx, gollem.FunctionCall{ID: "c1", Name: "probe"}); err != nil {
			return st, agentkit.Decision[[]byte]{}, err
		}
		return st, agentkit.Done([]byte("done")), nil
	}

	k, base, ag, gate := setupCancelScript(t, step, hs)
	p := cancelAtBoundary(t, k, base, ag, gate)

	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)
	gt.Value(t, steps.Load()).Equal(int32(2))
	gt.Bool(t, agentkit.HasOpenToolCallForTest(committedHistory(t, hs, p))).False()
}

// A conversation that never closes does not hold a cancelled Process: the bound
// ends it, and the transcript is left as it stands.
func TestCancelBoundEndsAnUnclosedConversation(t *testing.T) {
	hs := histmem.New()
	var steps atomic.Int32
	step := func(ctx context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		steps.Add(1)
		if st.N == 0 {
			if _, err := sys.Session().Generate(ctx, []gollem.Input{gollem.Text("go")}); err != nil {
				return st, agentkit.Decision[[]byte]{}, err
			}
			st.N = 1
		}
		return st, agentkit.Continue[[]byte](), nil // never answers the call.
	}

	k, base, ag, gate := setupCancelScript(t, step, hs)
	p := cancelAtBoundary(t, k, base, ag, gate, agentkit.WithMaxCancelDeferrals(2))

	gt.Value(t, p.Status).Equal(agentkit.ProcessCancelled)
	gt.Value(t, steps.Load()).Equal(int32(3)) // the first transition, then exactly two deferrals.
	gt.Bool(t, agentkit.HasOpenToolCallForTest(committedHistory(t, hs, p))).True()
}

// Zero is the behaviour from before the check existed, kept reachable.
func TestCancelWithoutDeferralsStopsAtOnce(t *testing.T) {
	hs := histmem.New()
	var steps atomic.Int32
	step := func(ctx context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		steps.Add(1)
		if st.N == 0 {
			if _, err := sys.Session().Generate(ctx, []gollem.Input{gollem.Text("go")}); err != nil {
				return st, agentkit.Decision[[]byte]{}, err
			}
			st.N = 1
		}
		return st, agentkit.Continue[[]byte](), nil
	}

	k, base, ag, gate := setupCancelScript(t, step, hs)
	p := cancelAtBoundary(t, k, base, ag, gate, agentkit.WithMaxCancelDeferrals(0))

	gt.Value(t, p.Status).Equal(agentkit.ProcessCancelled)
	gt.Value(t, steps.Load()).Equal(int32(1))
	gt.Bool(t, agentkit.HasOpenToolCallForTest(committedHistory(t, hs, p))).True()
}

// An agent that keeps History in its own state has no managed conversation for
// the kernel to read, so its cancels are unchanged.
func TestCancelIsImmediateWithoutAHistoryStore(t *testing.T) {
	var steps atomic.Int32
	step := func(ctx context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		steps.Add(1)
		if st.N == 0 {
			if _, err := sys.Generate(ctx, []gollem.Input{gollem.Text("go")}); err != nil {
				return st, agentkit.Decision[[]byte]{}, err
			}
			st.N = 1
		}
		return st, agentkit.Continue[[]byte](), nil
	}

	k, base, ag, gate := setupCancelScript(t, step, nil)
	p := cancelAtBoundary(t, k, base, ag, gate)

	gt.Value(t, p.Status).Equal(agentkit.ProcessCancelled)
	gt.Value(t, steps.Load()).Equal(int32(1))
}

// A waiting Process is not at a boundary any worker owns, so Cancel finalizes it
// inline as it always has — including when the conversation is mid-round. The
// deferral covers the stopping points the kernel chooses, not this one.
func TestCancelWhileWaitingStillFinalizesInline(t *testing.T) {
	ctx := context.Background()
	hs := histmem.New()
	step := func(ctx context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		if _, err := sys.Session().Generate(ctx, []gollem.Input{gollem.Text("go")}); err != nil {
			return st, agentkit.Decision[[]byte]{}, err
		}
		return st, agentkit.Suspend[[]byte](agentkit.Timer("later", sys.Now().Add(time.Hour))), nil
	}

	repo := memory.New()
	reg := agentkit.NewRegistry()
	ag, err := agentkit.Register(reg, "main", 1, &scriptStrategy{step: step}, agentkit.WithHistoryStore[[]byte](hs))
	gt.NoError(t, err)
	k, err := agentkit.New(repo, openToolCallLLM(t, "c1", "probe"), reg)
	gt.NoError(t, err)

	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	serveUntil(t, k, repo, pid, 5*time.Second, func(p *agentkit.Process) bool {
		return p.Status == agentkit.ProcessWaiting
	})

	gt.NoError(t, k.Cancel(ctx, pid, "user stopped"))
	p, err := repo.GetProcess(ctx, pid)
	gt.NoError(t, err)
	gt.Value(t, p.Status).Equal(agentkit.ProcessCancelled)
	gt.Bool(t, agentkit.HasOpenToolCallForTest(committedHistory(t, hs, p))).True()
}

// The bound has to stay below the claim: reached only after the claim released,
// it would restart on the next claim and never land.
func TestCancelDeferralBoundIsClampedBelowTheClaim(t *testing.T) {
	cases := map[string]struct {
		opts []agentkit.ServeOption
		want int
	}{
		"default is the rest of the claim": {nil, 15},
		"follows a smaller claim":          {[]agentkit.ServeOption{agentkit.WithMaxStepsPerClaim(4)}, 3},
		"explicit value is kept":           {[]agentkit.ServeOption{agentkit.WithMaxCancelDeferrals(2)}, 2},
		"negative becomes none":            {[]agentkit.ServeOption{agentkit.WithMaxCancelDeferrals(-1)}, 0},
		"clamped under a short claim": {[]agentkit.ServeOption{
			agentkit.WithMaxStepsPerClaim(1), agentkit.WithMaxCancelDeferrals(4),
		}, 0},
		"an invalid claim length falls back to the default": {
			[]agentkit.ServeOption{agentkit.WithMaxStepsPerClaim(0)}, 15,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			gt.Value(t, agentkit.MaxCancelDeferralsForTest(tc.opts...)).Equal(tc.want)
		})
	}
}

// --- settling a claimed row when the worker is asked to stop ---

// ctxRepo refuses every call whose context is already done, the way a store
// reached over a cancelled connection does. The bundled memory Repository
// ignores its context, so without this a write made on a cancelled context
// still lands and the settle path looks correct when it is not.
type ctxRepo struct {
	agentkit.Repository
	refusedApplies atomic.Int64
}

func (r *ctxRepo) GetProcess(ctx context.Context, pid agentkit.ProcessID) (*agentkit.Process, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return r.Repository.GetProcess(ctx, pid)
}

func (r *ctxRepo) Apply(ctx context.Context, cs agentkit.ChangeSet) error {
	if err := ctx.Err(); err != nil {
		r.refusedApplies.Add(1)
		return err
	}
	return r.Repository.Apply(ctx, cs)
}

func (r *ctxRepo) ClaimNextProcess(ctx context.Context, workerID string, leaseUntil, now time.Time) (*agentkit.Process, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return r.Repository.ClaimNextProcess(ctx, workerID, leaseUntil, now)
}

// blockThenDone is a Step that parks the first transition until the claim
// context is cancelled and fails with that context's own error — what a
// transition does when the worker is asked to stop. Every later call finishes.
func blockThenDone(entered chan<- struct{}) stepFn {
	var calls atomic.Int64
	return func(ctx context.Context, _ agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		if calls.Add(1) == 1 {
			entered <- struct{}{}
			<-ctx.Done()
			return st, agentkit.Decision[[]byte]{}, ctx.Err()
		}
		return st, agentkit.Done([]byte("done")), nil
	}
}

// serveThenCancel runs Serve, waits for the strategy to enter Step, cancels the
// Serve context and returns once Serve has returned — the shutdown sequence.
func serveThenCancel(t *testing.T, k *agentkit.Kernel, entered <-chan struct{}, opts ...agentkit.ServeOption) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		_ = k.Serve(ctx, append([]agentkit.ServeOption{
			agentkit.WithPollInterval(2 * time.Millisecond),
		}, opts...)...)
		close(done)
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the strategy never entered Step")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after its context was cancelled")
	}
}

// runClaimCancelled drives one claim and cancels its context once the strategy
// has entered Step.
func runClaimCancelled(t *testing.T, k *agentkit.Kernel, proc *agentkit.Process,
	entered <-chan struct{}, opts ...agentkit.ServeOption) agentkit.ClaimOutcome {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res := make(chan agentkit.ClaimOutcome, 1)
	go func() { res <- k.RunClaimForTest(ctx, proc, opts...) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the strategy never entered Step")
	}
	cancel()
	select {
	case o := <-res:
		return o
	case <-time.After(5 * time.Second):
		t.Fatal("the claim did not settle after its context was cancelled")
		return ""
	}
}

func mustGet(t *testing.T, repo agentkit.Repository, pid agentkit.ProcessID) *agentkit.Process {
	t.Helper()
	p, err := repo.GetProcess(context.Background(), pid)
	gt.NoError(t, err)
	return p
}

func mustClaim(t *testing.T, k *agentkit.Kernel, pid agentkit.ProcessID) *agentkit.Process {
	t.Helper()
	proc, ok := k.ClaimSpecificForTest(context.Background(), pid)
	gt.Bool(t, ok).True()
	return proc
}

func decodeScriptState(t *testing.T, p *agentkit.Process) scriptState {
	t.Helper()
	var st scriptState
	gt.NoError(t, json.Unmarshal(p.State, &st))
	return st
}

// noBackoff keeps a requeued row claimable straight away, so a test observes
// what the settle wrote rather than how long it waits.
func noBackoff() agentkit.ServeOption {
	return agentkit.WithRetryBackoff(func(int) time.Duration { return 0 })
}

// (1) A cancelled Serve context must leave the row claimable, not running under
// a lease nobody renews. The long lease is what makes the difference visible: a
// row left running would be unreachable for 30s, well past the recovery budget.
func TestServeCancelMidTransitionLeavesTheRowClaimable(t *testing.T) {
	ctx := context.Background()
	entered := make(chan struct{}, 4)
	repo := &ctxRepo{Repository: memory.New()}
	reg := agentkit.NewRegistry()
	ag, err := agentkit.Register(reg, "main", 1, &scriptStrategy{step: blockThenDone(entered)})
	gt.NoError(t, err)
	k, err := agentkit.New(repo, growingLLM(), reg)
	gt.NoError(t, err)

	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	serveThenCancel(t, k, entered, agentkit.WithLease(30*time.Second), noBackoff())

	p := mustGet(t, repo, pid)
	gt.Value(t, p.Status).Equal(agentkit.ProcessPending)
	gt.Nil(t, p.LeaseUntil)
	gt.Value(t, p.UncleanReclaims).Equal(0)

	// Recovery is immediate, not lease-expiry driven.
	done := serveUntil(t, k, repo, pid, 3*time.Second, isTerminal, agentkit.WithLease(30*time.Second))
	gt.Value(t, done.Status).Equal(agentkit.ProcessSucceeded)
	gt.Value(t, done.UncleanReclaims).Equal(0)
}

// (2) The regression test for the reported failure: the store rejects every
// write made on a cancelled context, so a settle that inherited the claim's
// cancellation could not land. The settle must not even be attempted there.
func TestSettleWriteDoesNotRideTheCancelledContext(t *testing.T) {
	ctx := context.Background()
	entered := make(chan struct{}, 4)
	repo := &ctxRepo{Repository: memory.New()}
	reg := agentkit.NewRegistry()
	ag, err := agentkit.Register(reg, "main", 1, &scriptStrategy{step: blockThenDone(entered)})
	gt.NoError(t, err)
	k, err := agentkit.New(repo, growingLLM(), reg)
	gt.NoError(t, err)

	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	serveThenCancel(t, k, entered, agentkit.WithLease(30*time.Second), noBackoff())

	p := mustGet(t, repo, pid)
	gt.Value(t, p.Status).Equal(agentkit.ProcessPending)
	gt.Value(t, repo.refusedApplies.Load()).Equal(int64(0))
}

// oneShotGetRepo fails the first GetProcess made while armed.
type oneShotGetRepo struct {
	agentkit.Repository
	armed atomic.Bool
	fired atomic.Bool
	err   error
}

func (r *oneShotGetRepo) GetProcess(ctx context.Context, pid agentkit.ProcessID) (*agentkit.Process, error) {
	if r.armed.Load() && r.fired.CompareAndSwap(false, true) {
		return nil, r.err
	}
	return r.Repository.GetProcess(ctx, pid)
}

// (3) A read that fails at the claim loop head is not evidence the lease was
// lost, and no transition was attempted, so the row goes back with its attempt
// counter untouched.
func TestLoopHeadReadFailureRequeuesWithoutChargingAnAttempt(t *testing.T) {
	ctx := context.Background()
	var steps atomic.Int64
	repo := &oneShotGetRepo{Repository: memory.New(), err: gollemErr("read boom")}
	reg := agentkit.NewRegistry()
	step := func(_ context.Context, _ agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		steps.Add(1)
		return st, agentkit.Done([]byte("done")), nil
	}
	ag, err := agentkit.Register(reg, "main", 1, &scriptStrategy{step: step})
	gt.NoError(t, err)
	k, err := agentkit.New(repo, growingLLM(), reg)
	gt.NoError(t, err)

	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	proc := mustClaim(t, k, pid)
	repo.armed.Store(true)

	gt.Value(t, k.RunClaimForTest(ctx, proc, noBackoff())).Equal(agentkit.ClaimRequeued)
	p := mustGet(t, repo, pid)
	gt.Value(t, p.Status).Equal(agentkit.ProcessPending)
	gt.Value(t, p.StepAttempts).Equal(0)
	gt.Nil(t, p.LeaseUntil)
	gt.Value(t, steps.Load()).Equal(int64(0)) // no transition was attempted.
}

// oneShotApplyRepo fails the first Apply made while armed, without reaching the
// store — a commit that provably did not land.
type oneShotApplyRepo struct {
	agentkit.Repository
	armed atomic.Bool
	fired atomic.Bool
	err   error
}

func (r *oneShotApplyRepo) Apply(ctx context.Context, cs agentkit.ChangeSet) error {
	if r.armed.Load() && r.fired.CompareAndSwap(false, true) {
		return r.err
	}
	return r.Repository.Apply(ctx, cs)
}

// lostAckRepo lets the first Apply made while armed reach the store and then
// reports an error — a commit that landed and lost its response.
type lostAckRepo struct {
	agentkit.Repository
	armed atomic.Bool
	fired atomic.Bool
	err   error
}

func (r *lostAckRepo) Apply(ctx context.Context, cs agentkit.ChangeSet) error {
	if r.armed.Load() && r.fired.CompareAndSwap(false, true) {
		if err := r.Repository.Apply(ctx, cs); err != nil {
			return err
		}
		return r.err
	}
	return r.Repository.Apply(ctx, cs)
}

// advanceThenDone advances the state once and then finishes, recording how many
// times Step ran so a test can tell a re-run from a doubled commit.
func advanceThenDone(calls *atomic.Int64) stepFn {
	return func(_ context.Context, _ agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		calls.Add(1)
		if st.N == 0 {
			st.N = 1
			return st, agentkit.Continue[[]byte](), nil
		}
		return st, agentkit.Done([]byte("done")), nil
	}
}

// (4) A non-conflict Apply error with nothing committed puts the row back, and
// the retry applies the transition once rather than twice.
func TestTransitionApplyFailureWithNothingCommittedRequeues(t *testing.T) {
	ctx := context.Background()
	var steps atomic.Int64
	repo := &oneShotApplyRepo{Repository: memory.New(), err: gollemErr("disk gone at commit")}
	reg := agentkit.NewRegistry()
	ag, err := agentkit.Register(reg, "main", 1, &scriptStrategy{step: advanceThenDone(&steps)})
	gt.NoError(t, err)
	k, err := agentkit.New(repo, growingLLM(), reg)
	gt.NoError(t, err)

	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	proc := mustClaim(t, k, pid)
	repo.armed.Store(true)

	gt.Value(t, k.RunClaimForTest(ctx, proc, agentkit.WithMaxStepsPerClaim(1), noBackoff())).
		Equal(agentkit.ClaimRequeued)
	p := mustGet(t, repo, pid)
	gt.Value(t, p.Status).Equal(agentkit.ProcessPending)
	gt.Value(t, p.StepAttempts).Equal(1)
	gt.Nil(t, p.LeaseUntil)
	gt.Value(t, p.StateSeq).Equal(0) // nothing committed.

	// The retry re-runs Step and commits it once.
	proc = mustClaim(t, k, pid)
	gt.Value(t, k.RunClaimForTest(ctx, proc, agentkit.WithMaxStepsPerClaim(1), noBackoff())).
		Equal(agentkit.ClaimReleased)
	p = mustGet(t, repo, pid)
	gt.Value(t, steps.Load()).Equal(int64(2)) // Step ran twice...
	gt.Value(t, p.StateSeq).Equal(1)          // ...and committed once.
	gt.Value(t, decodeScriptState(t, p).N).Equal(1)
	gt.Value(t, p.StepAttempts).Equal(0)
}

// (5) The same error after the commit landed. The committed values must survive
// the settle, and the run must not be charged for a transition that succeeded.
func TestTransitionApplyFailureAfterTheCommitLandedKeepsTheCommit(t *testing.T) {
	ctx := context.Background()
	var steps atomic.Int64
	repo := &lostAckRepo{Repository: memory.New(), err: gollemErr("response lost at commit")}
	reg := agentkit.NewRegistry()
	ag, err := agentkit.Register(reg, "main", 1, &scriptStrategy{step: advanceThenDone(&steps)})
	gt.NoError(t, err)
	k, err := agentkit.New(repo, growingLLM(), reg)
	gt.NoError(t, err)

	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	proc := mustClaim(t, k, pid)
	repo.armed.Store(true)

	gt.Value(t, k.RunClaimForTest(ctx, proc, agentkit.WithMaxStepsPerClaim(1), noBackoff())).
		Equal(agentkit.ClaimRequeued)
	p := mustGet(t, repo, pid)
	// The row is claimable again and the commit the store accepted is intact.
	gt.Value(t, p.Status).Equal(agentkit.ProcessPending)
	gt.Nil(t, p.LeaseUntil)
	gt.Value(t, p.StateSeq).Equal(1)
	gt.Value(t, decodeScriptState(t, p).N).Equal(1)
	// A commit that landed is not the strategy's failure.
	gt.Value(t, p.StepAttempts).Equal(0)
	gt.Value(t, steps.Load()).Equal(int64(1))
}

// (6) A terminal commit that landed and lost only its response is reported as
// what it is. The row must not be rewritten.
func TestTerminalCommitFailureAfterTheCommitLandedReportsFinished(t *testing.T) {
	ctx := context.Background()
	repo := &lostAckRepo{Repository: memory.New(), err: gollemErr("response lost at terminal commit")}
	reg := agentkit.NewRegistry()
	step := func(_ context.Context, _ agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		return st, agentkit.Done([]byte("done")), nil
	}
	ag, err := agentkit.Register(reg, "main", 1, &scriptStrategy{step: step})
	gt.NoError(t, err)
	k, err := agentkit.New(repo, growingLLM(), reg)
	gt.NoError(t, err)

	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	proc := mustClaim(t, k, pid)
	repo.armed.Store(true)

	gt.Value(t, k.RunClaimForTest(ctx, proc, noBackoff())).Equal(agentkit.ClaimFinished)
	p := mustGet(t, repo, pid)
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)
	gt.Value(t, string(p.Output)).Equal("done")
	gt.Value(t, p.Rev).Equal(proc.Rev + 1) // the terminal Apply, and nothing after it.
}

// (7) A terminal commit that did not land leaves a row that must not stay
// running, and the retry reaches the same terminal state exactly once.
func TestTerminalCommitFailureWithNothingCommittedRequeues(t *testing.T) {
	ctx := context.Background()
	repo := &oneShotApplyRepo{Repository: memory.New(), err: gollemErr("disk gone at terminal commit")}
	reg := agentkit.NewRegistry()
	step := func(_ context.Context, _ agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		return st, agentkit.Done([]byte("done")), nil
	}
	ag, err := agentkit.Register(reg, "main", 1, &scriptStrategy{step: step})
	gt.NoError(t, err)
	k, err := agentkit.New(repo, growingLLM(), reg)
	gt.NoError(t, err)

	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	proc := mustClaim(t, k, pid)
	repo.armed.Store(true)

	gt.Value(t, k.RunClaimForTest(ctx, proc, noBackoff())).Equal(agentkit.ClaimRequeued)
	p := mustGet(t, repo, pid)
	gt.Value(t, p.Status).Equal(agentkit.ProcessPending)
	gt.Nil(t, p.LeaseUntil)

	proc = mustClaim(t, k, pid)
	gt.Value(t, k.RunClaimForTest(ctx, proc, noBackoff())).Equal(agentkit.ClaimFinished)
	p = mustGet(t, repo, pid)
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)

	evs, err := repo.ListEvents(ctx, pid, agentkit.EventQuery{})
	gt.NoError(t, err)
	finished := 0
	for _, e := range evs {
		if e.Type == agentkit.EventProcessFinished {
			finished++
		}
	}
	gt.Value(t, finished).Equal(1)
}

// (8) A transition the worker interrupted produced no decision, so the next run
// is a first attempt and must not present itself as a replay.
func TestCancelledTransitionIsNotReportedAsAReplay(t *testing.T) {
	ctx := context.Background()
	entered := make(chan struct{}, 4)
	var mu sync.Mutex
	var replays []bool
	var calls atomic.Int64
	repo := &ctxRepo{Repository: memory.New()}
	reg := agentkit.NewRegistry()
	step := func(c context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		mu.Lock()
		replays = append(replays, sys.Attempt().IsReplay())
		mu.Unlock()
		if calls.Add(1) == 1 {
			entered <- struct{}{}
			<-c.Done()
			return st, agentkit.Decision[[]byte]{}, c.Err()
		}
		return st, agentkit.Done([]byte("done")), nil
	}
	ag, err := agentkit.Register(reg, "main", 1, &scriptStrategy{step: step})
	gt.NoError(t, err)
	k, err := agentkit.New(repo, growingLLM(), reg)
	gt.NoError(t, err)

	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	serveThenCancel(t, k, entered, noBackoff())

	gt.Value(t, mustGet(t, repo, pid).StepAttempts).Equal(0)
	serveUntil(t, k, repo, pid, 3*time.Second, isTerminal)

	mu.Lock()
	defer mu.Unlock()
	gt.Array(t, replays).Length(2)
	gt.Bool(t, replays[1]).False() // the run after the cancellation.
}

// (9) A Process one attempt short of its budget must survive repeated shutdowns:
// a cancellation is not the strategy's failure and must not exhaust the budget.
func TestCancellationsDoNotExhaustTheRetryBudget(t *testing.T) {
	ctx := context.Background()
	entered := make(chan struct{}, 8)
	mode := make(chan string, 8)
	repo := &ctxRepo{Repository: memory.New()}
	reg := agentkit.NewRegistry()
	step := func(c context.Context, _ agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		switch <-mode {
		case "error":
			return st, agentkit.Decision[[]byte]{}, gollemErr("a real transition failure")
		case "cancel":
			entered <- struct{}{}
			<-c.Done()
			return st, agentkit.Decision[[]byte]{}, c.Err()
		default:
			return st, agentkit.Done([]byte("done")), nil
		}
	}
	ag, err := agentkit.Register(reg, "main", 1, &scriptStrategy{step: step})
	gt.NoError(t, err)
	k, err := agentkit.New(repo, growingLLM(), reg)
	gt.NoError(t, err)

	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)

	// One real failure puts the Process at the last attempt its budget allows.
	mode <- "error"
	gt.Value(t, k.RunClaimForTest(ctx, mustClaim(t, k, pid),
		agentkit.WithMaxStepAttempts(1), noBackoff())).Equal(agentkit.ClaimRequeued)
	gt.Value(t, mustGet(t, repo, pid).StepAttempts).Equal(1)

	for i := 0; i < 3; i++ {
		mode <- "cancel"
		gt.Value(t, runClaimCancelled(t, k, mustClaim(t, k, pid), entered,
			agentkit.WithMaxStepAttempts(1), noBackoff())).Equal(agentkit.ClaimRequeued)
		p := mustGet(t, repo, pid)
		gt.Value(t, p.Status).Equal(agentkit.ProcessPending)
		gt.Value(t, p.StepAttempts).Equal(1)
	}

	mode <- "done"
	gt.Value(t, k.RunClaimForTest(ctx, mustClaim(t, k, pid),
		agentkit.WithMaxStepAttempts(1), noBackoff())).Equal(agentkit.ClaimFinished)
	gt.Value(t, mustGet(t, repo, pid).Status).Equal(agentkit.ProcessSucceeded)
}

// (10) The exemption is for the claim context only. A deadline the strategy set
// on its own work is a real transition failure and keeps costing an attempt.
func TestStrategyOwnDeadlineStillConsumesAnAttempt(t *testing.T) {
	ctx := context.Background()
	repo := &ctxRepo{Repository: memory.New()}
	reg := agentkit.NewRegistry()
	step := func(c context.Context, _ agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		own, cancel := context.WithTimeout(c, time.Millisecond)
		defer cancel()
		<-own.Done()
		return st, agentkit.Decision[[]byte]{}, own.Err()
	}
	ag, err := agentkit.Register(reg, "main", 1, &scriptStrategy{step: step})
	gt.NoError(t, err)
	k, err := agentkit.New(repo, growingLLM(), reg)
	gt.NoError(t, err)

	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	gt.Value(t, k.RunClaimForTest(ctx, mustClaim(t, k, pid), noBackoff())).Equal(agentkit.ClaimRequeued)

	p := mustGet(t, repo, pid)
	gt.Value(t, p.Status).Equal(agentkit.ProcessPending)
	gt.Value(t, p.StepAttempts).Equal(1)
}

// (11) The invariant's one exception: a row naming another lease token is the
// only proof this claim no longer owns it, and it is walked away from untouched.
func TestAnotherLeaseTokenIsAbandonedWithoutAWrite(t *testing.T) {
	ctx := context.Background()
	var steps atomic.Int64
	repo := memory.New()
	reg := agentkit.NewRegistry()
	step := func(_ context.Context, _ agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		steps.Add(1)
		return st, agentkit.Done([]byte("done")), nil
	}
	ag, err := agentkit.Register(reg, "main", 1, &scriptStrategy{step: step})
	gt.NoError(t, err)
	k, err := agentkit.New(repo, growingLLM(), reg)
	gt.NoError(t, err)

	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	proc := mustClaim(t, k, pid)

	// Another worker takes the lease.
	stolen := agentkit.CloneProcess(mustGet(t, repo, pid))
	stolen.LeaseToken = "another-worker"
	gt.NoError(t, repo.Apply(ctx, agentkit.ChangeSet{Processes: []*agentkit.Process{stolen}}))
	before := mustGet(t, repo, pid)

	gt.Value(t, k.RunClaimForTest(ctx, proc, noBackoff())).Equal(agentkit.ClaimAbandoned)
	after := mustGet(t, repo, pid)
	gt.Value(t, after.Rev).Equal(before.Rev)
	gt.Value(t, after.LeaseToken).Equal("another-worker")
	gt.Value(t, after.Status).Equal(agentkit.ProcessRunning)
	gt.Value(t, steps.Load()).Equal(int64(0))
}

// blockingApplyRepo parks every Apply made while armed until its own context is
// done — a store that stopped answering. It records whether any of those calls
// arrived on a context that was already done, which is what would happen if a
// settle rode the claim's cancellation instead of its own context.
type blockingApplyRepo struct {
	agentkit.Repository
	armed      atomic.Bool
	sawDoneCtx atomic.Bool
}

func (r *blockingApplyRepo) Apply(ctx context.Context, cs agentkit.ChangeSet) error {
	if r.armed.Load() {
		if ctx.Err() != nil {
			r.sawDoneCtx.Store(true)
			return ctx.Err()
		}
		<-ctx.Done()
		return ctx.Err()
	}
	return r.Repository.Apply(ctx, cs)
}

// (12) The settle runs on a context the caller cannot cancel, so the timeout is
// the only thing that can stop it. A store that never answers must not hold the
// worker open past it.
func TestSettleTimeoutBoundsAStoreThatNeverAnswers(t *testing.T) {
	ctx := context.Background()
	entered := make(chan struct{}, 4)
	repo := &blockingApplyRepo{Repository: memory.New()}
	reg := agentkit.NewRegistry()
	ag, err := agentkit.Register(reg, "main", 1, &scriptStrategy{step: blockThenDone(entered)})
	gt.NoError(t, err)
	k, err := agentkit.New(repo, growingLLM(), reg)
	gt.NoError(t, err)

	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	proc := mustClaim(t, k, pid)
	repo.armed.Store(true)

	const settle = 80 * time.Millisecond
	start := time.Now()
	outcome := runClaimCancelled(t, k, proc, entered, agentkit.WithSettleTimeout(settle), noBackoff())
	elapsed := time.Since(start)

	gt.Value(t, outcome).Equal(agentkit.ClaimAbandoned)
	// The settle reached the store on a live context and was stopped by its own
	// timeout, not by the cancellation that triggered it.
	gt.Bool(t, repo.sawDoneCtx.Load()).False()
	gt.Bool(t, elapsed >= settle).True()
	gt.Bool(t, elapsed < 2*time.Second).True()
}

// deadlineRepo records the deadline every store call arrives with, for calls
// that have one — which is exactly the settle's.
type deadlineRepo struct {
	agentkit.Repository
	armed     atomic.Bool
	failApply atomic.Bool
	err       error
	mu        sync.Mutex
	deadlines []time.Time
}

func (r *deadlineRepo) record(ctx context.Context) {
	if d, ok := ctx.Deadline(); ok && r.armed.Load() {
		r.mu.Lock()
		r.deadlines = append(r.deadlines, d)
		r.mu.Unlock()
	}
}

func (r *deadlineRepo) seen() []time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Time(nil), r.deadlines...)
}

func (r *deadlineRepo) GetProcess(ctx context.Context, pid agentkit.ProcessID) (*agentkit.Process, error) {
	r.record(ctx)
	return r.Repository.GetProcess(ctx, pid)
}

func (r *deadlineRepo) Apply(ctx context.Context, cs agentkit.ChangeSet) error {
	if r.failApply.CompareAndSwap(true, false) {
		return r.err
	}
	r.record(ctx)
	return r.Repository.Apply(ctx, cs)
}

// WithSettleTimeout bounds one settle, not each store call inside it. A settle
// that reads the row and then writes it must run both under the same deadline,
// or the documented bound is twice what it says.
func TestSettleTimeoutBoundsTheWholeSettleOnce(t *testing.T) {
	ctx := context.Background()
	var steps atomic.Int64
	repo := &deadlineRepo{Repository: memory.New(), err: gollemErr("disk gone at commit")}
	reg := agentkit.NewRegistry()
	ag, err := agentkit.Register(reg, "main", 1, &scriptStrategy{step: advanceThenDone(&steps)})
	gt.NoError(t, err)
	k, err := agentkit.New(repo, growingLLM(), reg)
	gt.NoError(t, err)

	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	proc := mustClaim(t, k, pid)
	repo.armed.Store(true)
	repo.failApply.Store(true)

	gt.Value(t, k.RunClaimForTest(ctx, proc, agentkit.WithMaxStepsPerClaim(1), noBackoff())).
		Equal(agentkit.ClaimRequeued)

	// The settle's re-read and its requeue write, and nothing else.
	seen := repo.seen()
	gt.Array(t, seen).Length(2)
	gt.Value(t, seen[1]).Equal(seen[0])
}

// A concurrent Cancel advances the Rev of a running row without committing
// anything, so the settle must not read a Rev bump as its own commit landing.
// Getting this wrong loses the run's metrics and its retry attempt.
func TestConcurrentCancelIsNotMistakenForACommittedTransition(t *testing.T) {
	ctx := context.Background()
	var steps atomic.Int64
	repo := &hookedApplyRepo{Repository: memory.New(), err: gollemErr("disk gone at commit")}
	reg := agentkit.NewRegistry()
	ag, err := agentkit.Register(reg, "main", 1, &scriptStrategy{step: advanceThenDone(&steps)})
	gt.NoError(t, err)
	k, err := agentkit.New(repo, growingLLM(), reg)
	gt.NoError(t, err)

	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	proc := mustClaim(t, k, pid)
	// The commit fails without writing, and a Cancel lands before the settle
	// re-reads: the row's Rev has moved on, but this transition did not commit.
	repo.onApply = func(agentkit.Repository) { gt.NoError(t, k.Cancel(ctx, pid, "user stopped")) }
	repo.armed.Store(true)

	gt.Value(t, k.RunClaimForTest(ctx, proc, agentkit.WithMaxStepsPerClaim(1), noBackoff())).
		Equal(agentkit.ClaimRequeued)
	p := mustGet(t, repo, pid)
	gt.Value(t, p.Status).Equal(agentkit.ProcessPending)
	gt.Value(t, p.StateSeq).Equal(0)     // nothing committed...
	gt.Value(t, p.StepAttempts).Equal(1) // ...so the transition is owed a retry.
	gt.Bool(t, p.CancelRequested).True() // and the other writer's field survived.
}

// A suspend commit that landed and lost only its response leaves a waiting row.
// Reporting that as abandoned would tell a ClaimMiddleware the claim failed.
func TestSuspendCommitFailureAfterTheCommitLandedReportsSuspended(t *testing.T) {
	ctx := context.Background()
	repo := &lostAckRepo{Repository: memory.New(), err: gollemErr("response lost at commit")}
	reg := agentkit.NewRegistry()
	step := func(_ context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		return st, agentkit.Suspend[[]byte](agentkit.Timer("t:1", sys.Now().Add(time.Hour))), nil
	}
	ag, err := agentkit.Register(reg, "main", 1, &scriptStrategy{step: step})
	gt.NoError(t, err)
	k, err := agentkit.New(repo, growingLLM(), reg)
	gt.NoError(t, err)

	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	proc := mustClaim(t, k, pid)
	repo.armed.Store(true)

	gt.Value(t, k.RunClaimForTest(ctx, proc, noBackoff())).Equal(agentkit.ClaimSuspended)
	p := mustGet(t, repo, pid)
	gt.Value(t, p.Status).Equal(agentkit.ProcessWaiting)
	gt.Nil(t, p.LeaseUntil)
}

// A commit conflict is retried from a fresh read, but that read can fail because
// the worker was asked to stop between the two. The row still has to go back.
func TestCommitConflictThenCancellationStillSettles(t *testing.T) {
	inner := &ctxRepo{Repository: memory.New()}
	repo := &hookedApplyRepo{Repository: inner, err: agentkit.ErrConflict}
	var steps atomic.Int64
	reg := agentkit.NewRegistry()
	ag, err := agentkit.Register(reg, "main", 1, &scriptStrategy{step: advanceThenDone(&steps)})
	gt.NoError(t, err)
	k, err := agentkit.New(repo, growingLLM(), reg)
	gt.NoError(t, err)

	pid, err := ag.Spawn(context.Background(), k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	proc := mustClaim(t, k, pid)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	repo.onApply = func(agentkit.Repository) { cancel() }
	repo.armed.Store(true)

	gt.Value(t, k.RunClaimForTest(ctx, proc, agentkit.WithMaxStepsPerClaim(1), noBackoff())).
		Equal(agentkit.ClaimRequeued)
	p := mustGet(t, repo, pid)
	gt.Value(t, p.Status).Equal(agentkit.ProcessPending)
	gt.Nil(t, p.LeaseUntil)
	gt.Value(t, p.StateSeq).Equal(0)
	gt.Value(t, p.StepAttempts).Equal(0)
	gt.Value(t, inner.refusedApplies.Load()).Equal(int64(0))
}

// The same window on the terminal commit. A re-read that fails there says
// nothing about whether the Process is finished, so it must not be reported as
// finished — and the row must not be left running.
func TestTerminalCommitConflictThenCancellationStillSettles(t *testing.T) {
	inner := &ctxRepo{Repository: memory.New()}
	repo := &hookedApplyRepo{Repository: inner, err: agentkit.ErrConflict}
	reg := agentkit.NewRegistry()
	step := func(_ context.Context, _ agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		return st, agentkit.Done([]byte("done")), nil
	}
	ag, err := agentkit.Register(reg, "main", 1, &scriptStrategy{step: step})
	gt.NoError(t, err)
	k, err := agentkit.New(repo, growingLLM(), reg)
	gt.NoError(t, err)

	pid, err := ag.Spawn(context.Background(), k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	proc := mustClaim(t, k, pid)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	repo.onApply = func(agentkit.Repository) { cancel() }
	repo.armed.Store(true)

	gt.Value(t, k.RunClaimForTest(ctx, proc, noBackoff())).Equal(agentkit.ClaimRequeued)
	p := mustGet(t, repo, pid)
	gt.Value(t, p.Status).Equal(agentkit.ProcessPending)
	gt.Nil(t, p.LeaseUntil)
	gt.Value(t, p.StepAttempts).Equal(0) // a shutdown is not the strategy's failure.
}

// context.Canceled and context.DeadlineExceeded carry no origin, so a strategy
// deadline that expires while the claim context is also done reads as a
// shutdown. That is the intended reading — the worker is going away either way —
// and this pins it so a later change has to be deliberate.
func TestStrategyDeadlineUnderAClosingClaimIsTreatedAsShutdown(t *testing.T) {
	repo := &ctxRepo{Repository: memory.New()}
	reg := agentkit.NewRegistry()
	step := func(c context.Context, _ agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		own, cancel := context.WithTimeout(c, time.Millisecond)
		defer cancel()
		<-own.Done()
		<-c.Done() // the claim context closes too before the error surfaces.
		return st, agentkit.Decision[[]byte]{}, own.Err()
	}
	ag, err := agentkit.Register(reg, "main", 1, &scriptStrategy{step: step})
	gt.NoError(t, err)
	k, err := agentkit.New(repo, growingLLM(), reg)
	gt.NoError(t, err)

	pid, err := ag.Spawn(context.Background(), k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	proc := mustClaim(t, k, pid)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	gt.Value(t, k.RunClaimForTest(ctx, proc, noBackoff())).Equal(agentkit.ClaimRequeued)
	p := mustGet(t, repo, pid)
	gt.Value(t, p.Status).Equal(agentkit.ProcessPending)
	gt.Value(t, p.StepAttempts).Equal(0)
}

// hookedApplyRepo runs onApply instead of the first Apply made while armed and
// then reports err. The hook is how a test stages what another worker did in the
// window between a failed commit and the settle's re-read.
type hookedApplyRepo struct {
	agentkit.Repository
	armed   atomic.Bool
	fired   atomic.Bool
	onApply func(inner agentkit.Repository)
	err     error
}

func (r *hookedApplyRepo) Apply(ctx context.Context, cs agentkit.ChangeSet) error {
	if r.armed.Load() && r.fired.CompareAndSwap(false, true) {
		r.onApply(r.Repository)
		return r.err
	}
	return r.Repository.Apply(ctx, cs)
}

// A terminal row is not by itself proof that this claim finished it: another
// owner that took the lease over and finalized the Process leaves one too. The
// lease decides, so this claim is not credited with someone else's work.
func TestTerminalCommitFailureDoesNotClaimAnotherOwnersFinish(t *testing.T) {
	ctx := context.Background()
	repo := &hookedApplyRepo{Repository: memory.New(), err: gollemErr("disk gone at terminal commit")}
	reg := agentkit.NewRegistry()
	step := func(_ context.Context, _ agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		return st, agentkit.Done([]byte("ours")), nil
	}
	ag, err := agentkit.Register(reg, "main", 1, &scriptStrategy{step: step})
	gt.NoError(t, err)
	k, err := agentkit.New(repo, growingLLM(), reg)
	gt.NoError(t, err)

	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	proc := mustClaim(t, k, pid)
	// While this claim's terminal commit is failing, another owner takes the lease
	// and finishes the Process.
	repo.onApply = func(inner agentkit.Repository) {
		stolen := agentkit.CloneProcess(mustGet(t, inner, pid))
		stolen.LeaseToken = "another-worker"
		stolen.LeaseUntil = nil
		stolen.Status = agentkit.ProcessSucceeded
		stolen.Output = []byte("theirs")
		gt.NoError(t, inner.Apply(ctx, agentkit.ChangeSet{Processes: []*agentkit.Process{stolen}}))
	}
	repo.armed.Store(true)

	gt.Value(t, k.RunClaimForTest(ctx, proc, noBackoff())).Equal(agentkit.ClaimAbandoned)
	gt.Value(t, string(mustGet(t, repo, pid).Output)).Equal("theirs")
}

// conflictThenUnreadableRepo makes the first Apply made while armed conflict and
// the re-read that follows it fail — a store that goes away in the window
// between a commit conflict and the rebuild.
type conflictThenUnreadableRepo struct {
	agentkit.Repository
	armed    atomic.Bool
	fired    atomic.Bool
	readFail atomic.Bool
}

func (r *conflictThenUnreadableRepo) Apply(ctx context.Context, cs agentkit.ChangeSet) error {
	if r.armed.Load() && r.fired.CompareAndSwap(false, true) {
		r.readFail.Store(true)
		return agentkit.ErrConflict
	}
	return r.Repository.Apply(ctx, cs)
}

func (r *conflictThenUnreadableRepo) GetProcess(ctx context.Context, pid agentkit.ProcessID) (*agentkit.Process, error) {
	if r.readFail.CompareAndSwap(true, false) {
		return nil, gollemErr("read boom")
	}
	return r.Repository.GetProcess(ctx, pid)
}

// A conflict proves nothing committed, but the re-read that would rebuild the
// commit can fail on its own. That failure says nothing about the lease, so the
// row still has to go back rather than be left running.
func TestCommitConflictWithAnUnreadableRowStillRequeues(t *testing.T) {
	ctx := context.Background()
	var steps atomic.Int64
	repo := &conflictThenUnreadableRepo{Repository: memory.New()}
	reg := agentkit.NewRegistry()
	ag, err := agentkit.Register(reg, "main", 1, &scriptStrategy{step: advanceThenDone(&steps)})
	gt.NoError(t, err)
	k, err := agentkit.New(repo, growingLLM(), reg)
	gt.NoError(t, err)

	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	proc := mustClaim(t, k, pid)
	repo.armed.Store(true)

	gt.Value(t, k.RunClaimForTest(ctx, proc, agentkit.WithMaxStepsPerClaim(1), noBackoff())).
		Equal(agentkit.ClaimRequeued)
	p := mustGet(t, repo, pid)
	gt.Value(t, p.Status).Equal(agentkit.ProcessPending)
	gt.Nil(t, p.LeaseUntil)
	gt.Value(t, p.StateSeq).Equal(0) // the conflict proved nothing committed.
	gt.Value(t, p.StepAttempts).Equal(0)
}

// A non-positive settle timeout would make every settle fail before it reached
// the store, which is the failure the bound exists to prevent.
func TestSettleTimeoutRejectsNonPositiveValues(t *testing.T) {
	cases := map[string]struct {
		opts []agentkit.ServeOption
		want time.Duration
	}{
		"default":              {nil, 5 * time.Second},
		"zero is the default":  {[]agentkit.ServeOption{agentkit.WithSettleTimeout(0)}, 5 * time.Second},
		"negative is likewise": {[]agentkit.ServeOption{agentkit.WithSettleTimeout(-time.Second)}, 5 * time.Second},
		"explicit value kept":  {[]agentkit.ServeOption{agentkit.WithSettleTimeout(time.Second)}, time.Second},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			gt.Value(t, agentkit.SettleTimeoutForTest(tc.opts...)).Equal(tc.want)
		})
	}
}
