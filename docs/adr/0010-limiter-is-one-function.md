# ADR-0010: Execution limits are one Limiter function

## Summary

The kernel **measures**; the caller **decides**. Measurement is a fixed set of
counters in `Metrics` (input tokens, output tokens, LLM calls, tool calls, steps,
spawns). The decision is one injected closure:

```go
type Limiter func(ctx context.Context, proc *Process, metrics Metrics) error
```

It is called before every `Generate`, `CallTool` and `SpawnChild`, and again at
each transition boundary. A nil return continues. A non-nil return stops: before
an effect it reaches the strategy as `ErrLimitExceeded`; at a transition boundary
it finalizes the process as `failed` with `FailureLimitExceeded`. A nil `Limiter`
means unlimited.

`Process.Metrics` counts a Process's own effects **plus every child that has
terminated, once each**, so one closure expresses a subtree budget as well as a
per-Process one. It is a budget veto, not admission control: it decides whether
to continue, and has no way to wait.

## Context

The original design had two layers: a static `Limits map[Metric]int64` table,
overridable per agent definition and per spawn, plus a `Governor` port for
dynamic throttling. Between them they expressed only what a table can express,
while each new policy shape (rate limits, monetary budgets, per-agent ceilings,
whole-tree budgets) demanded new structure.

## Decision

Delete both layers. Ship the counters and one function.

A static cap is a few lines of closure. A per-agent cap reads `proc.Agent`. A
rate limit consults the caller's own limiter. All through one interface, and none
of it in agentkit.

`metrics` is a live snapshot: committed cumulative (`proc.Metrics`) plus what the
current run has accumulated so far. There is no way for an effect to consume
budget without the next check seeing it.

**A whole-tree budget is the same closure, because the counters roll up.** A
child adds its own `Metrics` to its parent in its terminal transition
(`reportToParent`, `worker.go`), and since it had already absorbed its own
children the same way, a root ends up holding the tree. This originally required
the caller to key its own accounting by `proc.RootID`; a fan-out strategy like
`planexec` made that mandatory for anyone who cared about total spend, which is a
lot of machinery to demand for a question the kernel was already measuring the
parts of.

**The fold is keyed to the child, not to an await.** A Process terminates exactly
once, so counting there is correct by construction — no dedup check, no set of
already-counted ids. Keying it to await resolution instead was the first attempt
and it was wrong twice over: a child named by two await keys was counted twice,
and a child nobody waited on — which ADR-0009 permits — was never counted at all.
`ChildResult.Metrics` still carries the figure to the parent's strategy, which is
a separate concern from the accounting.

The fold rides the child's own terminal `ChangeSet`, which already writes the
parent row, so it stays one write per transition. What it does not do is track a
child still running; a parent's view of its subtree is as of the last child that
finished.

`limit_exceeded` is not a process status. It is `failed` with
`FailureCode == FailureLimitExceeded`, which keeps the state machine at six
statuses and puts the reason where every other failure reason already lives.

## Alternatives rejected

- **`Limits map[Metric]int64` plus per-agent and per-spawn overrides.** Fixes the
  shape of a policy to "a number per counter", which most real policies are not.
- **A `Governor` port for dynamic throttling.** A second mechanism for the same
  question; the closure already covers it.
- **Cost metrics in the kernel** (`cost_micro_usd` and friends). Pricing depends
  on model, date and contract — knowledge the kernel does not have and cannot
  acquire. Emitting a number it cannot compute correctly would be worse than
  emitting nothing. Callers derive cost from the token counters.
- **Tool-reported metrics.** `gollem.Tool.Run` returns `map[string]any`, and
  conforming to that signature (ADR-0001) matters more. Tools count as
  `tool_calls` only. An optional interface can add this later without a break.
- **Giving the `Limiter` a second argument for subtree totals**, or widening it
  into a request struct. Both reopen the shape question this record closed, to
  deliver something the existing `Metrics` argument can carry once the counters
  roll up.
- **A `Repository` query by `RootID`, so a caller can total the tree itself.**
  Another SPI method every implementer owes, to recompute on demand what the
  kernel can accumulate as it goes — and racy besides, since a sibling can
  finish between the query and the decision.
- **Letting a `Limiter` block until budget frees up.** It runs on the transition
  hot path while the claim holds its lease; waiting there converts a throttle
  into a lease expiry and an unclean reclaim (ADR-0015). Work that must wait
  belongs in a timer await.

## Consequences

- Reading `Metrics` is meaningful; interpreting it is not the kernel's job. Cost,
  quota and fairness all live in caller code.
- A `Limiter` must be cheap and non-blocking. It runs before every effect, on the
  transition hot path.
- A `Limiter` that returns an error mid-transition surfaces as `ErrLimitExceeded`
  to the strategy, which may handle it (checkpoint what it has and `Suspend`) or
  propagate it. That is the strategy author's call.
- Metrics from a failed attempt are still folded in on requeue and on
  `retry_exhausted`, so a crash-looping process cannot consume unbounded budget
  invisibly.
- **A parent's recorded counters can exceed its own cap.** It passes the check,
  spawns children that spend the rest of the budget, and trips on its next
  transition — by which point the subtree really has spent that much. The cap
  still binds what the parent goes on to do; it is not a bound on what is
  recorded. `examples/fanout` is written around this case.
- Reading "what did this one Process spend" is no longer a single field once it
  has children. The per-Process figure is recoverable from the children's own
  rows, which keep their own totals.

## History

| Date | Change |
|---|---|
| 2026-07-20 | Initial record, extracted from the initial implementation spec (D6, D9, D10, D33). |
| 2026-07-26 | `Process.Metrics` now includes every terminated child, counted once each, so one `Limiter` expresses a subtree budget. Previously a tree budget needed the caller's own accounting keyed by `RootID`, which a fan-out strategy made mandatory. The signature is unchanged. The fold is keyed to the child's terminal transition rather than to await resolution — the first attempt keyed it to the await, which double-counted a child named by two await keys and missed one nobody waited on. |
