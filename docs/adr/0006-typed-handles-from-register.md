# ADR-0006: Define/Register returns a typed handle

## Summary

`any` does not appear in agentkit's public API. Registration returns a typed
handle that carries the type forward to every use site:

- `Register[S, I](registry, name, version, strategy)` returns `Agent[I]`, and
  `Agent[I]` is the **only** way to spawn. `Kernel.Spawn` does not exist.
- `DefineModelRole(name)` returns a `ModelRole`, a sealed interface (unexported
  marker method) that cannot be implemented or constructed outside the package.

Type erasure is confined to unexported code: `BindStrategy` folds a
`Strategy[S, I]` into closures, and the input closure re-asserts `input.(I)`.

## Context

The kernel stores strategy state as opaque bytes and dispatches by agent name,
so internally it must be type-erased. Exposing that erasure — `Spawn(name, input
any)` — pushes an unchecked cast to the caller, where a mismatch surfaces as a
runtime error at spawn time.

The same problem appeared with model roles. A `type ModelRole string` compared
by value means a typo silently falls back to the default model: the wrong model
runs, nothing errors, and the bug is invisible.

## Decision

Every place where a name-keyed lookup would otherwise return `any`, the
registration function returns a typed value instead:

| Registration | Handle | What the type carries |
|---|---|---|
| `Register[S, I]` | `Agent[I]` | the launch input type |
| `DefineModelRole` | `ModelRole` | a unique identity |
| `planexec.Register[T]` | `Agent[planexec.Input]` | plus `makeInput func(TaskSpec) (T, error)` binding the child agent's input type |

`ModelRole` is sealed by an unexported `modelRole()` method and resolved by
pointer identity, so two `DefineModelRole("planner")` calls are distinct roles.
Only a value returned by `DefineModelRole` can exist; strategies export theirs
as package variables (`planexec.RolePlanner`, `planexec.RoleSummarizer`).

`AgentName` is deliberately **not** sealed. It is a wire value persisted in
`Process.Agent`, and a typo is caught at spawn time as `ErrUnknownAgent`.

## Alternatives rejected

- **`Kernel.Spawn(ctx, name, input any)`.** Leaves an unchecked hole at the one
  place the caller most wants checking. Making only `Strategy.Init` typed while
  the spawn entry point stays `any` moves the hole rather than closing it.
- **`type ModelRole string` compared by value.** A typo degrades silently to the
  default model — the worst kind of failure.
- **An opaque struct with unexported fields for `ModelRole`.** Blocks
  construction but not external implementation; a sealed interface blocks both.

## Consequences

- `Register` must be called before any `Spawn` or `Serve`; the `Registry` is
  read-only afterwards. Registration returns the handle the rest of the program
  needs, which makes wiring order explicit at the call site.
- Generic subpackages become generic themselves: `planexec.Register[T]` takes
  `taskAgent Agent[T]` and `makeInput func(TaskSpec) (T, error)`, so any agent
  can be a task agent without either side knowing the other's type.
- `BindStrategy` is exported so tests can build fake strategies. It is the one
  sanctioned entry into the erased form.
- `Strategy.Init(input I)` receives its input typed and unserialized, because it
  runs synchronously inside `Spawn` (see ADR-0007) — no encode/decode round trip
  inside a single process.

## History

| Date | Change |
|---|---|
| 2026-07-20 | Initial record, extracted from the initial implementation spec (D32, D41, D42, D43). |
