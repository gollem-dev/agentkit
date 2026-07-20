// Package repotest is a contract conformance suite for agentkit.Repository
// implementations. The bundled memory and filesystem implementations call it
// from their tests, and external implementers (e.g. a postgres Repository) can
// run it against their own implementation.
package repotest

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gollem-dev/agentkit"
	"github.com/m-mizutani/gt"
)

var idCounter int64

// newPID returns a unique ProcessID. It combines a nanosecond timestamp with a
// process-global counter so parallel runs and same-nanosecond calls never
// collide, and so no hardcoded IDs are used.
func newPID() agentkit.ProcessID {
	n := atomic.AddInt64(&idCounter, 1)
	return agentkit.ProcessID(fmt.Sprintf("proc-%d-%d", time.Now().UnixNano(), n))
}

func uniqueStr(prefix string) string {
	n := atomic.AddInt64(&idCounter, 1)
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano(), n)
}

func mkProc(pid agentkit.ProcessID) *agentkit.Process {
	now := time.Now()
	return &agentkit.Process{
		ID:        pid,
		Agent:     "conformance-agent",
		Status:    agentkit.ProcessPending,
		RootID:    pid,
		Rev:       0,
		CreatedAt: now,
		UpdatedAt: now,
	}
}

// Run executes the full Repository conformance suite. factory must return a
// fresh, empty Repository each time it is called.
func Run(t *testing.T, factory func(t *testing.T) agentkit.Repository) {
	ctx := context.Background()

	t.Run("InsertGetAndRevIncrement", func(t *testing.T) {
		repo := factory(t)
		pid := newPID()
		gt.NoError(t, repo.Apply(ctx, agentkit.ChangeSet{Processes: []*agentkit.Process{mkProc(pid)}}))

		got, err := repo.GetProcess(ctx, pid)
		gt.NoError(t, err)
		gt.Value(t, got.ID).Equal(pid)
		gt.Value(t, got.Rev).Equal(int64(1)) // insert of Rev=0 stores Rev=1.
		gt.Value(t, got.Status).Equal(agentkit.ProcessPending)

		// Update with correct Rev -> Rev+1.
		upd := got
		upd.Status = agentkit.ProcessRunning
		gt.NoError(t, repo.Apply(ctx, agentkit.ChangeSet{Processes: []*agentkit.Process{upd}}))
		got2, err := repo.GetProcess(ctx, pid)
		gt.NoError(t, err)
		gt.Value(t, got2.Rev).Equal(int64(2))
		gt.Value(t, got2.Status).Equal(agentkit.ProcessRunning)

		// Update with wrong Rev -> ErrConflict, nothing written.
		stale := mkProc(pid)
		stale.Rev = 0 // stored is 2.
		stale.Status = agentkit.ProcessFailed
		err = repo.Apply(ctx, agentkit.ChangeSet{Processes: []*agentkit.Process{stale}})
		gt.Error(t, err).Is(agentkit.ErrConflict)
		got3, err := repo.GetProcess(ctx, pid)
		gt.NoError(t, err)
		gt.Value(t, got3.Rev).Equal(int64(2))
		gt.Value(t, got3.Status).Equal(agentkit.ProcessRunning)
	})

	t.Run("GetAbsentReturnsNotFound", func(t *testing.T) {
		repo := factory(t)
		_, err := repo.GetProcess(ctx, newPID())
		gt.Error(t, err).Is(agentkit.ErrProcessNotFound)
		_, err = repo.FindProcessByIdempotencyKey(ctx, uniqueStr("nope"))
		gt.Error(t, err).Is(agentkit.ErrProcessNotFound)
		_, err = repo.FindOpenProcessBySubject(ctx, agentkit.SubjectRef{Kind: "k", ID: uniqueStr("s")})
		gt.Error(t, err).Is(agentkit.ErrProcessNotFound)
	})

	t.Run("ApplyAtomicityRevMismatch", func(t *testing.T) {
		repo := factory(t)
		p1, p2 := newPID(), newPID()
		gt.NoError(t, repo.Apply(ctx, agentkit.ChangeSet{Processes: []*agentkit.Process{mkProc(p1), mkProc(p2)}}))

		// Same ChangeSet: p1 valid (Rev 1), p2 stale (Rev 0, stored 1).
		u1 := mkProc(p1)
		u1.Rev = 1
		u1.Status = agentkit.ProcessRunning
		u2 := mkProc(p2)
		u2.Rev = 0 // wrong.
		u2.Status = agentkit.ProcessRunning
		err := repo.Apply(ctx, agentkit.ChangeSet{Processes: []*agentkit.Process{u1, u2}})
		gt.Error(t, err).Is(agentkit.ErrConflict)

		// Neither row was written: both still Rev 1 and pending.
		g1, err := repo.GetProcess(ctx, p1)
		gt.NoError(t, err)
		gt.Value(t, g1.Rev).Equal(int64(1))
		gt.Value(t, g1.Status).Equal(agentkit.ProcessPending)
		g2, err := repo.GetProcess(ctx, p2)
		gt.NoError(t, err)
		gt.Value(t, g2.Rev).Equal(int64(1))
		gt.Value(t, g2.Status).Equal(agentkit.ProcessPending)
	})

	t.Run("ApplyAtomicityUniquenessWithinChangeSet", func(t *testing.T) {
		repo := factory(t)
		key := uniqueStr("idem")
		p1, p2 := newPID(), newPID()
		a := mkProc(p1)
		a.IdempotencyKey = key
		b := mkProc(p2)
		b.IdempotencyKey = key // collides within the same ChangeSet.
		err := repo.Apply(ctx, agentkit.ChangeSet{Processes: []*agentkit.Process{a, b}})
		gt.Error(t, err).Is(agentkit.ErrConflict)

		// Nothing written.
		_, err = repo.GetProcess(ctx, p1)
		gt.Error(t, err).Is(agentkit.ErrProcessNotFound)
		_, err = repo.GetProcess(ctx, p2)
		gt.Error(t, err).Is(agentkit.ErrProcessNotFound)
	})

	t.Run("Guards", func(t *testing.T) {
		repo := factory(t)
		child, parent := newPID(), newPID()
		gt.NoError(t, repo.Apply(ctx, agentkit.ChangeSet{Processes: []*agentkit.Process{mkProc(child), mkProc(parent)}}))
		// Both now Rev 1.

		// Matching guard passes; the guarded child's Rev is NOT advanced.
		up := mkProc(parent)
		up.Rev = 1
		up.Status = agentkit.ProcessWaiting
		gt.NoError(t, repo.Apply(ctx, agentkit.ChangeSet{
			Guards:    []agentkit.ProcessGuard{{ProcessID: child, Rev: 1}},
			Processes: []*agentkit.Process{up},
		}))
		gc, err := repo.GetProcess(ctx, child)
		gt.NoError(t, err)
		gt.Value(t, gc.Rev).Equal(int64(1)) // guard did not write.
		gp, err := repo.GetProcess(ctx, parent)
		gt.NoError(t, err)
		gt.Value(t, gp.Rev).Equal(int64(2))

		// Mismatched guard -> ErrConflict, nothing written.
		up2 := mkProc(parent)
		up2.Rev = 2
		up2.Status = agentkit.ProcessRunning
		err = repo.Apply(ctx, agentkit.ChangeSet{
			Guards:    []agentkit.ProcessGuard{{ProcessID: child, Rev: 999}},
			Processes: []*agentkit.Process{up2},
		})
		gt.Error(t, err).Is(agentkit.ErrConflict)
		gp2, err := repo.GetProcess(ctx, parent)
		gt.NoError(t, err)
		gt.Value(t, gp2.Rev).Equal(int64(2)) // parent untouched.
		gt.Value(t, gp2.Status).Equal(agentkit.ProcessWaiting)
	})

	t.Run("UniquenessIdempotencyAcrossApplies", func(t *testing.T) {
		repo := factory(t)
		key := uniqueStr("idem")
		p1, p2 := newPID(), newPID()
		a := mkProc(p1)
		a.IdempotencyKey = key
		gt.NoError(t, repo.Apply(ctx, agentkit.ChangeSet{Processes: []*agentkit.Process{a}}))

		found, err := repo.FindProcessByIdempotencyKey(ctx, key)
		gt.NoError(t, err)
		gt.Value(t, found.ID).Equal(p1)

		b := mkProc(p2)
		b.IdempotencyKey = key
		err = repo.Apply(ctx, agentkit.ChangeSet{Processes: []*agentkit.Process{b}})
		gt.Error(t, err).Is(agentkit.ErrConflict)
		_, err = repo.GetProcess(ctx, p2)
		gt.Error(t, err).Is(agentkit.ErrProcessNotFound)
	})

	t.Run("UniquenessOpenSubject", func(t *testing.T) {
		repo := factory(t)
		subj := agentkit.SubjectRef{Kind: "turn", ID: uniqueStr("s")}
		p1, p2, p3 := newPID(), newPID(), newPID()

		a := mkProc(p1)
		a.Subject = &subj
		gt.NoError(t, repo.Apply(ctx, agentkit.ChangeSet{Processes: []*agentkit.Process{a}}))

		found, err := repo.FindOpenProcessBySubject(ctx, subj)
		gt.NoError(t, err)
		gt.Value(t, found.ID).Equal(p1)

		// Second open Process on the same subject -> conflict.
		b := mkProc(p2)
		b.Subject = &subj
		err = repo.Apply(ctx, agentkit.ChangeSet{Processes: []*agentkit.Process{b}})
		gt.Error(t, err).Is(agentkit.ErrConflict)
		_, err = repo.GetProcess(ctx, p2)
		gt.Error(t, err).Is(agentkit.ErrProcessNotFound)

		// Close p1 (terminal) -> subject is freed.
		cl := found
		cl.Status = agentkit.ProcessSucceeded
		gt.NoError(t, repo.Apply(ctx, agentkit.ChangeSet{Processes: []*agentkit.Process{cl}}))
		_, err = repo.FindOpenProcessBySubject(ctx, subj)
		gt.Error(t, err).Is(agentkit.ErrProcessNotFound) // no open holder now.

		// A new open Process may take the freed subject.
		c := mkProc(p3)
		c.Subject = &subj
		gt.NoError(t, repo.Apply(ctx, agentkit.ChangeSet{Processes: []*agentkit.Process{c}}))
		found3, err := repo.FindOpenProcessBySubject(ctx, subj)
		gt.NoError(t, err)
		gt.Value(t, found3.ID).Equal(p3)
	})

	t.Run("AwaitUpsertUniqueKey", func(t *testing.T) {
		repo := factory(t)
		pid := newPID()
		gt.NoError(t, repo.Apply(ctx, agentkit.ChangeSet{Processes: []*agentkit.Process{mkProc(pid)}}))
		key := agentkit.AwaitKey(uniqueStr("await"))

		a1 := &agentkit.Await{ProcessID: pid, Key: key, Kind: agentkit.AwaitQuestion, Status: agentkit.AwaitOpen, CreatedAt: time.Now()}
		gt.NoError(t, repo.Apply(ctx, agentkit.ChangeSet{Awaits: []*agentkit.Await{a1}}))

		// Upsert the same (ProcessID, Key): it replaces, it does not duplicate.
		a2 := &agentkit.Await{ProcessID: pid, Key: key, Kind: agentkit.AwaitQuestion, Status: agentkit.AwaitResponded, Response: []byte("yes"), CreatedAt: a1.CreatedAt}
		gt.NoError(t, repo.Apply(ctx, agentkit.ChangeSet{Awaits: []*agentkit.Await{a2}}))

		awaits, err := repo.ListAwaits(ctx, pid)
		gt.NoError(t, err)
		gt.Array(t, awaits).Length(1) // uniqueness maintained via upsert.
		gt.Value(t, awaits[0].Status).Equal(agentkit.AwaitResponded)
		gt.Value(t, string(awaits[0].Response)).Equal("yes")
	})

	t.Run("ListEventsAppendOrder", func(t *testing.T) {
		repo := factory(t)
		pid := newPID()
		gt.NoError(t, repo.Apply(ctx, agentkit.ChangeSet{Processes: []*agentkit.Process{mkProc(pid)}}))

		e1 := &agentkit.Event{ProcessID: pid, Type: agentkit.EventProcessCreated, Payload: []byte("1"), At: time.Now()}
		e2 := &agentkit.Event{ProcessID: pid, Type: agentkit.EventAwaitCreated, Payload: []byte("2"), At: time.Now()}
		gt.NoError(t, repo.Apply(ctx, agentkit.ChangeSet{Events: []*agentkit.Event{e1, e2}}))
		e3 := &agentkit.Event{ProcessID: pid, Type: agentkit.EventProcessFinished, Payload: []byte("3"), At: time.Now()}
		gt.NoError(t, repo.Apply(ctx, agentkit.ChangeSet{Events: []*agentkit.Event{e3}}))

		events, err := repo.ListEvents(ctx, pid)
		gt.NoError(t, err)
		gt.Array(t, events).Length(3)
		gt.Value(t, string(events[0].Payload)).Equal("1")
		gt.Value(t, string(events[1].Payload)).Equal("2")
		gt.Value(t, string(events[2].Payload)).Equal("3")
	})

	t.Run("ClaimPending", func(t *testing.T) {
		repo := factory(t)
		pid := newPID()
		gt.NoError(t, repo.Apply(ctx, agentkit.ChangeSet{Processes: []*agentkit.Process{mkProc(pid)}}))

		now := time.Now()
		claimed, err := repo.ClaimNextProcess(ctx, "worker-1", now.Add(time.Hour), now)
		gt.NoError(t, err)
		gt.NotNil(t, claimed)
		gt.Value(t, claimed.ID).Equal(pid)
		gt.Value(t, claimed.Status).Equal(agentkit.ProcessRunning)
		gt.Value(t, claimed.LeaseOwner).Equal("worker-1")
		gt.Bool(t, claimed.LeaseToken != "").True()
		gt.NotNil(t, claimed.LeaseUntil)

		// No more runnable targets.
		none, err := repo.ClaimNextProcess(ctx, "worker-1", now.Add(time.Hour), now)
		gt.NoError(t, err)
		gt.Nil(t, none)
	})

	t.Run("ClaimWaitingWakeAt", func(t *testing.T) {
		repo := factory(t)
		now := time.Now()

		// waiting, WakeAt in the past -> claimable.
		due := newPID()
		pDue := mkProc(due)
		pDue.Status = agentkit.ProcessWaiting
		past := now.Add(-time.Minute)
		pDue.WakeAt = &past
		// waiting, WakeAt in the future -> NOT claimable.
		notDue := newPID()
		pNot := mkProc(notDue)
		pNot.Status = agentkit.ProcessWaiting
		future := now.Add(time.Hour)
		pNot.WakeAt = &future
		gt.NoError(t, repo.Apply(ctx, agentkit.ChangeSet{Processes: []*agentkit.Process{pDue, pNot}}))

		claimed, err := repo.ClaimNextProcess(ctx, "w", now.Add(time.Hour), now)
		gt.NoError(t, err)
		gt.NotNil(t, claimed)
		gt.Value(t, claimed.ID).Equal(due)

		// The future-wake one is still not claimable.
		none, err := repo.ClaimNextProcess(ctx, "w", now.Add(time.Hour), now)
		gt.NoError(t, err)
		gt.Nil(t, none)
	})

	t.Run("ClaimRunningLeaseExpiryAndReclaimToken", func(t *testing.T) {
		repo := factory(t)
		pid := newPID()
		gt.NoError(t, repo.Apply(ctx, agentkit.ChangeSet{Processes: []*agentkit.Process{mkProc(pid)}}))

		now1 := time.Now()
		// Claim with an already-expired lease so the row is reclaimable at now2.
		c1, err := repo.ClaimNextProcess(ctx, "w", now1.Add(-time.Second), now1)
		gt.NoError(t, err)
		gt.NotNil(t, c1)

		now2 := now1.Add(time.Second)
		c2, err := repo.ClaimNextProcess(ctx, "w", now2.Add(time.Hour), now2)
		gt.NoError(t, err)
		gt.NotNil(t, c2) // running with expired lease is reclaimed.
		gt.Value(t, c2.ID).Equal(pid)
		gt.Bool(t, c1.LeaseToken != c2.LeaseToken).True() // fresh token every claim.
	})

	// Contract 4: a claim that takes over a running row — i.e. one whose previous
	// claim died mid-transition — increments UncleanReclaims. This is what bounds
	// re-execution after a crash, so an implementation that skips it degrades to
	// unbounded replay.
	t.Run("ClaimUncleanReclaimCounts", func(t *testing.T) {
		repo := factory(t)
		pid := newPID()
		gt.NoError(t, repo.Apply(ctx, agentkit.ChangeSet{Processes: []*agentkit.Process{mkProc(pid)}}))

		now1 := time.Now()
		// A first claim from pending: clean, so nothing is counted.
		c1, err := repo.ClaimNextProcess(ctx, "w", now1.Add(-time.Second), now1)
		gt.NoError(t, err)
		gt.NotNil(t, c1)
		gt.Value(t, c1.UncleanReclaims).Equal(0)

		// Taking over the running row counts once.
		now2 := now1.Add(time.Second)
		c2, err := repo.ClaimNextProcess(ctx, "w", now2.Add(-time.Second), now2)
		gt.NoError(t, err)
		gt.NotNil(t, c2)
		gt.Value(t, c2.UncleanReclaims).Equal(1)
		gt.Value(t, c2.StepAttempts).Equal(0) // never touched by a claim.

		// And again, cumulatively.
		now3 := now2.Add(time.Second)
		c3, err := repo.ClaimNextProcess(ctx, "w", now3.Add(time.Hour), now3)
		gt.NoError(t, err)
		gt.NotNil(t, c3)
		gt.Value(t, c3.UncleanReclaims).Equal(2)
		gt.Value(t, c3.StepAttempts).Equal(0)
	})

	t.Run("ClaimCleanDoesNotCountUnclean", func(t *testing.T) {
		repo := factory(t)
		now := time.Now()

		// pending -> clean.
		pend := newPID()
		// waiting with a due WakeAt -> also clean.
		wait := newPID()
		pWait := mkProc(wait)
		pWait.Status = agentkit.ProcessWaiting
		past := now.Add(-time.Minute)
		pWait.WakeAt = &past
		// A stored StepAttempts must survive a claim untouched.
		pWait.StepAttempts = 2
		gt.NoError(t, repo.Apply(ctx, agentkit.ChangeSet{
			Processes: []*agentkit.Process{mkProc(pend), pWait},
		}))

		for range 2 {
			claimed, err := repo.ClaimNextProcess(ctx, "w", now.Add(time.Hour), now)
			gt.NoError(t, err)
			gt.NotNil(t, claimed)
			gt.Value(t, claimed.UncleanReclaims).Equal(0)
			if claimed.ID == wait {
				gt.Value(t, claimed.StepAttempts).Equal(2)
			}
		}
	})

	// Both counters are ordinary persisted fields: the worker clears them on a
	// successful commit, so a Repository must round-trip whatever it is given.
	t.Run("AttemptCountersRoundTrip", func(t *testing.T) {
		repo := factory(t)
		pid := newPID()
		p := mkProc(pid)
		p.StepAttempts = 3
		p.UncleanReclaims = 2
		gt.NoError(t, repo.Apply(ctx, agentkit.ChangeSet{Processes: []*agentkit.Process{p}}))

		got, err := repo.GetProcess(ctx, pid)
		gt.NoError(t, err)
		gt.Value(t, got.StepAttempts).Equal(3)
		gt.Value(t, got.UncleanReclaims).Equal(2)

		got.StepAttempts = 0
		got.UncleanReclaims = 0
		gt.NoError(t, repo.Apply(ctx, agentkit.ChangeSet{Processes: []*agentkit.Process{got}}))

		back, err := repo.GetProcess(ctx, pid)
		gt.NoError(t, err)
		gt.Value(t, back.StepAttempts).Equal(0)
		gt.Value(t, back.UncleanReclaims).Equal(0)
	})

	t.Run("ClaimLiveLeaseNotClaimed", func(t *testing.T) {
		repo := factory(t)
		pid := newPID()
		gt.NoError(t, repo.Apply(ctx, agentkit.ChangeSet{Processes: []*agentkit.Process{mkProc(pid)}}))

		now := time.Now()
		c1, err := repo.ClaimNextProcess(ctx, "w", now.Add(time.Hour), now)
		gt.NoError(t, err)
		gt.NotNil(t, c1)

		// Live lease fences the row.
		c2, err := repo.ClaimNextProcess(ctx, "w", now.Add(time.Hour), now)
		gt.NoError(t, err)
		gt.Nil(t, c2)
	})

	t.Run("ClaimConcurrentNoDoubleClaim", func(t *testing.T) {
		repo := factory(t)
		const n = 100
		base := time.Now()
		ids := make(map[agentkit.ProcessID]bool, n)
		cs := agentkit.ChangeSet{}
		for i := 0; i < n; i++ {
			pid := newPID()
			ids[pid] = true
			p := mkProc(pid)
			p.CreatedAt = base.Add(time.Duration(i) * time.Millisecond)
			cs.Processes = append(cs.Processes, p)
		}
		gt.NoError(t, repo.Apply(ctx, cs))

		now := time.Now()
		leaseUntil := now.Add(time.Hour)
		var wg sync.WaitGroup
		results := make(chan *agentkit.Process, n)
		errCh := make(chan error, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				p, err := repo.ClaimNextProcess(ctx, "w", leaseUntil, now)
				if err != nil {
					errCh <- err
					return
				}
				results <- p
			}()
		}
		wg.Wait()
		close(results)
		close(errCh)

		for err := range errCh {
			gt.NoError(t, err)
		}
		seenID := make(map[agentkit.ProcessID]bool, n)
		seenTok := make(map[string]bool, n)
		count := 0
		for p := range results {
			if p == nil {
				continue
			}
			count++
			gt.Bool(t, ids[p.ID]).True()              // claimed one of ours.
			gt.Bool(t, seenID[p.ID]).False()          // never double-claimed.
			gt.Bool(t, seenTok[p.LeaseToken]).False() // distinct token per claim.
			seenID[p.ID] = true
			seenTok[p.LeaseToken] = true
			gt.Value(t, p.Status).Equal(agentkit.ProcessRunning)
		}
		gt.Value(t, count).Equal(n) // every Process claimed exactly once.
	})

	t.Run("DeepCopyProcess", func(t *testing.T) {
		repo := factory(t)
		pid := newPID()
		p := mkProc(pid)
		p.Metadata = map[string]string{"tenant": "acme"}
		p.Output = []byte("orig")
		subj := agentkit.SubjectRef{Kind: "turn", ID: uniqueStr("s")}
		p.Subject = &subj
		gt.NoError(t, repo.Apply(ctx, agentkit.ChangeSet{Processes: []*agentkit.Process{p}}))

		got, err := repo.GetProcess(ctx, pid)
		gt.NoError(t, err)
		got.Status = agentkit.ProcessFailed
		got.Metadata["tenant"] = "evil"
		got.Output[0] = 'X'
		got.Subject.ID = "mutated"

		again, err := repo.GetProcess(ctx, pid)
		gt.NoError(t, err)
		gt.Value(t, again.Status).Equal(agentkit.ProcessPending)
		gt.Value(t, again.Metadata["tenant"]).Equal("acme")
		gt.Value(t, string(again.Output)).Equal("orig")
		gt.Value(t, again.Subject.ID).Equal(subj.ID)
	})

	t.Run("DeepCopyAwaitAndEvent", func(t *testing.T) {
		repo := factory(t)
		pid := newPID()
		gt.NoError(t, repo.Apply(ctx, agentkit.ChangeSet{Processes: []*agentkit.Process{mkProc(pid)}}))
		key := agentkit.AwaitKey(uniqueStr("await"))
		gt.NoError(t, repo.Apply(ctx, agentkit.ChangeSet{
			Awaits: []*agentkit.Await{{ProcessID: pid, Key: key, Kind: agentkit.AwaitQuestion, Status: agentkit.AwaitOpen, Response: []byte("orig"), CreatedAt: time.Now()}},
			Events: []*agentkit.Event{{ProcessID: pid, Type: agentkit.EventProcessCreated, Payload: []byte("orig"), At: time.Now()}},
		}))

		awaits, err := repo.ListAwaits(ctx, pid)
		gt.NoError(t, err)
		gt.Array(t, awaits).Length(1)
		awaits[0].Status = agentkit.AwaitExpired
		awaits[0].Response[0] = 'X'

		events, err := repo.ListEvents(ctx, pid)
		gt.NoError(t, err)
		gt.Array(t, events).Length(1)
		events[0].Payload[0] = 'X'

		a2, err := repo.ListAwaits(ctx, pid)
		gt.NoError(t, err)
		gt.Value(t, a2[0].Status).Equal(agentkit.AwaitOpen)
		gt.Value(t, string(a2[0].Response)).Equal("orig")
		e2, err := repo.ListEvents(ctx, pid)
		gt.NoError(t, err)
		gt.Value(t, string(e2[0].Payload)).Equal("orig")
	})
}
