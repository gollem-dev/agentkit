# Consistency model

What holds when workers crash and run concurrently, and by which mechanism.

## The two guarantees

Everything below serves exactly two promises:

1. **A committed transition is never lost.**
2. **One transition commits atomically** — state, awaits, events, spawned
   children and metrics land together or not at all.

Anything not on that list is not guaranteed. In particular there is **no
exactly-once execution and no deterministic replay**: a transition that crashes
before committing is re-run from the last checkpoint, LLM and tool calls run
again, and the re-run may take a different path (ADR-0003).

This is not a limitation being apologized for. It follows from what an LLM is,
and pretending otherwise produced a design that was wrong in ways that only
showed up under concurrency.

## Three mechanisms

| Mechanism | Question it answers | Where it lives |
|---|---|---|
| `Rev` CAS | has this row changed since I read it? | every process row in a `ChangeSet` |
| `ChangeSet.Guards` | has a row I *read but did not write* changed? | `WaitChildren` resolution |
| `LeaseToken` | am I still the worker that owns this claim? | in-process, compared on conflict |

They are complementary, and conflating them is the easiest way to introduce a
bug here.

### Rev: optimistic concurrency on writes

Every process row in a change set carries the `Rev` it was read at. `Apply`
checks all of them; one mismatch and nothing is written. On success each written
row's `Rev` advances by one.

This is the fence against stale-worker commits. A worker whose lease expired
still holds an old `Rev`, so its commit fails rather than clobbering the work of
whoever claimed the process next.

### Guards: extending CAS to the read set

Some decisions depend on rows the transition does not write. `WaitChildren` is
the case: the parent reads its children's states to decide whether to suspend or
elide, then commits. Between the read and the commit, a child can finalize.

A `ProcessGuard` puts that read set into the commit's preconditions — checked,
never written, `Rev` not advanced. If a child moved, the commit conflicts and the
transition is rebuilt against fresh state. This makes check-then-act atomic
without a lock (ADR-0009).

### LeaseToken: claim identity

`Rev` says *the row changed*. It does not say *by whom*. That distinction decides
whether a conflicted worker should retry or give up, so it needs its own
mechanism.

Every claim mints a fresh `LeaseToken`, including a re-claim by the same worker
process. On conflict the worker re-reads and compares:

- **stored token == mine** → I still own this claim. Something benign moved the
  row (a concurrent `Cancel`, a sibling's finalize). Rebuild against fresh state
  and retry.
- **stored token != mine** → someone else owns it now. Abandon immediately and
  never rebase my `Rev` onto theirs.

`LeaseOwner` is diagnostic only — several concurrent claims within one `Serve`
share a worker id, so it cannot serve as the fence.

The same distinction applies to external callers (`Cancel`, `Respond`), who hold
no lease at all. Their conflicts are propagated up so they re-read and
re-decide, rather than being retried against a state they never saw.

## The failure windows, and how each is closed

### A worker dies mid-transition

Its lease expires, another worker claims the process and re-runs `Step` from the
last committed state. Uncommitted work is gone — including buffered child
inserts and buffered events, which is the point of buffering them.

Committed work is intact, because it was committed atomically.

**Cost:** LLM and tool calls from the lost attempt may have already happened.
The LLM re-charge is accepted. Tool side effects are the tool author's problem
(see [responsibility-boundaries.md](responsibility-boundaries.md)).

### A worker dies between committing and releasing

Nothing is lost. The row is already committed at its new `Rev`; the lease simply
expires and someone re-claims.

### Two workers claim the same process

They cannot. `ClaimNextProcess` is atomic by contract, and `repotest` verifies
it under 100-way concurrency.

The reachable variant is: worker A's lease expires while it is still alive,
worker B claims, and A then tries to commit. A's `Rev` is stale, `Apply`
conflicts, A compares lease tokens, sees B's, and abandons.

### A child finishes before the parent waits on it

Handled at declaration: `WaitChildren` reads the children and, if all are
already terminal, writes the await as `responded` and keeps the process
`running` — no suspend, no wake needed. Partially-terminal children contribute
`Guards`, so a child finishing during the window conflicts the commit
(ADR-0009).

### Two siblings finish at the same time

Each child's finalize includes the parent's row in its change set — even when
the wake is a no-op. Two concurrent finalizes therefore contend on the parent's
`Rev`, one wins, the loser retries against fresh state and sees the sibling's
result. Neither can conclude "someone else will wake the parent" and be wrong.

### Two humans answer the same question

First writer wins. Only an `open` await accepts a response; the second gets
`ErrAwaitClosed`. `Respond` conflicts are retried by re-reading and re-judging,
so a race resolves to a single accepted answer with a recorded responder.

### A deadline fires while a response is in flight

Both paths include the process row in their `Apply`, so they serialize on its
`Rev`. Whichever commits first settles the await; the other sees it is no longer
`open`.

## What a Repository implementation must provide

The mechanisms above are only as strong as the store beneath them. The full
contract is in ADR-0004 and, executably, in `repository/repotest`.

The non-negotiable parts:

- `Apply` is genuinely all-or-nothing across every row and guard in the set.
- `Rev` CAS is checked before any write, and a violation writes nothing.
- `ClaimNextProcess` never double-claims and always mints a fresh `LeaseToken`.
- Uniqueness holds on `idempotency_key`, on an open process's `Subject`, and on
  `(process_id, await_key)`.
- `ListEvents` preserves append order.
- Reads deep-copy, so a caller mutating a returned value cannot reach stored
  state.

Run `repotest.Run(t, factory)` against any implementation. A store that passes
it satisfies the kernel's assumptions; one that has not been run against it has
not been verified, whatever it looks like.

The bundled implementations are references, not production stores.
`repository/memory` is in-process. `repository/filesystem` is single-process by
construction — it holds an exclusive `flock` on its directory and rewrites the
whole snapshot on every write, atomically via temp file → fsync → rename →
directory fsync, with the rename as the commit point. Neither is suitable for
multi-host workers.
