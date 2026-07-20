package agentkit_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/gollem-dev/agentkit"
	"github.com/gollem-dev/agentkit/repository/memory"
	"github.com/gollem-dev/gollem"
	"github.com/gollem-dev/gollem/mock"
	"github.com/m-mizutani/gt"
)

// --- helpers ----------------------------------------------------------------

// recorder collects ordered strings from middleware running on worker
// goroutines.
type recorder struct {
	mu   sync.Mutex
	seen []string
}

func (r *recorder) add(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, s)
}

func (r *recorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.seen...)
}

// countingTool is mockTool plus a call counter and an argument capture.
func countingTool(name string) (gollem.Tool, *int32Box, *mapBox) {
	calls := &int32Box{}
	args := &mapBox{}
	tool := &mock.ToolMock{
		SpecFunc: func() gollem.ToolSpec { return gollem.ToolSpec{Name: name} },
		RunFunc: func(_ context.Context, a map[string]any) (map[string]any, error) {
			calls.inc()
			args.set(a)
			return map[string]any{"ok": true}, nil
		},
	}
	return tool, calls, args
}

type int32Box struct {
	mu sync.Mutex
	n  int
}

func (b *int32Box) inc() { b.mu.Lock(); b.n++; b.mu.Unlock() }
func (b *int32Box) get() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.n
}

type mapBox struct {
	mu sync.Mutex
	m  map[string]any
}

func (b *mapBox) set(m map[string]any) { b.mu.Lock(); b.m = m; b.mu.Unlock() }
func (b *mapBox) get() map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.m
}

// setupParentChild registers a "child" agent that finishes immediately and a
// "parent" whose first transition spawns one child and waits for it.
func setupParentChild(t *testing.T, model gollem.LLMClient, opts ...agentkit.KernelOption) (*agentkit.Kernel, agentkit.Repository, agentkit.Agent[scriptInput]) {
	t.Helper()
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
			id, e := child.SpawnChild(c, sys, scriptInput{Seed: "kid"})
			if e != nil {
				return st, agentkit.Decision[[]byte]{}, e
			}
			st.N = 1
			return st, agentkit.Suspend[[]byte](agentkit.WaitChildren("kids", id)), nil
		}
		return st, agentkit.Done([]byte("done")), nil
	}
	parent, err := agentkit.Register(reg, "parent", 1, &scriptStrategy{step: parentStep})
	gt.NoError(t, err)

	k, err := agentkit.New(repo, model, reg, opts...)
	gt.NoError(t, err)
	return k, repo, parent
}

// --- Init -------------------------------------------------------------------

func TestInitMiddlewareOnAppSpawn(t *testing.T) {
	ctx := context.Background()
	model, _ := mockLLM(textResponse("x"))

	var mu sync.Mutex
	var gotReq *agentkit.InitRequest
	var gotSeed string
	var gotOK bool
	mw := func(next agentkit.InitHandler) agentkit.InitHandler {
		return func(c context.Context, req *agentkit.InitRequest) (*agentkit.InitResult, error) {
			in, ok := agentkit.InitInput[scriptInput](req)
			mu.Lock()
			gotReq, gotSeed, gotOK = req, in.Seed, ok
			mu.Unlock()
			return next(c, req)
		}
	}

	k, _, ag := setupScript(t, doneStep(), model, agentkit.WithInitMiddleware(mw))
	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	gt.Value(t, gotOK).Equal(true)
	gt.Value(t, gotSeed).Equal("s")
	gt.Value(t, gotReq.ProcessID).Equal(pid) // the id is minted before Init.
	gt.Value(t, gotReq.Agent).Equal(agentkit.AgentName("main"))
	gt.Value(t, gotReq.Parent).Equal(nil) // application entry point.
}

func TestInitMiddlewareOnChildSpawn(t *testing.T) {
	ctx := context.Background()
	model, _ := mockLLM(textResponse("x"))
	rec := &recorder{}

	var mu sync.Mutex
	var childParent *agentkit.EffectContext
	initMW := func(next agentkit.InitHandler) agentkit.InitHandler {
		return func(c context.Context, req *agentkit.InitRequest) (*agentkit.InitResult, error) {
			rec.add("init:" + string(req.Agent))
			if req.Agent == "child" {
				mu.Lock()
				childParent = req.Parent
				mu.Unlock()
			}
			return next(c, req)
		}
	}
	spawnMW := func(next agentkit.SpawnHandler) agentkit.SpawnHandler {
		return func(c context.Context, req *agentkit.SpawnRequest) (agentkit.ProcessID, error) {
			rec.add("spawn:" + string(req.Agent))
			return next(c, req)
		}
	}

	k, repo, parent := setupParentChild(t, model,
		agentkit.WithInitMiddleware(initMW), agentkit.WithSpawnMiddleware(spawnMW))
	pid, err := parent.Spawn(ctx, k, scriptInput{Seed: "p"})
	gt.NoError(t, err)
	p := serveUntil(t, k, repo, pid, 5*time.Second, isTerminal)
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)

	// Init for the parent (app spawn), then Spawn wrapping Init for the child.
	gt.Value(t, rec.snapshot()).Equal([]string{"init:parent", "spawn:child", "init:child"})

	mu.Lock()
	defer mu.Unlock()
	gt.Value(t, childParent != nil).Equal(true)
	gt.Value(t, childParent.ProcessID).Equal(pid)
	gt.Value(t, childParent.Agent).Equal(agentkit.AgentName("parent"))
	gt.Value(t, childParent.StateSeq).Equal(1) // the spawning transition.
}

func TestInitMiddlewareRewritesInput(t *testing.T) {
	ctx := context.Background()
	model, _ := mockLLM(textResponse("x"))

	var original string
	mw := func(next agentkit.InitHandler) agentkit.InitHandler {
		return func(c context.Context, req *agentkit.InitRequest) (*agentkit.InitResult, error) {
			in, ok := agentkit.InitInput[scriptInput](req)
			if !ok {
				return next(c, req)
			}
			derived := agentkit.NewInitRequest(req, scriptInput{Seed: "rewritten"})
			// Deriving must not disturb the request the outer layer holds.
			back, _ := agentkit.InitInput[scriptInput](req)
			original = back.Seed
			_ = in
			return next(c, derived)
		}
	}

	k, repo, ag := setupScript(t, doneStep(), model, agentkit.WithInitMiddleware(mw))
	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "given"})
	gt.NoError(t, err)

	gt.Value(t, original).Equal("given") // copy-on-write, not in-place.

	proc, err := repo.GetProcess(ctx, pid)
	gt.NoError(t, err)
	var st scriptState
	gt.NoError(t, json.Unmarshal(proc.State, &st))
	gt.Value(t, st.Seed).Equal("rewritten")
}

func TestInitMiddlewareShortCircuit(t *testing.T) {
	ctx := context.Background()
	model, _ := mockLLM(textResponse("x"))

	// The script strategy's Init rejects an empty Seed; short-circuiting means
	// it never runs, so an empty Seed is accepted.
	mw := func(_ agentkit.InitHandler) agentkit.InitHandler {
		return func(_ context.Context, _ *agentkit.InitRequest) (*agentkit.InitResult, error) {
			return agentkit.NewInitResult(scriptState{Seed: "injected"}), nil
		}
	}

	k, repo, ag := setupScript(t, doneStep(), model, agentkit.WithInitMiddleware(mw))
	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: ""})
	gt.NoError(t, err)

	proc, err := repo.GetProcess(ctx, pid)
	gt.NoError(t, err)
	var st scriptState
	gt.NoError(t, json.Unmarshal(proc.State, &st))
	gt.Value(t, st.Seed).Equal("injected")
}

func TestInitMiddlewareRejects(t *testing.T) {
	ctx := context.Background()
	model, _ := mockLLM(textResponse("x"))
	sentinel := gollemErr("init denied")

	mw := func(_ agentkit.InitHandler) agentkit.InitHandler {
		return func(_ context.Context, _ *agentkit.InitRequest) (*agentkit.InitResult, error) {
			return nil, sentinel
		}
	}
	k, _, ag := setupScript(t, doneStep(), model, agentkit.WithInitMiddleware(mw))
	_, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.Value(t, errors.Is(err, sentinel)).Equal(true)
}

// E19: Init runs before the idempotency lookup, so an idempotent no-op Spawn
// still fires the middleware and discards the id minted for that call.
func TestInitMiddlewareFiresOnIdempotentSpawn(t *testing.T) {
	ctx := context.Background()
	model, _ := mockLLM(textResponse("x"))

	var mu sync.Mutex
	var ids []agentkit.ProcessID
	mw := func(next agentkit.InitHandler) agentkit.InitHandler {
		return func(c context.Context, req *agentkit.InitRequest) (*agentkit.InitResult, error) {
			mu.Lock()
			ids = append(ids, req.ProcessID)
			mu.Unlock()
			return next(c, req)
		}
	}

	k, _, ag := setupScript(t, doneStep(), model, agentkit.WithInitMiddleware(mw))
	first, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"}, agentkit.WithIdempotencyKey("k"))
	gt.NoError(t, err)
	second, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"}, agentkit.WithIdempotencyKey("k"))
	gt.NoError(t, err)

	gt.Value(t, second).Equal(first) // same Process returned.
	mu.Lock()
	defer mu.Unlock()
	gt.Array(t, ids).Length(2)                // the middleware fired twice,
	gt.Value(t, ids[1] == first).Equal(false) // and the second id was discarded.
}

// E22: Init runs on the caller's goroutine, so a panic reaches the caller.
func TestInitMiddlewarePanicPropagates(t *testing.T) {
	ctx := context.Background()
	model, _ := mockLLM(textResponse("x"))
	mw := func(_ agentkit.InitHandler) agentkit.InitHandler {
		return func(_ context.Context, _ *agentkit.InitRequest) (*agentkit.InitResult, error) {
			panic("boom")
		}
	}
	k, _, ag := setupScript(t, doneStep(), model, agentkit.WithInitMiddleware(mw))

	panicked := func() (p bool) {
		defer func() {
			if r := recover(); r != nil {
				p = true
			}
		}()
		_, _ = ag.Spawn(ctx, k, scriptInput{Seed: "s"})
		return false
	}()
	gt.Value(t, panicked).Equal(true)
}

// --- Step -------------------------------------------------------------------

func TestStepMiddlewareObservesStateAndDecision(t *testing.T) {
	ctx := context.Background()
	model, _ := mockLLM(textResponse("x"))
	rec := &recorder{}

	// Two transitions: N=0 -> Continue, N=1 -> Done.
	step := func(_ context.Context, _ agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		if st.N == 0 {
			st.N = 1
			return st, agentkit.Continue[[]byte](), nil
		}
		return st, agentkit.Done([]byte("fin")), nil
	}
	mw := func(next agentkit.StepHandler) agentkit.StepHandler {
		return func(c context.Context, req *agentkit.StepRequest) (*agentkit.StepResult, error) {
			in, _ := agentkit.StepState[scriptState](req)
			res, err := next(c, req)
			if err != nil {
				return nil, err
			}
			out, _ := agentkit.ResultState[scriptState](res)
			rec.add(fmt.Sprintf("seq=%d in=%d out=%d dec=%s",
				req.Effect.StateSeq, in.N, out.N, agentkit.DecisionKindOf(res)))
			return res, nil
		}
	}

	k, repo, ag := setupScript(t, step, model, agentkit.WithStepMiddleware(mw))
	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	p := serveUntil(t, k, repo, pid, 3*time.Second, isTerminal)
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)

	gt.Value(t, rec.snapshot()).Equal([]string{
		"seq=1 in=0 out=1 dec=continue",
		"seq=2 in=1 out=1 dec=done",
	})
}

func TestStepMiddlewareOrder(t *testing.T) {
	ctx := context.Background()
	model, _ := mockLLM(textResponse("x"))
	rec := &recorder{}

	mk := func(tag string) agentkit.StepMiddleware {
		return func(next agentkit.StepHandler) agentkit.StepHandler {
			return func(c context.Context, req *agentkit.StepRequest) (*agentkit.StepResult, error) {
				rec.add(tag + "-in")
				res, err := next(c, req)
				rec.add(tag + "-out")
				return res, err
			}
		}
	}
	step := func(_ context.Context, _ agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		rec.add("base")
		return st, agentkit.Done([]byte("ok")), nil
	}

	k, repo, ag := setupScript(t, step, model, agentkit.WithStepMiddleware(mk("0"), mk("1"), mk("2")))
	pid, _ := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	serveUntil(t, k, repo, pid, 3*time.Second, isTerminal)

	gt.Value(t, rec.snapshot()).Equal([]string{
		"0-in", "1-in", "2-in", "base", "2-out", "1-out", "0-out",
	})
}

func TestStepMiddlewareReplacesState(t *testing.T) {
	ctx := context.Background()
	model, _ := mockLLM(textResponse("x"))

	var seenSecond int
	step := func(_ context.Context, _ agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		if st.N < 99 {
			return st, agentkit.Continue[[]byte](), nil
		}
		seenSecond = st.N
		return st, agentkit.Done([]byte("ok")), nil
	}
	// Replace the state on the way in for the first transition only.
	first := true
	mw := func(next agentkit.StepHandler) agentkit.StepHandler {
		return func(c context.Context, req *agentkit.StepRequest) (*agentkit.StepResult, error) {
			if !first {
				return next(c, req)
			}
			first = false
			st, ok := agentkit.StepState[scriptState](req)
			if !ok {
				return next(c, req)
			}
			st.N = 99
			return next(c, agentkit.NewStepRequest(req, st))
		}
	}

	k, repo, ag := setupScript(t, step, model, agentkit.WithStepMiddleware(mw))
	pid, _ := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	p := serveUntil(t, k, repo, pid, 3*time.Second, isTerminal)
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)
	gt.Value(t, seenSecond).Equal(99) // survived encode + decode.
}

func TestStepMiddlewareShortCircuit(t *testing.T) {
	ctx := context.Background()
	model, _ := mockLLM(textResponse("x"))

	stepCalls := 0
	step := func(_ context.Context, _ agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		stepCalls++
		return st, agentkit.Done([]byte("strategy")), nil
	}
	mw := func(_ agentkit.StepHandler) agentkit.StepHandler {
		return func(_ context.Context, req *agentkit.StepRequest) (*agentkit.StepResult, error) {
			st, _ := agentkit.StepState[scriptState](req)
			return agentkit.NewStepResult(st, agentkit.Done([]byte("mw"))), nil
		}
	}

	k, repo, ag := setupScript(t, step, model, agentkit.WithStepMiddleware(mw))
	pid, _ := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	p := serveUntil(t, k, repo, pid, 3*time.Second, isTerminal)
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)
	gt.Value(t, string(p.Output)).Equal("mw")
	gt.Value(t, stepCalls).Equal(0)
}

// countingSyscalls wraps Syscalls by embedding it: the sealed interface's
// unexported method is satisfied through promotion.
type countingSyscalls struct {
	agentkit.Syscalls
	calls *int32Box
}

func (c countingSyscalls) CallTool(ctx context.Context, call gollem.FunctionCall) (map[string]any, error) {
	c.calls.inc()
	return c.Syscalls.CallTool(ctx, call)
}

func TestStepMiddlewareWrapsSyscalls(t *testing.T) {
	ctx := context.Background()
	model, _ := mockLLM(textResponse("x"))
	tool, toolCalls, _ := countingTool("t")
	wrapped := &int32Box{}

	step := func(c context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		if _, err := sys.CallTool(c, gollem.FunctionCall{ID: "1", Name: "t", Arguments: map[string]any{}}); err != nil {
			return st, agentkit.Decision[[]byte]{}, err
		}
		return st, agentkit.Done([]byte("ok")), nil
	}
	mw := func(next agentkit.StepHandler) agentkit.StepHandler {
		return func(c context.Context, req *agentkit.StepRequest) (*agentkit.StepResult, error) {
			req.Sys = countingSyscalls{Syscalls: req.Sys, calls: wrapped}
			return next(c, req)
		}
	}
	tf := func(_ context.Context, _ *agentkit.Process) ([]gollem.Tool, error) {
		return []gollem.Tool{tool}, nil
	}

	k, repo, ag := setupScript(t, step, model,
		agentkit.WithStepMiddleware(mw), agentkit.WithToolFactory(tf))
	pid, _ := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	p := serveUntil(t, k, repo, pid, 3*time.Second, isTerminal)

	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)
	gt.Value(t, wrapped.get()).Equal(1)   // the wrapper saw the call,
	gt.Value(t, toolCalls.get()).Equal(1) // and the real tool still ran.
}

// D-E: a transition that does not commit re-runs Step, and the middleware sees
// it. Here Suspend declares no await, which fails buildCommit every time.
func TestStepMiddlewareRerunsOnUncommittedTransition(t *testing.T) {
	ctx := context.Background()
	model, _ := mockLLM(textResponse("x"))
	calls := &int32Box{}

	step := func(_ context.Context, _ agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		return st, agentkit.Suspend[[]byte](), nil
	}
	mw := func(next agentkit.StepHandler) agentkit.StepHandler {
		return func(c context.Context, req *agentkit.StepRequest) (*agentkit.StepResult, error) {
			calls.inc()
			return next(c, req)
		}
	}

	k, repo, ag := setupScript(t, step, model, agentkit.WithStepMiddleware(mw))
	pid, _ := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	p := serveUntil(t, k, repo, pid, 10*time.Second, isTerminal, agentkit.WithMaxStepAttempts(2))

	gt.Value(t, p.Status).Equal(agentkit.ProcessFailed)
	gt.Value(t, p.Failure.Code).Equal(agentkit.FailureRetryExhausted)
	gt.Value(t, calls.get() >= 2).Equal(true) // called again on the re-run.
}

// A Step handler runs at most once per transition: its effects buffer per
// transition, so a second attempt would commit the first attempt's children and
// events alongside the second's state.
func TestStepMiddlewareRejectsSecondNext(t *testing.T) {
	ctx := context.Background()
	model, _ := mockLLM(textResponse("x"))
	repo := memory.New()
	reg := agentkit.NewRegistry()

	child, err := agentkit.Register(reg, "child", 1, &scriptStrategy{step: doneStep()})
	gt.NoError(t, err)
	// Spawn a child, then fail — the classic "retry the step" shape.
	parentStep := func(c context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		if _, e := child.SpawnChild(c, sys, scriptInput{Seed: "kid"}); e != nil {
			return st, agentkit.Decision[[]byte]{}, e
		}
		return st, agentkit.Decision[[]byte]{}, gollemErr("transient")
	}
	parent, err := agentkit.Register(reg, "parent", 1, &scriptStrategy{step: parentStep})
	gt.NoError(t, err)

	var second error
	mw := func(next agentkit.StepHandler) agentkit.StepHandler {
		return func(c context.Context, req *agentkit.StepRequest) (*agentkit.StepResult, error) {
			if _, err := next(c, req); err != nil {
				_, second = next(c, req) // refused: the first attempt's child is still buffered.
				return nil, second
			}
			return next(c, req)
		}
	}

	k, err := agentkit.New(repo, model, reg, agentkit.WithStepMiddleware(mw))
	gt.NoError(t, err)
	pid, _ := parent.Spawn(ctx, k, scriptInput{Seed: "p"})
	p := serveUntil(t, k, repo, pid, 10*time.Second, isTerminal, agentkit.WithMaxStepAttempts(1))

	gt.Value(t, p.Status).Equal(agentkit.ProcessFailed)
	gt.Value(t, errors.Is(second, agentkit.ErrInvalidRequest)).Equal(true)
	gt.S(t, second.Error()).Contains("more than once")
}

// The Process handed to a Step middleware is a copy: writing to it — including
// Rev, which would make every Apply conflict — must not reach the commit.
func TestStepMiddlewareProcessIsACopy(t *testing.T) {
	ctx := context.Background()
	model, _ := mockLLM(textResponse("x"))

	mw := func(next agentkit.StepHandler) agentkit.StepHandler {
		return func(c context.Context, req *agentkit.StepRequest) (*agentkit.StepResult, error) {
			req.Process.Rev = 9999
			req.Process.Metadata = map[string]string{"injected": "yes"}
			req.Process.Status = agentkit.ProcessCancelled
			return next(c, req)
		}
	}
	k, repo, ag := setupScript(t, doneStep(), model, agentkit.WithStepMiddleware(mw))
	pid, _ := ag.Spawn(ctx, k, scriptInput{Seed: "s"}, agentkit.WithMetadata(map[string]string{"real": "yes"}))
	p := serveUntil(t, k, repo, pid, 3*time.Second, isTerminal)

	// The transition committed normally: no conflict storm, no injected values.
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)
	gt.Value(t, p.Metadata["real"]).Equal("yes")
	gt.Value(t, p.Metadata["injected"]).Equal("")
	gt.Value(t, p.Rev != 9999).Equal(true)
}

// The child's recorded lineage must come from the live transition, not from a
// field a middleware can overwrite.
func TestSpawnMiddlewareCannotForgeParentContext(t *testing.T) {
	ctx := context.Background()
	model, _ := mockLLM(textResponse("x"))

	spawnMW := func(next agentkit.SpawnHandler) agentkit.SpawnHandler {
		return func(c context.Context, req *agentkit.SpawnRequest) (agentkit.ProcessID, error) {
			req.Effect = agentkit.EffectContext{
				ProcessID: "forged", RootID: "forged", Agent: "forged", StateSeq: 999,
			}
			return next(c, req)
		}
	}
	var mu sync.Mutex
	var seen *agentkit.EffectContext
	initMW := func(next agentkit.InitHandler) agentkit.InitHandler {
		return func(c context.Context, req *agentkit.InitRequest) (*agentkit.InitResult, error) {
			if req.Agent == "child" {
				mu.Lock()
				seen = req.Parent
				mu.Unlock()
			}
			return next(c, req)
		}
	}

	k, repo, parent := setupParentChild(t, model,
		agentkit.WithSpawnMiddleware(spawnMW), agentkit.WithInitMiddleware(initMW))
	pid, _ := parent.Spawn(ctx, k, scriptInput{Seed: "p"})
	p := serveUntil(t, k, repo, pid, 5*time.Second, isTerminal)
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)

	mu.Lock()
	defer mu.Unlock()
	gt.Value(t, seen != nil).Equal(true)
	gt.Value(t, seen.ProcessID).Equal(pid) // the real parent, not "forged".
	gt.Value(t, seen.Agent).Equal(agentkit.AgentName("parent"))
}

// E17: a replaced state of the wrong type is an error, not a panic.
func TestStepStateTypeMismatch(t *testing.T) {
	ctx := context.Background()
	model, _ := mockLLM(textResponse("x"))

	mw := func(next agentkit.StepHandler) agentkit.StepHandler {
		return func(c context.Context, req *agentkit.StepRequest) (*agentkit.StepResult, error) {
			return next(c, agentkit.NewStepRequest(req, struct{ X int }{1}))
		}
	}
	k, repo, ag := setupScript(t, doneStep(), model,
		agentkit.WithStepMiddleware(mw))
	pid, _ := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	p := serveUntil(t, k, repo, pid, 10*time.Second, isTerminal, agentkit.WithMaxStepAttempts(1))

	gt.Value(t, p.Status).Equal(agentkit.ProcessFailed)
	gt.Value(t, p.Failure.Code).Equal(agentkit.FailureRetryExhausted)
	gt.S(t, p.Failure.Message).Contains("step state type mismatch")
}

// E17 (encode side): the result state is what gets encoded.
func TestStepResultStateTypeMismatch(t *testing.T) {
	ctx := context.Background()
	model, _ := mockLLM(textResponse("x"))

	mw := func(_ agentkit.StepHandler) agentkit.StepHandler {
		return func(_ context.Context, _ *agentkit.StepRequest) (*agentkit.StepResult, error) {
			return agentkit.NewStepResult(struct{ X int }{1}, agentkit.Done([]byte("x"))), nil
		}
	}
	k, repo, ag := setupScript(t, doneStep(), model, agentkit.WithStepMiddleware(mw))
	pid, _ := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	p := serveUntil(t, k, repo, pid, 10*time.Second, isTerminal, agentkit.WithMaxStepAttempts(1))

	gt.Value(t, p.Status).Equal(agentkit.ProcessFailed)
	gt.S(t, p.Failure.Message).Contains("encode state type mismatch")
}

// E20: a nil Syscalls is the middleware's mistake; the worker survives it.
func TestStepMiddlewareNilSys(t *testing.T) {
	ctx := context.Background()
	model, _ := mockLLM(textResponse("x"))

	step := func(c context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		_, err := sys.Generate(c, []gollem.Input{gollem.Text("go")})
		return st, agentkit.Done([]byte("ok")), err
	}
	mw := func(next agentkit.StepHandler) agentkit.StepHandler {
		return func(c context.Context, req *agentkit.StepRequest) (*agentkit.StepResult, error) {
			req.Sys = nil
			return next(c, req)
		}
	}
	k, repo, ag := setupScript(t, step, model, agentkit.WithStepMiddleware(mw))
	pid, _ := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	p := serveUntil(t, k, repo, pid, 10*time.Second, isTerminal, agentkit.WithMaxStepAttempts(1))
	gt.Value(t, p.Status).Equal(agentkit.ProcessFailed)
}

// --- Generate / CallTool ----------------------------------------------------

func TestGenerateMiddlewareOrder(t *testing.T) {
	ctx := context.Background()
	model, _ := mockLLM(textResponse("x"))
	rec := &recorder{}

	mk := func(tag string) agentkit.GenerateMiddleware {
		return func(next agentkit.GenerateHandler) agentkit.GenerateHandler {
			return func(c context.Context, req *agentkit.GenerateRequest) (*agentkit.GenerateResult, error) {
				rec.add(tag + "-in")
				res, err := next(c, req)
				rec.add(tag + "-out")
				return res, err
			}
		}
	}
	step := func(c context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		if _, err := sys.Generate(c, []gollem.Input{gollem.Text("go")}); err != nil {
			return st, agentkit.Decision[[]byte]{}, err
		}
		return st, agentkit.Done([]byte("ok")), nil
	}

	k, repo, ag := setupScript(t, step, model, agentkit.WithGenerateMiddleware(mk("0"), mk("1"), mk("2")))
	pid, _ := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	serveUntil(t, k, repo, pid, 3*time.Second, isTerminal)

	gt.Value(t, rec.snapshot()).Equal([]string{
		"0-in", "1-in", "2-in", "2-out", "1-out", "0-out",
	})
}

func TestGenerateMiddlewareRewritesRole(t *testing.T) {
	ctx := context.Background()
	defaultModel, defaultCount := mockLLM(textResponse("default"))
	roleModel, roleCount := mockLLM(textResponse("role"))
	planner := agentkit.DefineModelRole("planner")

	mw := func(next agentkit.GenerateHandler) agentkit.GenerateHandler {
		return func(c context.Context, req *agentkit.GenerateRequest) (*agentkit.GenerateResult, error) {
			req.Role = planner // concrete field: assigned directly.
			return next(c, req)
		}
	}
	step := func(c context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		if _, err := sys.Generate(c, []gollem.Input{gollem.Text("go")}); err != nil {
			return st, agentkit.Decision[[]byte]{}, err
		}
		return st, agentkit.Done([]byte("ok")), nil
	}

	k, repo, ag := setupScript(t, step, defaultModel,
		agentkit.WithModelRole(planner, roleModel), agentkit.WithGenerateMiddleware(mw))
	pid, _ := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	serveUntil(t, k, repo, pid, 3*time.Second, isTerminal)

	gt.Value(t, *roleCount).Equal(1)
	gt.Value(t, *defaultCount).Equal(0)
}

// E10: an unregistered role falls back to the default model, as before.
func TestGenerateMiddlewareUnknownRoleFallsBack(t *testing.T) {
	ctx := context.Background()
	model, count := mockLLM(textResponse("x"))
	unknown := agentkit.DefineModelRole("unregistered")

	mw := func(next agentkit.GenerateHandler) agentkit.GenerateHandler {
		return func(c context.Context, req *agentkit.GenerateRequest) (*agentkit.GenerateResult, error) {
			req.Role = unknown
			return next(c, req)
		}
	}
	step := func(c context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		if _, err := sys.Generate(c, []gollem.Input{gollem.Text("go")}); err != nil {
			return st, agentkit.Decision[[]byte]{}, err
		}
		return st, agentkit.Done([]byte("ok")), nil
	}

	k, repo, ag := setupScript(t, step, model, agentkit.WithGenerateMiddleware(mw))
	pid, _ := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	p := serveUntil(t, k, repo, pid, 3*time.Second, isTerminal)
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)
	gt.Value(t, *count).Equal(1)
}

// E4: calling next twice charges twice.
func TestGenerateMiddlewareRetriesNext(t *testing.T) {
	ctx := context.Background()
	model, count := mockLLM(textResponse("x"))

	mw := func(next agentkit.GenerateHandler) agentkit.GenerateHandler {
		return func(c context.Context, req *agentkit.GenerateRequest) (*agentkit.GenerateResult, error) {
			if _, err := next(c, req); err != nil {
				return nil, err
			}
			return next(c, req)
		}
	}
	step := func(c context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		if _, err := sys.Generate(c, []gollem.Input{gollem.Text("go")}); err != nil {
			return st, agentkit.Decision[[]byte]{}, err
		}
		return st, agentkit.Done([]byte("ok")), nil
	}

	k, repo, ag := setupScript(t, step, model, agentkit.WithGenerateMiddleware(mw))
	pid, _ := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	p := serveUntil(t, k, repo, pid, 3*time.Second, isTerminal)

	gt.Value(t, *count).Equal(2)
	gt.Value(t, p.Metrics[agentkit.MetricLLMCalls]).Equal(int64(2))
}

func TestToolCallMiddlewareRewritesRequest(t *testing.T) {
	ctx := context.Background()
	model, _ := mockLLM(textResponse("x"))
	tool, _, args := countingTool("t")

	mw := func(next agentkit.ToolCallHandler) agentkit.ToolCallHandler {
		return func(c context.Context, req *agentkit.ToolCallRequest) (map[string]any, error) {
			req.Call.Arguments["x"] = "rewritten"
			return next(c, req)
		}
	}
	step := func(c context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		if _, err := sys.CallTool(c, gollem.FunctionCall{ID: "1", Name: "t", Arguments: map[string]any{"x": "given"}}); err != nil {
			return st, agentkit.Decision[[]byte]{}, err
		}
		return st, agentkit.Done([]byte("ok")), nil
	}
	tf := func(_ context.Context, _ *agentkit.Process) ([]gollem.Tool, error) {
		return []gollem.Tool{tool}, nil
	}

	k, repo, ag := setupScript(t, step, model,
		agentkit.WithToolCallMiddleware(mw), agentkit.WithToolFactory(tf))
	pid, _ := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	serveUntil(t, k, repo, pid, 3*time.Second, isTerminal)

	gt.Value(t, args.get()["x"]).Equal("rewritten")
}

// E3: a middleware that does not call next stops everything behind it.
func TestToolCallMiddlewareShortCircuit(t *testing.T) {
	ctx := context.Background()
	model, _ := mockLLM(textResponse("x"))
	tool, toolCalls, _ := countingTool("t")
	denied := gollemErr("denied")

	mw := func(_ agentkit.ToolCallHandler) agentkit.ToolCallHandler {
		return func(_ context.Context, _ *agentkit.ToolCallRequest) (map[string]any, error) {
			return nil, denied
		}
	}
	var got error
	step := func(c context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		_, got = sys.CallTool(c, gollem.FunctionCall{ID: "1", Name: "t", Arguments: map[string]any{}})
		return st, agentkit.Done([]byte("ok")), nil
	}
	tf := func(_ context.Context, _ *agentkit.Process) ([]gollem.Tool, error) {
		return []gollem.Tool{tool}, nil
	}

	k, repo, ag := setupScript(t, step, model,
		agentkit.WithToolCallMiddleware(mw), agentkit.WithToolFactory(tf))
	pid, _ := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	p := serveUntil(t, k, repo, pid, 3*time.Second, isTerminal)

	gt.Value(t, errors.Is(got, denied)).Equal(true)
	gt.Value(t, toolCalls.get()).Equal(0)
	gt.Value(t, p.Metrics[agentkit.MetricToolCalls]).Equal(int64(0))
}

// E9: rewriting to an unknown tool surfaces ErrToolNotFound.
func TestToolCallMiddlewareRewritesToUnknownTool(t *testing.T) {
	ctx := context.Background()
	model, _ := mockLLM(textResponse("x"))
	tool := mockTool("t", map[string]any{"ok": true})

	mw := func(next agentkit.ToolCallHandler) agentkit.ToolCallHandler {
		return func(c context.Context, req *agentkit.ToolCallRequest) (map[string]any, error) {
			req.Call.Name = "nope"
			return next(c, req)
		}
	}
	var got error
	step := func(c context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		_, got = sys.CallTool(c, gollem.FunctionCall{ID: "1", Name: "t", Arguments: map[string]any{}})
		return st, agentkit.Done([]byte("ok")), nil
	}
	tf := func(_ context.Context, _ *agentkit.Process) ([]gollem.Tool, error) {
		return []gollem.Tool{tool}, nil
	}

	k, repo, ag := setupScript(t, step, model,
		agentkit.WithToolCallMiddleware(mw), agentkit.WithToolFactory(tf))
	pid, _ := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	serveUntil(t, k, repo, pid, 3*time.Second, isTerminal)

	gt.Value(t, errors.Is(got, agentkit.ErrToolNotFound)).Equal(true)
}

// E12/D-B: the chain is outside the Limiter, so a refused call is observable.
func TestMiddlewareObservesLimitExceeded(t *testing.T) {
	ctx := context.Background()
	model, _ := mockLLM(textResponse("x"))
	tool, toolCalls, _ := countingTool("t")

	var mu sync.Mutex
	var observed error
	mw := func(next agentkit.ToolCallHandler) agentkit.ToolCallHandler {
		return func(c context.Context, req *agentkit.ToolCallRequest) (map[string]any, error) {
			out, err := next(c, req)
			mu.Lock()
			observed = err
			mu.Unlock()
			return out, err
		}
	}
	// The Limiter is consulted at the transition boundary first (worker.go) and
	// then again inside CallTool. Let the boundary through so the transition
	// actually starts, and refuse from the effect check onward.
	limitCalls := &int32Box{}
	limiter := func(_ context.Context, _ *agentkit.Process, _ agentkit.Metrics) error {
		limitCalls.inc()
		if limitCalls.get() == 1 {
			return nil // the transition boundary.
		}
		return gollemErr("over budget")
	}
	step := func(c context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		_, _ = sys.CallTool(c, gollem.FunctionCall{ID: "1", Name: "t", Arguments: map[string]any{}})
		return st, agentkit.Done([]byte("ok")), nil
	}
	tf := func(_ context.Context, _ *agentkit.Process) ([]gollem.Tool, error) {
		return []gollem.Tool{tool}, nil
	}

	k, repo, ag := setupScript(t, step, model,
		agentkit.WithToolCallMiddleware(mw), agentkit.WithToolFactory(tf), agentkit.WithLimiter(limiter))
	pid, _ := ag.Spawn(ctx, k, scriptInput{Seed: "s"}, agentkit.WithMetadata(map[string]string{"seed": "1"}))
	serveUntil(t, k, repo, pid, 3*time.Second, isTerminal)

	mu.Lock()
	defer mu.Unlock()
	gt.Value(t, errors.Is(observed, agentkit.ErrLimitExceeded)).Equal(true)
	gt.Value(t, toolCalls.get()).Equal(0) // the tool never ran.
}

// --- SpawnChild -------------------------------------------------------------

func TestSpawnMiddlewareRewritesInput(t *testing.T) {
	ctx := context.Background()
	model, _ := mockLLM(textResponse("x"))

	var original string
	mw := func(next agentkit.SpawnHandler) agentkit.SpawnHandler {
		return func(c context.Context, req *agentkit.SpawnRequest) (agentkit.ProcessID, error) {
			in, ok := agentkit.SpawnInput[scriptInput](req)
			if !ok {
				return next(c, req)
			}
			derived := agentkit.NewSpawnRequest(req, scriptInput{Seed: "child2"})
			back, _ := agentkit.SpawnInput[scriptInput](req)
			original = back.Seed
			_ = in
			return next(c, derived)
		}
	}

	k, repo, parent := setupParentChild(t, model, agentkit.WithSpawnMiddleware(mw))
	pid, _ := parent.Spawn(ctx, k, scriptInput{Seed: "p"})
	p := serveUntil(t, k, repo, pid, 5*time.Second, isTerminal)
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)

	gt.Value(t, original).Equal("kid") // copy-on-write.

	awaits, err := repo.ListAwaits(ctx, pid)
	gt.NoError(t, err)
	gt.Array(t, awaits).Length(1)
	gt.Array(t, awaits[0].Results).Length(1)
	gt.Value(t, string(awaits[0].Results[0].Output)).Equal("child2")
}

func TestSpawnMiddlewareOnCommitSuccess(t *testing.T) {
	ctx := context.Background()
	model, _ := mockLLM(textResponse("x"))

	var mu sync.Mutex
	var errs []error
	var persistedAtCallback bool
	var repoRef agentkit.Repository

	mw := func(next agentkit.SpawnHandler) agentkit.SpawnHandler {
		return func(c context.Context, req *agentkit.SpawnRequest) (agentkit.ProcessID, error) {
			cid, err := next(c, req)
			if err != nil {
				return "", err
			}
			// Registered AFTER next, so it is bound to a child that exists.
			req.OnCommit(func(commitErr error) {
				_, gerr := repoRef.GetProcess(context.Background(), cid)
				mu.Lock()
				errs = append(errs, commitErr)
				persistedAtCallback = gerr == nil
				mu.Unlock()
			})
			return cid, nil
		}
	}

	k, repo, parent := setupParentChild(t, model, agentkit.WithSpawnMiddleware(mw))
	repoRef = repo
	pid, _ := parent.Spawn(ctx, k, scriptInput{Seed: "p"})
	p := serveUntil(t, k, repo, pid, 5*time.Second, isTerminal)
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)

	mu.Lock()
	defer mu.Unlock()
	gt.Array(t, errs).Length(1)
	gt.Value(t, errs[0]).Equal(nil)
	gt.Value(t, persistedAtCallback).Equal(true)
}

// E7: OnCommit is transition-scoped. Registered before next and the transition
// then fails to commit, it still fires — with the error.
func TestSpawnOnCommitFiresOnAbortedTransition(t *testing.T) {
	ctx := context.Background()
	model, _ := mockLLM(textResponse("x"))
	repo := memory.New()
	reg := agentkit.NewRegistry()

	child, err := agentkit.Register(reg, "child", 1, &scriptStrategy{step: doneStep()})
	gt.NoError(t, err)
	// Spawn a child and then Suspend with no await: buildCommit rejects it.
	parentStep := func(c context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		if _, e := child.SpawnChild(c, sys, scriptInput{Seed: "kid"}); e != nil {
			return st, agentkit.Decision[[]byte]{}, e
		}
		return st, agentkit.Suspend[[]byte](), nil
	}
	parent, err := agentkit.Register(reg, "parent", 1, &scriptStrategy{step: parentStep})
	gt.NoError(t, err)

	var mu sync.Mutex
	var errs []error
	mw := func(next agentkit.SpawnHandler) agentkit.SpawnHandler {
		return func(c context.Context, req *agentkit.SpawnRequest) (agentkit.ProcessID, error) {
			req.OnCommit(func(commitErr error) { // registered BEFORE next.
				mu.Lock()
				errs = append(errs, commitErr)
				mu.Unlock()
			})
			return next(c, req)
		}
	}

	k, err := agentkit.New(repo, model, reg, agentkit.WithSpawnMiddleware(mw))
	gt.NoError(t, err)
	pid, _ := parent.Spawn(ctx, k, scriptInput{Seed: "p"})
	p := serveUntil(t, k, repo, pid, 10*time.Second, isTerminal, agentkit.WithMaxStepAttempts(1))
	gt.Value(t, p.Status).Equal(agentkit.ProcessFailed)

	mu.Lock()
	defer mu.Unlock()
	// Each transition ATTEMPT registers and fires its own callback (the buffer
	// is cleared after firing, so one attempt never double-fires). A retried
	// transition therefore yields one entry per attempt — all of them failures,
	// because no child was ever committed.
	gt.Value(t, len(errs) >= 1).Equal(true)
	for _, e := range errs {
		gt.Value(t, e != nil).Equal(true)
	}
}

// commitTerminal path: a transition that spawns a child and finishes in the
// same step commits both in one Apply, and the callback reports that.
func TestSpawnOnCommitOnTerminalTransition(t *testing.T) {
	ctx := context.Background()
	model, _ := mockLLM(textResponse("x"))
	repo := memory.New()
	reg := agentkit.NewRegistry()

	child, err := agentkit.Register(reg, "child", 1, &scriptStrategy{step: doneStep()})
	gt.NoError(t, err)
	// Spawn and finish in the same transition: this commits through
	// commitTerminal, not buildCommit.
	parentStep := func(c context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		if _, e := child.SpawnChild(c, sys, scriptInput{Seed: "kid"}); e != nil {
			return st, agentkit.Decision[[]byte]{}, e
		}
		return st, agentkit.Done([]byte("done")), nil
	}
	parent, err := agentkit.Register(reg, "parent", 1, &scriptStrategy{step: parentStep})
	gt.NoError(t, err)

	var mu sync.Mutex
	var errs []error
	var cid agentkit.ProcessID
	mw := func(next agentkit.SpawnHandler) agentkit.SpawnHandler {
		return func(c context.Context, req *agentkit.SpawnRequest) (agentkit.ProcessID, error) {
			id, err := next(c, req)
			if err != nil {
				return "", err
			}
			cid = id
			req.OnCommit(func(commitErr error) {
				mu.Lock()
				errs = append(errs, commitErr)
				mu.Unlock()
			})
			return id, nil
		}
	}

	k, err := agentkit.New(repo, model, reg, agentkit.WithSpawnMiddleware(mw))
	gt.NoError(t, err)
	pid, _ := parent.Spawn(ctx, k, scriptInput{Seed: "p"})
	p := serveUntil(t, k, repo, pid, 5*time.Second, isTerminal)
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)

	mu.Lock()
	defer mu.Unlock()
	gt.Array(t, errs).Length(1)
	gt.Value(t, errs[0]).Equal(nil)
	_, gerr := repo.GetProcess(ctx, cid) // the child really landed.
	gt.NoError(t, gerr)
}

// A repository failure that is not ErrConflict aborts the commit, and the
// callback must report it rather than claiming success.
func TestSpawnOnCommitOnApplyFailure(t *testing.T) {
	ctx := context.Background()
	model, _ := mockLLM(textResponse("x"))
	base := memory.New()
	hr := &hookRepo{Repository: base}
	reg := agentkit.NewRegistry()

	child, err := agentkit.Register(reg, "child", 1, &scriptStrategy{step: doneStep()})
	gt.NoError(t, err)
	parentStep := func(c context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		if _, e := child.SpawnChild(c, sys, scriptInput{Seed: "kid"}); e != nil {
			return st, agentkit.Decision[[]byte]{}, e
		}
		return st, agentkit.Done([]byte("done")), nil
	}
	parent, err := agentkit.Register(reg, "parent", 1, &scriptStrategy{step: parentStep})
	gt.NoError(t, err)

	var mu sync.Mutex
	var errs []error
	mw := func(next agentkit.SpawnHandler) agentkit.SpawnHandler {
		return func(c context.Context, req *agentkit.SpawnRequest) (agentkit.ProcessID, error) {
			id, err := next(c, req)
			if err != nil {
				return "", err
			}
			req.OnCommit(func(commitErr error) {
				mu.Lock()
				errs = append(errs, commitErr)
				mu.Unlock()
			})
			return id, nil
		}
	}

	k, err := agentkit.New(hr, model, reg, agentkit.WithSpawnMiddleware(mw))
	gt.NoError(t, err)
	pid, err := parent.Spawn(ctx, k, scriptInput{Seed: "p"})
	gt.NoError(t, err)

	// Fail every Apply that carries the buffered child.
	hr.setFailApply(func(cs agentkit.ChangeSet) error {
		for _, p := range cs.Processes {
			if p.ParentID != nil && *p.ParentID == pid {
				return gollemErr("storage down")
			}
		}
		return nil
	})

	// The commit can never land, so the Process never reaches a terminal state —
	// wait on the callback itself instead.
	fired := func(*agentkit.Process) bool {
		mu.Lock()
		defer mu.Unlock()
		return len(errs) > 0
	}
	serveUntil(t, k, hr, pid, 10*time.Second, fired, agentkit.WithMaxStepAttempts(1))

	mu.Lock()
	defer mu.Unlock()
	gt.Value(t, len(errs) >= 1).Equal(true)
	for _, e := range errs {
		gt.Value(t, e != nil).Equal(true) // never reported as committed.
	}
}

// E6: a panic inside an OnCommit callback is recovered; the worker lives.
func TestSpawnOnCommitPanicRecovered(t *testing.T) {
	ctx := context.Background()
	model, _ := mockLLM(textResponse("x"))
	reached := &int32Box{}

	mw := func(next agentkit.SpawnHandler) agentkit.SpawnHandler {
		return func(c context.Context, req *agentkit.SpawnRequest) (agentkit.ProcessID, error) {
			cid, err := next(c, req)
			if err != nil {
				return "", err
			}
			req.OnCommit(func(error) { panic("boom") })
			req.OnCommit(func(error) { reached.inc() })
			return cid, nil
		}
	}

	k, repo, parent := setupParentChild(t, model, agentkit.WithSpawnMiddleware(mw))
	pid, _ := parent.Spawn(ctx, k, scriptInput{Seed: "p"})
	p := serveUntil(t, k, repo, pid, 5*time.Second, isTerminal)

	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)
	gt.Value(t, reached.get()).Equal(1) // the second callback still ran.
}

// E8: an input of the wrong type is caught by the binding, at run time.
func TestSpawnMiddlewareInputTypeMismatch(t *testing.T) {
	ctx := context.Background()
	model, _ := mockLLM(textResponse("x"))

	mw := func(next agentkit.SpawnHandler) agentkit.SpawnHandler {
		return func(c context.Context, req *agentkit.SpawnRequest) (agentkit.ProcessID, error) {
			return next(c, agentkit.NewSpawnRequest(req, struct{ X int }{1}))
		}
	}
	k, repo, parent := setupParentChild(t, model, agentkit.WithSpawnMiddleware(mw))
	pid, _ := parent.Spawn(ctx, k, scriptInput{Seed: "p"})
	p := serveUntil(t, k, repo, pid, 10*time.Second, isTerminal, agentkit.WithMaxStepAttempts(1))

	gt.Value(t, p.Status).Equal(agentkit.ProcessFailed)
	gt.S(t, p.Failure.Message).Contains("type mismatch")
}

// E18: the same, on the Init short-circuit path.
func TestInitResultStateTypeMismatch(t *testing.T) {
	ctx := context.Background()
	model, _ := mockLLM(textResponse("x"))

	mw := func(_ agentkit.InitHandler) agentkit.InitHandler {
		return func(_ context.Context, _ *agentkit.InitRequest) (*agentkit.InitResult, error) {
			return agentkit.NewInitResult(struct{ X int }{1}), nil
		}
	}
	k, _, ag := setupScript(t, doneStep(), model, agentkit.WithInitMiddleware(mw))
	_, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.Value(t, errors.Is(err, agentkit.ErrInvalidRequest)).Equal(true)
}

// --- configuration ----------------------------------------------------------

// E1: a nil element is a construction error, for all five kinds.
func TestNilMiddlewareRejected(t *testing.T) {
	build := func(opt agentkit.KernelOption) error {
		reg := agentkit.NewRegistry()
		_, err := agentkit.Register(reg, "a", 1, &scriptStrategy{step: doneStep()})
		gt.NoError(t, err)
		model, _ := mockLLM(textResponse("x"))
		_, err = agentkit.New(memory.New(), model, reg, opt)
		return err
	}

	for name, opt := range map[string]agentkit.KernelOption{
		"init":     agentkit.WithInitMiddleware(nil),
		"step":     agentkit.WithStepMiddleware(nil),
		"generate": agentkit.WithGenerateMiddleware(nil),
		"toolcall": agentkit.WithToolCallMiddleware(nil),
		"spawn":    agentkit.WithSpawnMiddleware(nil),
	} {
		t.Run(name, func(t *testing.T) {
			gt.Value(t, errors.Is(build(opt), agentkit.ErrInvalidConfig)).Equal(true)
		})
	}
}

// E2/E14/E15/E16: a middleware that returns a nil handler, or a nil result, is
// reported as a configuration error rather than a nil dereference.
func TestMiddlewareReturningNil(t *testing.T) {
	ctx := context.Background()

	t.Run("generate handler", func(t *testing.T) {
		model, _ := mockLLM(textResponse("x"))
		var got error
		step := func(c context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
			_, got = sys.Generate(c, []gollem.Input{gollem.Text("go")})
			return st, agentkit.Done([]byte("ok")), nil
		}
		mw := func(_ agentkit.GenerateHandler) agentkit.GenerateHandler { return nil }
		k, repo, ag := setupScript(t, step, model, agentkit.WithGenerateMiddleware(mw))
		pid, _ := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
		serveUntil(t, k, repo, pid, 3*time.Second, isTerminal)
		gt.Value(t, errors.Is(got, agentkit.ErrInvalidConfig)).Equal(true)
	})

	t.Run("init handler", func(t *testing.T) {
		model, _ := mockLLM(textResponse("x"))
		mw := func(_ agentkit.InitHandler) agentkit.InitHandler { return nil }
		k, _, ag := setupScript(t, doneStep(), model, agentkit.WithInitMiddleware(mw))
		_, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
		gt.Value(t, errors.Is(err, agentkit.ErrInvalidConfig)).Equal(true)
	})

	t.Run("init result", func(t *testing.T) {
		model, _ := mockLLM(textResponse("x"))
		mw := func(_ agentkit.InitHandler) agentkit.InitHandler {
			return func(context.Context, *agentkit.InitRequest) (*agentkit.InitResult, error) {
				return nil, nil
			}
		}
		k, _, ag := setupScript(t, doneStep(), model, agentkit.WithInitMiddleware(mw))
		_, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
		gt.Value(t, errors.Is(err, agentkit.ErrInvalidConfig)).Equal(true)
	})

	t.Run("step handler", func(t *testing.T) {
		model, _ := mockLLM(textResponse("x"))
		mw := func(_ agentkit.StepHandler) agentkit.StepHandler { return nil }
		k, repo, ag := setupScript(t, doneStep(), model, agentkit.WithStepMiddleware(mw))
		pid, _ := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
		p := serveUntil(t, k, repo, pid, 10*time.Second, isTerminal, agentkit.WithMaxStepAttempts(1))
		gt.Value(t, p.Status).Equal(agentkit.ProcessFailed)
	})

	t.Run("step result", func(t *testing.T) {
		model, _ := mockLLM(textResponse("x"))
		mw := func(_ agentkit.StepHandler) agentkit.StepHandler {
			return func(context.Context, *agentkit.StepRequest) (*agentkit.StepResult, error) {
				return nil, nil
			}
		}
		k, repo, ag := setupScript(t, doneStep(), model, agentkit.WithStepMiddleware(mw))
		pid, _ := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
		p := serveUntil(t, k, repo, pid, 10*time.Second, isTerminal, agentkit.WithMaxStepAttempts(1))
		gt.Value(t, p.Status).Equal(agentkit.ProcessFailed)
		gt.S(t, p.Failure.Message).Contains("nil result")
	})
}

// E5: a panic in in-transition middleware becomes a transition error; the
// worker survives (this test process running to completion proves it).
func TestMiddlewarePanicFailsProcess(t *testing.T) {
	ctx := context.Background()

	t.Run("step", func(t *testing.T) {
		model, _ := mockLLM(textResponse("x"))
		mw := func(_ agentkit.StepHandler) agentkit.StepHandler {
			return func(context.Context, *agentkit.StepRequest) (*agentkit.StepResult, error) {
				panic("boom")
			}
		}
		k, repo, ag := setupScript(t, doneStep(), model, agentkit.WithStepMiddleware(mw))
		pid, _ := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
		p := serveUntil(t, k, repo, pid, 10*time.Second, isTerminal, agentkit.WithMaxStepAttempts(1))
		gt.Value(t, p.Status).Equal(agentkit.ProcessFailed)
		gt.Value(t, p.Failure.Code).Equal(agentkit.FailureRetryExhausted)
	})

	t.Run("generate", func(t *testing.T) {
		model, _ := mockLLM(textResponse("x"))
		step := func(c context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
			_, err := sys.Generate(c, []gollem.Input{gollem.Text("go")})
			return st, agentkit.Done([]byte("ok")), err
		}
		mw := func(_ agentkit.GenerateHandler) agentkit.GenerateHandler {
			return func(context.Context, *agentkit.GenerateRequest) (*agentkit.GenerateResult, error) {
				panic("boom")
			}
		}
		k, repo, ag := setupScript(t, step, model, agentkit.WithGenerateMiddleware(mw))
		pid, _ := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
		p := serveUntil(t, k, repo, pid, 10*time.Second, isTerminal, agentkit.WithMaxStepAttempts(1))
		gt.Value(t, p.Status).Equal(agentkit.ProcessFailed)
	})
}

// The generic-observation form: passing any as the type argument always
// succeeds, which is how a middleware that spans every agent logs a payload.
func TestPayloadObservationWithAny(t *testing.T) {
	ctx := context.Background()
	model, _ := mockLLM(textResponse("x"))

	var mu sync.Mutex
	var initOK, stepOK, resultOK bool
	var initVal, stepVal any

	initMW := func(next agentkit.InitHandler) agentkit.InitHandler {
		return func(c context.Context, req *agentkit.InitRequest) (*agentkit.InitResult, error) {
			v, ok := agentkit.InitInput[any](req)
			mu.Lock()
			initVal, initOK = v, ok
			mu.Unlock()
			return next(c, req)
		}
	}
	stepMW := func(next agentkit.StepHandler) agentkit.StepHandler {
		return func(c context.Context, req *agentkit.StepRequest) (*agentkit.StepResult, error) {
			v, ok := agentkit.StepState[any](req)
			res, err := next(c, req)
			if err != nil {
				return nil, err
			}
			_, rok := agentkit.ResultState[any](res)
			mu.Lock()
			stepVal, stepOK, resultOK = v, ok, rok
			mu.Unlock()
			return res, nil
		}
	}

	k, repo, ag := setupScript(t, doneStep(), model,
		agentkit.WithInitMiddleware(initMW), agentkit.WithStepMiddleware(stepMW))
	pid, _ := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	serveUntil(t, k, repo, pid, 3*time.Second, isTerminal)

	mu.Lock()
	defer mu.Unlock()
	gt.Value(t, initOK).Equal(true)
	gt.Value(t, stepOK).Equal(true)
	gt.Value(t, resultOK).Equal(true)
	gt.Value(t, initVal).Equal(scriptInput{Seed: "s"})
	gt.Value(t, stepVal).Equal(scriptState{Seed: "s"})
}

// E23: a middleware asking for a type this agent does not use just gets
// ok == false — the everyday case for a chain that spans agents.
func TestPayloadAccessorForeignType(t *testing.T) {
	ctx := context.Background()
	model, _ := mockLLM(textResponse("x"))

	var mu sync.Mutex
	var ok bool
	var zero struct{ X int }
	mw := func(next agentkit.InitHandler) agentkit.InitHandler {
		return func(c context.Context, req *agentkit.InitRequest) (*agentkit.InitResult, error) {
			v, got := agentkit.InitInput[struct{ X int }](req)
			mu.Lock()
			ok, zero = got, v
			mu.Unlock()
			return next(c, req)
		}
	}
	k, _, ag := setupScript(t, doneStep(), model, agentkit.WithInitMiddleware(mw))
	_, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err) // not an error: the chain just passes through.

	mu.Lock()
	defer mu.Unlock()
	gt.Value(t, ok).Equal(false)
	gt.Value(t, zero).Equal(struct{ X int }{})
}

// The audit-trail shape the deleted Observer used to cover, now on all five
// hooks at once.
func TestMiddlewareAuditTrail(t *testing.T) {
	ctx := context.Background()
	model, _ := mockLLM(textResponse("x"))
	tool, _, _ := countingTool("t")

	var mu sync.Mutex
	var genEC, toolEC, stepEC []agentkit.EffectContext
	var initAgents []agentkit.AgentName

	step := func(c context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		if _, err := sys.Generate(c, []gollem.Input{gollem.Text("go")}); err != nil {
			return st, agentkit.Decision[[]byte]{}, err
		}
		if _, err := sys.CallTool(c, gollem.FunctionCall{ID: "1", Name: "t", Arguments: map[string]any{}}); err != nil {
			return st, agentkit.Decision[[]byte]{}, err
		}
		return st, agentkit.Done([]byte("ok")), nil
	}
	tf := func(_ context.Context, _ *agentkit.Process) ([]gollem.Tool, error) {
		return []gollem.Tool{tool}, nil
	}

	k, repo, ag := setupScript(t, step, model,
		agentkit.WithToolFactory(tf),
		agentkit.WithInitMiddleware(func(next agentkit.InitHandler) agentkit.InitHandler {
			return func(c context.Context, req *agentkit.InitRequest) (*agentkit.InitResult, error) {
				mu.Lock()
				initAgents = append(initAgents, req.Agent)
				mu.Unlock()
				return next(c, req)
			}
		}),
		agentkit.WithStepMiddleware(func(next agentkit.StepHandler) agentkit.StepHandler {
			return func(c context.Context, req *agentkit.StepRequest) (*agentkit.StepResult, error) {
				mu.Lock()
				stepEC = append(stepEC, req.Effect)
				mu.Unlock()
				return next(c, req)
			}
		}),
		agentkit.WithGenerateMiddleware(func(next agentkit.GenerateHandler) agentkit.GenerateHandler {
			return func(c context.Context, req *agentkit.GenerateRequest) (*agentkit.GenerateResult, error) {
				mu.Lock()
				genEC = append(genEC, req.Effect)
				mu.Unlock()
				return next(c, req)
			}
		}),
		agentkit.WithToolCallMiddleware(func(next agentkit.ToolCallHandler) agentkit.ToolCallHandler {
			return func(c context.Context, req *agentkit.ToolCallRequest) (map[string]any, error) {
				mu.Lock()
				toolEC = append(toolEC, req.Effect)
				mu.Unlock()
				return next(c, req)
			}
		}),
	)
	pid, _ := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	p := serveUntil(t, k, repo, pid, 3*time.Second, isTerminal)
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)

	mu.Lock()
	defer mu.Unlock()
	gt.Array(t, initAgents).Length(1)
	gt.Array(t, stepEC).Length(1)
	gt.Array(t, genEC).Length(1)
	gt.Array(t, toolEC).Length(1)
	gt.Value(t, initAgents[0]).Equal(agentkit.AgentName("main"))
	gt.Value(t, genEC[0].ProcessID).Equal(pid)
	gt.Value(t, genEC[0].RootID).Equal(pid)
	gt.Value(t, toolEC[0].Agent).Equal(agentkit.AgentName("main"))
	gt.Value(t, stepEC[0].StateSeq).Equal(1)
}
