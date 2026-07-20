# ADR-0005: Required arguments positional, optional as functional options

## Summary

Every constructor and entry point in agentkit puts **required** values in the
signature and **optional** values behind functional options. There is no
config struct mixing the two.

```go
New(repo, model, agents, ...KernelOption)
Register[S, I, O](registry, name, version, strategy, ...RegisterOption[O]) // -> Agent[I]
Agent[I].Spawn(ctx, kernel, input, ...SpawnOption)
Agent[I].SpawnChild(ctx, sys, input, ...SpawnOption)
Syscalls.Generate(ctx, input, ...GenerateOption)
Kernel.Respond(ctx, pid, key, response, ...RespondOption)
Kernel.Serve(ctx, ...ServeOption)
planexec.Register[T](registry, name, version, taskAgent, makeInput, ...Option)
```

## Context

A `Config` struct carrying both required and optional fields lets a caller omit
something mandatory and find out at runtime. Adding a field to such a struct is
also a silent behaviour change for existing callers.

agentkit has genuinely mandatory dependencies (a `Repository`, a default LLM
client, a `Registry`) and a long tail of optional ones (extra model roles, a
tool factory, a limiter, middleware, a logger, a clock).

## Decision

Required in the signature; optional as `WithX(...)` options. A missing required
value is a compile error. Adding a new option is backward compatible.

Two consequences of applying the rule strictly:

- **The default model is a positional argument of `New`, not an option.** At
  least one model is mandatory, so it cannot be optional. Callers using several
  models layer `WithModelRole(role, client)` on top; the default model backs
  every role that has no explicit binding (a nil `ModelRole` *is* the default).
  A `map[ModelRole]gollem.LLMClient` argument was rejected for putting map-shaped
  cognitive load on the single-model case, which is the common one.
- **`AgentDef` does not exist.** Its three required fields — name, version,
  strategy — are positional arguments of `Register`.

## Alternatives rejected

- **`Config`/`Request` structs with mixed requiredness.** The failure mode is a
  runtime "field X is required", which is precisely what a type system should
  catch.
- **Everything as options, validated in the constructor.** Same runtime failure,
  plus the signature stops documenting what the type actually needs.

## Consequences

- Each optional knob costs one exported `WithX` function and one unexported
  config field. That verbosity is accepted.
- A new required dependency is a breaking change, by construction. That is
  correct — it *is* one — and it forces the question of whether it is really
  required.
- Option types are per-call-site (`KernelOption`, `RegisterOption`,
  `SpawnOption`, `ServeOption`, `GenerateOption`, `RespondOption`,
  `AwaitOption`), so an option cannot be
  passed where it has no meaning.

## History

| Date | Change |
|---|---|
| 2026-07-20 | Initial record, extracted from the initial implementation spec (D26, D27). |
| 2026-07-20 | `Register` gained `RegisterOption[O]`, carrying the optional completion handler (ADR-0014). |
