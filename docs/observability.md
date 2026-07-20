# Observability

Four separate mechanisms, easy to confuse. They differ in one property —
**durability** — and that difference decides which one you should use.

| Mechanism | Durable? | Can stop execution? | For |
|---|---|---|---|
| `Metrics` | yes (on the process row) | no | usage accounting |
| `Limiter` | — (a decision, not a record) | **yes** | budgets and caps |
| `Event` | yes (committed with the transition) | no | progress your application consumes |
| `Observer` | **no** | no | audit trails, tracing, metrics export |

## Metrics

The kernel maintains six counters per process:

`input_tokens`, `output_tokens`, `llm_calls`, `tool_calls`, `steps`, `spawns`.

Read the committed totals from `proc.Metrics`, or the live value inside a
strategy with `sys.Metrics()` — which returns committed totals plus what the
current run has accumulated so far.

There is no cost metric. Pricing depends on model, date and contract, which the
kernel cannot know; emitting a number it cannot compute correctly would be worse
than emitting none. Derive cost from the token counters
([ADR-0010](adr/0010-limiter-is-one-function.md)).

Metrics are **not** rolled up from children to parents. Each process meters
independently, and `RootID` lets you aggregate a whole tree yourself.

## Limiter

The kernel measures; you decide. One closure:

```go
agentkit.WithLimiter(func(ctx context.Context, proc *agentkit.Process, m agentkit.Metrics) error {
    if m[agentkit.MetricLLMCalls] > 50 {
        return goerr.New("llm call budget exhausted")
    }
    if m[agentkit.MetricInputTokens]+m[agentkit.MetricOutputTokens] > 1_000_000 {
        return goerr.New("token budget exhausted")
    }
    return nil
})
```

It runs **before every `Generate`, `CallTool` and `SpawnChild`, and again at
every transition boundary.** Nil means continue; non-nil means stop:

- before an effect, it reaches the strategy as `ErrLimitExceeded`, so a strategy
  may catch it, checkpoint what it has, and suspend;
- at a transition boundary, it finalizes the process as `failed` with
  `FailureLimitExceeded`.

No `Limiter` means unlimited.

Because it is a closure and not a table, policies that a static limit table
cannot express are ordinary code — per-agent caps from `proc.Agent`, tree-wide
budgets from `proc.RootID` plus your own store, rate limits from your own
limiter:

```go
agentkit.WithLimiter(func(ctx context.Context, proc *agentkit.Process, m agentkit.Metrics) error {
    budget := budgetFor(proc.Agent)
    if m[agentkit.MetricLLMCalls] > budget {
        return goerr.New("agent budget exhausted", goerr.V("agent", proc.Agent))
    }
    return myRateLimiter.Check(ctx, string(proc.RootID))
})
```

Keep it cheap and non-blocking — it is on the hot path.

## Events

Events are durable. `sys.Emit` buffers one, and it is written as part of the
transition's atomic commit, in order:

```go
sys.Emit(ctx, "my.progress", []byte(`{"round":2,"tasks":5}`))
```

The kernel emits three of its own: `process.created`, `process.finished`, and
`await.created` (questions only — timers and children awaits are internal). Your
own type names should avoid those three; it is a recommendation, not enforced.

Read them back per process:

```go
events, err := kernel.ListEvents(ctx, pid)
```

There is no global feed and no cursor API. Delivering events to Slack, a queue,
or anywhere else is your application's job — tailing your own database is
simpler than a delivery port every `Repository` implementation would have to
support ([ADR-0012](adr/0012-observer-is-best-effort-observation.md)).

Payload bytes are stored verbatim; encoding is yours.

## Observer

Three span-shaped hooks. Each is called at the start of an effect and returns a
callback invoked at completion — record intent at the start, outcome at the end:

```go
agentkit.WithObserver(agentkit.Observer{
    ToolCall: func(ctx context.Context, ec agentkit.EffectContext, call gollem.FunctionCall) func(map[string]any, error) {
        span := tracer.Start(ctx, "tool:"+call.Name,
            attribute.String("process", string(ec.ProcessID)),
            attribute.String("root", string(ec.RootID)),
            attribute.Int("seq", ec.StateSeq),
        )
        return func(res map[string]any, err error) { span.End(err) }
    },
    Generate: func(ctx context.Context, ec agentkit.EffectContext, in []gollem.Input, role agentkit.ModelRole) func(*agentkit.GenerateResult, error) {
        ...
    },
    Spawn: func(ctx context.Context, ec agentkit.EffectContext, child agentkit.ProcessID, agent agentkit.AgentName) func(error) {
        ...
    },
})
```

`EffectContext` carries `ProcessID`, `RootID`, `Agent` and `StateSeq` — enough
to key an audit row and to correlate a whole process tree. A nil field is never
called.

Four properties to design around:

- **Nothing is persisted by the framework.** This is not a journal. Whatever you
  want kept, you keep.
- **Hooks cannot stop execution.** Denial belongs inside a tool
  ([tools.md](tools.md)).
- **They fire on replays.** Effects run at least once, so the same logical
  operation may be observed more than once. That is intentional — an audit trail
  should record what actually happened. De-duplicate on your side if you need to.
- **Arguments are deep copies**, so mutating them cannot affect execution. Panics
  are recovered and logged rather than failing the transition.

`Spawn` is the one hook with commit-aware timing: the start callback fires when
the strategy requests the child, and the completion callback fires *after* the
transition commit persists it — with `nil` on success, or the error if the
transition never committed. So the record matches whether a child actually came
into existence.

## Choosing

- **A durable audit record that must exist before an action happens** → inside
  the tool's `Run`. Not an observer: observers are best-effort by design.
- **Progress your application reacts to** → `Emit`, then tail your own store.
- **Distributed tracing / metrics export** → `Observer`.
- **Enforcing a budget** → `Limiter`.
- **Reporting usage** → `Metrics`.

## Logging

`WithLogger(*slog.Logger)` supplies the kernel's logger, used for things like
recovered observer panics. It defaults to `slog.Default()`.
