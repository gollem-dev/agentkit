# ADR-0011: The kernel has no tenancy concept

## Summary

agentkit has no tenant, scope, or environment abstraction. There is no
`EnvRef`, no `ScopeResolver`, no opaque scope key threaded through the runtime.

Applications that need a tenant context propagate it themselves, by any of:

- `WithMetadata(map[string]string)` at spawn, readable as `Process.Metadata` by
  a `ToolFactory` (this is the intended path for infrastructure-facing scope);
- the `context.Context` passed to `Serve`, for process-independent dependencies;
- the strategy's own typed `Input`, folded into state by `Init`;
- the application's own storage, keyed by `ProcessID`.

## Context

The design proposal carried a scope-resolution layer: an opaque `EnvRef` on
every process, an `EnvProvider` port resolving it to a `ScopedEnv` of clients and
stores. It was general enough to hold anything and specific about nothing — the
kernel would carry a key it never interprets, and every implementer would have to
decide what it means.

## Decision

Drop the layer. Dependencies are injected at `New` (positional and optional, per
ADR-0005), and anything that must vary per process is derived inside the
caller's own `ToolFactory` or `gollem.LLMClient`, both of which can dispatch
dynamically.

`Process.Metadata` exists as the one process-scoped, kernel-opaque
`map[string]string`. It is deliberately separate from strategy input:

| | `Input` (via `Init`) | `Metadata` (via `WithMetadata`) |
|---|---|---|
| Lifetime | folded into `State`, transient | persisted on the process row |
| Audience | the strategy | infrastructure (`ToolFactory`, a `Limiter`) |
| Typing | typed `I` | `map[string]string` |

This generalizes to a standing rule: **agentkit does not ship vocabulary it does
not interpret.** A type with no specification behind it is a footgun, because
every implementer invents a different meaning. The same reasoning removed
`ToolSelector` and a declared capability list on agent definitions — the agent
kind *is* the selector, and `ToolFactory(ctx, proc)` decides from `proc.Agent`.

## Alternatives rejected

- **`EnvRef` / `EnvProvider` / `ScopedEnv`.** An uninterpreted key in the core
  plus a port whose contract is "whatever you want".
- **A narrower `ScopeID` / `ScopeResolver`.** Same problem with a shorter name.
- **A first-class `TenantID` on `Process`.** Would commit agentkit to one
  multi-tenancy model, which callers with a different one then have to work
  around.

## Consequences

- **`Process.Metadata` is not a credential and must never be treated as one.**
  It is caller-supplied data stored verbatim. A `ToolFactory` reading
  `metadata["tenant"]` is trusting whoever called `Spawn`. The caller must derive
  that value server-side from an already-validated principal *before* spawning —
  validate first, then establish scope, never the reverse. This warning belongs
  in every document that mentions `Metadata`.
- Multi-tenant deployments do slightly more wiring, in exchange for agentkit not
  guessing their model.
- `RootID` is available for tree-wide correlation without any tenancy concept
  existing.

## History

| Date | Change |
|---|---|
| 2026-07-20 | Initial record, extracted from the initial implementation spec (D4, D31, D42). |
