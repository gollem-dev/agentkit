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

Eager dispatch claims through the same fence rather than a second one: it
targets a specific `pending` row with an ordinary `Apply` `Rev` CAS instead of
`ClaimNextProcess`, so it can race a poll loop claiming the same row. The
contract requires `ClaimNextProcess` and `Apply` to be mutually linearizable on
one `Process` row — of two claimants reading it at the same `Rev`, exactly one
advances it and the other observes the new `Rev` (a claim finds nothing to
claim; an `Apply` gets `ErrConflict`) — which is what lets eager dispatch claim
a pending row via `Apply` without a dedicated SPI method
([ADR-0004](../adr/0004-repository-changeset-rev-cas.md), `repository.go`
contract item 4).

An eager claim's `Apply` can also fail *indeterminately* — the filesystem
store's post-rename failure is the example — meaning the row may already be
committed as `running` under the claim's token even though `Apply` returned an
error. Eager dispatch still abandons on any non-`ErrConflict` error rather than
assuming success, so that row's lease then simply expires and a poller
reclaims it, counted as an unclean reclaim
([ADR-0015](../adr/0015-unclean-reclaims-are-counted-and-bounded.md)). Rare,
bounded by `WithMaxUncleanReclaims`, and always recovered by polling — an
`Apply` error here is not a claim that nothing was committed.

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

## History is a separate, best-effort store

Conversation History (`*gollem.History`) is the one piece of durable state that
does **not** ride the atomic `Apply`. When an agent opts in with
`WithHistoryRepository`, the worker persists History to a separate blob store
(keyed by `ProcessID`) *before* each transition commits — including terminal
commits — because the commit is the completion marker
([ADR-0017](../adr/0017-history-is-a-decoupled-best-effort-store.md)). History is
append-only and can outgrow what a transactional row should hold, which is why it
is decoupled from the Process row.

The two guarantees above therefore do **not** extend to History; its save is an
at-least-once effect ([ADR-0003](../adr/0003-at-least-once-replay-no-effect-journal.md)):

- **A crash between the History save and the commit** leaves a saved History for
  a transition that did not commit. The next claim reads that superseded History
  back and the re-run appends to it, so the conversation can carry a duplicated
  turn. This window is accepted, not closed.
- **Save-before-commit removes the opposite "amnesia" window** (History lagging
  State): once a transition commits, its History is already durable, so a
  post-commit crash loses nothing.
- **Same-lease conflict retries do not duplicate.** The committed baseline is
  held in memory per claim and advanced only on a successful `Apply`; a retry
  re-seeds from that baseline, not from the repo, so only a real crash — never an
  in-process retry — reaches the duplication window.
- **A worker that lost its lease cannot clobber a newer worker's committed
  History.** The blob store has no Rev/LeaseToken fence of its own, so right
  before the save the worker re-reads the row and skips the write when the lease
  token changed (`ownsLease` in `worker.go`). Without it, a stalled old worker's
  late save would overwrite a newer worker's committed History — a regression,
  not just a duplicate. The re-check narrows that window to the gap between the
  read and the write; with a single-key store it is narrowed, not fully closed.

Because the save precedes the commit, a History-store outage prevents the
transition from committing at all, and the Process eventually fails
`retry_exhausted`: liveness is coupled to the History store when an agent opts
in.

Keeping the next LLM request well-formed across a duplication is the strategy's
responsibility. A `SessionGenerate`-using strategy must keep a tool-call round within one
Step, so a persisted History never ends on a dangling `tool_use`; a strategy that
splits a round across steps keeps History in its own State instead (raw
`Generate` + `WithHistory`). The kernel does not inspect History
([ADR-0011](../adr/0011-kernel-has-no-tenancy.md)).

## What a Repository implementation must provide

The mechanisms above are only as strong as the store beneath them. The full
contract is in ADR-0004 and, executably, in `repository/repotest`.

The non-negotiable parts:

- `Apply` is genuinely all-or-nothing across every row and guard in the set.
- `Rev` CAS is checked before any write, and a violation writes nothing.
- `ClaimNextProcess` never double-claims and always mints a fresh `LeaseToken`.
- `ClaimNextProcess` increments `unclean_reclaims` when — and only when — the row
  it claimed was `running`. That is the store's job because only the atomic claim
  can still see which state the row was in
  ([ADR-0015](../adr/0015-unclean-reclaims-are-counted-and-bounded.md)).
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
