package agentkit_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/gollem-dev/agentkit"
	"github.com/gollem-dev/gollem"
	"github.com/m-mizutani/gt"
)

func TestObserverHooks(t *testing.T) {
	ctx := context.Background()
	model, _ := mockLLM(textResponse("x"))
	var mu sync.Mutex
	var genEC, toolEC []agentkit.EffectContext
	obs := agentkit.Observer{
		Generate: func(_ context.Context, ec agentkit.EffectContext, _ []gollem.Input, _ agentkit.ModelRole) func(*agentkit.GenerateResult, error) {
			mu.Lock()
			genEC = append(genEC, ec)
			mu.Unlock()
			return func(*agentkit.GenerateResult, error) {}
		},
		ToolCall: func(_ context.Context, ec agentkit.EffectContext, _ gollem.FunctionCall) func(map[string]any, error) {
			mu.Lock()
			toolEC = append(toolEC, ec)
			mu.Unlock()
			return func(map[string]any, error) {}
		},
	}
	tf := func(_ context.Context, _ *agentkit.Process) ([]gollem.Tool, error) {
		return []gollem.Tool{mockTool("t", map[string]any{"ok": true})}, nil
	}
	step := func(c context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision, error) {
		if _, err := sys.Generate(c, []gollem.Input{gollem.Text("go")}); err != nil {
			return st, agentkit.Decision{}, err
		}
		if _, err := sys.CallTool(c, gollem.FunctionCall{ID: "1", Name: "t", Arguments: map[string]any{}}); err != nil {
			return st, agentkit.Decision{}, err
		}
		return st, agentkit.Done([]byte("ok")), nil
	}
	k, repo, ag := setupScript(t, step, model, agentkit.WithObserver(obs), agentkit.WithToolFactory(tf))
	pid, _ := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	p := serveUntil(t, k, repo, pid, 3*time.Second, isTerminal)
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)

	mu.Lock()
	defer mu.Unlock()
	gt.Array(t, genEC).Length(1)
	gt.Array(t, toolEC).Length(1)
	gt.Value(t, genEC[0].ProcessID).Equal(pid)
	gt.Value(t, toolEC[0].Agent).Equal(agentkit.AgentName("main"))
}
