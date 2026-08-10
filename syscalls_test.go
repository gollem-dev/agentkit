package agentkit_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/gollem-dev/agentkit"
	"github.com/gollem-dev/agentkit/repository/memory"
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

// --- child Metadata inheritance and event cursor reads ----------------------

// idBox carries the child's ProcessID out of the parent's step, which is the
// only place it is named — there is no API to list a Process's children.
type idBox struct {
	mu sync.Mutex
	id agentkit.ProcessID
}

func (b *idBox) set(id agentkit.ProcessID) { b.mu.Lock(); b.id = id; b.mu.Unlock() }
func (b *idBox) get() agentkit.ProcessID {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.id
}

// setupMetadataParentChild registers a child that finishes immediately and a
// parent that spawns exactly one child with the given options, then waits for
// it. childOpts is what the assertions are about.
func setupMetadataParentChild(t *testing.T, childOpts []agentkit.SpawnOption, kopts ...agentkit.KernelOption) (*agentkit.Kernel, agentkit.Repository, agentkit.Agent[scriptInput], *idBox) {
	t.Helper()
	repo := memory.New()
	reg := agentkit.NewRegistry()
	child, err := agentkit.Register(reg, "child", 1, &scriptStrategy{
		step: func(_ context.Context, _ agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
			return st, agentkit.Done([]byte(st.Seed)), nil
		},
	})
	gt.NoError(t, err)

	box := &idBox{}
	parentStep := func(c context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		if st.N == 0 {
			id, e := child.SpawnChild(c, sys, scriptInput{Seed: "kid"}, childOpts...)
			if e != nil {
				return st, agentkit.Decision[[]byte]{}, e
			}
			box.set(id)
			st.N = 1
			return st, agentkit.Suspend[[]byte](agentkit.WaitChildren("kids", id)), nil
		}
		return st, agentkit.Done([]byte("done")), nil
	}
	parent, err := agentkit.Register(reg, "parent", 1, &scriptStrategy{step: parentStep})
	gt.NoError(t, err)

	model, _ := mockLLM(textResponse("x"))
	k, err := agentkit.New(repo, model, reg, kopts...)
	gt.NoError(t, err)
	return k, repo, parent, box
}

// runToChild drives the parent to completion and returns the one child Process.
func runToChild(t *testing.T, k *agentkit.Kernel, repo agentkit.Repository, pid agentkit.ProcessID, box *idBox) *agentkit.Process {
	t.Helper()
	serveUntil(t, k, repo, pid, 5*time.Second, func(p *agentkit.Process) bool {
		return p.Status == agentkit.ProcessSucceeded
	})
	cid := box.get()
	gt.Value(t, cid).NotEqual(agentkit.ProcessID(""))
	child, err := k.GetProcess(context.Background(), cid)
	gt.NoError(t, err)
	return child
}

// A ToolFactory keying off metadata["tenant"] has to behave the same in a child
// as in its parent, so an unspecified child map inherits rather than starting
// empty.
func TestSpawnChildInheritsParentMetadata(t *testing.T) {
	ctx := context.Background()
	k, repo, parent, box := setupMetadataParentChild(t, nil)

	pid, err := parent.Spawn(ctx, k, scriptInput{Seed: "s"},
		agentkit.WithMetadata(map[string]string{"tenant": "acme", "workspace": "T123"}))
	gt.NoError(t, err)

	child := runToChild(t, k, repo, pid, box)
	gt.Value(t, child.Metadata["tenant"]).Equal("acme")
	gt.Value(t, child.Metadata["workspace"]).Equal("T123")
}

// WithMetadata replaces rather than merges: a caller that names a map gets
// exactly that map, which is what makes dropping a parent key possible.
func TestSpawnChildMetadataReplacesRatherThanMerges(t *testing.T) {
	ctx := context.Background()
	k, repo, parent, box := setupMetadataParentChild(t,
		[]agentkit.SpawnOption{agentkit.WithMetadata(map[string]string{"tenant": "other"})})

	pid, err := parent.Spawn(ctx, k, scriptInput{Seed: "s"},
		agentkit.WithMetadata(map[string]string{"tenant": "acme", "workspace": "T123"}))
	gt.NoError(t, err)

	child := runToChild(t, k, repo, pid, box)
	gt.Value(t, child.Metadata["tenant"]).Equal("other")
	_, hasWorkspace := child.Metadata["workspace"]
	gt.Value(t, hasWorkspace).Equal(false)
}

// An explicitly empty map is how a caller says "this child gets none", which is
// only distinguishable from "unspecified" because WithMetadata records that it
// was called.
func TestSpawnChildEmptyMetadataOptsOutOfInheritance(t *testing.T) {
	ctx := context.Background()
	k, repo, parent, box := setupMetadataParentChild(t,
		[]agentkit.SpawnOption{agentkit.WithMetadata(map[string]string{})})

	pid, err := parent.Spawn(ctx, k, scriptInput{Seed: "s"},
		agentkit.WithMetadata(map[string]string{"tenant": "acme"}))
	gt.NoError(t, err)

	child := runToChild(t, k, repo, pid, box)
	gt.Value(t, len(child.Metadata)).Equal(0)
}

func TestSpawnChildNilMetadataOptsOutOfInheritance(t *testing.T) {
	ctx := context.Background()
	k, repo, parent, box := setupMetadataParentChild(t,
		[]agentkit.SpawnOption{agentkit.WithMetadata(nil)})

	pid, err := parent.Spawn(ctx, k, scriptInput{Seed: "s"},
		agentkit.WithMetadata(map[string]string{"tenant": "acme"}))
	gt.NoError(t, err)

	child := runToChild(t, k, repo, pid, box)
	gt.Value(t, len(child.Metadata)).Equal(0)
}

func TestSpawnChildWithParentHavingNoMetadata(t *testing.T) {
	ctx := context.Background()
	k, repo, parent, box := setupMetadataParentChild(t, nil)

	pid, err := parent.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)

	child := runToChild(t, k, repo, pid, box)
	gt.Value(t, len(child.Metadata)).Equal(0)
}

// A strategy cannot obtain a History version to inherit — Syscalls hands out no
// HistoryRef — so WithInheritedHistory on a child could only carry a reference
// from outside the runtime, and the one a strategy would reach for (its own) is
// released by this very transition's commit. It is rejected as the static misuse
// it is, before any middleware sees the request.
func TestSpawnChildRejectsInheritedHistory(t *testing.T) {
	ctx := context.Background()

	var mu sync.Mutex
	var spawnErr error
	repo := memory.New()
	reg := agentkit.NewRegistry()
	child, err := agentkit.Register(reg, "child", 1, &scriptStrategy{step: doneStep()})
	gt.NoError(t, err)

	var childID agentkit.ProcessID
	parentStep := func(c context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		id, e := child.SpawnChild(c, sys, scriptInput{Seed: "kid"},
			agentkit.WithInheritedHistory(sys.ProcessID()))
		mu.Lock()
		spawnErr, childID = e, id
		mu.Unlock()
		// Swallowed on purpose: the point is what SpawnChild returned, and a
		// committed Process makes the "no child was buffered" check meaningful.
		return st, agentkit.Done([]byte("done")), nil
	}
	parent, err := agentkit.Register(reg, "parent", 1, &scriptStrategy{step: parentStep})
	gt.NoError(t, err)
	model, _ := mockLLM(textResponse("x"))
	k, err := agentkit.New(repo, model, reg)
	gt.NoError(t, err)

	pid, err := parent.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	p := serveUntil(t, k, repo, pid, 5*time.Second, isTerminal)
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)

	mu.Lock()
	gotErr, gotID := spawnErr, childID
	mu.Unlock()
	gt.Error(t, gotErr).Is(agentkit.ErrInvalidRequest)
	gt.Value(t, gotID).Equal(agentkit.ProcessID("")) // no id was minted.

	// And no child row exists: the parent's own commit was the only write.
	now := time.Now()
	claimed, err := repo.ClaimNextProcess(ctx, "probe", now.Add(time.Minute), now)
	gt.NoError(t, err)
	gt.Nil(t, claimed)
}

// Inheritance runs before the chain, so a SpawnMiddleware is the place to strip
// keys a child must not carry.
func TestSpawnMiddlewareSeesAndCanStripInheritedMetadata(t *testing.T) {
	ctx := context.Background()

	var mu sync.Mutex
	var seen map[string]string
	mw := func(next agentkit.SpawnHandler) agentkit.SpawnHandler {
		return func(c context.Context, req *agentkit.SpawnRequest) (agentkit.ProcessID, error) {
			mu.Lock()
			seen = map[string]string{}
			for k, v := range req.Metadata {
				seen[k] = v
			}
			mu.Unlock()
			delete(req.Metadata, "secret")
			return next(c, req)
		}
	}

	k, repo, parent, box := setupMetadataParentChild(t, nil, agentkit.WithSpawnMiddleware(mw))
	pid, err := parent.Spawn(ctx, k, scriptInput{Seed: "s"},
		agentkit.WithMetadata(map[string]string{"tenant": "acme", "secret": "shh"}))
	gt.NoError(t, err)

	child := runToChild(t, k, repo, pid, box)

	mu.Lock()
	defer mu.Unlock()
	gt.Value(t, seen["tenant"]).Equal("acme")
	gt.Value(t, seen["secret"]).Equal("shh") // the middleware saw the inherited map...
	gt.Value(t, child.Metadata["tenant"]).Equal("acme")
	_, hasSecret := child.Metadata["secret"]
	gt.Value(t, hasSecret).Equal(false) // ...and its edit reached the child.

	// The parent keeps what it was spawned with: the child got a copy.
	parentProc, err := k.GetProcess(ctx, pid)
	gt.NoError(t, err)
	gt.Value(t, parentProc.Metadata["secret"]).Equal("shh")
}

func TestListEventsCursorAndLimit(t *testing.T) {
	ctx := context.Background()
	model, _ := mockLLM(textResponse("x"))

	emitStep := func(_ context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		gt.NoError(t, sys.Emit(ctx, "test.a", []byte(`{"n":1}`)))
		gt.NoError(t, sys.Emit(ctx, "test.b", []byte(`{"n":2}`)))
		return st, agentkit.Done([]byte("ok")), nil
	}
	k, repo, ag := setupScript(t, emitStep, model)
	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	serveUntil(t, k, repo, pid, 5*time.Second, func(p *agentkit.Process) bool {
		return p.Status == agentkit.ProcessSucceeded
	})

	all, err := k.ListEvents(ctx, pid)
	gt.NoError(t, err)
	gt.Array(t, all).Longer(3) // created + the two emitted + finished.
	for _, e := range all {
		gt.Value(t, e.ID).NotEqual(agentkit.EventID(""))
	}

	t.Run("resumes after the cursor", func(t *testing.T) {
		rest, err := k.ListEvents(ctx, pid, agentkit.WithAfterEvent(all[0].ID))
		gt.NoError(t, err)
		gt.Array(t, rest).Length(len(all) - 1)
		gt.Value(t, rest[0].ID).Equal(all[1].ID)
	})
	t.Run("the last event yields nothing more", func(t *testing.T) {
		rest, err := k.ListEvents(ctx, pid, agentkit.WithAfterEvent(all[len(all)-1].ID))
		gt.NoError(t, err)
		gt.Array(t, rest).Length(0)
	})
	t.Run("limit caps the count", func(t *testing.T) {
		some, err := k.ListEvents(ctx, pid, agentkit.WithEventLimit(2))
		gt.NoError(t, err)
		gt.Array(t, some).Length(2)
		gt.Value(t, some[0].ID).Equal(all[0].ID)
	})
	t.Run("cursor and limit compose", func(t *testing.T) {
		some, err := k.ListEvents(ctx, pid, agentkit.WithAfterEvent(all[0].ID), agentkit.WithEventLimit(1))
		gt.NoError(t, err)
		gt.Array(t, some).Length(1)
		gt.Value(t, some[0].ID).Equal(all[1].ID)
	})
	t.Run("a non-positive limit is uncapped", func(t *testing.T) {
		some, err := k.ListEvents(ctx, pid, agentkit.WithEventLimit(0))
		gt.NoError(t, err)
		gt.Array(t, some).Length(len(all))
	})
	// A stale cursor must not read as a burst of new events.
	t.Run("an unknown cursor is ErrEventNotFound", func(t *testing.T) {
		got, err := k.ListEvents(ctx, pid, agentkit.WithAfterEvent("no-such-event"))
		gt.Error(t, err).Is(agentkit.ErrEventNotFound)
		gt.Array(t, got).Length(0)
	})
}

// --- prompt-cache token breakdown --------------------------------------------

// cacheBreakdown mirrors the four token fields GenerateResult carries, so a
// step can smuggle what it saw out through Process.Output for the test to
// inspect after the transition commits.
type cacheBreakdown struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

func TestGenerateBasePropagatesCacheTokenBreakdown(t *testing.T) {
	ctx := context.Background()
	// Four distinct values so a mapper that assigns the wrong field to the
	// wrong counter cannot pass by coincidence.
	model, _ := mockLLM(&gollem.Response{
		Texts:                   []string{"ok"},
		InputToken:              11,
		OutputToken:             13,
		CacheCreationInputToken: 17,
		CacheReadInputToken:     19,
	})

	step := func(c context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		res, err := sys.Generate(c, []gollem.Input{gollem.Text(st.Seed)})
		if err != nil {
			return st, agentkit.Decision[[]byte]{}, err
		}
		out, err := json.Marshal(cacheBreakdown{
			InputTokens:              res.InputTokens,
			OutputTokens:             res.OutputTokens,
			CacheReadInputTokens:     res.CacheReadInputTokens,
			CacheCreationInputTokens: res.CacheCreationInputTokens,
		})
		if err != nil {
			return st, agentkit.Decision[[]byte]{}, err
		}
		return st, agentkit.Done(out), nil
	}

	k, repo, ag := setupScript(t, step, model)
	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "hello"})
	gt.NoError(t, err)
	p := serveUntil(t, k, repo, pid, 3*time.Second, isTerminal)
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)

	// GenerateResult carried the breakdown to the strategy.
	var got cacheBreakdown
	gt.NoError(t, json.Unmarshal(p.Output, &got))
	gt.Value(t, got).Equal(cacheBreakdown{
		InputTokens: 11, OutputTokens: 13, CacheReadInputTokens: 19, CacheCreationInputTokens: 17,
	})

	// Metrics (committed) carries the same breakdown, plus the counters
	// generateBase and the transition themselves add.
	gt.Value(t, p.Metrics).Equal(agentkit.Metrics{
		InputTokens: 11, OutputTokens: 13, CacheReadInputTokens: 19, CacheCreationInputTokens: 17,
		LLMCalls: 1, Steps: 1,
	})
}

// --- Syscalls.Count ---------------------------------------------------------

// A counter the caller defined is only useful if a Limit can read it, so these
// assert the whole path: counted in a Step, visible at once, committed on the
// row, and moving the verdict the way a metered effect does.

func TestSyscallsCountRejectsWhatItCannotCount(t *testing.T) {
	docs := agentkit.DefineMetricKey("test.count.docs")
	var nilKeyErr, negativeErr, zeroErr error
	var afterZero agentkit.Metrics

	step := func(c context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		nilKeyErr = sys.Count(c, nil, 1)
		negativeErr = sys.Count(c, docs, -1)
		zeroErr = sys.Count(c, docs, 0)
		afterZero = sys.Metrics()
		return st, agentkit.Done([]byte("ok")), nil
	}
	model, _ := mockLLM(textResponse("x"))
	k, repo, ag := setupScript(t, step, model)
	pid, err := ag.Spawn(context.Background(), k, scriptInput{Seed: "s"})
	gt.NoError(t, err)

	p := serveUntil(t, k, repo, pid, 3*time.Second, isTerminal)
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)

	gt.Error(t, nilKeyErr).Is(agentkit.ErrInvalidRequest)
	gt.Error(t, negativeErr).Is(agentkit.ErrInvalidRequest)
	gt.NoError(t, zeroErr)
	// A rejected or empty count leaves nothing behind, on the run or the row.
	gt.Value(t, afterZero.Count(docs)).Equal(int64(0))
	gt.Value(t, p.Metrics.Count(docs)).Equal(int64(0))
}

func TestSyscallsCountAccumulatesAndCommits(t *testing.T) {
	docs := agentkit.DefineMetricKey("test.count.docs")
	credits := agentkit.DefineMetricKey("test.count.credits")
	var live agentkit.Metrics

	step := func(c context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		gt.NoError(t, sys.Count(c, docs, 2))
		gt.NoError(t, sys.Count(c, docs, 3))
		gt.NoError(t, sys.Count(c, credits, 7))
		live = sys.Metrics()
		return st, agentkit.Done([]byte("ok")), nil
	}
	model, _ := mockLLM(textResponse("x"))
	k, repo, ag := setupScript(t, step, model)
	pid, err := ag.Spawn(context.Background(), k, scriptInput{Seed: "s"})
	gt.NoError(t, err)

	p := serveUntil(t, k, repo, pid, 3*time.Second, isTerminal)
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)

	// Adds rather than replaces: two counts of the same key make five.
	gt.Value(t, live.Count(docs)).Equal(int64(5))
	gt.Value(t, live.Count(credits)).Equal(int64(7))
	// And the same numbers are on the committed row, beside the kernel's own.
	gt.Value(t, p.Metrics.Count(docs)).Equal(int64(5))
	gt.Value(t, p.Metrics.Count(credits)).Equal(int64(7))
	gt.Value(t, p.Metrics.Steps).Equal(int64(1))
}

// Count re-evaluates Limit the way a metered effect does, so LimitStatus() and
// Metrics() still describe the same moment. It cannot be refused — what it
// records has already happened — but the refusal it provokes binds the next
// effect.
func TestSyscallsCountMovesTheVerdictWithoutFailing(t *testing.T) {
	docs := agentkit.DefineMetricKey("test.count.docs")
	var countErr, genErr error
	var verdict agentkit.LimitDecision

	step := func(c context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		countErr = sys.Count(c, docs, 3)
		verdict = sys.LimitStatus()
		_, genErr = sys.Generate(c, []gollem.Input{gollem.Text("go")})
		return st, agentkit.Done([]byte("ok")), nil
	}
	limiter := func(_ context.Context, _ *agentkit.Process, m agentkit.Metrics) agentkit.LimitDecision {
		if m.Count(docs) >= 3 {
			return agentkit.LimitStop("too many documents")
		}
		return agentkit.LimitPass()
	}
	model, count := mockLLM(textResponse("x"))
	k, repo, ag := setupScriptLimited(t, step, model, limiter)
	pid, err := ag.Spawn(context.Background(), k, scriptInput{Seed: "s"})
	gt.NoError(t, err)

	p := serveUntil(t, k, repo, pid, 3*time.Second, isTerminal)
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)

	gt.NoError(t, countErr) // the count itself is never refused
	gt.Value(t, verdict.Kind()).Equal(agentkit.LimitKindStop)
	gt.Value(t, verdict.Message()).Equal("too many documents")
	// The Generate that followed is the one that actually gets refused.
	gt.Error(t, genErr).Is(agentkit.ErrLimitExceeded)
	gt.Value(t, *count).Equal(0)
}

// --- MeteredTool ------------------------------------------------------------

// meteredTool is a gollem.Tool that also reports its own consumption. It records
// what Metered was called with, which is what a tool holding no state between
// calls depends on.
type meteredTool struct {
	name         string
	result       map[string]any
	runErr       error
	report       map[agentkit.MetricKey]int64
	panicOnMeter bool

	mu    sync.Mutex
	calls []meteredCall
}

type meteredCall struct {
	call   gollem.FunctionCall
	result map[string]any
	err    error
}

func (t *meteredTool) Spec() gollem.ToolSpec { return gollem.ToolSpec{Name: t.name} }

func (t *meteredTool) Run(_ context.Context, _ map[string]any) (map[string]any, error) {
	return t.result, t.runErr
}

func (t *meteredTool) Metered(call gollem.FunctionCall, result map[string]any,
	runErr error) map[agentkit.MetricKey]int64 {
	if t.panicOnMeter {
		panic("metering exploded")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.calls = append(t.calls, meteredCall{call: call, result: result, err: runErr})
	return t.report
}

func (t *meteredTool) seen() []meteredCall {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]meteredCall(nil), t.calls...)
}

// runToolStep calls the tool once and finishes, whatever the tool returned.
func runToolStep(t *testing.T, tool gollem.Tool) (*agentkit.Process, error) {
	t.Helper()
	var toolErr error
	step := func(c context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		_, toolErr = sys.CallTool(c, gollem.FunctionCall{ID: "1", Name: tool.Spec().Name,
			Arguments: map[string]any{"q": "go"}})
		return st, agentkit.Done([]byte("ok")), nil
	}
	tf := func(_ context.Context, _ *agentkit.Process) ([]gollem.Tool, error) {
		return []gollem.Tool{tool}, nil
	}
	model, _ := mockLLM(textResponse("x"))
	k, repo, ag := setupScript(t, step, model, agentkit.WithToolFactory(tf))
	pid, err := ag.Spawn(context.Background(), k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	return serveUntil(t, k, repo, pid, 3*time.Second, isTerminal), toolErr
}

func TestMeteredToolCountsBesideToolCalls(t *testing.T) {
	bytesFetched := agentkit.DefineMetricKey("test.tool.bytes")

	t.Run("the report is folded in with tool_calls", func(t *testing.T) {
		tool := &meteredTool{name: "fetch", result: map[string]any{"ok": true},
			report: map[agentkit.MetricKey]int64{bytesFetched: 4096}}
		p, err := runToolStep(t, tool)
		gt.NoError(t, err)
		gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)
		gt.Value(t, p.Metrics.ToolCalls).Equal(int64(1))
		gt.Value(t, p.Metrics.Count(bytesFetched)).Equal(int64(4096))

		// Metered saw that Run's own call and result.
		seen := tool.seen()
		gt.Array(t, seen).Length(1)
		gt.Value(t, seen[0].call.Name).Equal("fetch")
		gt.Value(t, seen[0].call.Arguments).Equal(map[string]any{"q": "go"})
		gt.Value(t, seen[0].result).Equal(map[string]any{"ok": true})
		gt.NoError(t, seen[0].err)
	})

	t.Run("a nil report counts nothing extra", func(t *testing.T) {
		tool := &meteredTool{name: "fetch", result: map[string]any{"ok": true}}
		p, err := runToolStep(t, tool)
		gt.NoError(t, err)
		gt.Value(t, p.Metrics.ToolCalls).Equal(int64(1))
		gt.Value(t, p.Metrics.Counters()).Nil()
	})

	// The effect has already run when Metered is called, so a bug in the
	// accounting drops the bad entry and keeps everything else.
	t.Run("invalid entries are dropped, the rest are kept", func(t *testing.T) {
		negative := agentkit.DefineMetricKey("test.tool.negative")
		tool := &meteredTool{name: "fetch", report: map[agentkit.MetricKey]int64{
			bytesFetched: 512,
			negative:     -1,
			nil:          9,
		}}
		p, err := runToolStep(t, tool)
		gt.NoError(t, err)
		gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)
		gt.Value(t, p.Metrics.ToolCalls).Equal(int64(1))
		gt.Value(t, p.Metrics.Count(bytesFetched)).Equal(int64(512))
		gt.Value(t, p.Metrics.Count(negative)).Equal(int64(0))
	})

	// A call that failed halfway may still have spent something.
	t.Run("a failed Run is still metered", func(t *testing.T) {
		runErr := gollemErr("upstream refused")
		tool := &meteredTool{name: "fetch", runErr: runErr,
			report: map[agentkit.MetricKey]int64{bytesFetched: 128}}
		p, err := runToolStep(t, tool)
		gt.Error(t, err) // the Run error reaches the strategy
		gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)
		gt.Value(t, p.Metrics.ToolCalls).Equal(int64(1))
		gt.Value(t, p.Metrics.Count(bytesFetched)).Equal(int64(128))

		seen := tool.seen()
		gt.Array(t, seen).Length(1)
		gt.Value(t, seen[0].err).Equal(runErr)
	})

	t.Run("a plain tool is unchanged", func(t *testing.T) {
		p, err := runToolStep(t, mockTool("plain", map[string]any{"ok": true}))
		gt.NoError(t, err)
		gt.Value(t, p.Metrics.ToolCalls).Equal(int64(1))
		gt.Value(t, p.Metrics.Counters()).Nil()
	})
}

// Metered is tool-author code running inside the transition, so a panic there
// goes to the worker's recover — a transition error, exactly like a panic in Run.
func TestMeteredToolPanicIsATransitionError(t *testing.T) {
	tool := &meteredTool{name: "fetch", panicOnMeter: true}
	step := func(c context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		if _, err := sys.CallTool(c, gollem.FunctionCall{ID: "1", Name: "fetch",
			Arguments: map[string]any{}}); err != nil {
			return st, agentkit.Decision[[]byte]{}, err
		}
		return st, agentkit.Done([]byte("ok")), nil
	}
	tf := func(_ context.Context, _ *agentkit.Process) ([]gollem.Tool, error) {
		return []gollem.Tool{tool}, nil
	}
	model, _ := mockLLM(textResponse("x"))
	k, repo, ag := setupScript(t, step, model, agentkit.WithToolFactory(tf))
	pid, err := ag.Spawn(context.Background(), k, scriptInput{Seed: "s"})
	gt.NoError(t, err)

	p := serveUntil(t, k, repo, pid, 3*time.Second, isTerminal, agentkit.WithMaxStepAttempts(0))
	gt.Value(t, p.Status).Equal(agentkit.ProcessFailed)
	gt.Value(t, p.Failure.Code).Equal(agentkit.FailureRetryExhausted)
	// The panic lands between Run and the meter that follows it, so this call is
	// not counted — the same window a panic in Run itself falls into.
	gt.Value(t, p.Metrics.ToolCalls).Equal(int64(0))
}
