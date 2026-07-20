# ADR-0009: Child processes commit atomically with the transition

## Summary

`SpawnChild` does not insert a row. It mints a `ProcessID`, builds the child
`Process` (running the child strategy's `Init` immediately), and buffers it. The
child is inserted as part of the transition's single `Apply`. If the transition
never commits, the child never existed — so there are no orphans and no
deduplication problem.

The reverse direction is closed the same way: when a child reaches a terminal
state, waking the parent is part of **the child's own finalize `Apply`**, not a
follow-up write.

Child processes are the one place where the framework itself guarantees
coordination, because unlike an external effect, a child process is a framework
object.

## Context

Under at-least-once replay (ADR-0003), an immediate child insert is a side
effect that survives a failed transition: the transition re-runs, spawns again,
and the tree accumulates orphans. Deduplicating them requires a stable identity
for "the same child", which non-deterministic replay cannot provide.

Waking a parent had three separate race windows, all found in design review:

- **Early completion** — a child finishes before the parent commits its
  `WaitChildren`, so nobody is waiting when the wake fires and nobody wakes when
  the wait appears.
- **The crash window after a terminal commit** — a child commits `Done` in one
  `Apply` and wakes the parent in a second; a crash between them leaves the
  parent asleep forever.
- **Write skew across siblings** — two children finishing concurrently each read
  "some siblings are still running" and neither wakes the parent.

## Decision

**Spawning.** `Syscalls.spawn` mints a uuid v7, runs the child binding's `Init`
and `EncodeState`, appends the `*Process` to a transition buffer, and counts a
`spawns` metric. `buildCommit` appends the buffer to `cs.Processes`, so child
inserts ride the same CAS-checked, all-or-nothing `Apply` as the parent's new
state. A replay mints new IDs; the old ones were never persisted.

`WithIdempotencyKey` is rejected on `SpawnChild` (`ErrInvalidRequest`) — the
atomic insert already provides what the key would have been for.

**Waiting.** `WaitChildren` reads the referenced children at declaration time
and does three things:

- Rejects any process that is not a direct child (`ErrInvalidRequest`), so the
  await cannot be used to read an arbitrary process by id.
- If **all** children are already terminal, the await row is created directly as
  `responded` with `Results` filled in, and the process stays `running` — the
  suspend is *elided* and the claim proceeds to the next transition. This closes
  the early-completion race.
- Otherwise, every already-persisted child contributes a `ProcessGuard{ID, Rev}`
  to `cs.Guards`. The read set is thus part of the commit's preconditions: if a
  child finalized between the read and the `Apply`, the guard fails with
  `ErrConflict` and the transition is rebuilt against fresh state. This makes
  check-then-act atomic.

**Waking.** A child's terminal commit is one `Apply` that carries the child's
terminal row, its parent's updated await row, and the parent row flipped from
`waiting` to `pending`. The parent row is included **even when the wake is a
no-op**, so concurrent sibling finalizes serialize on the parent's `Rev` and
write skew is impossible. If any sibling read needed to build that set fails, the
whole finalize is aborted rather than committed partially — losing a wakeup is
worse than retrying after the lease expires.

## Alternatives rejected

- **Insert the child immediately in `SpawnChild`.** Orphans on any failed
  transition, and dedup has no stable key under non-deterministic replay.
- **Finalize the child, then wake the parent in a second `Apply`.** The crash
  window between the two writes is exactly the bug.
- **Let the parent poll its children on a timer.** Turns a zero-write wait into
  periodic writes and adds latency for no benefit.

## Consequences

- `Repository.Apply` must support several process rows in one change set, and
  `ChangeSet.Guards` must be honoured as write-free preconditions. Both are in
  the contract (ADR-0004) and exercised by `repotest`.
- Metrics are **not** rolled up from child to parent. Each process meters
  independently; a `Limiter` sees `ParentID` and `RootID` and can aggregate
  across a tree itself. Sharing a budget would need a distributed counter, which
  is not worth the complexity here.
- `RootID` is inherited by every descendant, so a whole tree is queryable and
  correlatable by one id.
- A parent that spawns children and then does *not* wait on them is legal; the
  children run to completion independently.

## History

| Date | Change |
|---|---|
| 2026-07-20 | Initial record, extracted from the initial implementation spec (D15, D20, D46, D48). |
