# ADR-0012: Observer is best-effort observation, not control

## Summary

`Observer` provides three span-shaped hooks — `Generate`, `ToolCall`, `Spawn` —
each called at the start of an effect and returning a callback invoked at its
completion. They exist for audit trails, tracing and metrics export.

They are strictly observation. Nothing recorded there is persisted by the
framework, a hook cannot stop execution, hooks receive deep copies so they cannot
mutate what runs, and a panic inside one is recovered and logged rather than
failing the transition.

Because effects run at least once (ADR-0003), hooks fire on **every** execution
including replays. That is intentional: an audit trail should record what
actually happened, not an idealized version of it.

## Context

Removing the effect journal (ADR-0003) also removed the only durable record of
what a process did. Audit and tracing are real needs, but they are different
needs: an audit trail belongs in the caller's own store with the caller's own
schema and retention, and a trace belongs in the caller's tracing backend.

## Decision

Expose observation points; persist nothing.

```go
type Observer struct {
    Generate func(ctx, EffectContext, []gollem.Input, ModelRole) func(*GenerateResult, error)
    ToolCall func(ctx, EffectContext, gollem.FunctionCall) func(map[string]any, error)
    Spawn    func(ctx, EffectContext, ProcessID, AgentName) func(error)
}
```

`EffectContext` carries `ProcessID`, `RootID`, `Agent` and `StateSeq` — enough
to key an audit row and to correlate a whole process tree. A nil field is not
called.

`Spawn` has commit-aware timing: the start callback fires when the strategy
requests the child, and the completion callback fires **after** the transition
commit persists it (with `nil`), or with the error if the transition did not
commit. So the audit matches whether a child actually came into existence.

Events are separate and complementary. `Syscalls.Emit` writes an `Event` row
inside the transition commit, so events *are* durable — but delivery is not
agentkit's job. `ListEvents` is per-process; there is no global feed and no
cursor API, because an outbox relay is tightly coupled to the store and a caller
tailing their own database is simpler than a port that every implementation must
support.

## Alternatives rejected

- **Reinstate a journal for audit purposes.** Brings back the design ADR-0003
  removed, and an audit trail does not need replay semantics.
- **Let observers veto an effect.** Enforcement must be fail-closed, which a
  best-effort hook with recovered panics cannot be. Denial belongs inside the
  tool, in `Run`.
- **A global event feed / cursor subscription API.** Couples the port to a
  delivery mechanism. The core guarantees events are *written*, in order,
  atomically with their transition.

## Consequences

- **An observer is not an audit gate.** If an action must be recorded durably
  *before* it happens, wrap the tool: record inside `Run`, before the effect.
  Observer hooks are the wrong tool for that and should never be presented
  otherwise.
- Observers must tolerate duplicates and must not assume a hook corresponds to a
  distinct logical operation. De-duplication, if wanted, is the observer's job.
- Observer code must not block: it runs inline on the transition path.
- Mutating a hook argument has no effect on execution — arguments are deep
  copies (`FunctionCall.Arguments`, `FunctionResponse.Data`, `GenerateResult`
  including a cloned `History`).

## History

| Date | Change |
|---|---|
| 2026-07-20 | Initial record, extracted from the initial implementation spec (D21) and the post-journal observability design. |
