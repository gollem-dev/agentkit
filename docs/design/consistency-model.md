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

Each child's finalize includes the parent's row in its change set — always, since
that write also carries the child's metrics up (ADR-0010), and so even when the
wake is a no-op or there is no await at all. Two concurrent finalizes therefore
contend on the parent's `Rev`, one wins, the loser retries against fresh state
and sees the sibling's result. Neither can conclude "someone else will wake the
parent" and be wrong, and neither can lose its own metrics to the other's write.

### Two humans answer the same question

First writer wins. Only an `open` await accepts a response; the second gets
`ErrAwaitClosed`. `Respond` conflicts are retried by re-reading and re-judging,
so a race resolves to a single accepted answer with a recorded responder.

### A deadline fires while a response is in flight

Both paths include the process row in their `Apply`, so they serialize on its
`Rev`. Whichever commits first settles the await; the other sees it is no longer
`open`.

## History rides the commit by reference

Conversation History (`*gollem.History`) is the one piece of durable state whose
bytes do **not** ride the atomic `Apply` — it is append-only and can outgrow what
a transactional row should hold, so it lives in a separate blob store. What rides
the `Apply` is the *pointer*: `Process.HistoryRef` names the committed version,
and it is written in the same change set as State
([ADR-0017](../adr/0017-history-is-an-immutable-versioned-store.md)).

Versions are immutable. When an agent opts in with `WithHistoryStore`, the worker
saves a **new** version before each transition commits — including terminal
commits, because the commit is the completion marker — and the commit publishes
it by recording its ref. Nothing is ever rewritten in place, so a save whose
transition did not commit leaves a version the record never names.

That is what extends the two guarantees above to History:

- **A crash between the save and the commit changes nothing.** The record still
  names the previous version, so the next claim re-seeds from exactly the state
  the last commit left. State, awaits and History advance together or roll back
  together; there is no window where one is ahead of the other.
- **Save-before-commit removes the "amnesia" window** (History lagging State):
  once a transition commits, its version is already durable.
- **Same-lease conflict retries do not duplicate.** The committed version is held
  in memory per claim and advanced only on a successful `Apply`, so a retry
  re-seeds from it rather than from the abandoned attempt.
- **A worker that lost its lease cannot clobber anything.** It can only add a
  version nobody references. The blob store needs no fence of its own, which is
  why there is no lease re-check around the save.

A Process can also start from a version *another* Process committed
(`Spawn` + `WithInheritedHistory`). The pair naming it lives in
`Process.InheritedHistory`, deliberately not in `HistoryRef`: `HistoryRef` is
what the post-commit release treats as superseded, and it releases it under the
committing Process's own id, so an inherited ref placed there would announce
another Process's live version as garbage. Kept apart, the inherited version is
only ever read — never released — and `HistoryRef` takes over as soon as the
Process commits a version of its own.

What is *not* guaranteed: reclamation. After a commit the worker tells the store
the superseded version is no longer referenced, but that call is a notification —
the store decides when, or whether, to reclaim. Versions left by a crash between
a save and its commit are never announced at all, so a store needs its own policy
for them. Nor is the survival of an inherited version guaranteed: the Process
that issued it releases it on its own next commit, so inheriting from a Process
still running means the version may be gone by the time it is read. And the effect model is unchanged ([ADR-0003](../adr/0003-at-least-once-replay-no-effect-journal.md)):
a replay still re-calls the LLM and re-runs tools.

Because the save precedes the commit, a History-store outage prevents the
transition from committing at all, and the Process eventually fails
`retry_exhausted`: liveness is coupled to the History store when an agent opts
in.

**A Step boundary may fall in the middle of a tool round.** A committed History
ending on a `tool_use` whose `tool_result` belongs to a later Step is safe now,
because a re-run always starts from the version the record names — so a strategy
can generate a tool call, suspend for a human, and answer the call in the next
Step. `sys.Session().CallTool` appends its result to the conversation, letting a
Step close the pair without another LLM turn; that is the one place the kernel
constructs a message (a single `RoleTool` entry) rather than treating History as
opaque. It still never interprets what is inside
([ADR-0011](../adr/0011-kernel-has-no-tenancy.md)), and it cannot tell whether a
call was one the model asked for — pairing a `tool_result` to a real `tool_use`
stays the strategy's responsibility.

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
- `ListEvents` preserves append order, returns each event's kernel-assigned `ID`
  unchanged, and honours the `EventQuery` cursor and cap.
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
