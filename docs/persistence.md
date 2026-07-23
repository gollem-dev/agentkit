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
lives in a separate, opt-in blob store keyed by process id. See
[writing-strategies.md#persisting-conversation-history](writing-strategies.md#persisting-conversation-history)
and [ADR-0017](adr/0017-history-is-a-decoupled-best-effort-store.md).

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
   atomically — `pending`, or `waiting` with `WakeAt` in the past, or `running`
   with an expired lease — and mints a **fresh `LeaseToken` on every claim**,
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
6. **`ListEvents` preserves append order** per process.
7. **Reads deep-copy.** A caller mutating a returned `*Process`, `*Await` or
   `*Event` must not be able to reach stored state.

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

**Claim ordering.** The bundled implementations claim the oldest eligible
process by `CreatedAt`. Nothing in the contract requires that ordering; pick
whatever your store makes cheap and fair.

**Indexes worth having.** The claim predicate (status, `WakeAt`, `LeaseUntil`),
`idempotency_key`, `Subject` restricted to open processes, and
`(process_id, await_key)`. The last three are also the uniqueness constraints, so
they can be the same indexes.

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
