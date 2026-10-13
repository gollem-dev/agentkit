# ADR-0021: A cancel reaches every descendant

## Summary

When a Process commits as `cancelled`, every descendant that has not finished —
every Process whose `ParentID` chain leads to it — is cancelled as if `Cancel`
had been called on it, with the nearest cancelled ancestor's `CancelReason`.
That includes the deferral of
[ADR-0020](0020-cancel-waits-for-a-closed-conversation.md): a running
descendant whose conversation is mid-round runs further transitions first, and
one of them may still end it as `succeeded` or `failed`.

The guarantee rests on the descendant's side. At the top of every transition the
worker reads the Process's ancestors and treats a cancelled one exactly like the
Process's own `CancelRequested`. On the parent's side, right after the cancelled
commit, the children named in the awaits that commit closed are passed to
`Kernel.Cancel`. That cascade only makes waiting children end at once; losing it
changes when a descendant ends, never whether its cancel is observed.

No `Repository` method, `Process` field or public signature is added.

## Context

`Kernel.Cancel` used to end only the Process it was given. `commitFinal` closed
that Process's open awaits and reported to its parent, and wrote nothing to its
children. A parent that spawned children and suspended on `WaitChildren` could be
cancelled while they kept running: the workers claimed them, called the model and
spent money on results that no Process would ever read.

Two facts about the data decide where the fix can live:

- A child always reaches its ancestors through `ParentID`.
- A parent reaches its children only through the `Children` of its own children
  awaits. No list of children is stored, so a child the parent never waited on —
  which [ADR-0009](0009-child-processes-committed-atomically.md) permits — and a
  child spawned in the parent's last transitions are invisible from the parent.

## Decision

**The descendant checks.** `driveClaim` calls `cancelledAncestor` at the loop
head, after the lease check and only when the row's own `CancelRequested` is not
set. It walks `ParentID` to the root and returns the first cancelled ancestor.
If there is one, the Process takes the same branch as its own cancel request,
with that ancestor's `CancelReason`.

- **Every ancestor, not only the parent.** When the cascade is lost, an
  intermediate Process waiting on its own children never ends by itself, so the
  parent alone would hide a cancelled root above it.
- **A cancelled ancestor anywhere counts.** A descendant is defined by the
  `ParentID` chain, so the walk continues past an ancestor that succeeded or
  failed.
- **A missing ancestor ends the walk**, the same reading `reportToParent` gives a
  missing parent. Treating it as a failed read would requeue the Process forever.
- **A failed read runs nothing.** The claim is put back with `settleFailedClaim`,
  without charging an attempt, like a failed read of the row itself.
- **The deferral applies.** A cancel derived from an ancestor waits for the
  conversation to close exactly as an own request does
  ([ADR-0020](0020-cancel-waits-for-a-closed-conversation.md)).

**The parent cascades.** After a cancelled commit lands, `commitFinal` passes the
children of the children awaits that commit closed to `cancelChildren`, which
calls `Kernel.Cancel` on each with the parent's reason. A pending or waiting child
ends inline, and its own terminal commit cascades in turn. A running child gets
`CancelRequested`. `ErrProcessFinished` is ignored, and any other error is logged
and not returned. The cascade runs before the completion handler, whose duration
is the caller's, and in the caller's goroutine, because `Cancel` is also called
from an application process that runs no `Serve` and has no dispatcher.

**Why this is not a second write in one transition.** The parent's transition is
still one `Apply`. Each child's cancel is a write to that child's own row — its
terminal commit, or the `CancelRequested` flag of a running child — made by its
own `Apply`. Nothing depends on the cascade having happened: a
lost cascade degrades to the loop-head check, just as lost eager dispatch
degrades to polling ([ADR-0016](0016-eager-dispatch-is-a-scheduling-optimization.md)).

**Usage is not stranded at a terminated parent.** A cancel ends a parent before its children,
and a parent reports its own total upward once, at its terminal commit. A
descendant that ends later would therefore stop at its terminated parent. So
`reportToParent` adds a child's `Metrics` to its parent and, while that parent is
terminal, to each ancestor above it, through the first one that is not terminal
or the root. Each ancestor reports exactly once, so nothing is counted twice, and
every row written is a CAS target that serializes against that ancestor's own
terminal commit.

## Alternatives rejected

- **Write the descendants in the parent's terminal `ChangeSet`.** A running
  descendant is leased by another worker, so the parent could only set its flag,
  not end it. Every descendant would become a CAS target and contend with its own
  commits. Grandchildren are not reachable from the parent's awaits at all.
- **Add a `Repository` method that lists a Process's children.** A change to the
  SPI, every implementation and `repotest`, and running descendants would still
  have to be ended by their own workers. The loop-head check gives the same
  guarantee without it.
- **Check only the direct parent.** Misses everything below an intermediate
  Process that waits on its own children once the cascade is lost.
- **Copy the cancel down into each descendant's row.** The copy depends on the
  parent reaching the child, which it cannot do for unwaited children, and a
  stored copy that can disagree with the ancestor's row needs a rule for which one
  wins.
- **Run the cascade through eager dispatch.** An application process that calls
  `Cancel` without running `Serve` has no dispatcher, so waiting children would
  never end there.
- **Fold a late descendant's usage into its parent only.** Under a cancel that is
  the usual order, so the root of a cancelled tree would always under-report what
  the tree spent, and no API lists a tree's rows for a caller to add them up.

## Consequences

- **A child cannot outlive a cancelled ancestor**, beyond the deferral a running
  child gets. This includes a child nobody waited on. Work that must outlive its parent is started as its own root with
  `Agent.Spawn`, not with `SpawnChild`.
- **Every transition of a Process with a parent costs up to its depth in extra
  `GetProcess` calls.** A root pays nothing.
- **A waiting descendant can stay waiting** when the cascade was lost — a crash
  between the parent's commit and the cascade, or a failed write. It calls no
  model while it waits, and it ends at the next claim: a response, a timer, or its
  own last child finishing.
- **Responding to such a descendant succeeds**, and the descendant ends as
  cancelled when it is claimed.
- **A pending descendant still runs its `ToolFactory`** before the loop-head check,
  as a Process with its own `CancelRequested` does. It calls no model.
- **A running child's in-flight non-terminal transition is discarded.** The
  cascade's `CancelRequested` advances the child's `Rev`, so a `Continue` or
  `Suspend` commit the child was about to make fails with `ErrConflict` and the
  child goes back to the loop head. That transition's LLM usage is not added to
  `Metrics`. A `Done` or `Fail` commit is retried under the same lease and lands.
  A direct `Cancel` of a running Process has always behaved this way.
- **Late usage now travels past terminated ancestors.** A child's usage is added
  to its parent and to each terminated ancestor above it, through the first one
  that has not terminated. A grandchild that ends after its parent succeeded now
  reaches the root when the root is that first one, where it used to stop at the
  parent. Above a non-terminated ancestor the usage arrives when that ancestor
  itself terminates, as before.

## History

| Date | Change |
|---|---|
| 2026-10-13 | Initial record. |
