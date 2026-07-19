package agentkit_test

import (
	"testing"
	"time"

	"github.com/gollem-dev/agentkit"
	"github.com/m-mizutani/gt"
)

func TestDecisionConstructors(t *testing.T) {
	t.Run("Continue", func(t *testing.T) {
		d := agentkit.Continue()
		gt.Value(t, d.Kind).Equal(agentkit.DecisionContinue)
	})
	t.Run("Done carries output", func(t *testing.T) {
		d := agentkit.Done([]byte("result"))
		gt.Value(t, d.Kind).Equal(agentkit.DecisionDone)
		gt.Value(t, string(d.Output)).Equal("result")
	})
	t.Run("Fail carries code and message", func(t *testing.T) {
		d := agentkit.Fail(agentkit.FailureStrategyError, "nope")
		gt.Value(t, d.Kind).Equal(agentkit.DecisionFail)
		gt.Value(t, d.Failure.Code).Equal(agentkit.FailureStrategyError)
		gt.Value(t, d.Failure.Message).Equal("nope")
	})
	t.Run("Suspend collects specs", func(t *testing.T) {
		d := agentkit.Suspend(agentkit.Timer("t:1", time.Unix(1, 0)))
		gt.Value(t, d.Kind).Equal(agentkit.DecisionSuspend)
		gt.Array(t, d.Awaits).Length(1)
	})
	t.Run("Suspend with no specs is legal (already-open case)", func(t *testing.T) {
		d := agentkit.Suspend()
		gt.Value(t, d.Kind).Equal(agentkit.DecisionSuspend)
		gt.Array(t, d.Awaits).Length(0)
	})
}

func TestAwaitSpecConstructors(t *testing.T) {
	t.Run("Question with deadline", func(t *testing.T) {
		dl := time.Unix(9999, 0)
		v := agentkit.ViewAwaitSpec(agentkit.Question("q:1", []byte("ask"), agentkit.WithDeadline(dl)))
		gt.Value(t, v.Key).Equal(agentkit.AwaitKey("q:1"))
		gt.Value(t, v.Kind).Equal(agentkit.AwaitQuestion)
		gt.Value(t, string(v.Payload)).Equal("ask")
		gt.Value(t, v.Deadline.Equal(dl)).Equal(true)
	})
	t.Run("Question without deadline has nil deadline", func(t *testing.T) {
		v := agentkit.ViewAwaitSpec(agentkit.Question("q:2", []byte("ask")))
		gt.Nil(t, v.Deadline)
	})
	t.Run("Timer sets deadline", func(t *testing.T) {
		until := time.Unix(42, 0)
		v := agentkit.ViewAwaitSpec(agentkit.Timer("t:1", until))
		gt.Value(t, v.Kind).Equal(agentkit.AwaitTimer)
		gt.Value(t, v.Deadline.Equal(until)).Equal(true)
	})
	t.Run("WaitChildren collects children", func(t *testing.T) {
		v := agentkit.ViewAwaitSpec(agentkit.WaitChildren("w:1", "c-1", "c-2"))
		gt.Value(t, v.Kind).Equal(agentkit.AwaitChildren)
		gt.Array(t, v.Children).Length(2).Has("c-1").Has("c-2")
	})
}
