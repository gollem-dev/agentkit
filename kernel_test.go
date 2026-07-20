package agentkit_test

import (
	"context"
	"testing"
	"time"

	"github.com/gollem-dev/agentkit"
	"github.com/gollem-dev/agentkit/repository/memory"
	"github.com/m-mizutani/gt"
)

func doneStep() func(context.Context, agentkit.Syscalls, scriptState) (scriptState, agentkit.Decision[[]byte], error) {
	return func(_ context.Context, _ agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		return st, agentkit.Done([]byte(`"ok"`)), nil
	}
}

func TestNewValidation(t *testing.T) {
	repo := memory.New()
	reg := agentkit.NewRegistry()
	model, _ := mockLLM(textResponse("hi"))

	t.Run("nil repo", func(t *testing.T) {
		_, err := agentkit.New(nil, model, reg)
		gt.Error(t, err).Is(agentkit.ErrInvalidConfig)
	})
	t.Run("nil model", func(t *testing.T) {
		_, err := agentkit.New(repo, nil, reg)
		gt.Error(t, err).Is(agentkit.ErrInvalidConfig)
	})
	t.Run("nil agents", func(t *testing.T) {
		_, err := agentkit.New(repo, model, nil)
		gt.Error(t, err).Is(agentkit.ErrInvalidConfig)
	})
	t.Run("nil role binding", func(t *testing.T) {
		_, err := agentkit.New(repo, model, reg, agentkit.WithModelRole(nil, model))
		gt.Error(t, err).Is(agentkit.ErrInvalidConfig)
	})
	t.Run("valid", func(t *testing.T) {
		k, err := agentkit.New(repo, model, reg)
		gt.NoError(t, err)
		gt.Value(t, k != nil).Equal(true)
	})
}

func TestSpawnValidation(t *testing.T) {
	ctx := context.Background()

	t.Run("Init error returns synchronously and creates no process", func(t *testing.T) {
		repo := memory.New()
		reg := agentkit.NewRegistry()
		ag, err := agentkit.Register(reg, "a", 1, &scriptStrategy{step: doneStep()})
		gt.NoError(t, err)
		model, _ := mockLLM(textResponse("x"))
		k, _ := agentkit.New(repo, model, reg)
		_, err = ag.Spawn(ctx, k, scriptInput{Seed: ""}) // empty seed -> Init error
		gt.Error(t, err)
	})

	t.Run("unknown agent", func(t *testing.T) {
		reg1 := agentkit.NewRegistry()
		ag, _ := agentkit.Register(reg1, "a", 1, &scriptStrategy{step: doneStep()})
		reg2 := agentkit.NewRegistry() // kernel with a DIFFERENT (empty) registry
		repo := memory.New()
		model, _ := mockLLM(textResponse("x"))
		k, _ := agentkit.New(repo, model, reg2)
		_, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
		gt.Error(t, err).Is(agentkit.ErrUnknownAgent)
	})

	t.Run("idempotency key returns same id, no new process", func(t *testing.T) {
		repo := memory.New()
		reg := agentkit.NewRegistry()
		ag, _ := agentkit.Register(reg, "a", 1, &scriptStrategy{step: doneStep()})
		model, _ := mockLLM(textResponse("x"))
		k, _ := agentkit.New(repo, model, reg)
		id1, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"}, agentkit.WithIdempotencyKey("dup"))
		gt.NoError(t, err)
		id2, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"}, agentkit.WithIdempotencyKey("dup"))
		gt.NoError(t, err)
		gt.Value(t, id1).Equal(id2)
	})

	t.Run("subject busy", func(t *testing.T) {
		repo := memory.New()
		reg := agentkit.NewRegistry()
		ag, _ := agentkit.Register(reg, "a", 1, &scriptStrategy{step: doneStep()})
		model, _ := mockLLM(textResponse("x"))
		k, _ := agentkit.New(repo, model, reg)
		subj := agentkit.SubjectRef{Kind: "case", ID: "42"}
		_, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"}, agentkit.WithSubject(subj))
		gt.NoError(t, err)
		_, err = ag.Spawn(ctx, k, scriptInput{Seed: "s"}, agentkit.WithSubject(subj))
		gt.Error(t, err).Is(agentkit.ErrSubjectBusy)
	})
}

func TestRespond(t *testing.T) {
	ctx := context.Background()

	newWaiting := func(t *testing.T) (*agentkit.Kernel, agentkit.Repository, agentkit.ProcessID, agentkit.AwaitKey) {
		repo := memory.New()
		reg := agentkit.NewRegistry()
		model, _ := mockLLM(textResponse("x"))
		k, _ := agentkit.New(repo, model, reg)
		pid := agentkit.ProcessID("p-" + randSuffix())
		key := agentkit.AwaitKey("q:1")
		now := time.Now()
		gt.NoError(t, repo.Apply(ctx, agentkit.ChangeSet{
			Processes: []*agentkit.Process{{ID: pid, Agent: "a", Status: agentkit.ProcessWaiting, RootID: pid, CreatedAt: now}},
			Awaits:    []*agentkit.Await{{ProcessID: pid, Key: key, Kind: agentkit.AwaitQuestion, Status: agentkit.AwaitOpen, CreatedAt: now}},
		}))
		return k, repo, pid, key
	}

	t.Run("process not found", func(t *testing.T) {
		k, _, _, key := newWaiting(t)
		err := k.Respond(ctx, "nope", key, []byte("yes"))
		gt.Error(t, err).Is(agentkit.ErrProcessNotFound)
	})
	t.Run("nil response", func(t *testing.T) {
		k, _, pid, key := newWaiting(t)
		err := k.Respond(ctx, pid, key, nil)
		gt.Error(t, err).Is(agentkit.ErrInvalidRequest)
	})
	t.Run("await not found", func(t *testing.T) {
		k, _, pid, _ := newWaiting(t)
		err := k.Respond(ctx, pid, "missing", []byte("yes"))
		gt.Error(t, err).Is(agentkit.ErrAwaitNotFound)
	})
	t.Run("first-wins: second Respond is ErrAwaitClosed", func(t *testing.T) {
		k, repo, pid, key := newWaiting(t)
		gt.NoError(t, k.Respond(ctx, pid, key, []byte("yes"), agentkit.WithRespondedBy("u1")))
		err := k.Respond(ctx, pid, key, []byte("no"), agentkit.WithRespondedBy("u2"))
		gt.Error(t, err).Is(agentkit.ErrAwaitClosed)
		// The process woke to pending, and RespondedBy stays the first value.
		p, _ := repo.GetProcess(ctx, pid)
		gt.Value(t, p.Status).Equal(agentkit.ProcessPending)
		aws, _ := repo.ListAwaits(ctx, pid)
		gt.Value(t, aws[0].RespondedBy).Equal("u1")
		gt.Value(t, string(aws[0].Response)).Equal("yes")
	})
}

func TestCancelPending(t *testing.T) {
	ctx := context.Background()
	repo := memory.New()
	reg := agentkit.NewRegistry()
	ag, _ := agentkit.Register(reg, "a", 1, &scriptStrategy{step: doneStep()})
	model, _ := mockLLM(textResponse("x"))
	k, _ := agentkit.New(repo, model, reg)
	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	gt.NoError(t, k.Cancel(ctx, pid, "user aborted"))
	p, _ := repo.GetProcess(ctx, pid)
	gt.Value(t, p.Status).Equal(agentkit.ProcessCancelled)
	events, _ := repo.ListEvents(ctx, pid)
	gt.Bool(t, hasEvent(events, agentkit.EventProcessFinished)).True()
	// cancelling a terminal process errors.
	gt.Error(t, k.Cancel(ctx, pid, "again")).Is(agentkit.ErrProcessFinished)
}

func TestRoleResolution(t *testing.T) {
	repo := memory.New()
	reg := agentkit.NewRegistry()
	def, _ := mockLLM(textResponse("default"))
	planner, _ := mockLLM(textResponse("planner"))
	rolePlanner := agentkit.DefineModelRole("planner")
	roleOther := agentkit.DefineModelRole("planner") // same name, distinct identity
	k, err := agentkit.New(repo, def, reg, agentkit.WithModelRole(rolePlanner, planner))
	gt.NoError(t, err)

	gt.Bool(t, k.ResolveModel(nil) == def).True()             // nil -> default
	gt.Bool(t, k.ResolveModel(rolePlanner) == planner).True() // bound role -> its client
	gt.Bool(t, k.ResolveModel(roleOther) == def).True()       // same name but different Define -> default
}

func hasEvent(events []*agentkit.Event, typ agentkit.EventType) bool {
	for _, e := range events {
		if e.Type == typ {
			return true
		}
	}
	return false
}

func randSuffix() string {
	return time.Now().Format("150405.000000000")
}

func TestCancelFiresFinishHandler(t *testing.T) {
	ctx := context.Background()
	repo := memory.New()
	reg := agentkit.NewRegistry()
	var rec finishRecorder
	ag, err := agentkit.Register(reg, "a", 1, &finishStrategy{step: finishDoneStep("unused")},
		agentkit.WithOnFinish(rec.handler))
	gt.NoError(t, err)
	model, _ := mockLLM(textResponse("x"))
	k, err := agentkit.New(repo, model, reg)
	gt.NoError(t, err)

	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	// Cancel commits from the caller's own process, so the handler runs here.
	gt.NoError(t, k.Cancel(ctx, pid, "user aborted"))

	calls, results := rec.snapshot()
	gt.Value(t, calls).Equal(1)
	gt.Value(t, results[0].Status).Equal(agentkit.ProcessCancelled)
	gt.Nil(t, results[0].Output)
	gt.Nil(t, results[0].Failure)

	// A second Cancel is rejected before any commit, so nothing fires again.
	gt.Error(t, k.Cancel(ctx, pid, "again")).Is(agentkit.ErrProcessFinished)
	calls, _ = rec.snapshot()
	gt.Value(t, calls).Equal(1)
}
