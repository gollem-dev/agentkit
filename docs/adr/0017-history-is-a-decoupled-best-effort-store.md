# ADR-0017: History is a decoupled, best-effort, per-agent store

## Summary

Conversation History (`*gollem.History`) is persisted **outside the Process
row**, in a separate blob store, opted into **per agent**. `Register` accepts
`WithHistoryRepository(gollem.HistoryRepository)`; when set, the worker lazily
loads History (keyed by `ProcessID.String()`) the first time a strategy uses
`sys.Session()`, and saves it **before every transition commit, including
terminal ones**. History is *not* part of the atomic `Repository.Apply`: its save
is an at-least-once effect ordered before the commit, so a crash between save and
commit can leave a superseded (duplicate) History — this is tolerated. The
kernel's `Repository` never carries History. Without the option, `sys.Session()`
still works but its History lives only for the duration of one claim.

## Context

Every strategy that talks to an LLM must thread `*gollem.History`: read it, pass
it to `Generate`, fold the result back, checkpoint it. `strategy/simple` and
`strategy/planexec` each reimplement this in their own state. History is also
unlike State: State is a snapshot replaced wholesale each transition, whereas
History is append-only and grows unbounded — potentially too large to sit in the
same transactional row (an RDB) as State. We wanted the runtime to absorb the
threading, without putting a large blob on the Process row and without the kernel
learning to (un)marshal caller data (ADR-0007).

## Decision

- **Reuse gollem's existing `HistoryRepository`** (`Load`/`Save` keyed by a
  session-id string) rather than defining our own, consistent with depending on
  gollem directly ([ADR-0001](0001-depend-on-gollem-directly.md)). agentkit keys
  it by `ProcessID.String()`. Reference implementations live in a separate tree,
  `historystore/{memory,filesystem}`; the filesystem one stores **one blob file
  per process**, not a single snapshot, reflecting the blob nature.
- **Opt-in is per agent.** `WithHistoryRepository` is a `RegisterOption`, carried
  on the `StrategyBinding`. `Kernel.New(repo, ...)` is unchanged; the
  transactional `Repository` and the blob `HistoryRepository` are injected
  through different channels.
- **`Syscalls.Session()`** returns a `Session` that carries History across
  `Generate` calls and injects the claim's tools (`sys.Tools()`). The
  claim-scoped committed baseline (`historyState`) is loaded once, lazily, and
  advanced only when a transition commits; a per-transition working copy is
  discarded if the transition does not commit, so a same-lease conflict retry
  re-seeds from committed state rather than from the abandoned attempt.
- **Save precedes commit.** In `worker.go` the History save runs ahead of both
  the `Apply` on the Continue/Suspend path and the `commitTerminal` on the
  Done/Fail path, because the commit is the completion marker: durable work comes
  first. A save failure is a transition error (requeue), never a silent skip.
- **A lost-lease write is fenced.** The blob store sits outside the
  Rev/LeaseToken fence, so right before the save the worker re-reads the Process
  row and skips the write when the lease token no longer matches its claim
  (`ownsLease` in `worker.go`). Without this, a worker whose lease expired mid-
  transition would run its save after a newer worker had already committed a
  fresher History, overwriting it — a regression, not mere duplication. The
  re-check narrows that window to the gap between the read and the write; a
  single-key store cannot close it completely, but the remaining window is
  negligible next to the lease duration.
- **History is deliberately outside the atomic commit set** of
  [ADR-0009](0009-child-processes-committed-atomically.md) / invariant 5. Its
  save is an at-least-once effect in the sense of
  [ADR-0003](0003-at-least-once-replay-no-effect-journal.md): a crashed-then-
  replayed transition may re-run `Generate` and re-save, and an attempt that
  saved but did not commit leaves a superseded History. This duplication is
  accepted; it is not exactly-once and there is no journal.

## Alternatives rejected

- **History on the Process row (a typed field like `Metrics`), folded into
  `Apply`.** Atomic and simplest, but a large History blob would be rewritten on
  every transition inside the transactional store — the size problem that
  motivated a separate store.
- **Zero coupling with save-*after*-commit.** Loses the last turn on a crash
  between commit and save ("amnesia"), which corrupts a `tool_use`/`tool_result`
  pairing on the next `Generate`. save-before-commit trades amnesia for at-worst
  duplication, and duplication of a *clean* turn stays well-formed.
- **Version-tag the save with `StateSeq` so a superseded attempt is ignored on
  load.** Eliminates duplication too, but reintroduces a coupling from History
  back to the Process's committed version. We chose to tolerate duplication and
  keep the stores fully decoupled.
- **Define an agentkit-specific `HistoryRepository`.** Redundant with gollem's
  existing port and would refuse a gollem-compatible store.

## Consequences

- History and a strategy's own tool bookkeeping (pending calls / results, held in
  State) can diverge across a crash. To keep the next request well-formed, a
  strategy that uses `sys.Session()` must keep a tool round **within one Step**
  (never persist a dangling `tool_use`). A strategy that splits a tool round
  across steps should instead keep History in its own State via raw
  `Syscalls.Generate` + `WithHistory` — still fully supported, and what
  `strategy/simple` does.
- Because save precedes commit and commit is the completion marker, a
  `HistoryRepository` outage stops the process from committing — including a
  `Done` — and it eventually fails as `FailureRetryExhausted`. Liveness is
  coupled to the History store when the agent opts in.
- Terminal commits save History too, so a future "restart / hand off a finished
  Process" capability can read the final transcript.
- The kernel still marshals nothing: the `HistoryRepository` implementation
  serializes `*gollem.History` (ADR-0007 unchanged). `gollem.History` carries a
  version gate, so a load of an incompatible stored version surfaces as an error
  rather than silent data loss.
- Tools are bound into the `Session` from `sys.Tools()` (the `ToolFactory`
  output, keyed on `proc.Agent`/`proc.Metadata`); a stable per-process tool set
  is also what prompt caching needs.

## History

| Date | Change |
|---|---|
| 2026-07-23 | Initial record. |
