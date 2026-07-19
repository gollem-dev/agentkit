package simple_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/gollem-dev/agentkit"
	"github.com/gollem-dev/agentkit/repository/memory"
	"github.com/gollem-dev/agentkit/strategy/simple"
	"github.com/gollem-dev/gollem"
	"github.com/gollem-dev/gollem/mock"
	"github.com/m-mizutani/gt"
)

// mockLLM returns an LLMClient whose sessions yield the given responses in order
// (cycling on the last one). callCount tracks how many Generate calls happened.
func mockLLM(responses ...*gollem.Response) (gollem.LLMClient, *int) {
	var mu sync.Mutex
	count := 0
	idx := 0
	hist := &gollem.History{LLType: gollem.LLMTypeClaude, Version: gollem.HistoryVersion}
	client := &mock.LLMClientMock{
		NewSessionFunc: func(_ context.Context, _ ...gollem.SessionOption) (gollem.Session, error) {
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

// serveUntil runs the kernel in a background goroutine and polls until want is
// satisfied (or a timeout), then stops the worker and returns the final Process.
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

	deadline := time.Now().Add(3 * time.Second)
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

// TestSimpleE2E exercises the full generate -> tool -> generate -> done loop.
func TestSimpleE2E(t *testing.T) {
	ctx := context.Background()

	// (1) return a tool call, then (2) return the final text answer.
	toolCall := &gollem.Response{
		FunctionCalls: []*gollem.FunctionCall{{ID: "call-1", Name: "echo", Arguments: map[string]any{}}},
		InputToken:    3, OutputToken: 4,
	}
	final := &gollem.Response{Texts: []string{"the final answer"}, InputToken: 5, OutputToken: 6}
	model, llmCount := mockLLM(toolCall, final)

	var toolCalls int
	var tmu sync.Mutex
	tool := &mock.ToolMock{
		SpecFunc: func() gollem.ToolSpec { return gollem.ToolSpec{Name: "echo"} },
		RunFunc: func(_ context.Context, _ map[string]any) (map[string]any, error) {
			tmu.Lock()
			toolCalls++
			tmu.Unlock()
			return map[string]any{"ok": true}, nil
		},
	}
	tf := func(_ context.Context, _ *agentkit.Process) ([]gollem.Tool, error) {
		return []gollem.Tool{tool}, nil
	}

	repo := memory.New()
	reg := agentkit.NewRegistry()
	assistant, err := simple.Register(reg, "assistant", 1)
	gt.NoError(t, err)
	k, err := agentkit.New(repo, model, reg, agentkit.WithToolFactory(tf))
	gt.NoError(t, err)

	pid, err := assistant.Spawn(ctx, k, simple.Input{Prompt: "hello"})
	gt.NoError(t, err)

	p := serveUntil(t, k, repo, pid, isTerminal)
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)

	var out simple.Output
	gt.NoError(t, json.Unmarshal(p.Output, &out))
	gt.Array(t, out.Texts).Equal([]string{"the final answer"})

	tmu.Lock()
	gt.Number(t, toolCalls).Equal(1)
	tmu.Unlock()
	gt.Number(t, *llmCount).GreaterOrEqual(2)

	gt.Number(t, p.Metrics[agentkit.MetricLLMCalls]).GreaterOrEqual(int64(2))
	gt.Number(t, p.Metrics[agentkit.MetricToolCalls]).GreaterOrEqual(int64(1))
}

// TestSimpleEmptyPromptInitError verifies Init's empty-prompt rejection surfaces
// synchronously from Spawn and creates no Process.
func TestSimpleEmptyPromptInitError(t *testing.T) {
	ctx := context.Background()
	model, _ := mockLLM(&gollem.Response{Texts: []string{"x"}})
	repo := memory.New()
	reg := agentkit.NewRegistry()
	assistant, err := simple.Register(reg, "assistant", 1)
	gt.NoError(t, err)
	k, err := agentkit.New(repo, model, reg)
	gt.NoError(t, err)

	pid, err := assistant.Spawn(ctx, k, simple.Input{Prompt: ""})
	gt.Error(t, err)
	gt.Value(t, pid).Equal(agentkit.ProcessID(""))

	// No Process was created.
	_, gerr := repo.GetProcess(ctx, pid)
	gt.Error(t, gerr)
}

// The version passed to simple.Register must reach Process.StateVersion (so
// DecodeState receives the right schema version), matching planexec.
func TestSimpleRegisterVersionPropagates(t *testing.T) {
	ctx := context.Background()
	repo := memory.New()
	reg := agentkit.NewRegistry()
	ag, err := simple.Register(reg, "assistant", 3)
	gt.NoError(t, err)
	model, _ := mockLLM(&gollem.Response{Texts: []string{"hi"}})
	k, err := agentkit.New(repo, model, reg)
	gt.NoError(t, err)
	pid, err := ag.Spawn(ctx, k, simple.Input{Prompt: "go"})
	gt.NoError(t, err)
	p, err := repo.GetProcess(ctx, pid)
	gt.NoError(t, err)
	gt.Value(t, p.StateVersion).Equal(3)
}
