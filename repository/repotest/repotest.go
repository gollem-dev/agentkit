// Package repotest is a contract conformance suite for agentkit.Repository
// implementations. The bundled memory and filesystem implementations call it
// from their tests, and external implementers (e.g. a postgres Repository) can
// run it against their own implementation.
package repotest

import (
	"context"
	"errors"
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

// mkEvent stands in for the kernel, which is what mints an Event's ID — the
// suite drives Apply directly, so it has to supply one the same way.
func mkEvent(pid agentkit.ProcessID, typ agentkit.EventType, payload string) *agentkit.Event {
	return &agentkit.Event{
		ID:        agentkit.EventID(uniqueStr("event")),
		ProcessID: pid,
		Type:      typ,
		Payload:   []byte(payload),
		At:        time.Now(),
	}
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

		e1 := mkEvent(pid, agentkit.EventProcessCreated, "1")
		e2 := mkEvent(pid, agentkit.EventAwaitCreated, "2")
		gt.NoError(t, repo.Apply(ctx, agentkit.ChangeSet{Events: []*agentkit.Event{e1, e2}}))
		e3 := mkEvent(pid, agentkit.EventProcessFinished, "3")
		gt.NoError(t, repo.Apply(ctx, agentkit.ChangeSet{Events: []*agentkit.Event{e3}}))

		events, err := repo.ListEvents(ctx, pid, agentkit.EventQuery{})
		gt.NoError(t, err)
		gt.Array(t, events).Length(3)
		gt.Value(t, string(events[0].Payload)).Equal("1")
		gt.Value(t, string(events[1].Payload)).Equal("2")
		gt.Value(t, string(events[2].Payload)).Equal("3")
	})

	// The ID is the handle a caller resumes from, so an implementation that
	// mints or rewrites it would resume the caller at the wrong place.
	t.Run("ListEventsRoundTripsID", func(t *testing.T) {
		repo := factory(t)
		pid := newPID()
		gt.NoError(t, repo.Apply(ctx, agentkit.ChangeSet{Processes: []*agentkit.Process{mkProc(pid)}}))

		e1, e2 := mkEvent(pid, agentkit.EventProcessCreated, "1"), mkEvent(pid, agentkit.EventAwaitCreated, "2")
		gt.NoError(t, repo.Apply(ctx, agentkit.ChangeSet{Events: []*agentkit.Event{e1, e2}}))

		events, err := repo.ListEvents(ctx, pid, agentkit.EventQuery{})
		gt.NoError(t, err)
		gt.Array(t, events).Length(2)
		gt.Value(t, events[0].ID).Equal(e1.ID)
		gt.Value(t, events[1].ID).Equal(e2.ID)
		gt.Value(t, events[0].ID).NotEqual(events[1].ID)
	})

	t.Run("ListEventsCursor", func(t *testing.T) {
		repo := factory(t)
		pid := newPID()
		gt.NoError(t, repo.Apply(ctx, agentkit.ChangeSet{Processes: []*agentkit.Process{mkProc(pid)}}))

		e1 := mkEvent(pid, agentkit.EventProcessCreated, "1")
		e2 := mkEvent(pid, agentkit.EventAwaitCreated, "2")
		e3 := mkEvent(pid, agentkit.EventProcessFinished, "3")
		gt.NoError(t, repo.Apply(ctx, agentkit.ChangeSet{Events: []*agentkit.Event{e1, e2, e3}}))

		t.Run("after is exclusive", func(t *testing.T) {
			events, err := repo.ListEvents(ctx, pid, agentkit.EventQuery{After: e1.ID})
			gt.NoError(t, err)
			gt.Array(t, events).Length(2)
			gt.Value(t, string(events[0].Payload)).Equal("2")
			gt.Value(t, string(events[1].Payload)).Equal("3")
		})
		t.Run("after the last event is empty, not an error", func(t *testing.T) {
			events, err := repo.ListEvents(ctx, pid, agentkit.EventQuery{After: e3.ID})
			gt.NoError(t, err)
			gt.Array(t, events).Length(0)
		})
		t.Run("limit caps the count", func(t *testing.T) {
			events, err := repo.ListEvents(ctx, pid, agentkit.EventQuery{Limit: 2})
			gt.NoError(t, err)
			gt.Array(t, events).Length(2)
			gt.Value(t, string(events[1].Payload)).Equal("2")
		})
		t.Run("limit larger than the remainder returns the remainder", func(t *testing.T) {
			events, err := repo.ListEvents(ctx, pid, agentkit.EventQuery{After: e2.ID, Limit: 99})
			gt.NoError(t, err)
			gt.Array(t, events).Length(1)
		})
		t.Run("limit <= 0 is uncapped", func(t *testing.T) {
			events, err := repo.ListEvents(ctx, pid, agentkit.EventQuery{Limit: -1})
			gt.NoError(t, err)
			gt.Array(t, events).Length(3)
		})
		t.Run("an unknown cursor is ErrEventNotFound with no events", func(t *testing.T) {
			events, err := repo.ListEvents(ctx, pid, agentkit.EventQuery{After: "no-such-event"})
			gt.Error(t, err).Is(agentkit.ErrEventNotFound)
			gt.Array(t, events).Length(0)
		})
	})

	// A children await carries each child's usage, which is how a parent's
	// Limiter sees what its subtree spent after a restart.
	t.Run("ChildResultMetricsRoundTripAndDeepCopy", func(t *testing.T) {
		repo := factory(t)
		pid := newPID()
		gt.NoError(t, repo.Apply(ctx, agentkit.ChangeSet{Processes: []*agentkit.Process{mkProc(pid)}}))

		// Every counter gets its own distinct value, so a mapper that assigns the
		// wrong column to the wrong field cannot pass by coincidence.
		wantMetrics := agentkit.Metrics{
			InputTokens: 1, OutputTokens: 2, CacheReadInputTokens: 3, CacheCreationInputTokens: 4,
			LLMCalls: 5, ToolCalls: 6, Steps: 7, Spawns: 8,
		}
		key := agentkit.AwaitKey(uniqueStr("kids"))
		kid := newPID()
		gt.NoError(t, repo.Apply(ctx, agentkit.ChangeSet{
			Awaits: []*agentkit.Await{{
				ProcessID: pid, Key: key, Kind: agentkit.AwaitChildren,
				Status: agentkit.AwaitResponded, Children: []agentkit.ProcessID{kid},
				Results: []agentkit.ChildResult{{
					ProcessID: kid,
					Status:    agentkit.ProcessSucceeded,
					Metrics:   wantMetrics,
				}},
				CreatedAt: time.Now(),
			}},
		}))

		got, err := repo.ListAwaits(ctx, pid)
		gt.NoError(t, err)
		gt.Array(t, got).Length(1)
		gt.Value(t, got[0].Results[0].Metrics).Equal(wantMetrics)

		// Reads must not alias stored state. Metrics is a struct of scalars now, so
		// an implementation that copies the ChildResult at all satisfies this; the
		// case still runs because one returning a pointer into its own storage
		// would not.
		got[0].Results[0].Metrics.LLMCalls = 999
		again, err := repo.ListAwaits(ctx, pid)
		gt.NoError(t, err)
		gt.Value(t, again[0].Results[0].Metrics).Equal(wantMetrics)
	})

	// Process.Metrics is the committed cumulative usage a root Limiter reads
	// after a restart; an implementor storing it as discrete columns rather than
	// a JSON blob must round-trip all eight counters, not just the ones an
	// earlier fixture happened to touch.
	t.Run("ProcessMetricsRoundTrip", func(t *testing.T) {
		repo := factory(t)
		pid := newPID()
		p := mkProc(pid)
		p.Metrics = agentkit.Metrics{
			InputTokens: 11, OutputTokens: 22, CacheReadInputTokens: 33, CacheCreationInputTokens: 44,
			LLMCalls: 55, ToolCalls: 66, Steps: 77, Spawns: 88,
		}
		gt.NoError(t, repo.Apply(ctx, agentkit.ChangeSet{Processes: []*agentkit.Process{p}}))

		got, err := repo.GetProcess(ctx, pid)
		gt.NoError(t, err)
		gt.Value(t, got.Metrics).Equal(p.Metrics)
	})

	t.Run("ListEventsOnProcessWithNoEvents", func(t *testing.T) {
		repo := factory(t)
		pid := newPID()
		gt.NoError(t, repo.Apply(ctx, agentkit.ChangeSet{Processes: []*agentkit.Process{mkProc(pid)}}))

		events, err := repo.ListEvents(ctx, pid, agentkit.EventQuery{})
		gt.NoError(t, err)
		gt.Array(t, events).Length(0)

		_, err = repo.ListEvents(ctx, pid, agentkit.EventQuery{After: "any"})
		gt.Error(t, err).Is(agentkit.ErrEventNotFound)
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

	t.Run("ClaimPendingWakeAt", func(t *testing.T) {
		repo := factory(t)
		now := time.Now()

		// pending, WakeAt in the future -> NOT claimable. This is the retry
		// backoff: the worker writes it when it puts a failed transition back.
		backoff := newPID()
		pBackoff := mkProc(backoff)
		past := now.Add(-time.Minute)
		future := now.Add(time.Hour)
		pBackoff.WakeAt = &future
		// pending, WakeAt in the past -> claimable (the backoff elapsed).
		elapsed := newPID()
		pElapsed := mkProc(elapsed)
		pElapsed.WakeAt = &past
		gt.NoError(t, repo.Apply(ctx, agentkit.ChangeSet{
			Processes: []*agentkit.Process{pBackoff, pElapsed},
		}))

		claimed, err := repo.ClaimNextProcess(ctx, "w", now.Add(time.Hour), now)
		gt.NoError(t, err)
		gt.NotNil(t, claimed)
		gt.Value(t, claimed.ID).Equal(elapsed)

		// The one still in backoff is not claimable.
		none, err := repo.ClaimNextProcess(ctx, "w", now.Add(time.Hour), now)
		gt.NoError(t, err)
		gt.Nil(t, none)

		// Once its wake time is due it becomes a target, without being written to.
		later, err := repo.ClaimNextProcess(ctx, "w", future.Add(time.Hour), future.Add(time.Second))
		gt.NoError(t, err)
		gt.NotNil(t, later)
		gt.Value(t, later.ID).Equal(backoff)
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

	// HistoryRef names the committed version of a Process's conversation History
	// in its HistoryStore. It is the pointer that makes History roll back with
	// State (ADR-0017), so a Repository that drops it would silently resurrect a
	// superseded conversation.
	t.Run("HistoryRefRoundTrip", func(t *testing.T) {
		repo := factory(t)
		pid := newPID()
		p := mkProc(pid)
		gt.Value(t, p.HistoryRef).Equal(agentkit.HistoryRef("")) // zero value: nothing committed yet.

		p.HistoryRef = agentkit.HistoryRef("version-" + string(pid))
		gt.NoError(t, repo.Apply(ctx, agentkit.ChangeSet{Processes: []*agentkit.Process{p}}))

		got, err := repo.GetProcess(ctx, pid)
		gt.NoError(t, err)
		gt.Value(t, got.HistoryRef).Equal(agentkit.HistoryRef("version-" + string(pid)))

		// And a later transition can replace it with another version.
		got.HistoryRef = agentkit.HistoryRef("next-" + string(pid))
		gt.NoError(t, repo.Apply(ctx, agentkit.ChangeSet{Processes: []*agentkit.Process{got}}))

		back, err := repo.GetProcess(ctx, pid)
		gt.NoError(t, err)
		gt.Value(t, back.HistoryRef).Equal(agentkit.HistoryRef("next-" + string(pid)))

		// Empty must come back empty, not as a stale value: a store that maps ""
		// to NULL and NULL to "the previous ref" would resurrect a released
		// version.
		back.HistoryRef = ""
		gt.NoError(t, repo.Apply(ctx, agentkit.ChangeSet{Processes: []*agentkit.Process{back}}))

		cleared, err := repo.GetProcess(ctx, pid)
		gt.NoError(t, err)
		gt.Value(t, cleared.HistoryRef).Equal(agentkit.HistoryRef(""))
	})

	// InheritedHistory names the version another Process committed that this one
	// started its conversation from. A Repository that drops it does not fail
	// loudly: the Process simply starts from an empty conversation, and the model
	// answers without the transcript it was supposed to continue
	// ([ADR-0017](../../../docs/adr/0017-history-is-an-immutable-versioned-store.md)).
	t.Run("InheritedHistoryRoundTrip", func(t *testing.T) {
		repo := factory(t)
		pid := newPID()
		p := mkProc(pid)
		gt.Nil(t, p.InheritedHistory) // nil: this Process inherited nothing.

		p.InheritedHistory = &agentkit.InheritedHistory{
			Process: agentkit.ProcessID("issuer-" + string(pid)),
			Ref:     agentkit.HistoryRef("inherited-" + string(pid)),
		}
		gt.NoError(t, repo.Apply(ctx, agentkit.ChangeSet{Processes: []*agentkit.Process{p}}))

		got, err := repo.GetProcess(ctx, pid)
		gt.NoError(t, err)
		gt.NotNil(t, got.InheritedHistory)
		gt.Value(t, *got.InheritedHistory).Equal(agentkit.InheritedHistory{
			Process: agentkit.ProcessID("issuer-" + string(pid)),
			Ref:     agentkit.HistoryRef("inherited-" + string(pid)),
		})

		// It survives later transitions unchanged: the kernel writes it once, at
		// Spawn, and every commit after that carries it along.
		got.HistoryRef = agentkit.HistoryRef("own-" + string(pid))
		gt.NoError(t, repo.Apply(ctx, agentkit.ChangeSet{Processes: []*agentkit.Process{got}}))

		back, err := repo.GetProcess(ctx, pid)
		gt.NoError(t, err)
		gt.NotNil(t, back.InheritedHistory)
		gt.Value(t, back.InheritedHistory.Ref).Equal(agentkit.HistoryRef("inherited-" + string(pid)))
		gt.Value(t, back.HistoryRef).Equal(agentkit.HistoryRef("own-" + string(pid)))

		// Reads must not alias stored state (the deep-copy rule reaches nested
		// pointers, not just the top-level row).
		back.InheritedHistory.Ref = "tampered"
		again, err := repo.GetProcess(ctx, pid)
		gt.NoError(t, err)
		gt.Value(t, again.InheritedHistory.Ref).Equal(agentkit.HistoryRef("inherited-" + string(pid)))
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

	// ClaimNextProcess and Apply must be mutually linearizable on the same row: a
	// poll claim and an eager Apply-claim that both start from Rev N cannot both
	// succeed. This is what lets eager dispatch claim a pending row via Apply
	// without a dedicated SPI, racing a poller safely (repository.go contract 4).
	t.Run("ClaimVsApplyLinearizable", func(t *testing.T) {
		repo := factory(t)
		const rounds = 50
		for i := 0; i < rounds; i++ {
			pid := newPID()
			gt.NoError(t, repo.Apply(ctx, agentkit.ChangeSet{Processes: []*agentkit.Process{mkProc(pid)}}))

			cur, err := repo.GetProcess(ctx, pid) // Rev 1, pending.
			gt.NoError(t, err)
			now := time.Now()
			lu := now.Add(time.Hour)
			cur.Status = agentkit.ProcessRunning
			cur.LeaseOwner = "eager"
			cur.LeaseToken = uniqueStr("tok")
			cur.LeaseUntil = &lu
			cur.UpdatedAt = now // Rev stays at the read value so Apply CAS races the claim.

			// A start barrier so both operations genuinely race from the same Rev,
			// rather than possibly running in sequence (which would pass vacuously).
			start := make(chan struct{})
			var wg sync.WaitGroup
			var claimProc *agentkit.Process
			var claimErr, applyErr error
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				claimProc, claimErr = repo.ClaimNextProcess(ctx, "poller", lu, now)
			}()
			go func() {
				defer wg.Done()
				<-start
				applyErr = repo.Apply(ctx, agentkit.ChangeSet{Processes: []*agentkit.Process{cur}})
			}()
			close(start)
			wg.Wait()

			gt.NoError(t, claimErr) // ClaimNextProcess never errors here (nil,nil if nothing).
			claimWon := claimProc != nil && claimProc.ID == pid
			applyWon := applyErr == nil

			// Exactly one path won the row.
			won := 0
			if claimWon {
				won++
			}
			if applyWon {
				won++
			}
			gt.Value(t, won).Equal(1)

			// The loser must lose in the contract-defined way, not by some other
			// error: an implementation that fails Apply with a non-ErrConflict error
			// while the claim wins would otherwise satisfy `won == 1` and slip past.
			if applyWon {
				gt.Bool(t, claimProc == nil).True() // claim saw the row already running.
			} else {
				gt.Bool(t, errors.Is(applyErr, agentkit.ErrConflict)).True() // Apply lost via Rev CAS.
			}

			final, err := repo.GetProcess(ctx, pid)
			gt.NoError(t, err)
			gt.Value(t, final.Status).Equal(agentkit.ProcessRunning)
			gt.Value(t, final.Rev).Equal(int64(2)) // exactly one write advanced Rev 1 -> 2.
		}
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
			Events: []*agentkit.Event{mkEvent(pid, agentkit.EventProcessCreated, "orig")},
		}))

		awaits, err := repo.ListAwaits(ctx, pid)
		gt.NoError(t, err)
		gt.Array(t, awaits).Length(1)
		awaits[0].Status = agentkit.AwaitExpired
		awaits[0].Response[0] = 'X'

		events, err := repo.ListEvents(ctx, pid, agentkit.EventQuery{})
		gt.NoError(t, err)
		gt.Array(t, events).Length(1)
		events[0].Payload[0] = 'X'

		a2, err := repo.ListAwaits(ctx, pid)
		gt.NoError(t, err)
		gt.Value(t, a2[0].Status).Equal(agentkit.AwaitOpen)
		gt.Value(t, string(a2[0].Response)).Equal("orig")
		e2, err := repo.ListEvents(ctx, pid, agentkit.EventQuery{})
		gt.NoError(t, err)
		gt.Value(t, string(e2[0].Payload)).Equal("orig")
	})

	runSemaphore(t, ctx, factory)
}

// semProc is a pending Process in its own tree, carrying a semaphore and holding
// no slot yet — the shape a Spawn writes.
func semProc(key, value string, slots int) *agentkit.Process {
	p := mkProc(newPID())
	p.Semaphore = &agentkit.ProcessSemaphore{Key: key, Value: value, Slots: slots}
	return p
}

// heldProc is a Process already holding its slot: running, held, and fenced by a
// live lease. The lease matters — a running row with none is an expired claim and
// ClaimNextProcess would reclaim it, which is not what these cases are about.
func heldProc(key, value string, slots int) *agentkit.Process {
	p := semProc(key, value, slots)
	p.Status = agentkit.ProcessRunning
	p.SemaphoreHeld = true
	lease := time.Now().Add(time.Hour)
	p.LeaseUntil = &lease
	p.LeaseOwner = "holder"
	p.LeaseToken = uniqueStr("lease")
	return p
}

// claimOne claims whatever is runnable now, asserting only that the call itself
// succeeded. It returns nil when nothing was claimable, which for a semaphore is
// the difference between "held back" (nil) and "the store broke" (an error).
func claimOne(t *testing.T, ctx context.Context, repo agentkit.Repository, worker string) *agentkit.Process {
	t.Helper()
	now := time.Now()
	claimed, err := repo.ClaimNextProcess(ctx, worker, now.Add(time.Hour), now)
	gt.NoError(t, err)
	return claimed
}

// terminate moves a row to succeeded, which is what releases its slot. It
// re-reads first so the CAS carries the stored Rev rather than whatever Rev the
// caller happens to be holding.
func terminate(t *testing.T, ctx context.Context, repo agentkit.Repository, pid agentkit.ProcessID) {
	t.Helper()
	done, err := repo.GetProcess(ctx, pid)
	gt.NoError(t, err)
	done.Status = agentkit.ProcessSucceeded
	done.LeaseUntil = nil
	gt.NoError(t, repo.Apply(ctx, agentkit.ChangeSet{Processes: []*agentkit.Process{done}}))
}

// runSemaphore covers contract items 4, 6 and 7 (the claim predicate, occupancy
// monotonicity, field immutability) plus GetSemaphoreStatus.
func runSemaphore(t *testing.T, ctx context.Context, factory func(t *testing.T) agentkit.Repository) {
	t.Run("SemaphoreClaimStopsAtSlots", func(t *testing.T) {
		repo := factory(t)
		key, value := uniqueStr("sem"), uniqueStr("v")
		a, b := semProc(key, value, 1), semProc(key, value, 1)
		gt.NoError(t, repo.Apply(ctx, agentkit.ChangeSet{Processes: []*agentkit.Process{a, b}}))

		first := claimOne(t, ctx, repo, "worker-1")
		gt.NotNil(t, first)
		gt.Bool(t, first.SemaphoreHeld).True()

		// The pair is full, so the other row is held back. Not an error: an error
		// here would fail every subsequent poll on this worker.
		gt.Nil(t, claimOne(t, ctx, repo, "worker-1"))
	})

	t.Run("SemaphoreSlotsGreaterThanOne", func(t *testing.T) {
		repo := factory(t)
		key, value := uniqueStr("sem"), uniqueStr("v")
		rows := []*agentkit.Process{semProc(key, value, 2), semProc(key, value, 2), semProc(key, value, 2)}
		gt.NoError(t, repo.Apply(ctx, agentkit.ChangeSet{Processes: rows}))

		gt.NotNil(t, claimOne(t, ctx, repo, "worker-1"))
		gt.NotNil(t, claimOne(t, ctx, repo, "worker-1"))
		gt.Nil(t, claimOne(t, ctx, repo, "worker-1"))
	})

	t.Run("SemaphoreHolderInWaitingStillBlocks", func(t *testing.T) {
		repo := factory(t)
		key, value := uniqueStr("sem"), uniqueStr("v")
		a, b := semProc(key, value, 1), semProc(key, value, 1)
		gt.NoError(t, repo.Apply(ctx, agentkit.ChangeSet{Processes: []*agentkit.Process{a, b}}))

		held := claimOne(t, ctx, repo, "worker-1")
		gt.NotNil(t, held)

		// Suspend it: waiting with no WakeAt, which is a Process parked on a
		// question. The slot is NOT released — occupancy is per Process, not per
		// claim.
		held.Status = agentkit.ProcessWaiting
		held.LeaseUntil = nil
		gt.NoError(t, repo.Apply(ctx, agentkit.ChangeSet{Processes: []*agentkit.Process{held}}))

		gt.Nil(t, claimOne(t, ctx, repo, "worker-1"))
	})

	t.Run("SemaphoreTerminalFreesSlot", func(t *testing.T) {
		repo := factory(t)
		key, value := uniqueStr("sem"), uniqueStr("v")
		a, b := semProc(key, value, 1), semProc(key, value, 1)
		gt.NoError(t, repo.Apply(ctx, agentkit.ChangeSet{Processes: []*agentkit.Process{a, b}}))

		held := claimOne(t, ctx, repo, "worker-1")
		gt.NotNil(t, held)
		gt.Nil(t, claimOne(t, ctx, repo, "worker-1"))

		terminate(t, ctx, repo, held.ID)

		// No release was written; the row leaving the non-terminal set is the
		// release.
		next := claimOne(t, ctx, repo, "worker-1")
		gt.NotNil(t, next)
		gt.Bool(t, next.SemaphoreHeld).True()
	})

	t.Run("SemaphoreSameRootShares", func(t *testing.T) {
		repo := factory(t)
		key, value := uniqueStr("sem"), uniqueStr("v")
		parent := semProc(key, value, 1)
		child := semProc(key, value, 1)
		child.RootID = parent.RootID // one tree, one slot.
		child.ParentID = &parent.ID
		gt.NoError(t, repo.Apply(ctx, agentkit.ChangeSet{Processes: []*agentkit.Process{parent, child}}))

		gt.NotNil(t, claimOne(t, ctx, repo, "worker-1"))
		// The second row is in the tree that already holds the pair, so it is
		// admitted rather than deadlocked behind its own ancestor.
		gt.NotNil(t, claimOne(t, ctx, repo, "worker-1"))
	})

	t.Run("SemaphoreDifferentValuesDoNotContend", func(t *testing.T) {
		repo := factory(t)
		key := uniqueStr("sem")
		a := semProc(key, uniqueStr("v"), 1)
		b := semProc(key, uniqueStr("v"), 1)
		gt.NoError(t, repo.Apply(ctx, agentkit.ChangeSet{Processes: []*agentkit.Process{a, b}}))

		// (Key, Value) is the unit that counts, so a different value is a different
		// limit even at one slot each.
		gt.NotNil(t, claimOne(t, ctx, repo, "worker-1"))
		gt.NotNil(t, claimOne(t, ctx, repo, "worker-1"))
	})

	t.Run("SemaphoreEmptyValueIsOneInstance", func(t *testing.T) {
		repo := factory(t)
		key := uniqueStr("sem")
		a, b := semProc(key, "", 1), semProc(key, "", 1)
		gt.NoError(t, repo.Apply(ctx, agentkit.ChangeSet{Processes: []*agentkit.Process{a, b}}))

		gt.NotNil(t, claimOne(t, ctx, repo, "worker-1"))
		gt.Nil(t, claimOne(t, ctx, repo, "worker-1"))
	})

	t.Run("SemaphoreApplyRejectsOverLimit", func(t *testing.T) {
		repo := factory(t)
		key, value := uniqueStr("sem"), uniqueStr("v")
		gt.NoError(t, repo.Apply(ctx,
			agentkit.ChangeSet{Processes: []*agentkit.Process{heldProc(key, value, 1)}}))

		// This is the fence eager dispatch relies on: it marks a row held without
		// counting, and the Apply is what refuses.
		second := heldProc(key, value, 1)
		err := repo.Apply(ctx, agentkit.ChangeSet{Processes: []*agentkit.Process{second}})
		gt.Bool(t, errors.Is(err, agentkit.ErrConflict)).True()

		_, gerr := repo.GetProcess(ctx, second.ID)
		gt.Bool(t, errors.Is(gerr, agentkit.ErrProcessNotFound)).True()
	})

	t.Run("SemaphoreStricterSlotsWins", func(t *testing.T) {
		repo := factory(t)
		key, value := uniqueStr("sem"), uniqueStr("v")
		gt.NoError(t, repo.Apply(ctx,
			agentkit.ChangeSet{Processes: []*agentkit.Process{heldProc(key, value, 2)}}))

		// Two deployments disagreeing on the slot count: the smallest binds, so a
		// second holder is refused even though the existing one allows two.
		err := repo.Apply(ctx,
			agentkit.ChangeSet{Processes: []*agentkit.Process{heldProc(key, value, 1)}})
		gt.Bool(t, errors.Is(err, agentkit.ErrConflict)).True()
	})

	t.Run("SemaphoreStricterSlotsBlocksClaimBothOrders", func(t *testing.T) {
		for _, tc := range []struct {
			name          string
			first, second int
		}{
			{"strict first", 1, 2},
			{"lenient first", 2, 1},
		} {
			t.Run(tc.name, func(t *testing.T) {
				repo := factory(t)
				key, value := uniqueStr("sem"), uniqueStr("v")
				// CreatedAt decides which the reference implementation picks first.
				a := semProc(key, value, tc.first)
				b := semProc(key, value, tc.second)
				b.CreatedAt = a.CreatedAt.Add(time.Second)
				gt.NoError(t, repo.Apply(ctx, agentkit.ChangeSet{Processes: []*agentkit.Process{a, b}}))

				gt.NotNil(t, claimOne(t, ctx, repo, "worker-1"))
				// The effective limit is 1 either way. The second call must report
				// "nothing claimable", NOT an error: an implementation whose predicate
				// admits what its Apply then refuses fails ClaimNextProcess itself, and
				// the same row is re-picked on every poll.
				gt.Nil(t, claimOne(t, ctx, repo, "worker-1"))
			})
		}
	})

	t.Run("SemaphoreStricterSlotsBlocksSameRootChild", func(t *testing.T) {
		repo := factory(t)
		key, value := uniqueStr("sem"), uniqueStr("v")
		h1, h2 := heldProc(key, value, 2), heldProc(key, value, 2)
		gt.NoError(t, repo.Apply(ctx, agentkit.ChangeSet{Processes: []*agentkit.Process{h1, h2}}))

		// Same tree as an existing holder, so the holder set would not grow — but a
		// smaller Slots drops the effective limit to 1, which two holders already
		// exceed. "Already in the tree" is therefore not sufficient on its own.
		child := semProc(key, value, 1)
		child.RootID = h1.RootID
		child.ParentID = &h1.ID
		gt.NoError(t, repo.Apply(ctx, agentkit.ChangeSet{Processes: []*agentkit.Process{child}}))

		gt.Nil(t, claimOne(t, ctx, repo, "worker-1"))
	})

	t.Run("SemaphoreOccupancyIsNotWorsened", func(t *testing.T) {
		repo := factory(t)
		key, value := uniqueStr("sem"), uniqueStr("v")

		// Three holders in three trees, at a limit of three: legal, and the most
		// occupied a conforming implementation can get.
		var held []*agentkit.Process
		for range 3 {
			held = append(held, heldProc(key, value, 3))
		}
		gt.NoError(t, repo.Apply(ctx, agentkit.ChangeSet{Processes: held}))

		// Worsening by adding a holder: refused.
		gt.Bool(t, errors.Is(repo.Apply(ctx,
			agentkit.ChangeSet{Processes: []*agentkit.Process{heldProc(key, value, 3)}}),
			agentkit.ErrConflict)).True()

		// Worsening by lowering the limit instead: a held row with a smaller Slots,
		// placed in a tree that already holds the pair so the holder count does not
		// grow. Also refused — that is what keeps a disagreeing deployment from
		// pushing the state over the limit sideways.
		strictSameTree := heldProc(key, value, 1)
		strictSameTree.RootID = held[0].RootID
		strictSameTree.ParentID = &held[0].ID
		gt.Bool(t, errors.Is(repo.Apply(ctx,
			agentkit.ChangeSet{Processes: []*agentkit.Process{strictSameTree}}), agentkit.ErrConflict)).True()

		// Draining works one row at a time, and an unrelated Process is never held
		// up by someone else's occupancy. This is the property the rule exists for:
		// a check phrased as "the state must not be over the limit" would reject
		// exactly these writes once a state produced OUTSIDE this contract — a stale
		// worker, a manual edit — put the pair over its limit, leaving no way back.
		for _, p := range held {
			unrelated := mkProc(newPID())
			gt.NoError(t, repo.Apply(ctx, agentkit.ChangeSet{Processes: []*agentkit.Process{unrelated}}))
			terminate(t, ctx, repo, p.ID)
		}

		st, err := repo.GetSemaphoreStatus(ctx, key, value)
		gt.NoError(t, err)
		gt.Value(t, st.Held).Equal(0)
	})

	t.Run("SemaphoreFieldsAreImmutable", func(t *testing.T) {
		repo := factory(t)
		key, value := uniqueStr("sem"), uniqueStr("v")
		p := semProc(key, value, 2)
		gt.NoError(t, repo.Apply(ctx, agentkit.ChangeSet{Processes: []*agentkit.Process{p}}))

		stored, err := repo.GetProcess(ctx, p.ID)
		gt.NoError(t, err)

		for _, tc := range []struct {
			name string
			sem  *agentkit.ProcessSemaphore
		}{
			{"different key", &agentkit.ProcessSemaphore{Key: uniqueStr("other"), Value: value, Slots: 2}},
			{"different value", &agentkit.ProcessSemaphore{Key: key, Value: uniqueStr("other"), Slots: 2}},
			{"raised slots", &agentkit.ProcessSemaphore{Key: key, Value: value, Slots: 99}},
			{"cleared", nil},
		} {
			t.Run(tc.name, func(t *testing.T) {
				mut := *stored
				mut.Semaphore = tc.sem
				err := repo.Apply(ctx, agentkit.ChangeSet{Processes: []*agentkit.Process{&mut}})
				gt.Bool(t, errors.Is(err, agentkit.ErrConflict)).True()

				again, gerr := repo.GetProcess(ctx, p.ID)
				gt.NoError(t, gerr)
				gt.NotNil(t, again.Semaphore)
				gt.Value(t, *again.Semaphore).Equal(*p.Semaphore)
			})
		}
	})

	t.Run("SemaphoreHeldIsMonotonic", func(t *testing.T) {
		repo := factory(t)
		key, value := uniqueStr("sem"), uniqueStr("v")
		p := semProc(key, value, 1)
		gt.NoError(t, repo.Apply(ctx, agentkit.ChangeSet{Processes: []*agentkit.Process{p}}))

		held := claimOne(t, ctx, repo, "worker-1")
		gt.NotNil(t, held)
		gt.Bool(t, held.SemaphoreHeld).True()

		// Clearing it would hand this Process a run outside its own limit.
		released := held
		released.SemaphoreHeld = false
		err := repo.Apply(ctx, agentkit.ChangeSet{Processes: []*agentkit.Process{released}})
		gt.Bool(t, errors.Is(err, agentkit.ErrConflict)).True()

		again, gerr := repo.GetProcess(ctx, p.ID)
		gt.NoError(t, gerr)
		gt.Bool(t, again.SemaphoreHeld).True()
	})

	t.Run("SemaphoreStatusReportsOccupancy", func(t *testing.T) {
		repo := factory(t)
		key, value := uniqueStr("sem"), uniqueStr("v")
		oldest := semProc(key, value, 1)
		mid := semProc(key, value, 1)
		newest := semProc(key, value, 1)
		mid.CreatedAt = oldest.CreatedAt.Add(time.Second)
		newest.CreatedAt = oldest.CreatedAt.Add(2 * time.Second)
		gt.NoError(t, repo.Apply(ctx,
			agentkit.ChangeSet{Processes: []*agentkit.Process{oldest, mid, newest}}))

		claimed := claimOne(t, ctx, repo, "worker-1")
		gt.NotNil(t, claimed)

		st, err := repo.GetSemaphoreStatus(ctx, key, value)
		gt.NoError(t, err)
		gt.Value(t, st.Key).Equal(key)
		gt.Value(t, st.Value).Equal(value)
		gt.Value(t, st.Held).Equal(1)
		gt.Value(t, st.Slots).Equal(1)
		gt.Value(t, st.Waiting).Equal(2)
		gt.NotNil(t, st.OldestWaiting)

		terminate(t, ctx, repo, claimed.ID)

		// A terminal row counts as neither a holder nor a waiter.
		after, err := repo.GetSemaphoreStatus(ctx, key, value)
		gt.NoError(t, err)
		gt.Value(t, after.Held).Equal(0)
		gt.Value(t, after.Waiting).Equal(2)
	})

	t.Run("SemaphoreStatusOnUnusedPairIsZero", func(t *testing.T) {
		repo := factory(t)
		key, value := uniqueStr("sem"), uniqueStr("v")

		// "Nothing is using it" is an answer, not an error.
		st, err := repo.GetSemaphoreStatus(ctx, key, value)
		gt.NoError(t, err)
		gt.NotNil(t, st)
		gt.Value(t, st.Key).Equal(key)
		gt.Value(t, st.Value).Equal(value)
		gt.Value(t, st.Held).Equal(0)
		gt.Value(t, st.Slots).Equal(0)
		gt.Value(t, st.Waiting).Equal(0)
		gt.Nil(t, st.OldestWaiting)
	})

	t.Run("SemaphoreStatusCountsOnlyBlockedRows", func(t *testing.T) {
		repo := factory(t)
		key, value := uniqueStr("sem"), uniqueStr("v")
		holder := heldProc(key, value, 1)
		// Same tree as the holder, so a slot is available to it — queued, not
		// blocked. Counting it would inflate the backlog on the very shape the
		// semaphore deliberately allows.
		sameTree := semProc(key, value, 1)
		sameTree.RootID = holder.RootID
		sameTree.ParentID = &holder.ID
		otherTree := semProc(key, value, 1)
		gt.NoError(t, repo.Apply(ctx, agentkit.ChangeSet{
			Processes: []*agentkit.Process{holder, sameTree, otherTree},
		}))

		st, err := repo.GetSemaphoreStatus(ctx, key, value)
		gt.NoError(t, err)
		gt.Value(t, st.Held).Equal(1)
		gt.Value(t, st.Waiting).Equal(1) // otherTree only.
		gt.NotNil(t, st.OldestWaiting)
	})

	t.Run("SemaphoreStatusIgnoresOtherValues", func(t *testing.T) {
		repo := factory(t)
		key := uniqueStr("sem")
		mine, other := uniqueStr("v"), uniqueStr("v")
		gt.NoError(t, repo.Apply(ctx, agentkit.ChangeSet{Processes: []*agentkit.Process{
			semProc(key, mine, 1), semProc(key, other, 1), semProc(key, other, 1),
		}}))

		st, err := repo.GetSemaphoreStatus(ctx, key, mine)
		gt.NoError(t, err)
		gt.Value(t, st.Waiting).Equal(1)
	})

	t.Run("SemaphoreRoundTrip", func(t *testing.T) {
		repo := factory(t)
		key, value := uniqueStr("sem"), uniqueStr("v")
		p := semProc(key, value, 3)
		gt.NoError(t, repo.Apply(ctx, agentkit.ChangeSet{Processes: []*agentkit.Process{p}}))

		got, err := repo.GetProcess(ctx, p.ID)
		gt.NoError(t, err)
		gt.NotNil(t, got.Semaphore)
		gt.Value(t, *got.Semaphore).Equal(*p.Semaphore)
		gt.Bool(t, got.SemaphoreHeld).False()

		// Reads deep-copy: mutating the result must not reach stored state.
		got.Semaphore.Value = "mutated"
		again, err := repo.GetProcess(ctx, p.ID)
		gt.NoError(t, err)
		gt.Value(t, again.Semaphore.Value).Equal(value)
	})

	t.Run("SemaphoreNilIsUnrestricted", func(t *testing.T) {
		repo := factory(t)
		var rows []*agentkit.Process
		for range 3 {
			rows = append(rows, mkProc(newPID()))
		}
		gt.NoError(t, repo.Apply(ctx, agentkit.ChangeSet{Processes: rows}))

		for range 3 {
			gt.NotNil(t, claimOne(t, ctx, repo, "worker-1"))
		}
	})
}
