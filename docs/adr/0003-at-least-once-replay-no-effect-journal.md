# ADR-0003: At-least-once replay, no effect journal

## Summary

agentkit does not journal effects and does not replay deterministically. A
`Process` is checkpointed after every committed transition; if a transition
crashes before it commits, another worker re-runs `Step` from the last committed
state. LLM calls and tool calls therefore execute **at least once** and a re-run
may take a different path, because the LLM is non-deterministic.

The framework guarantees exactly two things:

1. A committed transition is never lost.
2. One transition commits atomically — state, awaits, events, spawned children
   and metrics land in a single `Repository.Apply`.

Everything else is the caller's responsibility. A side-effecting tool must be
idempotent, deriving its idempotency key from the **meaning of its own
arguments**. For exactly-once external effects, commit the intent first and
execute it in the next transition.

## Context

The first design of agentkit had an effect journal: every LLM call, tool call
and spawn was written to a journal row keyed by an operation id, and a replay
would hit the journal instead of re-executing. That promised at-most-once
external effects from the framework itself.

A design review took it apart. The journal's correctness rested on an unstated
premise: **that a replay follows the same path as the original run**. It does
not. Re-running `Step` re-prompts the LLM, which may return different text, call
a different tool, or call the same tool with different arguments. A journal
keyed by position (`{process}/{seq}/{label}`) then serves a cached result for a
call that is not the same logical call. Around that core problem sat a cluster
of distributed-safety holes: fencing stale journal writes, indeterminate results
after a crash mid-effect, and metrics that a journal hit could bypass so a
`Limiter` never saw them.

## Decision

Delete the mechanism rather than patch it. There is no `Effect` row, no
`EffectID`, no operation label, no deterministic clock and no seeded RNG.
`Syscalls.Now()` reads the kernel clock (injectable via `WithClock` for tests,
but not reproducible across a replay) and there is no `Rand()`.

`Syscalls.Generate` and `Syscalls.CallTool` call gollem, accumulate `Metrics`,
and return. A replay calls them again. An LLM re-charge on replay is accepted.

Idempotency moves to the tool author, who is the only party that knows what
"the same operation" means for their effect. Idempotency stops *duplicate*
effects; it does not stop *divergent* ones (the model deciding differently on
re-run), so a strategy needing exactly-once behaviour must checkpoint the
decision before performing it.

## Alternatives rejected

- **Effect journal with deterministic replay.** Rejected above: the determinism
  premise is false for an LLM, and the surrounding fencing/indeterminacy problems
  were not closable at acceptable complexity.
- **The framework distributes an idempotency key** (`{pid}/{seq}/{label}`) to
  tools. A positional key does not identify "the same logical call" under
  non-deterministic replay, so it would be an idempotency key that is wrong
  exactly when it matters.
- **Suppress replay entirely by never re-running a crashed transition.** Turns
  every crash into a stuck `Process` and gives up crash resilience, which is the
  point of the runtime.

## Consequences

- Repository implementations get dramatically simpler: no effect table, no
  effect fencing, no indeterminate-effect state machine. The contract is
  `Apply` + Rev CAS + claim + uniqueness (see ADR-0004).
- **Tool authors carry a real obligation.** A side-effecting tool that is not
  idempotent will double-apply on a crash. This must be documented wherever
  tools are written, not buried here.
- Strategies must treat "have I already done this?" as *state*. Across a
  `Suspend`, a strategy records that a step happened in its own checkpointed
  state; the framework will not remember for it.
- Middleware fires on every execution including re-runs, which is what an audit
  trail wants — it records what actually happened (see ADR-0012). It also means
  a middleware must tolerate duplicates: one call is not one logical operation.
- The bundled strategies keep at most one `Generate` per transition, so a crash
  costs at most one LLM round. `planexec`'s plan phase is the sole exception: an
  in-transition correction retry on malformed plan JSON.

## History

| Date | Change |
|---|---|
| 2026-07-20 | Initial record. Supersedes the original effect-journal design (spec D11, D13, D14) with D44/D45; folds in the removal of `Now`/`Rand` determinism. |
| 2026-07-21 | Unchanged, but no longer the whole story: [ADR-0015](0015-unclean-reclaims-are-counted-and-bounded.md) bounds how many times replay may happen after a crash, and makes "this is a replay, and here is why" readable from a strategy. Replay is still at-least-once and there is still no journal. |
| 2026-07-23 | Conversation History persistence ([ADR-0017](0017-history-is-a-decoupled-best-effort-store.md)) is an at-least-once effect under this ADR: History is saved before each commit, outside the atomic `Apply`, and a replayed or saved-but-not-committed attempt may re-save, so History can carry a duplicated turn. No journal, no exactly-once — the same posture as tool calls. |
