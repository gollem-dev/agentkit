# ADR-0014: Completion handlers are best-effort and bound at registration

## Summary

`Register` accepts `WithOnFinish`, a handler that runs once a Process reaches a
terminal state and that state has been committed. It fires for `succeeded`,
`failed` and `cancelled` alike, receiving `FinishResult[O]`.

Delivery is **best-effort**: the handler never fires twice, but a crash between
the commit and the call loses it permanently, and nothing retries. Anything that
must not be lost belongs in a parent Process waiting on `WaitChildren`, where
every step is part of a committed transition.

The handler is bound at registration, not at spawn. It is code replicated across
every worker, so whichever instance commits the terminal transition can run it.
It runs synchronously on that instance; its error and its panic are logged and
change nothing about the committed Process.

## Context

Before this, the only way to observe a finished Process from outside the kernel
was to poll `Kernel.GetProcess`. The kernel-emitted `process.finished` event
carries a nil payload, so it says a Process ended without saying what it
produced.

Kernel middleware (ADR-0012) does not cover this. Its five hooks wrap *calls*,
and the one nearest the state machine is explicit that it stops short:
`StepMiddleware` wraps the `Step` call between `DecodeState` and `EncodeState`,
and "does not observe the transition's commit, which happens after the handler
returns". A terminal state is exactly the thing that only exists after a commit.

Two further constraints shaped the answer. `Spawn` is asynchronous and the
instance that spawns need not be the instance that runs the agent, so a closure
captured at spawn time cannot be relied on to exist where the Process finishes.
And ADR-0003 declines an effect journal, so there is no durable record to drive
a retry from.

## Decision

`WithOnFinish` is a `RegisterOption[O]`. The handler's signature is
`func(ctx, ProcessID, FinishResult[O]) error`; `FinishResult.Output` is non-nil
exactly when the status is `succeeded`, `Failure` exactly when `failed`.

The seven paths that terminate a Process all funnel through `commitFinal`
(`worker.go`), which fires the handler immediately after `Repository.Apply`
succeeds. This placement is the whole guarantee:

- Before the Apply, a handler could announce a transition that then fails to
  commit, or announce it twice across retries.
- After the Apply, the fact is settled. A worker that loses the CAS race
  abandons before reaching this point, so exactly one path fires.
- A crash in the window between the Apply and the call is unrecoverable, and is
  accepted as such.

`succeeded` is produced only by `commitTerminal` from a `Done`, so the value the
strategy passed to `Done` is still in memory when the handler runs. It is
forwarded directly rather than decoded back from `Process.Output`; this is why
`Strategy` has `EncodeOutput` but no `DecodeOutput` (ADR-0007).

The handler receives `context.WithoutCancel` of the worker's context. A worker
draining on shutdown has a cancelled context, and passing it through would drop
the notification for every Process finishing during the drain — which is when
notifications matter most. Values survive; the deadline is the handler's own
responsibility.

`Cancel` is the one path that runs outside a worker: `Kernel.Cancel` commits from
the calling application's own process, so a `cancelled` handler runs there.

The shape is not new: `SpawnRequest.OnCommit` (ADR-0012) already registers a
callback fired after the transition commits, outside the transition, with its
panic recovered and logged because it would otherwise kill the worker. This
handler follows that precedent rather than inventing a second convention for
post-commit notification.

Output reaching a parent is unaffected. `ChildResult.Output` stays `[]byte`: a
parent may wait on children of different agents, so no single output type
exists, and `Syscalls.Await` is an interface method, which Go does not allow to
be generic.

## Alternatives rejected

- **A sixth kernel middleware hook.** Middleware is a `next`-chain around a
  call, and there is no call here to wrap: the handler fires *after* an `Apply`,
  from inside `commitFinal`. ADR-0012 already declines the nearest thing —
  wrapping a whole `runClaim` iteration to get a true transition span — because
  the loop carries retry, lease loss and conflict rewind. Nothing about a
  post-commit notification needs that machinery.
- **Registering it on the `Kernel` rather than on the agent.** Kernel middleware
  is right for cross-cutting concerns precisely because it knows no agent's
  types, which is the opposite of what a handler receiving `O` needs. ADR-0012
  rejected per-agent *middleware* while leaving room for a per-agent layer "in
  addition to" it; this is that layer, scoped to one thing rather than to the
  five hook points.
- **A callback passed to `Spawn`.** A closure lives in one instance's memory.
  The worker that finishes the Process is frequently a different instance, so
  the callback would silently never run.
- **An outbox with retries, for real at-least-once delivery.** This is the
  effect journal ADR-0003 removed, reintroduced under another name. The cost is
  not the table; it is that every guarantee agentkit offers would then need to
  say which of two delivery models it means.
- **Failing the Process when the handler errors.** Rewriting a committed
  terminal state is a second write to close a window, which ADR-0009 rules out.
  The handler's failure is the handler's problem.
- **Running the handler in a goroutine.** It removes the block on the worker's
  next claim and adds a second way to lose the notification, with nothing owning
  the goroutine's lifetime. The block is real and is documented instead.

## Consequences

- A slow handler delays that worker's next claim. Handlers should be short, or
  hand off to something that is.
- Callers who need exactly-once follow-up work must model it as a parent
  Process. `docs/getting-started.md` says so where handlers are introduced.
- `Register` gained a third type parameter, inferred from the strategy and the
  handler together, so a mismatched handler is a compile error rather than a
  runtime one. Existing call sites that relied on inference did not change.
- The handler runs on whichever instance committed the transition, which for
  `cancelled` is the caller of `Cancel`. Handlers must not assume worker-local
  state.

## History

| Date | Change |
|---|---|
| 2026-07-20 | Initial record. |
