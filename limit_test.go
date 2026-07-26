package agentkit_test

import (
	"context"
	"testing"

	"github.com/gollem-dev/agentkit"
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
