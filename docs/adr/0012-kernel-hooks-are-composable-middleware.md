# ADR-0012: Kernel hooks are composable middleware

## Summary

The five points where the kernel calls out — `Init`, `Step`, `Generate`,
`CallTool`, `SpawnChild` — are each wrapped by a `next`-chain middleware,
registered on the `Kernel` with `WithInitMiddleware` and its four siblings.
A middleware can observe, rewrite the request, or refuse by not calling `next`.
An effect middleware may also call `next` more than once (a retry charges
twice, which is the truth); a `StepMiddleware` may not — see Consequences.

Middleware is registered on the Kernel, so one registration covers every agent.
That is what makes it right for cross-cutting concerns, and it has a
consequence: a kernel-level middleware does not know any particular strategy's
input type `I` or state type `S`, so **there is no static type safety over the
type-erased payloads at this layer**. Touching one is an opaque operation and
the responsibility is the middleware's.

Nothing here is persisted, and effects still run at least once (ADR-0003), so a
middleware fires on **every** execution including re-runs.

## Context

Removing the effect journal (ADR-0003) also removed the only durable record of
what a process did. Audit and tracing are real needs, but they are different
needs: an audit trail belongs in the caller's own store with the caller's own
schema and retention, and a trace belongs in the caller's tracing backend.

The first answer to that was an `Observer` struct: three span-shaped hooks
(`Generate`, `ToolCall`, `Spawn`), each called at the start of an effect and
returning a callback invoked at its completion, receiving deep copies so it
could not mutate what ran, with panics recovered and swallowed. It was
observation only, by construction.

Two things were wrong with it. It could not express control — rate limiting,
argument normalization, caching, refusing a call — because a hook that cannot
stop execution and whose panics are discarded has no way to. And it did not
reach the state machine at all: the `Init` and `Step` boundaries, which are
where "what did this strategy decide, and from what" lives, had no hook of any
kind.

A `next`-chain covers everything the `Observer` did — record intent before
calling `next`, record the outcome from its return — plus the control the
`Observer` structurally could not. Keeping both would put two hook systems on
the same points with no answer to "which runs first" or "does the observer fire
when a middleware short-circuits", so `Observer` was removed rather than kept
alongside.

## Decision

Five hook points, all registered on the `Kernel`. See `middleware.go` for the
handler and middleware types; the shape is uniform
(`func(next XHandler) XHandler`).

Effect middleware (`Generate`, `CallTool`, `Spawn`) is the **outermost** layer of
its syscall: it wraps the `Limiter` check, tool resolution and argument
validation. A refused call is therefore visible to it — which is the point,
since a rejected attempt is the interesting row in an audit log — and a
middleware that returns without calling `next` consumes neither quota nor
metrics. `Observer` fired inside the `Limiter` check and could not see any of
this.

`StepMiddleware` wraps the `Step` **call**, between `DecodeState` and
`EncodeState`. It does not observe the transition's commit, which happens after
the handler returns. A transition that fails to commit is re-run and the
middleware is called again; the at-least-once model is visible here exactly as
it is, and no `StepMiddleware` may be documented as if it reported commits.

`InitMiddleware` covers both entry points, since `Init` is called from
`Agent[I].Spawn` and from `SpawnChild` alike. This changes what `Strategy.Init`
guarantees: the signature still gives a *strategy author* no path to an effect,
but whoever configures the Kernel can now wrap `Init` with a `ctx`. `Init` is
not on the transition machine, so it is the safer of the two boundaries for
such an effect.

`SpawnRequest.OnCommit` preserves the one thing a plain `next`-chain cannot
express. Child creation is buffered into the transition commit (ADR-0009), so
whether the child exists is only known later. `OnCommit` registers a callback
fired with that outcome. Its scope is the **transition**, not the child:
registered before `next` and `next` then fails, it still fires. It runs after
the commit, outside the transition, so a panic in it is recovered and logged —
it would otherwise kill the worker.

Events remain separate and complementary. `Syscalls.Emit` writes an `Event` row
inside the transition commit, so events *are* durable — but delivery is not
agentkit's job. `ListEvents` is per-process; there is no global feed and no
cursor API, because an outbox relay is tightly coupled to the store and a caller
tailing their own database is simpler than a port that every implementation must
support.

## Alternatives rejected

- **Keep `Observer` alongside middleware.** Two hook systems on the same points,
  with no defensible answer to which runs first or whether the observer fires
  when a middleware short-circuits. Middleware is a strict superset.
- **Register middleware per agent, on `Register[S, I]`.** `S` and `I` are in
  scope there, so `Init` and `Step` middleware could be fully typed and the
  payload problem would disappear. Rejected because the use cases do not sit
  there: tracing, audit, tenant cost accounting, redaction, LLM retry, tool
  policy and kill switches all need only concrete fields, and all of them break
  badly if a newly added agent silently misses them. The one case that genuinely
  wants a type — normalizing a specific agent's input — is also the job of that
  strategy's own `Init`. A per-agent layer may be added later *in addition to*
  this one; it is not a replacement for it.
- **Reach the payloads through a token that proves their type** (a
  `StepPayload[S]` returned by the reader, with the writers as its methods, so a
  mismatch is a compile error). Tried and withdrawn: it could not close the
  short-circuit path for `Init` (no prior `S` exists to prove) nor the `[any]`
  escape hatch, and holes that cannot be closed are the sign of manufacturing a
  guarantee the layer does not have. It also cost three exported types and seven
  methods for something that only 10 plain functions otherwise need.
- **Expose the payloads as `any` fields and amend ADR-0006.** Plain, but `any`
  would appear at five points in the public API — the spine of the design, not a
  corner of it — which is too much to carve out of an invariant.
- **Middleware around `Kernel.Respond` / `Kernel.Cancel` / `ToolFactory` /
  `Limiter`.** The first two are called by the application itself, which already
  has a service layer to put authorization and audit in. The last two are
  already injected as function values and compose without kernel support.
  Middleware belongs only where the kernel owns the call site.
- **Wrap a whole `runClaim` iteration (Step *plus* commit).** That would be a
  true transition span, but the loop carries retry, lease loss and conflict
  rewind, so "one transition = one handler call" would have to be redefined
  first. Deferred, not dismissed.
- **Reinstate a journal for audit purposes.** Brings back the design ADR-0003
  removed, and an audit trail does not need replay semantics.
- **A global event feed / cursor subscription API.** Couples the port to a
  delivery mechanism. The core guarantees events are *written*, in order,
  atomically with their transition.

## Consequences

- **A `ToolCallMiddleware` can refuse fail-closed, and still is not an
  authorization gate.** It runs before the tool and its panics are not
  swallowed, so it is a real chokepoint for calls made through
  `Syscalls.CallTool` — but not the only path to a tool, since a strategy
  holding a `gollem.Tool` value can call `Run` on it directly. Enforcement
  belongs inside `Run` (ADR-0008); do not describe middleware as a security
  boundary.
- **A `StepMiddleware` may call `next` at most once**, and the second call
  returns `ErrInvalidRequest`. A Step's side effects — spawned children, emitted
  events, metrics — accumulate in the per-transition `Syscalls`, not per call, so
  a retried Step would commit the discarded attempt's effects together with the
  accepted attempt's state and Decision, breaking "one transition's work commits
  atomically" (ADR-0009). Supporting a real retry would mean per-call effect
  buffers merged only from the accepted attempt; that is not built. Effect
  middleware has no such constraint because each call *is* the whole effect.
- **`StepRequest.Process` is a copy, not the row.** Writing to it changes nothing
  that is committed. It has to be a copy: the worker builds the commit from the
  original, so a middleware setting `Rev` would make every `Apply` conflict and
  spin the claim loop. The clone is only taken when a Step middleware is
  registered.
- **`OnCommit` reports the transition, not the child.** It is called with `nil`
  when the transition committed. Registering it before `next` and having `next`
  fail still yields `nil` if the transition goes on to commit — with no child in
  existence. Register after a successful `next` to bind it to a real child.
- **Type mismatches on a payload are run-time errors.** Replacing a payload with
  the wrong type compiles and surfaces as `ErrInvalidRequest` from
  `BindStrategy`'s closures. Those assertions became comma-ok in the same change
  for exactly this reason — before middleware, the path was unreachable.
- Middleware must tolerate duplicates: a re-run transition calls it again, and
  it must not assume one call means one logical operation.
- Middleware runs inline on the transition path and must not block.
- A panic in middleware is not recovered by the chain. Inside a transition the
  worker converts it into a transition error and the Process retries; in `Init`
  it propagates to the `Spawn` caller. A control layer that fails should fail
  the work, not be silently ignored.
- An `InitMiddleware` fires on an idempotent `Spawn` that returns an existing
  Process, because `Init` runs before the idempotency lookup; the `ProcessID`
  minted for that call is discarded.
- If durable recording must happen *before* an action, middleware is still the
  wrong tool — record inside the tool's `Run`.

## History

| Date | Change |
|---|---|
| 2026-07-20 | Initial record, extracted from the initial implementation spec (D21) and the post-journal observability design. |
| 2026-07-20 | Replaced the `Observer` struct with `next`-chain middleware and extended the hooks from three to five (adding `Init` and `Step`). `Observer` could not express control and did not reach the state machine. Records the layer's lack of static type safety over type-erased payloads, and the per-agent registration alternative that was rejected with it. |
| 2026-07-20 | Narrowed "may call `next` more than once" to effect middleware only: a Step's effects buffer per transition, so a retried Step would have committed a discarded attempt's children and events. Made `StepRequest.Process` a copy after finding that writing `Rev` through it would break CAS fencing. Corrected `OnCommit` to be described consistently as reporting the transition, not the child. |
