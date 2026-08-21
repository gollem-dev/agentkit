package agentkit_test

import (
	"context"
	"testing"
	"time"

	"github.com/gollem-dev/agentkit"
	"github.com/gollem-dev/agentkit/repository/memory"
	"github.com/gollem-dev/gollem"
	"github.com/m-mizutani/gt"
)

func TestLimitDecision(t *testing.T) {
	t.Run("pass carries no message", func(t *testing.T) {
		d := agentkit.LimitPass()
		gt.Value(t, d.Kind()).Equal(agentkit.LimitKindPass)
		gt.Value(t, d.Message()).Equal("")
	})

	t.Run("notice carries its message", func(t *testing.T) {
		d := agentkit.LimitNotice("budget is nearly out")
		gt.Value(t, d.Kind()).Equal(agentkit.LimitKindNotice)
		gt.Value(t, d.Message()).Equal("budget is nearly out")
	})

	t.Run("stop carries its reason", func(t *testing.T) {
		d := agentkit.LimitStop("llm call budget exhausted")
		gt.Value(t, d.Kind()).Equal(agentkit.LimitKindStop)
		gt.Value(t, d.Message()).Equal("llm call budget exhausted")
	})

	// A notice with nothing to say would leave a reader testing both the kind and
	// the message before trusting either.
	t.Run("an empty notice is a pass", func(t *testing.T) {
		gt.Value(t, agentkit.LimitNotice("")).Equal(agentkit.LimitPass())
	})

	// Failure.Message must say something; the Process row is what an operator
	// reads to find out why the run ended.
	t.Run("an empty stop reason is replaced", func(t *testing.T) {
		d := agentkit.LimitStop("")
		gt.Value(t, d.Kind()).Equal(agentkit.LimitKindStop)
		gt.Value(t, d.Message()).Equal("limit exceeded")
	})

	// EffectContext.Limit and Syscalls.LimitStatus() hand out this value before
	// anything has been decided, so it has to read as "carry on" without the
	// callers special-casing it.
	t.Run("the zero value reads as a pass", func(t *testing.T) {
		var d agentkit.LimitDecision
		gt.Value(t, d.Kind()).Equal(agentkit.LimitKindPass)
		gt.Value(t, d.Message()).Equal("")
	})
}

func TestCallLimit(t *testing.T) {
	ctx := context.Background()
	proc := &agentkit.Process{ID: "p1", Agent: "a"}

	t.Run("passes the verdict through untouched", func(t *testing.T) {
		var gotMetrics agentkit.Metrics
		d, err := agentkit.CallLimitForTest(ctx,
			func(_ context.Context, p *agentkit.Process, m agentkit.Metrics) agentkit.LimitDecision {
				gt.Value(t, p.ID).Equal(agentkit.ProcessID("p1"))
				gotMetrics = m
				return agentkit.LimitNotice("nearly out")
			}, proc, agentkit.Metrics{LLMCalls: 3})
		gt.NoError(t, err)
		gt.Value(t, d.Kind()).Equal(agentkit.LimitKindNotice)
		gt.Value(t, d.Message()).Equal("nearly out")
		gt.Value(t, gotMetrics.LLMCalls).Equal(int64(3))
	})

	// The worker calls this outside runTransition's recover, so the panic has to
	// come back as a value rather than unwinding into the Serve goroutine.
	t.Run("a panic becomes an error and no verdict", func(t *testing.T) {
		d, err := agentkit.CallLimitForTest(ctx,
			func(_ context.Context, _ *agentkit.Process, _ agentkit.Metrics) agentkit.LimitDecision {
				panic("budget lookup exploded")
			}, proc, agentkit.Metrics{})
		gt.Error(t, err)
		gt.Value(t, d).Equal(agentkit.LimitDecision{})
		// The panic value is carried as context, not parsed out of the message.
		gt.S(t, err.Error()).Contains("strategy panic")
	})
}

// TestLimitCannotWriteToTheRow pins the reason Limit is handed a copy of the
// Process rather than the row: it is strategy-author code running on the
// transition path, and the row carries state the kernel and the store maintain.
//
// The agent deliberately declares NO semaphore. With one, the store's own
// precondition (Semaphore immutable, SemaphoreHeld monotone) refuses the commit,
// commitFinal re-reads, and every field is restored — so the test would pass
// whether or not Limit got a copy, and would be pinning the store's guard rather
// than this one. Without a semaphore there is no guard, and the assertions below
// fail unless all three Limiter call sites pass a clone.
func TestLimitCannotWriteToTheRow(t *testing.T) {
	ctx := context.Background()
	repo := memory.New()
	reg := agentkit.NewRegistry()

	var limitCalls int
	strat := &scriptStrategy{
		// Two transitions with an effect in the first, which is what reaches all
		// three call sites: the transition-boundary callLimit, the checkLimit before
		// the Generate, and the meter after it. A single terminal transition would
		// miss that a mid-run commit persisted the rewrite.
		step: func(c context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
			if st.N == 0 {
				if _, err := sys.Generate(c, []gollem.Input{gollem.Text("go")}); err != nil {
					return st, agentkit.Decision[[]byte]{}, err
				}
				st.N = 1
				return st, agentkit.Continue[[]byte](), nil
			}
			return st, agentkit.Done([]byte(`"ok"`)), nil
		},
		limit: func(_ context.Context, proc *agentkit.Process, _ agentkit.Metrics) agentkit.LimitDecision {
			limitCalls++
			// Reach for everything a Limiter could try to rewrite.
			proc.Semaphore = &agentkit.ProcessSemaphore{Key: "smuggled", Value: "v", Slots: 99}
			proc.SemaphoreHeld = true
			if proc.Metadata != nil {
				proc.Metadata["tenant"] = "rewritten"
			}
			return agentkit.LimitPass()
		},
	}
	ag, err := agentkit.Register(reg, "a", 1, strat)
	gt.NoError(t, err)

	model, _ := mockLLM(textResponse("x"))
	k, err := agentkit.New(repo, model, reg)
	gt.NoError(t, err)

	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "first"},
		agentkit.WithMetadata(map[string]string{"tenant": "acme"}))
	gt.NoError(t, err)

	final := serveUntil(t, k, repo, pid, 5*time.Second, isTerminal)
	if final.Status != agentkit.ProcessSucceeded {
		t.Fatalf("status=%s failure=%+v", final.Status, final.Failure)
	}

	// The verdict was consulted, so the writes really did happen — on a copy.
	gt.Bool(t, limitCalls > 0).True()
	gt.Nil(t, final.Semaphore)
	gt.Bool(t, final.SemaphoreHeld).False()
	gt.Value(t, final.Metadata["tenant"]).Equal("acme")
}

// The same writes on an agent that DOES carry a semaphore. Here two mechanisms
// overlap — the copy and the store's precondition — and the point is that the run
// finishes normally rather than looping: a rewrite reaching the commit would
// conflict on every attempt, and driveClaim rebuilds from the same mutated row.
func TestLimitCannotWriteToTheRowUnderSemaphore(t *testing.T) {
	ctx := context.Background()
	repo := memory.New()
	reg := agentkit.NewRegistry()

	strat := &scriptStrategy{
		step: func(c context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
			if st.N == 0 {
				if _, err := sys.Generate(c, []gollem.Input{gollem.Text("go")}); err != nil {
					return st, agentkit.Decision[[]byte]{}, err
				}
				st.N = 1
				return st, agentkit.Continue[[]byte](), nil
			}
			return st, agentkit.Done([]byte(`"ok"`)), nil
		},
		limit: func(_ context.Context, proc *agentkit.Process, _ agentkit.Metrics) agentkit.LimitDecision {
			proc.SemaphoreHeld = false // would hand this Process a free run.
			proc.Semaphore = nil
			return agentkit.LimitPass()
		},
	}
	ag, err := agentkit.Register(reg, "a", 1, strat, agentkit.WithSemaphoreKey[[]byte]("channel", 1))
	gt.NoError(t, err)

	model, _ := mockLLM(textResponse("x"))
	k, err := agentkit.New(repo, model, reg)
	gt.NoError(t, err)

	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "first"}, agentkit.WithSemaphore("channel", "C1"))
	gt.NoError(t, err)

	final := serveUntil(t, k, repo, pid, 5*time.Second, isTerminal)
	gt.Value(t, final.Status).Equal(agentkit.ProcessSucceeded)
	gt.NotNil(t, final.Semaphore)
	gt.Value(t, *final.Semaphore).Equal(agentkit.ProcessSemaphore{Key: "channel", Value: "C1", Slots: 1})
	gt.Bool(t, final.SemaphoreHeld).True()
}
