// See examples/quickstart/main_test.go for why these tests live in
// `package main` rather than a black-box `package main_test`.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/gollem-dev/agentkit"
	"github.com/gollem-dev/agentkit/examples/internal/demo"
	"github.com/gollem-dev/agentkit/strategy/planexec"
	"github.com/m-mizutani/gt"
)

func offline(t *testing.T) {
	t.Helper()
	t.Setenv(demo.ProjectEnv, "")
	t.Setenv(demo.LocationEnv, "")
}

func TestRunPlansExecutesAndSummarizes(t *testing.T) {
	offline(t)

	var out bytes.Buffer
	gt.NoError(t, run(context.Background(), &out, "should we adopt this?"))

	got := out.String()
	gt.String(t, got).Contains("status:  succeeded")
	gt.String(t, got).Contains("summary: Durable execution is worth adopting")
	gt.String(t, got).Contains("task 1:  [succeeded] Prior art")
	gt.String(t, got).Contains("task 2:  [succeeded] Trade-offs")
	gt.String(t, got).Contains("task 3:  [succeeded] Risks")
}

// execute drives one fanout run and returns the finished parent Process along
// with the kernel, so a test can inspect the child processes it created.
func execute(t *testing.T, maxLLMCalls int) (*agentkit.Kernel, *agentkit.Process) {
	t.Helper()
	offline(t)
	ctx := context.Background()

	k, researcher, err := newFanout(ctx, io.Discard, maxLLMCalls)
	gt.NoError(t, err)

	pid, err := researcher.Spawn(ctx, k, planexec.Input{Prompt: "should we adopt this?"})
	gt.NoError(t, err)

	serveCtx, stop := context.WithCancel(ctx)
	served := make(chan error, 1)
	go func() {
		served <- k.Serve(serveCtx,
			agentkit.WithPollConcurrency(4),
			agentkit.WithPollInterval(20*time.Millisecond))
	}()

	proc, waitErr := demo.WaitProcess(ctx, k, pid, demo.Terminal, 2*time.Minute)
	stop()
	<-served
	gt.NoError(t, waitErr)

	return k, proc
}

func TestChildrenBelongToTheParentTree(t *testing.T) {
	k, proc := execute(t, defaultMaxLLMCalls)
	gt.Value(t, proc.Status).Equal(agentkit.ProcessSucceeded)

	var out planexec.Output
	gt.NoError(t, json.Unmarshal(proc.Output, &out))
	gt.Array(t, out.Tasks).Length(3)

	for _, task := range out.Tasks {
		gt.Value(t, task.Status).Equal(agentkit.ProcessSucceeded)

		child, err := k.GetProcess(context.Background(), task.ProcessID)
		gt.NoError(t, err)
		gt.Value(t, child.Agent).Equal(taskAgent)
		// Every child names the parent, and the whole tree shares one root id.
		gt.NotNil(t, child.ParentID)
		gt.Value(t, *child.ParentID).Equal(proc.ID)
		gt.Value(t, child.RootID).Equal(proc.RootID)
	}
}

// TestBudgetStopsAProcessAtATransitionBoundary drives the limiter's own
// terminal path: the check that runs before each transition finalizes the
// Process itself. A budget of zero trips at the very first boundary, before any
// call is made.
func TestBudgetStopsAProcessAtATransitionBoundary(t *testing.T) {
	_, proc := execute(t, 0)

	gt.Value(t, proc.Status).Equal(agentkit.ProcessFailed)
	gt.NotNil(t, proc.Failure)
	gt.Value(t, proc.Failure.Code).Equal(agentkit.FailureLimitExceeded)
}

// TestBudgetRefusesToSpawnChildren covers the other path. A budget exhausted
// mid-transition surfaces as ErrLimitExceeded to the strategy, and planexec
// turns a failed spawn into a deterministic strategy_error rather than
// retrying it.
func TestBudgetRefusesToSpawnChildren(t *testing.T) {
	_, proc := execute(t, 1)

	gt.Value(t, proc.Status).Equal(agentkit.ProcessFailed)
	gt.NotNil(t, proc.Failure)
	gt.Value(t, proc.Failure.Code).Equal(agentkit.FailureStrategyError)
	gt.String(t, proc.Failure.Message).Contains("budget exhausted")
}

// TestBudgetIsNeverExceeded is the assertion that matters: the cap is a cap,
// not "a cap plus one". The limiter runs before each call, so a leaf Process
// must never end up having made more calls than its budget allows.
//
// A fan-out parent is the case to read carefully. Its metrics absorb every
// child once the children await resolves, so its recorded count can land above
// the cap -- the subtree really did spend that much. What the cap still
// guarantees there is that the parent stops: it cannot go on to make calls of
// its own once the folded total is over the line.
func TestBudgetIsNeverExceeded(t *testing.T) {
	for _, budget := range []int{0, 1, 2, 3} {
		k, proc := execute(t, budget)

		// Every leaf is bound directly, whether or not the parent survived: a
		// child has no children of its own, so its count is its own spend and the
		// limiter ran before each of those calls.
		for _, child := range childrenOf(t, k, proc) {
			gt.Number(t, child.Metrics.LLMCalls).LessOrEqual(int64(budget))
		}

		if proc.Status != agentkit.ProcessSucceeded {
			// Over budget is how an overspending subtree must end, and it is the
			// limiter that has to end it -- not a strategy error or a crash.
			gt.NotNil(t, proc.Failure)
			// Either the limiter finalized it directly, or planexec surfaced the
			// spawn it refused. Anything else means it died for another reason.
			byLimiter := proc.Failure.Code == agentkit.FailureLimitExceeded ||
				proc.Failure.Code == agentkit.FailureStrategyError
			gt.Value(t, byLimiter).Equal(true)
			continue
		}
		// A parent that still succeeded stayed within the cap for the whole tree.
		gt.Number(t, proc.Metrics.LLMCalls).LessOrEqual(int64(budget))
	}
}

// TestParentMetricsCountEachChildExactlyOnce is the other half: the fold is what
// makes a tree budget expressible with one per-Process Limiter, and it has to be
// an equality -- a fold that ran twice would still satisfy ">=".
func TestParentMetricsCountEachChildExactlyOnce(t *testing.T) {
	k, proc := execute(t, defaultMaxLLMCalls)
	gt.Value(t, proc.Status).Equal(agentkit.ProcessSucceeded)

	children := childrenOf(t, k, proc)
	gt.Array(t, children).Longer(0)

	var childCalls, childIn, childOut int64
	for _, child := range children {
		childCalls += child.Metrics.LLMCalls
		childIn += child.Metrics.InputTokens
		childOut += child.Metrics.OutputTokens
	}
	gt.Number(t, childCalls).Greater(int64(0))

	// The planner's own calls are what remains once the children are subtracted,
	// and it made at least one (it produced a plan).
	ownCalls := proc.Metrics.LLMCalls - childCalls
	gt.Number(t, ownCalls).Greater(int64(0))
	// Tokens have to subtract cleanly by the same accounting, which they cannot
	// do if any child were folded in twice.
	gt.Number(t, proc.Metrics.InputTokens-childIn).Greater(int64(0))
	gt.Number(t, proc.Metrics.OutputTokens-childOut).Greater(int64(0))
}

// childrenOf reads the task children off the parent's children await, which
// works whether or not the parent reached a successful Output.
func childrenOf(t *testing.T, k *agentkit.Kernel, parent *agentkit.Process) []*agentkit.Process {
	t.Helper()
	awaits, err := k.ListAwaits(context.Background(), parent.ID)
	gt.NoError(t, err)

	var out []*agentkit.Process
	seen := map[agentkit.ProcessID]bool{}
	for _, aw := range awaits {
		if aw.Kind != agentkit.AwaitChildren {
			continue
		}
		for _, cid := range aw.Children {
			if seen[cid] {
				continue
			}
			seen[cid] = true
			child, err := k.GetProcess(context.Background(), cid)
			gt.NoError(t, err)
			out = append(out, child)
		}
	}
	return out
}

func TestRunSurfacesAFailedProcess(t *testing.T) {
	offline(t)

	var out bytes.Buffer
	// The budget trips mid-run, so the example reports the failure rather than
	// printing a summary it does not have.
	err := runWith(context.Background(), &out, "should we adopt this?", 0)
	gt.Error(t, err)
	gt.String(t, out.String()).Contains(string(agentkit.FailureLimitExceeded))
}
