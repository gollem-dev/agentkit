# ADR-0015: Unclean reclaims are counted separately and bounded

## Summary

A claim that takes over a Process whose previous claim died mid-transition is an
**unclean reclaim**. `ClaimNextProcess` counts it on the row
(`Process.UncleanReclaims`), the worker bounds it with `WithMaxUncleanReclaims`
(default 3), and exceeding the bound terminates the Process as `failed` with
`FailureUncleanReclaim`.

That counter is deliberately **not** `StepAttempts`. An error tells a strategy
how far the previous attempt got; a vanished claim tells it nothing — the
transition may have run every effect and died before its commit, and a
lease-expiry reclaim may overlap a predecessor that is still running. Two
counters let a caller say "retry errors three times, but never re-run after a
crash", which one counter cannot express.

Both counters are readable from the strategy through `Syscalls.Attempt()` and
from middleware through `EffectContext.Attempt`, as `AttemptInfo`.

Counting only means anything while `running` really does imply "the previous
claim vanished", so the worker maintains that: **a claim never ends with its row
`running` under its own `LeaseToken` unless the row itself names a different
one**. Every read and write that moves a row out of `running` runs on a context
derived with `context.WithoutCancel` and bounded by `WithSettleTimeout`, and a
cancelled claim charges neither counter.

ADR-0003 is unchanged: replay is still at-least-once and there is still no
effect journal. What changes is that replay is now **bounded** and **visible**.

## Context

`claimable()` already distinguished "claim from pending" from "take over a
running row whose lease lapsed", but `ClaimNext` discarded that distinction and
wrote the same fields either way. Two consequences followed.

`StepAttempts` is incremented only by `requeue`, which runs only when `Step`
returned an error. A transition that crashed before committing never passes
through it, so **re-execution after a crash was unbounded**: a Process that
reliably killed its worker would be picked up, kill the next worker, and repeat
forever. `WithMaxStepAttempts` never applied to that path.

Separately, ADR-0003 tells strategy authors to treat "have I already done this?"
as state, but the kernel gave them nothing to condition on. A strategy could not
tell a first run from a replay, so the obligation had no matching tool.

## Decision

`Process.UncleanReclaims` is maintained by `ClaimNextProcess` and reset to 0 by
the worker on any successful commit — the same scope as `StepAttempts`. Putting
it in the Repository contract (item 4) rather than in the worker is what makes
it correct: only the store can atomically observe "this row was `running` when I
took it" as part of the claim itself.

In the reference store, `claimable` became `claimKindOf`, returning
`notClaimable` / `cleanClaim` / `uncleanClaim`. Naming the reason where it is
decided keeps a future claimable status from being silently counted as clean.

The counting is only meaningful because `running` really does imply "the previous
claim vanished", and that is a property the worker has to *maintain*, not one it
gets for free. Every orderly exit from a claim — suspend, terminate, requeue,
release — must move the Process off `running` and clear the lease. The two that
happen outside a commit, `requeue` and `release`, therefore retry their `Apply`
on `ErrConflict` from a fresh read and abandon only when the lease token has
changed. Letting a conflict there go by would leave the row `running`, and the
next claim would bill an orderly exit as a crash: an error the worker actually
observed would be charged to the wrong budget, and `StepAttempts` would never
record it. That is not a hypothetical — a concurrent `Respond` or a sibling
finalize advancing `Rev` mid-transition is enough to produce it.

Stated as the invariant the worker enforces: **a claim never ends while its row
is `running` under that claim's own `LeaseToken`, unless the row proves the lease
is gone (`LeaseToken` names someone else).** `ClaimAbandoned` therefore means
"another owner has it", not "our context died". Two mechanisms carry it:

- **Settling does not ride the claim's context.** `requeue`, `release` and
  `finalizeClaimed` derive theirs with `settleCtx` —
  `context.WithTimeout(context.WithoutCancel(parent), WithSettleTimeout)`, 5s by
  default. Cancelling the `Serve` context is what a host does to ask a worker to
  stop, and it is therefore the most common reason a row has to be settled at
  all; a settle inheriting it could never reach the store. The detach is
  unconditional rather than conditional on `parent.Err() != nil`, because the
  cancel can arrive between the test and the write. The timeout is what keeps a
  store that stopped answering from holding `Serve` open, and it bounds the
  settle as a whole: `settleCtx` returns a context that is already a settle
  context unchanged, so a settle that reads before it writes gets one budget
  rather than one per store call. The strategy's own work keeps the claim's
  context and still stops promptly: what survives a shutdown is only the
  bookkeeping recording that the work stopped.
- **An unknown commit outcome is resolved from the row, not the error.** A
  non-conflict `Apply` failure does not prove the write missed the store, so the
  worker re-reads on the settle context. For a transition commit `StateSeq`
  decides, because every commit this worker builds writes it and nothing else in
  the kernel touches it — `Rev` cannot, since a concurrent `Cancel` advances the
  `Rev` of a running row without committing anything. Unchanged means nothing
  committed and the transition is retried; advanced means the commit landed, so
  its metrics and attempt reset stand and the row is only put back (via
  `requeueInfra`, whose backoff is what stops a store failing this way from being
  re-claimed in a tight loop by eager dispatch). For a terminal commit the status
  decides, tested after the lease so that a Process another owner finished is not
  credited to this claim. Reporting follows the row: `ClaimFinished` for a
  terminal one, `ClaimSuspended` for a waiting one. That also ends a misreport —
  a landed terminal commit whose response was lost used to surface as
  `ClaimAbandoned`.

The reads that decide are treated the same way when they themselves fail. A read
failure at the claim loop head, or on the re-read that would rebuild a commit
after `ErrConflict`, is not evidence the lease was lost — only a row naming a
different token is — so the row is requeued as infrastructure rather than
abandoned. `commitFinal` separates the two cases for the same reason: it used to
fold a failed re-read into "already terminal by another path" and report the
commit as settled, which would announce a finished Process for a row that may
still be running under this claim's lease.

The bound is checked in `runClaim` **before** `Step`, unlike the `StepAttempts`
bound which is checked after a failure. An unclean reclaim is already counted by
the time the Process is claimed, so the only useful question is whether to run
at all; asking afterwards would make `WithMaxUncleanReclaims(0)` unable to mean
"do not re-run". The comparison is `UncleanReclaims > n`, matching the existing
`StepAttempts+1 > n` convention where n permits n further attempts after the
first.

`FailureUncleanReclaim` is a separate code because the two failures need
different responses: `retry_exhausted` says a strategy kept erroring and someone
should read its code, while `unclean_reclaim` says workers keep dying and
someone should look at the workers.

`AttemptInfo.Errors` counts attempts in which `Step` itself failed. A fault
before `Step` — a `ToolFactory` that could not build the tools, say — requeues
without consuming an attempt, because nothing the strategy could have done ran,
and reporting it as a replay would tell a strategy that effects may have fired
when none could have.

A transition that failed because the worker was asked to stop is exempt on the
same reasoning, even though `Step` did run: it produced no decision. `Step`
failing with the claim context's own `context.Canceled` while that context is
done requeues as infrastructure. Charging it would spend the retry budget on
shutdowns, so a Process unlucky enough to meet several restarts would end as
`FailureRetryExhausted` with nothing wrong with it. Both halves of the test
matter — a deadline the strategy set on its own work surfaces while the claim
context is still live, and that is a real transition failure that keeps costing
an attempt.

## Alternatives rejected

- **One counter for both.** The obvious simplification, and it collapses exactly
  the distinction that matters. The two signals differ in what is known (an
  error has a known reach; a crash has none), in danger (only a lease-expiry
  reclaim can overlap a live predecessor), and therefore in the bound an
  operator wants. Merged, "three error retries but no crash re-runs" becomes
  inexpressible.
- **Bounding crash replay inside the worker instead of the store.** The worker
  cannot distinguish "claimed from pending" from "took over a running row" after
  the fact — the claim already overwrote the status. Only the atomic claim sees
  it.
- **Making the bound infinite by default, to preserve today's behaviour
  exactly.** That leaves the hole open and merely renames it. A crash-looping
  Process burning an LLM budget forever is not a behaviour worth preserving; the
  default of 3 keeps the "a crash resumes" posture while ending the unbounded
  part.
- **Defaulting to 0 (never re-run after a crash).** Safe, and it inverts what
  agentkit is for. Callers who cannot tolerate duplicated side effects opt in.
- **Exposing the raw counters instead of `AttemptInfo`.** `Process` is already
  reachable from middleware, but a strategy sees only `Syscalls`, and the pair
  of numbers needs the explanation that `IsReplay` and the field docs carry. A
  named type is where that explanation lives.
- **Reinstating an effect journal so replay could be exact.** ADR-0003 removed
  it; nothing here needs it. Bounding and reporting replay is not the same as
  eliminating it.
- **Detaching a settle only when the parent context is already done.** It looks
  like the smaller change and it keeps the same defect: the cancel can arrive
  between the test and the write. A conditional detach narrows the window rather
  than closing it.
- **Detaching the transition commit and the terminal commit too.** Tempting,
  because the transition already ran and its LLM calls were already paid for. It
  was declined: those `Apply` calls start further work on the way out — eager
  dispatch of buffered children and the spawn `OnCommit` callbacks — and the
  at-least-once model (ADR-0003) already covers losing an uncommitted transition.
  Re-reading the row afterwards is what keeps the invariant without extending a
  dying instance's reach.
- **Leaving a row whose commit landed indeterminately to its lease.** That is the
  behaviour this record exists to prevent: it is unreachable for a full lease and
  the takeover is billed as a crash. The row is put back instead, and only a
  different `LeaseToken` permits walking away.

## Consequences

- **This is a behaviour change for existing callers.** A Process that used to be
  resumed indefinitely after crashes now terminates as `failed` with
  `FailureUncleanReclaim` on the fourth unclean reclaim. Callers who want the
  old behaviour set a large bound explicitly.
- Third-party `Repository` implementations gain an obligation. One that does not
  implement it leaves the counter at 0, the bound never trips, and behaviour
  degrades to the previous unbounded replay — no crash, but no protection
  either. `repotest` covers the contract.
- `Syscalls` gained a method. It is sealed against outside implementations, so
  only embedding wrappers need recompiling.
- `UncleanReclaims > 0` does not mean the predecessor is gone. A lease-expiry
  reclaim may run alongside a worker that is still alive and will only discover
  it lost at its next fence check. Anything a strategy does on the strength of
  `IsReplay()` must tolerate that.
- The bound only fires on the first transition of a claim that took over a dead
  one, because a successful commit clears the counter. That is intended, and it
  is why the check sits inside the loop rather than before it.
- A settle can outlive the cancellation that triggered it, by up to
  `WithSettleTimeout`. `Serve` joins its claims before returning, so that time is
  spent inside the host's grace period between asking a process to stop and
  killing it. The default of 5s assumes a grace period of roughly 10s; a shorter
  one needs a shorter setting.
- A claim ending after a shutdown does not fold its run's metrics: `requeueInfra`
  writes `Metrics{}`. Effects the interrupted transition already paid for are
  therefore missing from `Process.Metrics`, so a budget read from it under-counts
  after a shutdown.
- There is no `EventKind` for a claim that ended abnormally and no log line on
  the settle success path, so a clean shutdown followed by recovery leaves no
  record beyond the row itself.

## History

| Date | Change |
|---|---|
| 2026-07-21 | Initial record. |
| 2026-09-10 | Stated the settle invariant explicitly and recorded the two mechanisms that carry it: `settleCtx` detaching every row-settling read and write from the claim's cancellation (`WithSettleTimeout`, default 5s), and resolving an unknown commit outcome from the row. A cancelled claim now charges neither counter. Prompted by a rolling update in which a cancelled `Serve` context left claimed rows `running` for a full lease. |
