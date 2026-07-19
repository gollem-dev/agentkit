package agentkit_test

import (
	"testing"
	"time"

	"github.com/gollem-dev/agentkit"
	"github.com/m-mizutani/gt"
)

func TestProcessStatusTerminal(t *testing.T) {
	cases := map[agentkit.ProcessStatus]bool{
		agentkit.ProcessPending:   false,
		agentkit.ProcessRunning:   false,
		agentkit.ProcessWaiting:   false,
		agentkit.ProcessSucceeded: true,
		agentkit.ProcessFailed:    true,
		agentkit.ProcessCancelled: true,
	}
	for status, want := range cases {
		gt.Value(t, status.Terminal()).Equal(want)
	}
}

func TestProcessClone(t *testing.T) {
	t.Run("deep copy isolates mutation", func(t *testing.T) {
		parent := agentkit.ProcessID("p-parent")
		wake := time.Unix(100, 0)
		orig := &agentkit.Process{
			ID:       agentkit.ProcessID("p-1"),
			Metadata: map[string]string{"tenant": "acme"},
			State:    []byte(`{"round":1}`),
			Output:   []byte("out"),
			Failure:  &agentkit.Failure{Code: agentkit.FailureStrategyError, Message: "boom"},
			Subject:  &agentkit.SubjectRef{Kind: "case", ID: "42"},
			Metrics:  agentkit.Metrics{agentkit.MetricLLMCalls: 1},
			ParentID: &parent,
			WakeAt:   &wake,
		}
		cp := agentkit.CloneProcess(orig)

		// mutate the clone's nested values
		cp.Metadata["tenant"] = "other"
		cp.State[0] = 'X'
		cp.Metrics[agentkit.MetricLLMCalls] = 99
		cp.Failure.Message = "changed"
		*cp.ParentID = agentkit.ProcessID("p-other")

		// original is untouched
		gt.Value(t, orig.Metadata["tenant"]).Equal("acme")
		gt.Value(t, string(orig.State)).Equal(`{"round":1}`)
		gt.Value(t, orig.Metrics[agentkit.MetricLLMCalls]).Equal(int64(1))
		gt.Value(t, orig.Failure.Message).Equal("boom")
		gt.Value(t, *orig.ParentID).Equal(parent)
	})

	t.Run("nil clones to nil", func(t *testing.T) {
		gt.Nil(t, agentkit.CloneProcess(nil))
	})
}
