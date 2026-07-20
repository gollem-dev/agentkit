package demo_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gollem-dev/agentkit"
	"github.com/gollem-dev/agentkit/examples/internal/demo"
	"github.com/gollem-dev/agentkit/repository/memory"
	"github.com/gollem-dev/agentkit/strategy/simple"
	"github.com/gollem-dev/gollem"
	"github.com/m-mizutani/gt"
)

// offline forces stub mode regardless of the developer's environment, so no
// test in this package can reach Vertex AI.
func offline(t *testing.T) {
	t.Helper()
	t.Setenv(demo.ProjectEnv, "")
	t.Setenv(demo.LocationEnv, "")
}

// generate drives one Generate call through a client's session.
func generate(t *testing.T, client gollem.LLMClient) *gollem.Response {
	t.Helper()
	session, err := client.NewSession(context.Background())
	gt.NoError(t, err)
	resp, err := session.Generate(context.Background(), []gollem.Input{gollem.Text("hello")})
	gt.NoError(t, err)
	return resp
}

func TestNewLLMReplaysScriptAndRepeatsTheLastTurn(t *testing.T) {
	offline(t)

	client, live, err := demo.NewLLM(context.Background(),
		demo.Turn{Texts: []string{"first"}},
		demo.Turn{Texts: []string{"second"}},
	)
	gt.NoError(t, err)
	gt.Value(t, live).Equal(false)

	gt.Array(t, generate(t, client).Texts).Equal([]string{"first"})
	gt.Array(t, generate(t, client).Texts).Equal([]string{"second"})
	// The script is exhausted; the last turn repeats rather than panicking.
	gt.Array(t, generate(t, client).Texts).Equal([]string{"second"})
}

func TestNewLLMReportsTokensAndHistory(t *testing.T) {
	offline(t)

	client, _, err := demo.NewLLM(context.Background(), demo.Turn{Texts: []string{"only"}})
	gt.NoError(t, err)

	session, err := client.NewSession(context.Background())
	gt.NoError(t, err)

	resp, err := session.Generate(context.Background(), []gollem.Input{gollem.Text("hello")})
	gt.NoError(t, err)
	gt.Number(t, resp.InputToken).Greater(0)
	gt.Number(t, resp.OutputToken).Greater(0)

	hist, err := session.History()
	gt.NoError(t, err)
	gt.Value(t, hist.LLType).Equal(gollem.LLMTypeGemini)
}

func TestNewLLMRejectsAnEmptyScript(t *testing.T) {
	offline(t)

	_, _, err := demo.NewLLM(context.Background())
	gt.Error(t, err)
	gt.True(t, errors.Is(err, demo.ErrNoTurns))
}

func TestNewLLMRejectsAPartialVertexConfig(t *testing.T) {
	turn := demo.Turn{Texts: []string{"unused"}}

	t.Run("project without location", func(t *testing.T) {
		t.Setenv(demo.ProjectEnv, "my-project")
		t.Setenv(demo.LocationEnv, "")

		_, _, err := demo.NewLLM(context.Background(), turn)
		gt.Error(t, err)
		gt.True(t, errors.Is(err, demo.ErrPartialVertexConfig))
	})

	t.Run("location without project", func(t *testing.T) {
		t.Setenv(demo.ProjectEnv, "")
		t.Setenv(demo.LocationEnv, "us-central1")

		_, _, err := demo.NewLLM(context.Background(), turn)
		gt.Error(t, err)
		gt.True(t, errors.Is(err, demo.ErrPartialVertexConfig))
	})
}

// pendingKernel returns a kernel holding one spawned Process that no worker
// ever claims, so its status stays pending for the whole test.
func pendingKernel(t *testing.T) (*agentkit.Kernel, agentkit.ProcessID) {
	t.Helper()
	offline(t)

	model, _, err := demo.NewLLM(context.Background(), demo.Turn{Texts: []string{"unused"}})
	gt.NoError(t, err)

	reg := agentkit.NewRegistry()
	assistant, err := simple.Register(reg, "assistant", 1)
	gt.NoError(t, err)

	k, err := agentkit.New(memory.New(), model, reg)
	gt.NoError(t, err)

	pid, err := assistant.Spawn(context.Background(), k, simple.Input{Prompt: "hello"})
	gt.NoError(t, err)

	return k, pid
}

func TestWaitProcessReturnsAsSoonAsThePredicateHolds(t *testing.T) {
	k, pid := pendingKernel(t)

	proc, err := demo.WaitProcess(context.Background(), k, pid,
		func(p *agentkit.Process) bool { return p.Status == agentkit.ProcessPending },
		time.Second)
	gt.NoError(t, err)
	gt.Value(t, proc.ID).Equal(pid)
}

func TestWaitProcessTimesOut(t *testing.T) {
	k, pid := pendingKernel(t)

	_, err := demo.WaitProcess(context.Background(), k, pid, demo.Terminal, 100*time.Millisecond)
	gt.Error(t, err)
	gt.True(t, errors.Is(err, demo.ErrWaitTimeout))
}

func TestWaitProcessHonoursContextCancellation(t *testing.T) {
	k, pid := pendingKernel(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := demo.WaitProcess(ctx, k, pid, demo.Terminal, time.Minute)
	gt.Error(t, err)
	gt.True(t, errors.Is(err, context.Canceled))
}

func TestWaitProcessReportsAnUnknownProcess(t *testing.T) {
	k, _ := pendingKernel(t)

	_, err := demo.WaitProcess(context.Background(), k, "no-such-process", demo.Terminal, time.Second)
	gt.Error(t, err)
	gt.True(t, errors.Is(err, agentkit.ErrProcessNotFound))
}

func TestModelLabel(t *testing.T) {
	gt.String(t, demo.ModelLabel(true)).Contains("live")
	gt.String(t, demo.ModelLabel(false)).Contains(demo.ProjectEnv)
	gt.String(t, demo.ModelLabel(false)).Contains(demo.LocationEnv)
}

func TestPredicates(t *testing.T) {
	gt.True(t, demo.Terminal(&agentkit.Process{Status: agentkit.ProcessSucceeded}))
	gt.False(t, demo.Terminal(&agentkit.Process{Status: agentkit.ProcessWaiting}))
	gt.True(t, demo.Waiting(&agentkit.Process{Status: agentkit.ProcessWaiting}))
	gt.False(t, demo.Waiting(&agentkit.Process{Status: agentkit.ProcessRunning}))
}
