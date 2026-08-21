# ADR-0021: Key-scoped concurrency is a semaphore on the Process row

## Summary

A caller can cap how many Processes run at once under a key of their choosing.
The cap is declared at **registration** — `WithSemaphoreKey(key, slots)`, a
`RegisterOption` — and each `Spawn` / `SpawnChild` names which **instance** of
that key it belongs to with `WithSemaphore(key, value)`. The row then carries
`Semaphore *ProcessSemaphore{Key, Value, Slots}` and `SemaphoreHeld bool`.

The unit that counts is the `(Key, Value)` pair. At most `Slots` distinct
process **trees** (`RootID`) hold one pair at a time, so `Slots == 1` is a mutex
and a child naming its ancestor's pair is admitted rather than deadlocked behind
it.

Three properties define the mechanism:

- **A slot is taken by a claim, atomically.** `ClaimNextProcess` only targets a
  row the pair can admit, and sets `SemaphoreHeld` in the same write.
- **A slot is released by termination, with no write of its own.** The holder set
  is defined over non-terminal rows, so a terminal row stops counting. There is
  nothing to leak if a worker dies, and no second `Apply` to close a window.
- **Waiting has no state of its own.** A row that cannot take a slot stays
  `pending` and is simply not a claim target. No `ProcessStatus` was added.

Enforcement lives in the `Repository` contract (ADR-0004 items 4, 8 and 9), not
in the worker: it is a multiplicity invariant over persisted rows, which is the
same shape as "one open Process per `Subject`" generalized from one to N.

`Spawn` against a full pair **succeeds**. The Process waits, and nothing bounds
how many wait, so `Kernel.GetSemaphoreStatus(ctx, key, value)` reports the
occupancy and the backlog a caller throttles on.

## Context

Three mechanisms already existed near this problem and none of them answer it.

`Process.Subject` with `ErrSubjectBusy` (`kernel.go`) refuses the `Spawn`
outright, and only at multiplicity one. `WithMaxConcurrent` (`worker.go`) bounds
how many claims one `Serve` drives, which is per instance and not per key. A
`ClaimMiddleware` (ADR-0012) can refuse a claim, but it brackets one claim: there
is nowhere in it to release something held across a suspend, and the two
alternatives for releasing outside it both fail — `WithOnFinish` is best-effort
by ADR-0014, so a crash between the commit and the call loses the release
permanently, and a TTL cannot be sized when a `waiting` Process may be parked on
a human for an arbitrary time.

The requirement that ruled the design was that occupancy is **per Process**, not
per claim: it must survive `waiting`. Once that is fixed, the release has to be
tied to something that cannot be lost, and the only such event is the row
reaching a terminal status — which is already a committed transition.

## Decision

**The slot count is declared per agent, and the spawn supplies only the value.**
The store enforces the limit and cannot read the in-process `Registry`, so
`Slots` has to be on the row. The question is who puts it there. Accepting it as
a `SpawnOption` would let two spawns name different counts for one pair, leaving
the runtime to reconcile them; declaring it at registration removes the
possibility, and lets `Register` reject two agents that declare one key with
different counts — the `Registry` is the only place that can see both. Sharing a
key between agents at the same count stays legal, and is how two agents serialize
against each other.

ADR-0010 reached the same shape for a different question, and its reasoning for
per-agent (the `Registry` is in-process and every worker builds the same one)
applies here unchanged. It is cited as a precedent, not as a rule: its subject is
the `Metrics` budget, and admission control is not that.

**The claim predicate and the `Apply` check must agree.** The predicate tests the
*prospective* state — this row added to the holder set, against the smallest
`Slots` including its own. Two things follow that are easy to get wrong:

- Reading only the candidate's own `Slots` admits a claim the `Apply` then
  refuses. `ClaimNextProcess` returns that refusal as an **error**, and since the
  same row is picked again on the next poll, the worker stops making progress
  entirely.
- "This row's tree already holds the pair" is not sufficient on its own. With two
  trees holding under `Slots 2`, a child of one of them declaring `Slots 1` grows
  no holder set but drops the limit to one, which two holders already exceed.

**The `Apply` check is monotone, not absolute.** It refuses a change that *makes*
a pair's occupancy worse; it does not require the resulting state to be within
the limit. An absolute rule cannot be satisfied by the very writes that would fix
an over-limit state — a terminal commit that drops one of three holders under a
limit of one still leaves two — so one over-limit pair would reject every `Apply`
on every Process, permanently, since a `ChangeSet` may touch several rows. With
the monotone rule, a state produced outside this contract (a stale worker, a
manual edit) drains as its holders finish.

**`Semaphore` is immutable and `SemaphoreHeld` is monotone, enforced at the
store.** Every kernel path builds its update from a clone of the row, so no
conforming path trips this; it exists because the row carries scheduling state
that code outside the kernel must not be able to unset.

**`Strategy.Limit` receives a copy of the Process.** It previously received the
row the commit is built from, a property ADR-0011 recorded explicitly. Writing
`Metadata` through it breaks the caller's own scope; writing `SemaphoreHeld`
breaks an invariant the store maintains, and produces a Process running outside
its own limit. `ToolFactory` and `Kernel.GetProcess` still hand out the live row.

**Observation is a `Repository` method.** ADR-0004 declined a dedicated claim SPI
and ADR-0010 declined a query by `RootID`, both because an existing mechanism
already covered them. Neither applies here: the number of rows waiting on a pair
is not accumulated anywhere and cannot be derived from `Apply` or from
`Process.Metrics`. `GetSemaphoreStatus` reports `Held`, the effective `Slots`,
`Waiting` and `OldestWaiting`; a pair nothing references is a zero value rather
than an error. It is an observation and not a reservation, so it cannot stand in
for the semaphore.

**On ADR-0011.** The kernel assigns no meaning to `Key` or `Value` — what they
stand for is the caller's business, exactly as with `Metadata`. It does interpret
their **equality**, and it interprets `Slots` as a number, because that is what
the claim predicate decides on. That is the line: no vocabulary about what a key
*is*, and a specified rule about how two of them compare.

## Alternatives rejected

- **`ClaimMiddleware` plus a lock store the caller supplies.** Requires no kernel
  change, and cannot release a slot held across a suspend: the middleware's scope
  is one claim, `WithOnFinish` is best-effort (ADR-0014), and a TTL cannot be
  sized against a Process waiting on a person.
- **A fourth `ChangeSet` entity for lock rows, acquired and released by the
  kernel.** Adds an entity to every `Repository` implementation, and
  `ClaimNextProcess` would not see it — so a claim would be taken and then handed
  back, which is the wasted claim this design exists to avoid.
- **Generalizing `Subject`.** `Subject` fails the `Spawn` fast, is looked up
  through its own finder, and is bound at multiplicity one. Merging the two would
  change all of that to express something that is a different question: refusing
  a duplicate turn versus queueing behind a limit.
- **Per-Process occupancy instead of per-tree.** A parent holding a slot and
  waiting on a child that names the same pair would deadlock. Failing the
  `SpawnChild` instead would require walking the ancestor chain on every spawn
  and would make a strategy's declaration depend on what its ancestors declared.
- **A `Slots` argument on `Spawn`.** Two spawns could then disagree about one
  pair's limit, and the runtime would have to reconcile them at every claim.
- **An absolute "never over the limit" `Apply` check.** Wedges the whole store
  once any over-limit state exists, including the writes that would clear it.
- **A new `ProcessStatus` for "waiting for a slot".** Reaches `Terminal()`, the
  claim predicate, `Cancel` and every lifecycle diagram, to record something the
  claim predicate already knows.
- **A timeout on a held slot.** There is no correct duration: a `waiting` Process
  may legitimately be parked on a person for days. `Cancel` and await deadlines
  are the existing ways to end one.
- **Deadlock detection across keys.** Would need a wait-for graph in the store.
  A Process holding one pair while waiting on a child that needs another can
  deadlock, and that is documented as the caller's responsibility.
- **Guaranteeing FIFO acquisition.** Eager dispatch claims a named Process
  through `Apply` and can overtake an older waiting row, so ordering would need a
  claim path that sees all candidates. The contract promises no order.

## Consequences

- **A slot is held across `waiting`.** An agent that suspends on a question keeps
  its pair taken until the answer arrives. This is the intended semantics and is
  also the way to stall a queue by accident; it belongs in the decision to put a
  semaphore on such an agent.
- **Nothing bounds the number of waiting Processes.** `Spawn` always succeeds.
  Throttling is the caller's, on `GetSemaphoreStatus`.
- **Acquisition order is not guaranteed, and starvation is reachable in the
  reference implementation.** `CreatedAt` ordering applies to the polling path
  only; a freed slot can be taken by a newly spawned Process through eager
  dispatch before a poller reaches an older row.
- **`ErrConflict` from `claimSpecific` now also means "the pair was full".** It
  needs no distinct handling — the row stays pending and a poller takes it when a
  slot frees — but a reader of that code path should know both meanings.
- **A refused claim keeps the slot for the whole backoff.**
  `ClaimNextProcess` takes the slot before the `Claim` middleware chain runs, and
  a middleware that returns without calling `next` goes to `requeue`, which
  preserves `SemaphoreHeld`. So a row that never executed occupies its pair until
  the backoff elapses, and a middleware that keeps refusing keeps re-taking it.
  Combined with `Slots == 1` that stalls the pair rather than throttling it, which
  is worth knowing because ADR-0004's history names a refusing `ClaimMiddleware`
  as a supported throttle.
  This is consistent with the model — a claim happened, and occupancy is per
  Process from its first claim — and releasing the slot on refusal was rejected:
  the store cannot tell the kernel doing it from strategy code doing it, so it
  would mean dropping the `SemaphoreHeld` monotonicity rule that stops a `Limit`
  from handing itself a free run. Use one mechanism or the other on a given key,
  not both.
- **`Repository` is a breaking change**: one added method
  (`GetSemaphoreStatus`) and the contract items above. An implementation that
  ignores them keeps every existing guarantee and silently enforces no limit, and
  fails `repotest`.
- **`Strategy.Limit` is source-compatible but behaviourally changed**: writes to
  the Process it receives no longer reach the committed row.
- **Rolling to this version is not a no-downtime upgrade.** A worker on the
  previous version does not read `Semaphore`, does not set `SemaphoreHeld`, and
  does not check the limit, so it claims freely while both versions run. The
  filesystem reference implementation makes it worse in one direction: its
  snapshot is the `Process` struct, so an older binary drops the unknown fields
  on read and loses them on its next write. Stop the old workers before creating
  Processes under a semaphore, and roll back only after the ones holding slots
  have terminated.
- **Two agents sharing a key must agree on the count.** `Register` enforces it
  within one process; across a fleet mid-deploy, the smallest `Slots` present
  binds.

## History

| Date | Change |
|---|---|
| 2026-08-21 | Initial record. |
