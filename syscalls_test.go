package agentkit_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/gollem-dev/agentkit"
	"github.com/gollem-dev/agentkit/repository/memory"
	"github.com/m-mizutani/gt"
)

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
