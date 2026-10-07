package agentkit_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/gollem-dev/agentkit"
	histmem "github.com/gollem-dev/agentkit/historystore/memory"
	"github.com/gollem-dev/agentkit/repository/memory"
	"github.com/gollem-dev/gollem"
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

// WithInheritedHistory resolves the version to inherit from the issuing
// Process's record, so everything that can make that impossible is reported
// synchronously — before Init runs and before any row is written. A Process
// carrying a reference nothing can read is never created.
func TestSpawnInheritedHistoryValidation(t *testing.T) {
	ctx := context.Background()

	// nothingClaimable asserts no Process is waiting to run, which is how a test
	// says "the rejected Spawn wrote nothing" without a list API.
	nothingClaimable := func(t *testing.T, repo agentkit.Repository) {
		t.Helper()
		now := time.Now()
		got, err := repo.ClaimNextProcess(ctx, "probe", now.Add(time.Minute), now)
		gt.NoError(t, err)
		gt.Nil(t, got)
	}

	t.Run("empty process id", func(t *testing.T) {
		var mu sync.Mutex
		var seen []int
		k, repo, ag := registerWithHistory(t, sessionStep(&seen, &mu, 1), growingLLM(), histmem.New())
		_, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"}, agentkit.WithInheritedHistory(""))
		gt.Error(t, err).Is(agentkit.ErrInvalidRequest)
		nothingClaimable(t, repo)
	})

	t.Run("unknown process", func(t *testing.T) {
		var mu sync.Mutex
		var seen []int
		k, repo, ag := registerWithHistory(t, sessionStep(&seen, &mu, 1), growingLLM(), histmem.New())
		_, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"},
			agentkit.WithInheritedHistory(agentkit.ProcessID("no-such-process")))
		gt.Error(t, err).Is(agentkit.ErrProcessNotFound)
		nothingClaimable(t, repo)
	})

	t.Run("process with no committed conversation", func(t *testing.T) {
		var mu sync.Mutex
		var seen []int
		k, repo, ag := registerWithHistory(t, sessionStep(&seen, &mu, 1), growingLLM(), histmem.New())
		// Spawned but never served: its HistoryRef is still empty, so there is no
		// version to start from.
		fresh, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
		gt.NoError(t, err)
		_, err = ag.Spawn(ctx, k, scriptInput{Seed: "s"}, agentkit.WithInheritedHistory(fresh))
		gt.Error(t, err).Is(agentkit.ErrInvalidRequest)

		// The only claimable row is that first Process; the rejected Spawn added none.
		now := time.Now()
		claimed, err := repo.ClaimNextProcess(ctx, "probe", now.Add(time.Minute), now)
		gt.NoError(t, err)
		gt.NotNil(t, claimed)
		gt.Value(t, claimed.ID).Equal(fresh)
		nothingClaimable(t, repo)
	})

	t.Run("agent without a history store", func(t *testing.T) {
		repo := memory.New()
		reg := agentkit.NewRegistry()
		ag, err := agentkit.Register(reg, "a", 1, &scriptStrategy{step: doneStep()}) // no WithHistoryStore.
		gt.NoError(t, err)
		model, _ := mockLLM(textResponse("x"))
		k, err := agentkit.New(repo, model, reg)
		gt.NoError(t, err)

		_, err = ag.Spawn(ctx, k, scriptInput{Seed: "s"},
			agentkit.WithInheritedHistory(agentkit.ProcessID("whatever")))
		gt.Error(t, err).Is(agentkit.ErrHistoryNotConfigured)
		nothingClaimable(t, repo)
	})
}

// directAnswerSeed marks a Process that answers with one primitive
// sys.Generate and returns Done without touching Session(), so it commits no
// version of its own.
const directAnswerSeed = "direct"

// inheritChain is the strategy for a conversation carried across Processes,
// some of which answer directly. A Process seeded directAnswerSeed does that;
// any other runs two Session().Generate turns, so its run both supersedes a
// version and commits a final one. firstHistory records what Session().History
// returned on each session-using Process's first Step, keyed by Seed.
type inheritChain struct {
	mu           sync.Mutex
	seen         []int
	firstHistory map[string]*gollem.History
}

func (c *inheritChain) step() stepFn {
	session := sessionStep(&c.seen, &c.mu, 2)
	return func(ctx context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		if st.Seed == directAnswerSeed {
			if _, err := sys.Generate(ctx, []gollem.Input{gollem.Text("answer")}); err != nil {
				return st, agentkit.Decision[[]byte]{}, err
			}
			return st, agentkit.Done([]byte("answered")), nil
		}
		if st.N == 0 {
			h, err := sys.Session().History(ctx)
			if err != nil {
				return st, agentkit.Decision[[]byte]{}, err
			}
			c.mu.Lock()
			if c.firstHistory == nil {
				c.firstHistory = map[string]*gollem.History{}
			}
			c.firstHistory[st.Seed] = h
			c.mu.Unlock()
		}
		return session(ctx, sys, st)
	}
}

func (c *inheritChain) first(seed string) *gollem.History {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.firstHistory[seed]
}

// spawnDirect spawns a Process that answers without Session(), serves it to
// completion and returns its record.
func spawnDirect(t *testing.T, k *agentkit.Kernel, repo agentkit.Repository, ag agentkit.Agent[scriptInput],
	opts ...agentkit.SpawnOption) *agentkit.Process {
	t.Helper()
	pid, err := ag.Spawn(context.Background(), k, scriptInput{Seed: directAnswerSeed}, opts...)
	gt.NoError(t, err).Required()
	p := serveUntil(t, k, repo, pid, 5*time.Second, isTerminal)
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)
	gt.Value(t, p.HistoryRef).Equal(agentkit.HistoryRef(""))
	return p
}

// A Process that committed no version of its own still has a conversation — the
// one it inherited — so inheriting from it pins that same version, named under
// the Process that saved it.
func TestSpawnInheritsThroughProcessWithoutOwnVersion(t *testing.T) {
	ctx := context.Background()

	t.Run("one direct answer in between", func(t *testing.T) {
		chain := &inheritChain{}
		store := &probeStore{inner: histmem.New()}
		k, repo, ag := registerWithHistory(t, chain.step(), growingLLM(), store)

		a := runIssuer(t, k, repo, ag)
		b := spawnDirect(t, k, repo, ag, agentkit.WithInheritedHistory(a.ID))
		want := &agentkit.InheritedHistory{Process: a.ID, Ref: a.HistoryRef}
		gt.Value(t, b.InheritedHistory).Equal(want)

		cID, err := ag.Spawn(ctx, k, scriptInput{Seed: "c"}, agentkit.WithInheritedHistory(b.ID))
		gt.NoError(t, err).Required()
		spawned, err := repo.GetProcess(ctx, cID)
		gt.NoError(t, err).Required()
		gt.Value(t, spawned.InheritedHistory).Equal(want)

		c := serveUntil(t, k, repo, cID, 5*time.Second, isTerminal)
		gt.Value(t, c.Status).Equal(agentkit.ProcessSucceeded)
		// C's first turn started from A's conversation, as A committed it.
		gt.Value(t, chain.first("c")).Equal(committedHistory(t, store, a))
		gt.Value(t, histLen(committedHistory(t, store, c))).Equal(4)
	})

	t.Run("two direct answers in between", func(t *testing.T) {
		chain := &inheritChain{}
		store := &probeStore{inner: histmem.New()}
		k, repo, ag := registerWithHistory(t, chain.step(), growingLLM(), store)

		a := runIssuer(t, k, repo, ag)
		b1 := spawnDirect(t, k, repo, ag, agentkit.WithInheritedHistory(a.ID))
		b2 := spawnDirect(t, k, repo, ag, agentkit.WithInheritedHistory(b1.ID))
		want := &agentkit.InheritedHistory{Process: a.ID, Ref: a.HistoryRef}
		gt.Value(t, b2.InheritedHistory).Equal(want)

		cID, err := ag.Spawn(ctx, k, scriptInput{Seed: "c"}, agentkit.WithInheritedHistory(b2.ID))
		gt.NoError(t, err).Required()
		c := serveUntil(t, k, repo, cID, 5*time.Second, isTerminal)
		gt.Value(t, c.Status).Equal(agentkit.ProcessSucceeded)
		gt.Value(t, c.InheritedHistory).Equal(want)
		gt.Value(t, chain.first("c")).Equal(committedHistory(t, store, a))
	})

	// A Process that inherited a version and then committed its own passes on its
	// own: the inherited pair is where its conversation started, not what it is.
	t.Run("its own version takes precedence over the inherited one", func(t *testing.T) {
		chain := &inheritChain{}
		store := &probeStore{inner: histmem.New()}
		k, repo, ag := registerWithHistory(t, chain.step(), growingLLM(), store)

		a := runIssuer(t, k, repo, ag)
		bID, err := ag.Spawn(ctx, k, scriptInput{Seed: "b"}, agentkit.WithInheritedHistory(a.ID))
		gt.NoError(t, err).Required()
		b := serveUntil(t, k, repo, bID, 5*time.Second, isTerminal)
		gt.Value(t, b.Status).Equal(agentkit.ProcessSucceeded)
		gt.Value(t, b.HistoryRef).NotEqual(agentkit.HistoryRef(""))
		gt.Value(t, b.InheritedHistory).Equal(&agentkit.InheritedHistory{Process: a.ID, Ref: a.HistoryRef})

		cID, err := ag.Spawn(ctx, k, scriptInput{Seed: "c"}, agentkit.WithInheritedHistory(b.ID))
		gt.NoError(t, err).Required()
		c := serveUntil(t, k, repo, cID, 5*time.Second, isTerminal)
		gt.Value(t, c.Status).Equal(agentkit.ProcessSucceeded)
		gt.Value(t, c.InheritedHistory).Equal(&agentkit.InheritedHistory{Process: b.ID, Ref: b.HistoryRef})
		gt.Value(t, chain.first("c")).Equal(committedHistory(t, store, b))
	})

	// The version passed through is read, never released: C's commits announce
	// only C's own superseded version, and A's record still resolves.
	t.Run("the passed-through version is never discarded", func(t *testing.T) {
		chain := &inheritChain{}
		store := &probeStore{inner: histmem.New()}
		k, repo, ag := registerWithHistory(t, chain.step(), growingLLM(), store)

		a := runIssuer(t, k, repo, ag)
		b := spawnDirect(t, k, repo, ag, agentkit.WithInheritedHistory(a.ID))
		cID, err := ag.Spawn(ctx, k, scriptInput{Seed: "c"}, agentkit.WithInheritedHistory(b.ID))
		gt.NoError(t, err).Required()
		c := serveUntil(t, k, repo, cID, 5*time.Second, isTerminal)
		gt.Value(t, c.Status).Equal(agentkit.ProcessSucceeded)

		// saved() is in call order: A's two versions, then C's two. Each Process
		// released exactly its own first version, under its own id.
		refs := store.saved()
		gt.Array(t, refs).Length(4)
		gt.Value(t, store.discardedPairs()).Equal([]histCall{
			{pid: a.ID, ref: refs[0]},
			{pid: cID, ref: refs[2]},
		})
		gt.Value(t, discardedRef(store.discardedPairs(), a.HistoryRef)).Equal(false)
		gt.Value(t, histLen(committedHistory(t, store, a))).Equal(2)
	})

	t.Run("a process with neither its own nor an inherited version", func(t *testing.T) {
		chain := &inheritChain{}
		k, repo, ag := registerWithHistory(t, chain.step(), growingLLM(), histmem.New())

		// Finished, but it never had a conversation in either way.
		d := spawnDirect(t, k, repo, ag)
		gt.Nil(t, d.InheritedHistory)

		_, err := ag.Spawn(ctx, k, scriptInput{Seed: "c"}, agentkit.WithInheritedHistory(d.ID))
		gt.Error(t, err).Is(agentkit.ErrInvalidRequest)
		now := time.Now()
		claimed, err := repo.ClaimNextProcess(ctx, "probe", now.Add(time.Minute), now)
		gt.NoError(t, err)
		gt.Nil(t, claimed)
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
	events, _ := repo.ListEvents(ctx, pid, agentkit.EventQuery{})
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

// A Process that suspended on a deadlined question carries that deadline as its
// WakeAt. Now that WakeAt gates when a pending row may be claimed, Respond has
// to clear it -- otherwise the answer would sit unread until the deadline it was
// given to beat.
func TestRespondClearsTheSuspendedWakeTime(t *testing.T) {
	ctx := context.Background()
	step := func(_ context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		if aw, ok := sys.Await("q:1"); ok && aw.Status == agentkit.AwaitResponded {
			return st, agentkit.Done([]byte("answered")), nil
		}
		far := sys.Now().Add(time.Hour)
		return st, agentkit.Suspend[[]byte](
			agentkit.Question("q:1", []byte("?"), agentkit.WithDeadline(far))), nil
	}
	model, _ := mockLLM(textResponse("x"))
	k, repo, ag := setupScript(t, step, model)

	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	waiting := serveUntil(t, k, repo, pid, 3*time.Second,
		func(p *agentkit.Process) bool { return p.Status == agentkit.ProcessWaiting })
	gt.NotNil(t, waiting.WakeAt) // the question's deadline

	gt.NoError(t, k.Respond(ctx, pid, "q:1", []byte("yes")))

	woken, err := repo.GetProcess(ctx, pid)
	gt.NoError(t, err)
	gt.Value(t, woken.Status).Equal(agentkit.ProcessPending)
	gt.Nil(t, woken.WakeAt)

	// And it really is claimable now rather than an hour from now.
	p := serveUntil(t, k, repo, pid, 3*time.Second, isTerminal)
	gt.Value(t, p.Status).Equal(agentkit.ProcessSucceeded)
}

// The other side of that trade: a Process already pending in retry backoff keeps
// its wake time when a response arrives, so the answer waits out the remaining
// backoff. Bounded by the backoff cap, and preferred over letting a Respond
// erase the throttle on a Process that is failing.
func TestRespondKeepsARetryBackoffWakeTime(t *testing.T) {
	ctx := context.Background()
	repo := memory.New()
	reg := agentkit.NewRegistry()
	ag, err := agentkit.Register(reg, "main", 1, &scriptStrategy{step: doneStep()})
	gt.NoError(t, err)
	model, _ := mockLLM(textResponse("x"))
	k, err := agentkit.New(repo, model, reg)
	gt.NoError(t, err)

	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)

	// The state a failed transition leaves behind, with a question still open.
	p, err := repo.GetProcess(ctx, pid)
	gt.NoError(t, err)
	backoff := time.Now().Add(time.Hour)
	p.WakeAt = &backoff
	p.StepAttempts = 1
	aw := &agentkit.Await{
		ProcessID: pid, Key: "q:1", Kind: agentkit.AwaitQuestion,
		Status: agentkit.AwaitOpen, Question: []byte("?"), CreatedAt: time.Now(),
	}
	gt.NoError(t, repo.Apply(ctx, agentkit.ChangeSet{
		Processes: []*agentkit.Process{p}, Awaits: []*agentkit.Await{aw},
	}))

	gt.NoError(t, k.Respond(ctx, pid, "q:1", []byte("yes")))

	after, err := repo.GetProcess(ctx, pid)
	gt.NoError(t, err)
	gt.Value(t, after.Status).Equal(agentkit.ProcessPending)
	gt.NotNil(t, after.WakeAt)
	gt.Value(t, after.WakeAt.Unix()).Equal(backoff.Unix())
}
