package agentkit_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/gollem-dev/agentkit"
	"github.com/gollem-dev/gollem"
	"github.com/gollem-dev/gollem/mock"
	"github.com/m-mizutani/gt"
)

// --- helpers ----------------------------------------------------------------

// configBox records the SessionConfig every NewSession was built with, so a
// test can assert what actually reached gollem rather than what agentkit
// intended to send.
type configBox struct {
	mu   sync.Mutex
	cfgs []gollem.SessionConfig
}

func (b *configBox) add(c gollem.SessionConfig) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cfgs = append(b.cfgs, c)
}

// last returns the most recent config. A transition may be replayed, so tests
// assert on the latest rather than assuming exactly one call.
func (b *configBox) last(t *testing.T) gollem.SessionConfig {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.cfgs) == 0 {
		t.Fatal("NewSession was never called")
	}
	return b.cfgs[len(b.cfgs)-1]
}

func capturingLLM() (gollem.LLMClient, *configBox) {
	box := &configBox{}
	hist := &gollem.History{LLType: gollem.LLMTypeClaude, Version: gollem.HistoryVersion}
	client := &mock.LLMClientMock{
		NewSessionFunc: func(_ context.Context, opts ...gollem.SessionOption) (gollem.Session, error) {
			box.add(gollem.NewSessionConfig(opts...))
			return &mock.SessionMock{
				GenerateFunc: func(_ context.Context, _ []gollem.Input, _ ...gollem.GenerateOption) (*gollem.Response, error) {
					return textResponse("ok"), nil
				},
				HistoryFunc: func() (*gollem.History, error) { return hist, nil },
			}, nil
		},
	}
	return client, box
}

// genStep runs exactly one Generate with the given options, then finishes.
func genStep(opts ...agentkit.GenerateOption) stepFn {
	return func(c context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		res, err := sys.Generate(c, []gollem.Input{gollem.Text(st.Seed)}, opts...)
		if err != nil {
			return st, agentkit.Decision[[]byte]{}, err
		}
		return st, agentkit.Done([]byte(res.Texts[0])), nil
	}
}

// runGenStep spawns one Process running genStep(opts...) and waits for it to
// finish, returning the configs NewSession saw.
func runGenStep(t *testing.T, step stepFn, kopts ...agentkit.KernelOption) *configBox {
	t.Helper()
	model, box := capturingLLM()
	k, repo, ag := setupScript(t, step, model, kopts...)
	pid, err := ag.Spawn(context.Background(), k, scriptInput{Seed: "hello"})
	gt.NoError(t, err)
	p := serveUntil(t, k, repo, pid, 3*time.Second, isTerminal)
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)
	return box
}

// --- WithLLMSessionOptions --------------------------------------------------

func TestLLMSessionOptionsReachNewSession(t *testing.T) {
	box := runGenStep(t, genStep(agentkit.WithLLMSessionOptions(gollem.WithSessionPromptCache(true))))
	cfg := box.last(t)
	gt.Bool(t, cfg.PromptCache()).True()
}

func TestLLMSessionOptionsFromGenerateMiddleware(t *testing.T) {
	// The point of the field: one Kernel registration turns a session setting on
	// for every agent, whatever the strategy asked for.
	mw := func(next agentkit.GenerateHandler) agentkit.GenerateHandler {
		return func(c context.Context, req *agentkit.GenerateRequest) (*agentkit.GenerateResult, error) {
			req.LLMSessionOptions = append(req.LLMSessionOptions, gollem.WithSessionPromptCache(true))
			return next(c, req)
		}
	}
	box := runGenStep(t, genStep(), agentkit.WithGenerateMiddleware(mw))
	cfg := box.last(t)
	gt.Bool(t, cfg.PromptCache()).True()
}

func TestLLMSessionOptionsAbsentOrEmptyChangeNothing(t *testing.T) {
	t.Run("never called", func(t *testing.T) {
		box := runGenStep(t, genStep(agentkit.WithSystemPrompt("typed")))
		cfg := box.last(t)
		gt.Value(t, cfg.SystemPrompt()).Equal("typed")
		gt.Bool(t, cfg.PromptCache()).False()
	})

	t.Run("called with no options", func(t *testing.T) {
		box := runGenStep(t, genStep(
			agentkit.WithSystemPrompt("typed"),
			agentkit.WithLLMSessionOptions(),
		))
		cfg := box.last(t)
		gt.Value(t, cfg.SystemPrompt()).Equal("typed")
		gt.Bool(t, cfg.PromptCache()).False()
	})
}

func TestLLMSessionOptionsScalarOverridesTypedField(t *testing.T) {
	// Appended last, and gollem's scalar setters assign, so the pass-through wins.
	box := runGenStep(t, genStep(
		agentkit.WithSystemPrompt("from typed field"),
		agentkit.WithLLMSessionOptions(gollem.WithSessionSystemPrompt("from pass-through")),
	))
	cfg := box.last(t)
	gt.Value(t, cfg.SystemPrompt()).Equal("from pass-through")
}

func TestLLMSessionOptionsSliceAddsToTypedField(t *testing.T) {
	// gollem's slice setters append, so tools accumulate instead of replacing.
	box := runGenStep(t, genStep(
		agentkit.WithTools(mockTool("typed", nil)),
		agentkit.WithLLMSessionOptions(gollem.WithSessionTools(mockTool("passthrough", nil))),
	))
	cfg := box.last(t)
	names := make([]string, 0, len(cfg.Tools()))
	for _, tool := range cfg.Tools() {
		names = append(names, tool.Spec().Name)
	}
	gt.Value(t, names).Equal([]string{"typed", "passthrough"})
}

// --- Syscalls.Metadata ------------------------------------------------------

func TestSyscallsMetadataReadsSpawnMetadata(t *testing.T) {
	var seen map[string]string
	step := func(_ context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		seen = sys.Metadata()
		return st, agentkit.Done([]byte("done")), nil
	}
	model, _ := mockLLM(textResponse("x"))
	k, repo, ag := setupScript(t, step, model)
	pid, err := ag.Spawn(context.Background(), k, scriptInput{Seed: "s"},
		agentkit.WithMetadata(map[string]string{"tenant": "acme"}))
	gt.NoError(t, err)

	p := serveUntil(t, k, repo, pid, 3*time.Second, isTerminal)
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)
	gt.Value(t, seen).Equal(map[string]string{"tenant": "acme"})
}

func TestSyscallsMetadataNilWhenUnset(t *testing.T) {
	var seen map[string]string
	var called bool
	step := func(_ context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		seen, called = sys.Metadata(), true
		return st, agentkit.Done([]byte("done")), nil
	}
	model, _ := mockLLM(textResponse("x"))
	k, repo, ag := setupScript(t, step, model)
	pid, err := ag.Spawn(context.Background(), k, scriptInput{Seed: "s"})
	gt.NoError(t, err)

	serveUntil(t, k, repo, pid, 3*time.Second, isTerminal)
	gt.Bool(t, called).True()
	gt.Bool(t, seen == nil).True() // nil, not an empty map.
}

func TestSyscallsMetadataIsACopy(t *testing.T) {
	// A strategy writing to what it got back must reach neither the next read
	// nor the effect context nor the committed row.
	var second, fromEffect map[string]string
	step := func(c context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		first := sys.Metadata()
		first["tenant"] = "evil"
		first["injected"] = "yes"
		second = sys.Metadata()
		if _, err := sys.Generate(c, []gollem.Input{gollem.Text("go")}); err != nil {
			return st, agentkit.Decision[[]byte]{}, err
		}
		return st, agentkit.Done([]byte("done")), nil
	}
	mw := func(next agentkit.GenerateHandler) agentkit.GenerateHandler {
		return func(c context.Context, req *agentkit.GenerateRequest) (*agentkit.GenerateResult, error) {
			fromEffect = req.Effect.Metadata
			return next(c, req)
		}
	}
	model, _ := mockLLM(textResponse("x"))
	k, repo, ag := setupScript(t, step, model, agentkit.WithGenerateMiddleware(mw))
	pid, err := ag.Spawn(context.Background(), k, scriptInput{Seed: "s"},
		agentkit.WithMetadata(map[string]string{"tenant": "acme"}))
	gt.NoError(t, err)

	serveUntil(t, k, repo, pid, 3*time.Second, isTerminal)
	gt.Value(t, second).Equal(map[string]string{"tenant": "acme"})
	gt.Value(t, fromEffect).Equal(map[string]string{"tenant": "acme"})

	// Re-read from storage rather than trusting the copy serveUntil returned.
	stored, err := repo.GetProcess(context.Background(), pid)
	gt.NoError(t, err)
	gt.Value(t, stored.Metadata).Equal(map[string]string{"tenant": "acme"})
}
