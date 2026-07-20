package agentkit_test

import (
	"testing"
	"time"

	"github.com/gollem-dev/agentkit"
	"github.com/m-mizutani/gt"
)

// decOut is the output type used by the decision constructor tests.
type decOut struct {
	Text string `json:"text"`
}

func TestDecisionConstructors(t *testing.T) {
	t.Run("Continue", func(t *testing.T) {
		v := agentkit.ViewDecision(agentkit.Continue[decOut]())
		gt.Value(t, v.Kind).Equal(agentkit.DecisionContinue)
		gt.Nil(t, v.Typed)
	})
	t.Run("Done carries the typed output", func(t *testing.T) {
		v := agentkit.ViewDecision(agentkit.Done(decOut{Text: "result"}))
		gt.Value(t, v.Kind).Equal(agentkit.DecisionDone)
		gt.Value(t, v.Typed).Equal(decOut{Text: "result"})
	})
	t.Run("Fail carries code and message", func(t *testing.T) {
		v := agentkit.ViewDecision(agentkit.Fail[decOut](agentkit.FailureStrategyError, "nope"))
		gt.Value(t, v.Kind).Equal(agentkit.DecisionFail)
		gt.Value(t, v.Failure.Code).Equal(agentkit.FailureStrategyError)
		gt.Value(t, v.Failure.Message).Equal("nope")
		gt.Nil(t, v.Typed)
	})
	t.Run("Suspend collects specs", func(t *testing.T) {
		v := agentkit.ViewDecision(agentkit.Suspend[decOut](agentkit.Timer("t:1", time.Unix(1, 0))))
		gt.Value(t, v.Kind).Equal(agentkit.DecisionSuspend)
		gt.Array(t, v.Awaits).Length(1)
		gt.Nil(t, v.Typed)
	})
	t.Run("Suspend with no specs is legal (already-open case)", func(t *testing.T) {
		v := agentkit.ViewDecision(agentkit.Suspend[decOut]())
		gt.Value(t, v.Kind).Equal(agentkit.DecisionSuspend)
		gt.Array(t, v.Awaits).Length(0)
	})
	t.Run("Done of a zero output is still a Done", func(t *testing.T) {
		v := agentkit.ViewDecision(agentkit.Done(decOut{}))
		gt.Value(t, v.Kind).Equal(agentkit.DecisionDone)
		gt.Value(t, v.Typed).Equal(decOut{})
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
