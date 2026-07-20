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
was to poll `Kernel.GetProcess`. `Observer` (ADR-0012) has hooks for `Generate`,
`ToolCall` and `Spawn` but none for process completion, and the kernel-emitted
`process.finished` event carries a nil payload, so it says a Process ended
without saying what it produced.

Two constraints shaped the answer. `Spawn` is asynchronous and the instance that
spawns need not be the instance that runs the agent, so a closure captured at
spawn time cannot be relied on to exist where the Process finishes. And ADR-0003
declines an effect journal, so there is no durable record to drive a retry from.

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

Output reaching a parent is unaffected. `ChildResult.Output` stays `[]byte`: a
parent may wait on children of different agents, so no single output type
exists, and `Syscalls.Await` is an interface method, which Go does not allow to
be generic.

## Alternatives rejected

- **A `Finish` hook on `Observer`.** The cheapest option, and it would have made
  ADR-0012 untrue the first time someone put business logic in it. Observation
  and a business callback differ in lifetime and in what a failure means; a
  separate name keeps the option of strengthening one of them later.
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
