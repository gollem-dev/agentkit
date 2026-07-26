# ADR-0018: Durable delivery is built inside `Repository.Apply`

## Summary

agentkit ships no delivery mechanism, and a caller who needs one picks from
three tiers it does provide the material for:

| Tier | Guarantee | Cost |
|---|---|---|
| `WithOnFinish` (ADR-0014) | best-effort; a crash in the window between the commit and the call loses it silently | none |
| a parent Process on `WaitChildren` | durable | the follow-up must itself be agent work |
| an outbox row written inside the caller's own `Repository.Apply` | durable | a table, a relay, and its monitoring |

The third tier is the only one that covers durable delivery to an external
system, and it is deliberately outside the kernel: `Apply` already commits a
whole `ChangeSet` atomically (ADR-0004, ADR-0009), so a row added there lands
with the terminal state or not at all, and `(ProcessID, Rev)` is unique per
committed transition, which makes it a replay-stable dedup key. No `Repository`
method, no outbox type, and no delivery port is added to reach it.

None of the three achieves exactly-once. That is not available (ADR-0003); the
choice is which side of the window fails.

## Context

ADR-0014 settled the handler: `WithOnFinish` fires immediately after
`Repository.Apply` succeeds, exactly once, and a crash in the window between the
two loses the notification permanently. That is a real window and its loss is
silent — nothing in the store records that a notification was owed.

It also named the alternative for anything that must not be lost: a parent
Process waiting on `WaitChildren`, where every step is part of a committed
transition. That answer does not reach an external system. Making the Slack post
a child's tool call moves the problem rather than solving it, because tool calls
are at-least-once (ADR-0003) and the post is not idempotent.

`docs/observability.md` already assigns delivery to the application and points at
tailing its own database. What it did not say is *how* the application gets a
record that is atomic with the transition, which is the whole difficulty: the
obvious placements are all outside the commit, and reproduce the window ADR-0014
accepts.

Two facts decide where it belongs. `Repository` is an SPI the caller implements
over its own store — `repository/memory` and `repository/filesystem` are
reference implementations, not deployment targets — so every production
deployment already owns an `Apply`. And the contract that makes `Apply` usable
for this is already in force and already tested by `repository/repotest`.

## Decision

Durable delivery is built by writing an outbox row inside the caller's own
`Apply`, in the same transaction as the `ChangeSet`.

Two clauses of the `Repository` contract (`repository.go`) are what make it
work, and both are already verified by `repotest`:

1. `Apply` applies the whole `ChangeSet` atomically. A row the implementation
   adds to that write inherits the same atomicity: the Process cannot become
   terminal without the row, and the row cannot exist for a transition that did
   not commit.
2. `Apply` checks each Process row's stored `Rev` and a single mismatch writes
   nothing. At most one `Apply` succeeds for a given `(ProcessID, Rev)`, so that
   pair identifies a committed transition and survives replay — an attempt that
   crashes before committing leaves nothing behind, and the retry commits under
   a `Rev` of its own.

`ChangeSet.Processes` carries the terminal `Process`, so `Output` and `Failure`
are in hand where the row is written. This matters because the kernel's
`process.finished` event carries a nil payload (ADR-0014): a relay that did not
capture the result at write time has to read the Process back.

Delivery itself — claiming a row, backoff, poison handling — is the caller's,
and runs outside the worker so a stalled destination does not stall transitions.

`WithOnFinish` is kept rather than deprecated in favour of this. The outbox is a
table, a relay process, retry policy and a monitoring signal; charging that to
"post to a dev channel when a run ends" is charging the cost of a durable system
to a use case that does not need one. The tiers are a choice, and the reason to
document all three is that the cheap one is the right answer more often than
not.

What the tiers do *not* differ on is exactly-once, which no tier provides.
`WithOnFinish` fails toward silent loss; an outbox fails toward duplication that
the receiver can detect and suppress. Converting an unrecoverable failure into a
recoverable one is the entire benefit, and a non-idempotent destination still
needs its own dedup.

## Alternatives rejected

- **An outbox inside the kernel, behind a delivery port on `Repository`.**
  ADR-0014 rejected this and it stays rejected; what this record adds is the
  boundary. Rejected *in the kernel*, because every `Repository` implementation
  would then have to support the port, and every guarantee agentkit states would
  have to say which of two delivery models it means. Intended *in the caller's
  implementation*, where it costs the kernel no vocabulary at all.
- **A post-`Apply` hook, or decorating `Repository.Apply` from outside.** `Apply`
  is the transaction boundary; from outside it there is only "before" and
  "after". Writing after reproduces exactly the window ADR-0014 accepts. Writing
  before is worse: an `Apply` that returns `ErrConflict` leaves a row announcing
  a transition that never happened.
- **Handing the caller a transaction handle to join.** The `Repository` contract
  deliberately requires no transaction mechanism — RDB TX, Firestore TX, a
  conditional write and a mutex are all valid realizations — so there is no
  handle to hand over. Introducing one would make the SPI harder to implement
  for the stores that do not have transactions, in order to serve callers who,
  by owning `Apply`, do not need it.
- **Deprecating `WithOnFinish` now that a durable path is recorded.** Stated in
  `Decision`: it would make every caller pay for a guarantee most notifications
  do not need.

## Consequences

- Durable external delivery requires owning a `Repository`. The reference
  implementations cannot host an outbox: `repository/filesystem` realizes
  atomicity as one snapshot rename and has no second durable table to write to,
  and `repository/memory` has no durability at all.
- The relay and its operational surface are the caller's: the claim query, the
  retry budget, the dead-letter decision, and the signal worth alerting on (the
  age of the oldest pending row).
- Duplicates reach the destination. This is the designed failure direction, not
  a defect to be fixed later.
- `(ProcessID, Rev)` is visible only inside `Apply`, and it is the key to reach
  for there: it names the committed transition, which is the unit an outbox row
  corresponds to. A reader outside `Apply` uses `Event.ID` instead
  ([ADR-0019](0019-events-are-addressable-and-reads-resume.md)) — it identifies
  and orders events within one Process, which is all this record needs, since
  delivery is per-Process anyway.

## History

| Date | Change |
|---|---|
| 2026-07-26 | Initial record. |
