package planexec_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/gollem-dev/agentkit"
	"github.com/gollem-dev/agentkit/repository/memory"
	"github.com/gollem-dev/agentkit/strategy/planexec"
	"github.com/gollem-dev/agentkit/strategy/simple"
	"github.com/gollem-dev/gollem"
	"github.com/gollem-dev/gollem/mock"
	"github.com/m-mizutani/gt"
)

// mockLLM returns an LLMClient whose sessions yield the given responses in order
// (cycling on the last one). Distinct clients are bound per role so the planner,
// task children, and summarizer are scripted independently.
func mockLLM(responses ...*gollem.Response) gollem.LLMClient {
	var mu sync.Mutex
	idx := 0
	hist := &gollem.History{LLType: gollem.LLMTypeClaude, Version: gollem.HistoryVersion}
	return &mock.LLMClientMock{
		NewSessionFunc: func(_ context.Context, _ ...gollem.SessionOption) (gollem.Session, error) {
			return &mock.SessionMock{
				GenerateFunc: func(_ context.Context, _ []gollem.Input, _ ...gollem.GenerateOption) (*gollem.Response, error) {
					mu.Lock()
					defer mu.Unlock()
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
}

func jsonResponse(s string) *gollem.Response {
	return &gollem.Response{Texts: []string{s}, InputToken: 2, OutputToken: 3}
}

func serveUntil(t *testing.T, k *agentkit.Kernel, repo agentkit.Repository, pid agentkit.ProcessID, want func(*agentkit.Process) bool, extra ...agentkit.ServeOption) *agentkit.Process {
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

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if p, err := repo.GetProcess(context.Background(), pid); err == nil && want(p) {
			return p
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
	return nil
}

func isTerminal(p *agentkit.Process) bool { return p.Status.Terminal() }

// TestPlanExecE2E (UC4): planner -> 2 parallel task children -> replan finalize
// -> summarizer. The task agent is a plain simple.Agent[simple.Input] adapted
// through makeInput, proving the generic-T wiring.
func TestPlanExecE2E(t *testing.T) {
	ctx := context.Background()

	// Planner (bound to RolePlanner): call 1 = plan with 2 tasks, call 2 = finalize.
	planner := mockLLM(
		jsonResponse(`{"tasks":[{"title":"Research","prompt":"research the topic"},{"title":"Draft","prompt":"draft the report"}]}`),
		jsonResponse(`{"action":"finalize"}`),
	)
	// Summarizer (bound to RoleSummarizer): the final answer.
	summarizer := mockLLM(&gollem.Response{Texts: []string{"combined summary"}, InputToken: 1, OutputToken: 1})
	// Default model drives the task children (simple strategy, no tool calls).
	def := mockLLM(&gollem.Response{Texts: []string{"task complete"}, InputToken: 1, OutputToken: 1})

	repo := memory.New()
	reg := agentkit.NewRegistry()

	task, err := simple.Register(reg, "task", 1)
	gt.NoError(t, err)

	makeInput := func(spec planexec.TaskSpec) (simple.Input, error) {
		return simple.Input{Prompt: spec.Prompt}, nil
	}
	plan, err := planexec.Register(reg, "planner", 1, task, makeInput)
	gt.NoError(t, err)

	k, err := agentkit.New(repo, def, reg,
		agentkit.WithModelRole(planexec.RolePlanner, planner),
		agentkit.WithModelRole(planexec.RoleSummarizer, summarizer),
	)
	gt.NoError(t, err)

	pid, err := plan.Spawn(ctx, k, planexec.Input{Prompt: "write a report"})
	gt.NoError(t, err)

	p := serveUntil(t, k, repo, pid, isTerminal, agentkit.WithConcurrency(4))
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)

	var out planexec.Output
	gt.NoError(t, json.Unmarshal(p.Output, &out))

	gt.Array(t, out.Tasks).Length(2)
	titles := []string{out.Tasks[0].Title, out.Tasks[1].Title}
	gt.Array(t, titles).Has("Research").Has("Draft")
	for _, tr := range out.Tasks {
		gt.Value(t, tr.Status).Equal(agentkit.ProcessSucceeded)
		gt.Value(t, tr.ProcessID).NotEqual(agentkit.ProcessID(""))
		gt.Number(t, len(tr.Output)).Greater(0)
	}
	gt.Number(t, len(out.Summary)).Greater(0)
	gt.Value(t, out.Summary[0]).Equal("combined summary")
}

// TestRegisterValidation verifies the required-argument guard.
func TestRegisterValidation(t *testing.T) {
	reg := agentkit.NewRegistry()
	task, err := simple.Register(reg, "task", 1)
	gt.NoError(t, err)

	// nil makeInput -> ErrInvalidAgentDef.
	_, err = planexec.Register(reg, "p1", 1, task, nil)
	gt.Error(t, err)

	// zero taskAgent -> ErrInvalidAgentDef.
	_, err = planexec.Register(reg, "p2", 1, agentkit.Agent[simple.Input]{},
		func(planexec.TaskSpec) (simple.Input, error) { return simple.Input{}, nil })
	gt.Error(t, err)
}

// TestPlanExecEmptyPromptInitError verifies Init rejects an empty prompt.
func TestPlanExecEmptyPromptInitError(t *testing.T) {
	ctx := context.Background()
	def := mockLLM(&gollem.Response{Texts: []string{"x"}})
	repo := memory.New()
	reg := agentkit.NewRegistry()
	task, err := simple.Register(reg, "task", 1)
	gt.NoError(t, err)
	plan, err := planexec.Register(reg, "planner", 1, task,
		func(spec planexec.TaskSpec) (simple.Input, error) { return simple.Input{Prompt: spec.Prompt}, nil })
	gt.NoError(t, err)
	k, err := agentkit.New(repo, def, reg)
	gt.NoError(t, err)

	pid, err := plan.Spawn(ctx, k, planexec.Input{Prompt: ""})
	gt.Error(t, err)
	gt.Value(t, pid).Equal(agentkit.ProcessID(""))
}
