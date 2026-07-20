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

## Context

The original design had two layers: a static `Limits map[Metric]int64` table,
overridable per agent definition and per spawn, plus a `Governor` port for
dynamic throttling. Between them they expressed only what a table can express,
while each new policy shape (rate limits, monetary budgets, per-agent ceilings,
whole-tree budgets) demanded new structure.

## Decision

Delete both layers. Ship the counters and one function.

A static cap is a few lines of closure. A per-agent cap reads `proc.Agent`. A
whole-tree budget reads `proc.RootID` and queries the caller's own store. A rate
limit consults the caller's own limiter. All through one interface, and none of
it in agentkit.

`metrics` is a live snapshot: committed cumulative (`proc.Metrics`) plus what the
current run has accumulated so far. There is no way for an effect to consume
budget without the next check seeing it.

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

## History

| Date | Change |
|---|---|
| 2026-07-20 | Initial record, extracted from the initial implementation spec (D6, D9, D10, D33). |
