package agentkit_test

import (
	"context"
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
