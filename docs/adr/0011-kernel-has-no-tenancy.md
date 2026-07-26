# ADR-0011: The kernel has no tenancy concept

## Summary

agentkit has no tenant, scope, or environment abstraction. There is no
`EnvRef`, no `ScopeResolver`, no opaque scope key threaded through the runtime.

Applications that need a tenant context propagate it themselves, by any of:

- `WithMetadata(map[string]string)` at spawn, readable as `Process.Metadata` by
  a `ToolFactory` (this is the intended path for infrastructure-facing scope).
  `SpawnChild` copies the parent's map when the caller names none, so a child
  runs under its parent's scope by default; naming one replaces it outright;
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

**A child inherits the map unless the caller replaces it.** Copying is not
interpreting: the kernel moves the whole map without reading a key, which is the
same thing it does when it stores one. The reason to copy at all is that
`Metadata` is the documented channel for infrastructure scope, and a child
resolves its own `ToolFactory` — a child that started empty would hand out an
unscoped client while its parent, running the identical factory, got a scoped
one. `strategy/planexec` spawns its task children with no `SpawnOption` at all,
so before this every fan-out lost the scope silently.

`WithMetadata` on `SpawnChild` replaces rather than merges. Merging would leave
no way to drop a key the parent carries; replacing makes both reachable, with an
empty map meaning "this child gets none". Inheritance happens before the spawn
middleware chain, so a `SpawnMiddleware` sees the inherited map and is where an
allowlist belongs — the kernel does not grow one, because a per-key rule is a
rule about what keys mean.

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
- **Leaving children empty and documenting `SpawnMiddleware` as the fix.** It
  works — the middleware has the parent's map in `EffectContext.Metadata` — but
  it makes the correct behaviour opt-in, discovered only after a `ToolFactory`
  misbehaves in a child. The default was wrong, not the escape hatch.
- **Merging the parent's map into the child's instead of replacing.** Leaves no
  expression for "drop this key", and makes the result depend on a union the
  caller cannot see at the call site.
- **An inheritable-key allowlist in the kernel.** A rule about which keys may
  cross a boundary is a rule about what the keys mean, which is the tenancy
  vocabulary this record declines. It belongs in a `SpawnMiddleware`, where the
  application already knows.

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
- A child carries whatever the parent carried. Since `Metadata` is not a
  credential, that is data propagation, not privilege propagation — but a caller
  that puts something in the map it would not want a subagent's `ToolFactory` to
  see has to strip it in a `SpawnMiddleware`, or not put it there.

## History

| Date | Change |
|---|---|
| 2026-07-20 | Initial record, extracted from the initial implementation spec (D4, D31, D42). |
| 2026-07-26 | `SpawnChild` now copies the parent's `Metadata` when the caller names none (replacing, not merging, when one is named). Children previously started empty, so `planexec` — which spawns with no options — silently dropped the scope its own `ToolFactory` needed. |
