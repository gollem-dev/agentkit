# ADR-0019: Events are addressable, and reads resume from an id

## Summary

Every `Event` carries an `EventID` — a uuid v7 the kernel mints in `newEvent`
(`event.go`), the same way `ProcessID` is minted. A `Repository` stores it
verbatim and never assigns one of its own.

`ListEvents` takes that id as a cursor plus a count cap. `Kernel.ListEvents` is
an entry point, so the two optional values arrive as `ListEventsOption`
(`WithAfterEvent`, `WithEventLimit`) per ADR-0005; they resolve into an
`EventQuery`, which is what `Repository.ListEvents(ctx, pid, q)` receives. A zero
`EventQuery` means "all of them"; an id the Process has no event for is
`ErrEventNotFound` and returns nothing.

There is still no global feed and no ordering across Processes. Delivery remains
the caller's, built in its own `Apply` (ADR-0018); what this record adds is the
handle that makes such a reader resumable.

## Context

`event.go` has always said channel delivery is done by the caller subscribing to
these events, but the package gave a subscriber nothing to subscribe *with*.
`ListEvents` returned every event a Process had ever appended, and `Event`
carried no field distinguishing two occurrences of the same type: `At` comes
from `k.clock()` per `Emit` call, so a fixed test clock or a store that truncates
below its precision makes two events in one transition indistinguishable, and
clock skew between the workers that ran successive transitions can order them
backwards.

A caller writing its outbox inside `Apply` (ADR-0018) has an escape hatch:
`(ProcessID, Rev)` names one committed transition and is unique by the Rev CAS
of ADR-0004. But `Rev` is not on the read path, so a caller using the public
`Kernel.ListEvents` had no equivalent — it could not resume, and could not build
a deduplication key without inventing one inside its own `Payload`.

The cost of fixing this only grows: an id is a persisted field, so every
`Repository` implementation has to carry it. Doing it while implementations are
few is the cheap moment.

## Decision

**The id is a uuid v7, minted by the kernel.** It costs no coordination —
`uuid.NewV7` is already used for `ProcessID`, the child id in `syscalls.spawn`,
the worker id and every `LeaseToken` — and it leaves implementations a single
opaque column. The two alternatives both charged someone more for the same
result (below).

Minting is funnelled through `newEvent` rather than left to struct literals, so
the four sites that build an event — `kernel.go` (`process.created`),
`worker.go` (`await.created`, `process.finished`) and `syscalls.go` (`Emit`) —
cannot forget it.

**The cursor is exclusive, and an unknown one is an error.** Returning the whole
list for a cursor the Process does not have would be indistinguishable, at the
caller, from a genuine burst of new events; a delivery loop would resend
everything and only notice by the duplicates arriving downstream.
`ErrEventNotFound` names the one thing that actually happened: the stored cursor
went stale.

**The optional values stay optional on both sides of the boundary, in the form
each side can use.** ADR-0005 requires optional values behind functional options,
and `Kernel.ListEvents` takes them that way. Passing the same pair down as bare
positional arguments would have broken that rule at the SPI while claiming
`ClaimNextProcess(ctx, workerID, leaseUntil, now)` as precedent, which it is not:
every argument there is required, and required arguments are positional by the
same rule.

So the options resolve into `EventQuery`, an all-optional struct whose zero value
is the default read. That satisfies "no config struct mixes the two" — `pid` is
required and stays positional — while leaving an implementer reading two fields
rather than resolving closures over a type it does not own.

`repotest` verifies the parts an implementation can get wrong: that ids
round-trip unchanged and stay distinct, that the cursor is exclusive, that the
end of the list is empty rather than an error, that `limit <= 0` is uncapped,
and that an unknown cursor is `ErrEventNotFound` with no events.

## Alternatives rejected

- **A per-Process `Seq` counter.** It orders and dedups just as well, and it is
  what an append-only log usually carries. It loses on who pays: the kernel does
  not know a Process's current event count (`StateSeq` counts transitions, not
  events), so either the kernel reads before writing, or every `Repository`
  implementation maintains a per-Process sequence. A uuid asks the implementer
  for one opaque column and nothing else.
- **Stamping `(Rev, index-within-the-ChangeSet)`.** Free, replay-stable, and
  already meaningful — this is exactly what an outbox built inside `Apply` uses.
  It fails as a *type* field because it would require every event to travel with
  a Process row write, which the SPI does not demand: `repotest` applies a
  `ChangeSet` of events alone, and narrowing the contract to forbid that buys
  nothing the uuid does not already give.
- **Leaving identity out and documenting `(ProcessID, Rev)` instead.** Only
  reachable from inside `Apply`. It serves the caller writing an outbox and
  abandons the one reading through `Kernel.ListEvents`.
- **A global feed with a cross-Process cursor.** A uuid v7 sorts by time only
  approximately — skew between workers, and collisions inside a millisecond —
  so a global cursor would silently skip events that commit late. Making it
  correct needs a commit-ordered sequence, which is hard on the stores this SPI
  deliberately supports (a conditional write, a document store) and would push
  agentkit toward being the delivery system ADR-0018 declines to be.

## Consequences

- **Breaking for `Repository` implementers**, twice over: `ListEvents` changes
  shape, and `Event.ID` is a new column. There is no migration for events
  already stored — an existing row has no id, and a deployment carrying data
  across this change has to backfill one.
- `Kernel.ListEvents` is source-compatible: the options are variadic, so
  existing call sites keep working and keep returning everything.
- A caller can now deduplicate on `Event.ID` and resume on it, but only within
  one Process. Ordering across Processes is still not a thing agentkit offers.
- `newEvent` is unexported, so a `Repository` implementation cannot mint an
  event that looks kernel-issued. Test suites that build events directly supply
  their own ids, as `repotest` does.

## History

| Date | Change |
|---|---|
| 2026-07-26 | Initial record. |
