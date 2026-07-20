// See examples/quickstart/main_test.go for why these tests live in
// `package main` rather than a black-box `package main_test`.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/gollem-dev/agentkit"
	"github.com/gollem-dev/agentkit/examples/internal/demo"
	"github.com/gollem-dev/agentkit/repository/memory"
	"github.com/m-mizutani/gt"
)

func offline(t *testing.T) {
	t.Helper()
	t.Setenv(demo.ProjectEnv, "")
	t.Setenv(demo.LocationEnv, "")
}

func TestRunActsOnApproval(t *testing.T) {
	offline(t)

	var out bytes.Buffer
	gt.NoError(t, run(context.Background(), &out, "drop the old snapshot", "yes"))

	got := out.String()
	gt.String(t, got).Contains("status:   succeeded")
	gt.String(t, got).Contains("approved: true")
	// The work only happens after the answer arrives.
	gt.String(t, got).Contains("The snapshot has been deleted.")
}

func TestRunStopsOnRefusal(t *testing.T) {
	offline(t)

	var out bytes.Buffer
	gt.NoError(t, run(context.Background(), &out, "drop the old snapshot", "no"))

	got := out.String()
	gt.String(t, got).Contains("approved: false")
	gt.String(t, got).Contains("refused by user:alice")
	// A refusal must not reach the model at all.
	gt.String(t, got).NotContains("The snapshot has been deleted.")
}

func TestRunEmitsTheApprovalRequest(t *testing.T) {
	offline(t)

	var out bytes.Buffer
	gt.NoError(t, run(context.Background(), &out, "drop the old snapshot", "yes"))

	got := out.String()
	gt.String(t, got).Contains("question: Approve: drop the old snapshot (yes/no)")
	gt.String(t, got).Contains(string(approvalRequested))
	gt.String(t, got).Contains(string(agentkit.EventAwaitCreated))
}

// TestExpiredQuestionIsARefusal drives the deadline branch, which the example's
// own 10-minute deadline cannot reach in a test.
func TestExpiredQuestionIsARefusal(t *testing.T) {
	offline(t)

	ctx := context.Background()
	model, _, err := demo.NewLLM(ctx, demo.Turn{Texts: []string{"unused"}})
	gt.NoError(t, err)

	reg := agentkit.NewRegistry()
	approver, err := agentkit.Register(reg, agentName, 1, &strategy{deadline: 50 * time.Millisecond})
	gt.NoError(t, err)

	k, err := agentkit.New(memory.New(), model, reg)
	gt.NoError(t, err)

	pid, err := approver.Spawn(ctx, k, input{Request: "something nobody answers"})
	gt.NoError(t, err)

	serveCtx, stop := context.WithCancel(ctx)
	served := make(chan error, 1)
	go func() {
		served <- k.Serve(serveCtx, agentkit.WithPollInterval(20*time.Millisecond))
	}()
	defer func() { stop(); <-served }()

	// Nobody responds; the deadline passes and the kernel expires the question.
	proc, err := demo.WaitProcess(ctx, k, pid, demo.Terminal, 30*time.Second)
	gt.NoError(t, err)
	gt.Value(t, proc.Status).Equal(agentkit.ProcessSucceeded)

	var res output
	gt.NoError(t, json.Unmarshal(proc.Output, &res))
	gt.False(t, res.Approved)
	gt.String(t, res.Note).Contains("deadline")

	// No LLM call: an unanswered question never reaches the acting branch.
	gt.Number(t, proc.Metrics[agentkit.MetricLLMCalls]).Equal(0)
}

func TestInitRejectsAnEmptyRequest(t *testing.T) {
	s := &strategy{deadline: confirmDeadline}

	_, err := s.Init(input{})
	gt.Error(t, err)

	st, err := s.Init(input{Request: "do the thing"})
	gt.NoError(t, err)
	gt.Value(t, st.Request).Equal("do the thing")
	gt.False(t, st.Asked)
}

func TestStateRoundTrips(t *testing.T) {
	s := &strategy{deadline: confirmDeadline}

	raw, err := s.EncodeState(state{Request: "r", Asked: true, Note: "n"})
	gt.NoError(t, err)

	got, err := s.DecodeState(s.Version(), raw)
	gt.NoError(t, err)
	gt.Value(t, got).Equal(state{Request: "r", Asked: true, Note: "n"})
}

func TestRefusalNote(t *testing.T) {
	gt.Value(t, refusalNote("user:alice")).Equal("refused by user:alice")
	gt.Value(t, refusalNote("")).Equal("refused")
}
