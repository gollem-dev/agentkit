# Persistence

`Repository` is the contract agentkit needs from your store. Your application
implements it and injects it; it never calls it directly, because reads go
through the kernel (`GetProcess`, `ListAwaits`, `ListEvents`).

The interface asks for **data semantics, not a transaction mechanism**: atomic
application of a change set, and conditional writes keyed on a revision number.
How you realize that — an RDB transaction, a document-store transaction, a
conditional write, a mutex — is entirely yours
([ADR-0004](adr/0004-repository-changeset-rev-cas.md)).

Conversation History (`*gollem.History`) is **not** part of this contract — it
lives in a separate, opt-in blob store holding immutable versions, of which the
Process record names the committed one. See
[writing-strategies.md#persisting-conversation-history](writing-strategies.md#persisting-conversation-history)
and [ADR-0017](adr/0017-history-is-an-immutable-versioned-store.md).

## Bundled implementations

| Package | Scope | Use it for |
|---|---|---|
| `repository/memory` | in-process, not persistent | tests, development, one-shot runs |
| `repository/filesystem` | one local process, persistent | a single-node service that must survive restarts |

```go
repo := memory.New()

repo, err := filesystem.New("/var/lib/myagent")
defer repo.Close()
```

`filesystem` holds an exclusive `flock` on its directory, so a second `New` on
the same path fails. It writes the whole state as one `state.json`, replaced
atomically by temp file → fsync → rename → directory fsync, with the rename as
the commit point — no write-ahead log needed. Because it rewrites everything on
every write, its I/O is proportional to total state size.

If a failure occurs *after* the rename has committed, the on-disk state has
advanced but its durability is unconfirmed, so the repository fail-stops:
subsequent calls return `ErrRepositoryIndeterminate` until it is closed and
reopened. This is deliberate — reporting an uncertain state is better than
guessing.

**Neither is suitable for workers on more than one host.** For that, implement
the contract against a shared store.

## The contract

1. **`Apply` is atomic** — the whole `ChangeSet` or nothing.
2. **Each row in `cs.Processes` is a `Rev` CAS.** The stored `Rev` must equal the
   row's `Rev`; a single mismatch writes nothing and returns `ErrConflict`. On
   success each written row's `Rev` increments (an insert declares `Rev == 0` and
   is stored as `Rev == 1`). This is what fences a stale worker's commit.
3. **Each `ProcessGuard` in `cs.Guards` is a read-only precondition** — its `Rev`
   is checked, nothing is written, and its `Rev` is not advanced.
4. **`ClaimNextProcess` never double-claims.** It claims one runnable process
   atomically — `pending` with no `WakeAt` or one in the past, or `waiting` with
   `WakeAt` in the past, or `running` with an expired lease — and mints a
   **fresh `LeaseToken` on every claim**,
   including a re-claim by the same worker. No candidate means `(nil, nil)`, not
   an error. When the row it took was `running`, it also **increments
   `unclean_reclaims`**: that case means the previous claim died mid-transition,
   and the counter is what bounds how often the work is replayed
   ([ADR-0015](adr/0015-unclean-reclaims-are-counted-and-bounded.md)). A claim
   from `pending` or `waiting` leaves it alone, and a claim never writes
   `step_attempts`. Skipping this does not break the kernel — it silently
   restores unbounded replay after a crash.
5. **Uniqueness holds** on `idempotency_key`, on an open process's `Subject`, and
   on `(process_id, await_key)`. A violation writes nothing and returns
   `ErrConflict`.
6. **`ListEvents` preserves append order** per process, **returns each event's
   `ID` unchanged** (the kernel mints it; you store it), and honours the
   `EventQuery`: `After` is an exclusive cursor and `Limit` caps the count. An
   `After` the process has no event for is `ErrEventNotFound` and returns nothing
   ([ADR-0019](adr/0019-events-are-addressable-and-reads-resume.md)).
7. **Reads deep-copy.** A caller mutating a returned `*Process`, `*Await` or
   `*Event` must not be able to reach stored state.
8. **Every `Process` field round-trips**, `HistoryRef` included. It names the
   committed version of the conversation History in the agent's `HistoryStore`,
   and it must be written in the same `Apply` as `State` — that is the whole
   mechanism by which History rolls back with State
   ([ADR-0017](adr/0017-history-is-an-immutable-versioned-store.md)). A store
   that drops it silently resurrects a superseded conversation. It is an
   ordinary string column, empty when nothing has been committed yet.
   `InheritedHistory` rides along the same way: it names the version of *another*
   Process this one started its conversation from (`WithInheritedHistory`), it is
   written once at `Spawn` and never changed, and dropping it is just as silent —
   the Process starts from an empty conversation instead of the transcript it was
   supposed to continue.

9. **The semaphore is enforced here, not in the worker**
   ([ADR-0021](adr/0021-key-scoped-concurrency-is-a-process-semaphore.md)). For a
   `(Key, Value)` pair, its **holder set** is the `RootID`s of the rows carrying
   that pair with `semaphore_held` true and a non-terminal status, and its
   **effective limit** is the smallest `slots` among those rows. Three
   obligations follow:
   - **The claim predicate.** A row with `semaphore_held` false is a claim target
     only if adding its `RootID` to the holder set keeps the size within
     `min(effective limit, its own slots)`. The claim sets `semaphore_held` in
     the same atomic write. Note both halves: the row's own `slots` is in the
     minimum, and "its tree already holds the pair" is not sufficient by itself.
   - **`Apply` must not worsen occupancy.** Refuse when the result is over the
     effective limit *and* worse than before — more holders, or the same holders
     under a smaller limit. Also refuse a change to an existing row's `semaphore`
     and any write turning `semaphore_held` back to false.
   - **`GetSemaphoreStatus`** reports one pair's holders, effective limit, waiting
     count and oldest waiting time. A pair nothing references is a zero value, not
     an error. "Waiting" means the rows queued behind the pair; exclude a row
     whose `RootID` is already in the holder set, since it shares that slot rather
     than queueing for one.

   Two mistakes here are worse than not implementing the feature. If the claim
   predicate admits what `Apply` refuses, `ClaimNextProcess` returns an error and
   the same row is picked again on every poll, so the worker stops making
   progress. And if the `Apply` check is written as "the state must never be over
   the limit", one over-limit pair rejects every `Apply` on every Process —
   including the terminal commits that would clear it — because a `ChangeSet` may
   touch several rows.

Every one of these carries weight. The `Rev` CAS is what stops a worker whose
lease expired from clobbering its successor; the guards are what make the
`WaitChildren` check-then-act atomic; the fresh lease token is what lets a worker
tell "I still own this claim" from "someone took it"
([design/consistency-model.md](design/consistency-model.md)).

## Verify with repotest

```go
func TestMyRepository(t *testing.T) {
    repotest.Run(t, func(t *testing.T) agentkit.Repository {
        return mystore.New(newTestDB(t))
    })
}
```

The factory must return a fresh, empty repository each call. The suite covers:

- `Rev` increment on insert and update, and `ErrConflict` on a stale `Rev`
- atomicity across several rows, and across a uniqueness violation inside one
  change set
- guards: matching guards pass without advancing their `Rev`; a mismatch fails
  the whole apply
- idempotency-key uniqueness across separate applies
- open-subject uniqueness, including releasing the subject when a process
  terminates
- await upsert on `(process_id, await_key)`
- event append order
- claim eligibility for each of the three claimable conditions, and that a live
  lease is not claimable
- a fresh `LeaseToken` on every claim, including re-claims
- `unclean_reclaims` counted on a `running` takeover and left alone otherwise,
  and both attempt counters round-tripping through `Apply`
- `HistoryRef` round-tripping through `Apply`, including being replaced by a
  later transition and cleared back to empty
- `InheritedHistory` round-tripping through `Apply`, surviving a later
  transition unchanged, and not aliasing stored state on read
- no double-claim, with 100 processes and 100 concurrent claimers
- deep-copy-on-read for processes, awaits and events

**Run it.** A store that passes it satisfies the kernel's assumptions; one that
has not been run against it is unverified, however reasonable it looks.

## Notes for implementers

**Storing bytes.** `Process.State` and `Process.Output` are opaque `[]byte` —
the kernel never parses them. Store them as `bytea`, base64, or a blob
reference; the choice is yours.

**Typed row fields.** Kernel-owned data (`Await.Children`, `Await.Results`,
`Metrics`, and so on) arrives typed. Turning a row into your storage format is
your job — that boundary is exactly where serialization is supposed to live
([ADR-0007](adr/0007-kernel-neutral-to-serialization.md)).

**A nested field is still just a row.** `Process.InheritedHistory` is the one
optional struct on the row (a `ProcessID` and a `HistoryRef`). Two nullable text
columns or a single JSON column both satisfy the contract; what it must do is
come back exactly as it went in, `nil` included. Both fields are always set
together, so a store may treat "either column NULL" as `nil`. Adding it to an
existing schema needs no backfill and no rewrite of old rows: a Process that
inherited nothing has none, which is what a NULL already means.

**The semaphore columns.** `Process.Semaphore` is the second optional struct on
the row (a key, a value and a slot count), plus a boolean `SemaphoreHeld`. In SQL
that is `semaphore_key text NULL`, `semaphore_value text NULL`,
`semaphore_slots integer NULL` and
`semaphore_held boolean NOT NULL DEFAULT false`. Adding them to an existing schema
needs no backfill: a Process under no semaphore has none, which is what NULL and
`false` already mean. Dropping them is not silent in the same way the History
fields are — the limit simply stops being enforced.

**Claim ordering.** The bundled implementations claim the oldest eligible
process by `CreatedAt`. Nothing in the contract requires that ordering; pick
whatever your store makes cheap and fair. Note that this ordering is not a
queueing promise even in the bundled stores: eager dispatch claims a named
Process through `Apply`, so a Process waiting for a semaphore slot can be
overtaken by a newer one.

**`WakeAt` gates a `pending` row too**, not only a `waiting` one — that is what
makes the worker's retry backoff real. A `waiting` row differs in one way: it
needs a `WakeAt` to be eligible at all, since one without a deadline is waiting
for a response and must never wake by itself.

**Indexes worth having.** The claim predicate (status, `WakeAt`, `LeaseUntil`),
`idempotency_key`, `Subject` restricted to open processes, and
`(process_id, await_key)`. The last three are also the uniqueness constraints, so
they can be the same indexes. For the semaphore, the query the claim predicate and
the `Apply` check both need is "the holder set of this pair", so
`(semaphore_key, semaphore_value)` restricted to rows with `semaphore_held` true
and a non-terminal status.

**Wakeup is polling.** There is no push notification in the contract. A
`LISTEN`/`NOTIFY`-style optimization is store-specific; requiring it would tax
every implementation, and it can be added later as an optional interface without
breaking anyone. On the kernel side, an instance running `Serve` dispatches a
Process it just made runnable eagerly, in-process, rather than waiting for the
next poll — a latency optimization on top of `ClaimNextProcess`, not a
replacement for it ([ADR-0016](adr/0016-eager-dispatch-is-a-scheduling-optimization.md)).

**`ErrConflict` must be returned, not swallowed.** The kernel relies on it: its
retry loops re-read and re-decide when they see it. A repository that quietly
retries internally, or that returns a different error, breaks the fencing.
