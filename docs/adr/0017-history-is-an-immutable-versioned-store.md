# ADR-0017: History is an immutable, versioned, per-agent store

## Summary

Conversation History (`*gollem.History`) is persisted **outside the Process
record**, in a separate blob store, opted into **per agent**. `Register` accepts
`WithHistoryStore(HistoryStore)`, which enables `sys.Session()`. The store keeps
**immutable versions**: `Save` writes a new one and returns an opaque
`HistoryRef`, never rewriting an earlier version. Which version is current is
decided by `Process.HistoryRef`, committed in the same `Apply` as State, so the
bytes stay out of the transactional store while the pointer rides the commit.
A transition saves before it commits; if the commit does not happen, the version
it wrote is simply never named, and the next attempt re-seeds from the committed
one. History therefore advances and rolls back **together with State**. After a
commit the worker calls `Discard` on the superseded version — a notification, not
a deletion order. Without the option, `sys.Session()`'s methods return
`ErrHistoryNotConfigured` rather than silently running without persistence.

A new Process can **start from a version another Process committed**:
`Spawn(..., WithInheritedHistory(from))` resolves that Process's current
`HistoryRef` and pins the pair on `Process.InheritedHistory`. It is read-only —
the inheriting Process saves its own versions under its own id, and the kernel
never `Discard`s an inherited version, because the issuing Process's record may
still name it.

## Context

Every strategy that talks to an LLM must thread `*gollem.History`: read it, pass
it to `Generate`, fold the result back, checkpoint it. `strategy/simple` and
`strategy/planexec` each reimplement this in their own state. History is also
unlike State: State is a snapshot replaced wholesale each transition, whereas
History is append-only and grows unbounded — potentially too large to sit in the
same transactional record (an RDB) as State. We wanted the runtime to absorb the
threading, without putting a large blob on the Process record and without the
kernel learning to (un)marshal caller data (ADR-0007).

The first version of this decision stored History under a single key per
process, overwriting it on every save. That made the save **destructive**: a
crash between the save and the commit left the store one turn ahead of committed
State with no way back, because the previous content was gone. The consequence
was an obligation on strategies — keep a tool round inside one Step, so a
persisted History never ends on an unanswered `tool_use` — and that obligation
ruled out the case it most needed to support: pausing for a human between the
model asking for a tool and the tool running. Making versions immutable removes
the cause instead of documenting the symptom.

## Decision

- **Define an agentkit port, `HistoryStore`**, rather than reusing gollem's
  `HistoryRepository`. Its `Load`/`Save` pair addresses one blob per key and
  cannot express either a version or a release, both of which this design needs.
  The port is `Save(ctx, pid, h) (HistoryRef, error)`, `Load(ctx, pid, ref)`,
  `Discard(ctx, pid, ref)`. Reference implementations stay in
  `historystore/{memory,filesystem}`; the filesystem one now stores one file per
  version under a per-process directory.
- **The store mints the ref and hands it back.** The kernel treats it as an
  opaque string. Content-addressing it in the kernel would mean marshaling
  History there, which ADR-0007 forbids; leaving it to the store also lets an
  implementation choose UUIDv7 (the reference ones do, so refs sort by creation
  time) or a content hash. The one binding rule is that a ref returned once must
  never later name different content.
- **`Process.HistoryRef` is the pointer, and it commits atomically.** It is an
  ordinary typed field written in the transition's own `ChangeSet`, so nothing
  about invariant 5 ("one transition is one `Apply`") changes. `""` means no
  version has been committed yet, which is why a store must never return it.
- **Opt-in is per agent.** `WithHistoryStore` is a `RegisterOption`, carried on
  the `StrategyBinding`. `Kernel.New(repo, ...)` is unchanged; the transactional
  `Repository` and the blob `HistoryStore` are injected through different
  channels.
- **`Syscalls.Session()` returns a handle** with `Generate`, `CallTool` and
  `History`. It always returns a usable handle; the methods report
  `ErrHistoryNotConfigured` when the agent has no store, so a misconfiguration
  surfaces at the point of use rather than as a nil check a caller can forget.
  The handle is scoped to one transition, like the `Syscalls` it came from.
- **`Session().CallTool` appends the tool's result to the conversation**, so a
  Step can close a `tool_use`/`tool_result` pair without spending another LLM
  turn, and the following `Generate` can be called with no input at all. Exactly
  one `tool_response` is appended per call, carrying `IsError` when the call
  failed: the model asked for the call, so the next request has to answer it.
  This is the one place the kernel builds a `gollem.Message`; it still does not
  interpret History, and it cannot tell a model-requested call from a strategy's
  own, so `Syscalls.CallTool` remains the one to use for the latter.
- **Save precedes commit.** In `worker.go` the save runs ahead of both
  `buildCommit` (which records the ref it returns) and the `commitTerminal` on
  the Done/Fail path, because the commit is the completion marker: durable work
  comes first. A save failure is a transition error (requeue), never a silent
  skip.
- **`Discard` is a notification with no return value.** Reclaiming immediately,
  deferring, or ignoring are all conforming. The kernel would only log a failure
  and carry on, so accepting an `error` it cannot act on would be a lie in the
  type. It is called in exactly four places, all of which know the commit
  outcome: after a successful non-terminal commit and after a successful terminal
  one (releasing the superseded version), and on the two failures that *prove*
  nothing committed — a `buildCommit` error and an `ErrConflict` from `Apply`
  (releasing this attempt's version). Any other `Apply` error leaves the outcome
  unknown, so nothing is released; releasing there could destroy the version the
  record now names.
- **No lease fence around the save.** A worker that lost its lease can only add a
  version nobody references, so the pre-save `ownsLease` re-read the earlier
  design needed is gone, along with its extra `GetProcess` per transition.
- **A Process may start from another Process's version, by reference only.**
  `WithInheritedHistory(from ProcessID)` is a `SpawnOption`; the pair it resolves
  to lands on `Process.InheritedHistory`, a field distinct from `HistoryRef`.
  That separation is the whole point: `HistoryRef` is what the post-commit
  `Discard` releases as superseded, and it is released under the *committing*
  Process's id, so putting an inherited ref there would announce another
  Process's live version as garbage on the very first commit. Kept apart, the
  first commit's `prev` is `""` and nothing is released. `ensureLoaded` reads the
  inherited pair only while `HistoryRef` is empty; once the Process commits a
  version of its own, that one takes over and the inherited pair is never read
  again. It is not cleared — it costs no write to keep, and it records where the
  conversation came from.
- **The option takes no `HistoryRef`; the kernel resolves it.** The only version
  a caller could safely name is the one the issuing record currently names —
  every earlier one has already been released by the commit that superseded it —
  so accepting a ref would only add a value that can disagree with the record.
  Resolving at Spawn also pins it: a later turn of the issuer does not change
  what the new Process starts from. Spawn therefore reads that record, and
  reports `ErrProcessNotFound`, or `ErrInvalidRequest` when it has committed no
  conversation, instead of leaving a Process to fail on its first transition.
- **Not on `SpawnChild`.** `Syscalls` hands out no `HistoryRef`, so a strategy
  has no version to name; the one it would reach for — its own — is exactly what
  its current transition's commit releases. Rejected as `ErrInvalidRequest`
  before the middleware chain, like `WithIdempotencyKey`, and absent from
  `SpawnRequest`. Permitting it later is a compatible addition; withdrawing it
  would not be.

## Alternatives rejected

- **A single mutable key per process (the original decision).** Simple, and it
  used gollem's existing port unchanged, but the save is destructive: the window
  between a save and its commit cannot be closed, and closing it is the point of
  this ADR.
- **Two slots per process with a committed slot number.** Bounded storage and no
  new port method, but the stale worker and the new one derive the same "free"
  slot from the same committed pointer, so a lost-lease save still lands on the
  slot the record names. Adding slots does not help; only a name no other writer
  can pick does.
- **History on the Process record (a typed field like `Metrics`), folded into
  `Apply`.** Atomic and simplest, but a large History blob would be rewritten on
  every transition inside the transactional store, and item-size limits on some
  backends would push it back out to a blob store — reintroducing the split with
  no atomicity left.
- **An effect journal, so a replay reuses the recorded turn instead of
  re-generating.** This is how durable-execution runtimes avoid the problem, but
  it needs a per-effect identity and determinism from every strategy, and
  ADR-0003 declines both.
- **Version-tagging the save with `StateSeq` so a superseded attempt is ignored
  on load.** Considered in the original record. It stops the superseded version
  from being read, but with one mutable key there is nothing left to read
  instead; immutable versions are that idea carried through.
- **Reuse `gollem.HistoryRepository`.** Its `Load`/`Save` cannot name a version
  or release one, so keeping it would have kept us in the mutable-key design.
- **Inheriting by writing the inherited ref into `Process.HistoryRef`.** No new
  field, and `ensureLoaded` would need no change at all — but the post-commit
  `discardSuperseded` reads `HistoryRef` as "the version this commit replaced"
  and releases it under the committing Process's id. The first commit of the
  inheriting Process would therefore announce the issuer's live version as
  garbage, under the wrong id.
- **A `ProcessID` alone on the record, resolved to a version at first use.** One
  field instead of two, and no read at Spawn. Rejected because what the Process
  starts from would then depend on when it first runs: a turn the issuer commits
  in between silently changes the transcript. It would also put a `Repository`
  read inside the History load path.
- **Verifying at Spawn that the inherited version is still in the store.** It
  would couple Spawn to the blob store's availability and still not be a
  guarantee — the issuer's next commit can release the version immediately after
  the check passed. An unreachable version fails the first transition instead,
  which is loud enough.
- **Flat `SessionGenerate` / `SessionHistory` / `SessionCallTool` methods on
  `Syscalls`.** What the previous version of this ADR chose, when there were two
  of them. A third made the repeated prefix worse than a handle.
- **`Session() (Session, bool)`, or a nil handle when unconfigured.** The
  comma-ok form lets a caller ignore the bool and silently do nothing, which is
  exactly the "runs without persistence" this ADR refuses; the nil form turns the
  mistake into a panic, a worse diagnostic than a sentinel error.

## Consequences

- **A Step boundary may fall in the middle of a tool round.** Committing a
  History that ends on an unanswered `tool_use` is safe, because a re-run always
  starts from the version the record names. A strategy can generate a tool call,
  suspend for a human decision, and answer the call in a later Step —
  human-in-the-loop with the managed conversation, which the previous design
  could not express. Keeping History in strategy state via raw
  `Syscalls.Generate` + `WithHistory` is still fully supported, and is what
  `strategy/simple` does.
- **State, awaits and History move as one.** The duplication window the previous
  version accepted is closed: a crash between a save and its commit leaves an
  unreferenced version, not a conversation one turn ahead of its State.
- **Reclamation is not guaranteed.** In the normal path the store is told about
  each superseded version, so the steady state is one or two versions per
  process. Versions left behind by a crash, or by an `Apply` failure whose
  outcome is unknown, are never announced — a store needs its own policy (an
  object-store lifecycle rule, a sweep) for those.
- **The effect model is unchanged (ADR-0003).** A replayed transition re-calls
  the LLM and re-runs its tools. What rolls back is the conversation record, not
  the effects.
- Because save precedes commit and commit is the completion marker, a
  `HistoryStore` outage stops the process from committing — including a `Done` —
  and it eventually fails as `FailureRetryExhausted`. Liveness is coupled to the
  History store when the agent opts in.
- Terminal commits save History too, which is what makes `WithInheritedHistory`
  work on a finished Process: its final transcript is still named by its record.
  A Process cancelled mid-round leaves a committed History ending on an
  unanswered `tool_use`; an heir has to tolerate that, exactly as a re-run of the
  same Process would.
- **The kernel does not promise an inherited version survives.** It pins the pair
  at Spawn and never releases it, but the *issuing* Process, if it is still
  running, releases that version as superseded on its next commit — and whether a
  release is acted on is the store's call, since `Discard` is a notification.
  Inheriting from a finished Process is therefore the safe use; inheriting from a
  running one is allowed (branching off its current conversation is a legitimate
  thing to want) and the version may be gone by the time it is read, surfacing as
  a failed first transition. The kernel refuses to guess which one a caller meant,
  and it does no reference counting — that would need a mechanism this ADR
  deliberately does not have.
- Metrics, limits and cancellation do **not** cross an inheritance: only the
  conversation does. A Process that inherits starts with empty `Metrics`, its own
  `Limit` budget, and its own cancellation. That is what the capability is for —
  running one question of a longer conversation as a unit that can be stopped and
  bounded on its own.
- The kernel still marshals nothing: the `HistoryStore` implementation
  serializes `*gollem.History` (ADR-0007 unchanged). `gollem.History` carries a
  version gate, so a load of an incompatible stored version surfaces as an error
  rather than silent data loss.
- Tools are bound into `Session().Generate` from `sys.Tools()` (the `ToolFactory`
  output, keyed on `proc.Agent`/`proc.Metadata`); a stable per-process tool set
  is also what prompt caching needs.
- **No migration from the previous layout.** History stored under the old
  single-key scheme is not read: a Process carrying no `HistoryRef` starts its
  conversation empty. Operators discard the old store contents.

## History

| Date | Change |
|---|---|
| 2026-07-23 | Initial record: a decoupled, best-effort store under one mutable key per process, with a tolerated duplication window and an obligation to keep a tool round inside one Step. |
| 2026-08-01 | Rewritten. Versions are immutable and named by `Process.HistoryRef`, committed atomically, so History rolls back with State: the duplication window and the one-Step obligation are both gone, and human-in-the-loop works with the managed conversation. `gollem.HistoryRepository` is replaced by the agentkit `HistoryStore` port (`Save`/`Load`/`Discard`), the flat `Session*` methods by a `Session()` handle that also carries `CallTool`, and the pre-save `ownsLease` fence is removed as unnecessary. |
| 2026-08-03 | Added `WithInheritedHistory`: a new Process can start from a version another one committed, pinned at Spawn on `Process.InheritedHistory`. It is read-only and never `Discard`ed, which is why it is a field of its own rather than a value written into `HistoryRef` — the post-commit release reads that one. Rejected on `SpawnChild`. |
