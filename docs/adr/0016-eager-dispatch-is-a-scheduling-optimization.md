# ADR-0016: Eager dispatch is a scheduling optimization

## Summary

An instance running `Serve` drives a `Process` the moment it becomes runnable —
on `Spawn`, `Respond`, a spawned child, or a woken parent — instead of waiting
for the next poll. This is **eager dispatch**, and it is a pure scheduling
optimization: it changes *when* a runnable Process starts executing, never *how*
it executes. Polling remains the ground truth and the fallback; everything eager
dispatch does, a poll-driven claim also does.

The guarantees of ADR-0003 are untouched — at-least-once execution, no effect
journal, one transition per `Apply`. Eager dispatch reuses the exact claim and
transition machinery (`runClaim`), fenced identically by `Rev` CAS and
`LeaseToken`.

Concurrency is two-tier: `WithPollConcurrency` (soft — the number of poll loops,
default 1) and `WithMaxConcurrent` (hard — the maximum claims driven at once by
poll and eager combined, default 64). A single semaphore of the hard size is
shared by both; polling is sub-capped to it, and eager may burst up to it. When
the hard limit is full, eager dispatch is a no-op and the row is left for a
poller — the fallback that makes this safe to drop.

Eager runs execute under the **`Serve` context**, not the caller's request
context.

## Context

Before this change, the only path from "runnable" to "running" was a worker
polling `ClaimNextProcess`. `Spawn` wrote a `pending` row and returned; `Respond`
moved `waiting`→`pending`; a finishing child moved its parent `waiting`→`pending`
— and in every case nothing executed until some worker's next poll came around.
Latency per hop was therefore bounded below by the poll interval (default 500ms),
and an interactive turn that spans several hops (spawn → run → suspend → respond
→ resume, or a parent that fans out to children and collects them) paid that cost
repeatedly.

For work that runs in the background this is fine. For a task a user is waiting
on, it is the dominant cost. `process-lifecycle.md` had already anticipated this:
"Wakeup is polling only. A LISTEN/NOTIFY-style push is a store-specific
optimization ... It can be added later as an optional interface without breaking
anyone." Eager dispatch is the in-process form of that push — it needs no store
support at all.

## Decision

When `Serve` starts it installs a `dispatcher` on the `Kernel` (an
`atomic.Pointer`, cleared when `Serve` returns). `Spawn`, `Respond`, and the
worker's commit points, after their commit succeeds, hand the newly-runnable
`ProcessID` to it. If a hard-limit slot is free, the dispatcher drives the
Process on a goroutine — claim the specific pending row, then `runClaim` — and
otherwise does nothing.

Several deliberate choices make this an optimization rather than a new execution
model:

- **Reuse `runClaim`, don't fork it.** The dispatcher claims a specific pending
  row (`claimSpecific`) and calls the same `runClaim` a poll loop calls. All the
  fencing, retry, cancel-check, and at-least-once behaviour is inherited, not
  re-implemented.
- **Claim via `Apply`, not a new SPI.** `claimSpecific` writes `running` + a
  fresh `LeaseToken` with an ordinary `Rev` CAS `Apply`. It races a poller's
  `ClaimNextProcess` safely because the two are mutually linearizable on the row
  (now stated in the Repository contract, ADR-0004): exactly one advances the
  `Rev`, the other loses. It targets `pending` rows *only* — reclaiming an
  expired-lease `running` row stays `ClaimNextProcess`'s job, so eager never
  inflates `unclean_reclaims`.
- **Run under the `Serve` context.** The eager goroutine uses the `Serve`
  context for both cancellation and values — the same context a poll-driven claim
  and a crash-resume see. It does *not* inherit the caller's request context.
  Inheriting request values would make what `ToolFactory` / `Limiter` /
  middleware / `Step` observe depend on which trigger won the claim, and a
  crash-resume can only ever see the `Serve` context anyway — so request-context
  values would silently diverge between the eager run and its own replay.
  Request-scoped scope belongs in `Process.Metadata` (ADR-0011).
- **Hard limit is a ceiling on drivers, not on rows.** The shared semaphore
  bounds how many claims this `Serve` drives at once. It is not a count of
  `running` rows: a driver that panics frees its slot while the row stays
  `running` until its lease lapses, and rows on other instances are not counted.
- **One active `Serve` per Kernel.** The dispatcher and its semaphore are
  per-`Serve` state; a second concurrent `Serve` on the same Kernel returns
  `ErrServeActive` rather than silently clobbering them.
- **Best-effort, after the commit.** Dispatch happens *after* the durable commit
  and *before* the transition's `OnCommit` / `OnFinish` callbacks, so a slow
  handler cannot delay a runnable child or parent. A consequence is that a child
  may begin executing before its parent's `OnCommit` runs.

The `Kernel.dispatcher` field is the one piece of mutable state on an otherwise
immutable `Kernel`. It holds no business state — losing it only degrades to
polling — so it does not violate the intent of "no cross-request state in process
memory". It is the second explicit exception to that rule, after `Registry`.

## Alternatives rejected

- **A store-level `LISTEN`/`NOTIFY` push.** Would tax every `Repository`
  implementation and only helps cross-instance wakeups. Eager dispatch is
  in-process and needs no store support; cross-instance wakeup stays polling.
- **A dedicated `ClaimProcess(pid, ...)` SPI.** Cleaner responsibility for claim
  vs unclean-reclaim, but adds a method to every `Repository` and to `repotest`.
  Claiming via `Apply` plus a linearizability clause in the contract achieves the
  same with no new method (ADR-0004).
- **Inherit the caller's request context (values via `WithoutCancel`).** Rejected
  above: it makes execution semantics depend on the trigger and diverges from
  crash-resume.
- **Opt-in (disabled by default).** Rejected because eager dispatch is the point
  of the feature and there are no existing callers to protect; a forgotten flag
  would just silently reinstate the latency. There is no "disabled" state — to
  run polling-dominant, set the hard limit close to the soft limit.
- **Independent poll and eager pools (total = soft + hard).** Rejected: the hard
  limit is meant as an absolute ceiling on this instance's concurrency, so a
  single shared semaphore is correct.

## Consequences

- Interactive latency drops from "bounded by the poll interval per hop" to "the
  actual work time", on any instance running `Serve`.
- **Eager dispatch requires `Serve`.** A pure caller instance that never calls
  `Serve` gets no eager dispatch; its rows wait for some other instance's poll.
- A blocking `Step` is now worse than before: besides holding a claim, it sits on
  the interactive latency path and occupies a hard-limit slot. The
  "`Step` must not block" rule matters more (execution-model.md).
- An eager claim whose `Apply` fails *indeterminately* (the filesystem store's
  post-rename error) may leave the row `running` with a lease nobody drives; a
  poller reclaims it as an unclean reclaim. Rare, bounded by
  `WithMaxUncleanReclaims`, and always recovered by polling. "Apply returned an
  error" therefore does not mean "nothing was committed".
- Callback ordering is now specified: dispatch precedes `OnCommit`/`OnFinish`, so
  a child can start before the parent's `OnCommit` fires.

## History

| Date | Change |
|---|---|
| 2026-07-22 | Initial record. |
