# ADR-0008: Three await kinds; confirmation is a question

## Summary

A durable wait has exactly three kinds: `question` (a human answer), `timer`,
and `children` (child processes finishing). Human confirmation is **not** a
fourth kind and there is no approval gate in the kernel — a confirmation is a
yes/no question that the strategy chooses to ask.

Waits are *declared* as the output of a transition, via
`Suspend(Question(...) | Timer(...) | WaitChildren(...))`, and *read* through
`Syscalls.Await(key)`. Declaration is control flow, so it belongs in the
`Decision`; reading is a syscall.

## Context

The original design had a fourth await kind for approvals, plus an
`ApprovalPolicy` the kernel consulted before every side-effecting tool call, plus
`Kernel.Approve`/`Reject` methods and typed approval request/response rows.

Two problems. First, an approval *is* a go/no-go question — the machinery was a
duplicate of `question` with different field names. Second, and worse, a
kernel-level approval gate implies a security guarantee it cannot deliver: a
strategy can call `Syscalls.CallTool` directly, so "a human approves every
sensitive action" is not enforceable at the syscall layer.

Separately, an earlier API had strategies both declaring and reading waits
through `Syscalls` (`Ask`, `Sleep`, `WaitChildren` methods). That made every
strategy re-entrant in a confusing way: the same `Ask` call meant "raise a
question" on one pass and "here is the answer" on the next.

## Decision

Three kinds only, constructed by `Question`, `Timer`, `WaitChildren` — the
`AwaitSpec` fields are unexported, so these constructors are the only way to
build one.

Confirmation is a strategy pattern:

```go
if !st.Confirmed {
    return st, agentkit.Suspend(agentkit.Question("confirm", []byte("run X? (yes/no)"))), nil
}
```

answered by `Kernel.Respond(ctx, pid, "confirm", []byte("yes"))` and read back
with `sys.Await("confirm")`.

Declaration lives in `Decision`, reading in `Syscalls`. `Suspend` with no open
await and no `WaitChildren` elision is a transition error
(`ErrSuspendWithoutAwait`) — that combination is a permanent hang, so it is
caught rather than committed.

## Alternatives rejected

- **A distinct `approval` await kind with an enforcing gate.** Duplicates
  `question`, and the enforcement it advertises is not real at the kernel layer.
- **`Syscalls.Ask`/`Sleep`/`WaitChildren` methods.** Re-entrant idiom, and it
  puts control flow into the effect gateway.
- **A `Waits` argument on `Step`.** A new type on the hot signature, where
  `Syscalls.Await` already fits with no new surface.

## Consequences

- **Confirmation is not a security control.** A buggy or manipulated strategy
  can skip the question and call the tool. Real allow/deny enforcement goes
  inside the tools a `ToolFactory` returns, where the check runs in `Run` and
  cannot be routed around. This must be stated wherever confirmation is
  documented.
- Response semantics are uniform: only an `open` await accepts a response,
  first-writer-wins (`ErrAwaitClosed` for the second), and the responder is
  recorded via the optional `WithRespondedBy`. `Respond` targeting a non-question
  await is `ErrInvalidRequest`.
- A question may carry a deadline (`WithDeadline`); reaching it sets the await to
  `expired`, not `responded`, and the strategy sees the distinction. A timer past
  its deadline is marked `responded` with `Fired = true`.
- Deadlines drive `Process.WakeAt` (the minimum across open awaits), which is
  what makes a waiting process claimable again. Due awaits are settled at the
  start of the next claim, so a timer fires when the process is next picked up —
  not by a background scheduler.
- Re-declaring an already-closed await key is a no-op, which is what makes a
  `Suspend` safe to re-execute after a crash.

## History

| Date | Change |
|---|---|
| 2026-07-20 | Initial record, extracted from the initial implementation spec (D17, D35, D38, D47). |
