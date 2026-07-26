# Observability

Four separate mechanisms, easy to confuse. They differ in one property —
**durability** — and that difference decides which one you should use.

| Mechanism | Durable? | Can stop execution? | For |
|---|---|---|---|
| `Metrics` | yes (on the process row) | no | usage accounting |
| `Limiter` | — (a decision, not a record) | **yes** | budgets and caps |
| `Event` | yes (committed with the transition) | no | progress your application consumes |
| Middleware | no (unless you persist something yourself) | **yes** | tracing, audit, cost accounting, redaction, tool policy |

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

Children roll up into their parent. A child adds its totals to its parent's when
it terminates — once, whether or not the parent ever waited on it — and since a
child rolled up its own children the same way, a root ends up holding what the
whole tree spent. Two things follow: `proc.Metrics` on a process with children is
a subtree figure rather than that one process's own spend, and a child still
running is not in it yet.

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

Because the counters roll up, the same closure is a subtree budget on a fan-out
parent — you do not need separate accounting keyed by `RootID`. The case to
design around is that a parent can pass the check, spawn children that spend the
rest of the budget, and trip on its next transition, ending above its own cap.
The cap bounds what the parent goes on to do, not what its subtree already did.
`examples/fanout` is written around exactly this.

A `Limiter` is a **budget veto, not admission control**: it answers "may this
continue", and has no way to wait. Do not block in it for a rate-limit token —
it runs on the transition hot path while the claim holds its lease, so waiting
there turns a throttle into a lease expiry and an unclean reclaim. Work that has
to wait belongs behind a timer await.

A `Limiter` is not the only thing that ends a run without the strategy deciding
to. A process whose claims keep dying mid-transition finalizes as `failed` with
`FailureUncleanReclaim` once it exceeds `WithMaxUncleanReclaims`. Read that code
as a signal about your workers rather than about the strategy: `retry_exhausted`
means a strategy kept returning errors, whereas `unclean_reclaim` means the
worker running it kept disappearing
([ADR-0015](adr/0015-unclean-reclaims-are-counted-and-bounded.md)).

Because it is a closure and not a table, policies that a static limit table
cannot express are ordinary code — per-agent caps from `proc.Agent`, correlation
across a tree from `proc.RootID`, rate limits from your own limiter (a tree
budget needs none of that, since the counters already roll up):

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

Read them back per process, all at once or from where you left off:

```go
events, err := kernel.ListEvents(ctx, pid)

// Next time round, resume from the last id you saw.
more, err := kernel.ListEvents(ctx, pid,
    agentkit.WithAfterEvent(events[len(events)-1].ID),
    agentkit.WithEventLimit(100))
```

Every event carries an `EventID` the kernel minted, which is what makes both
resuming and deduplicating possible. A cursor the process has no event for is
`ErrEventNotFound` rather than a silent restart — that error means your stored
cursor went stale, and reading from the beginning again would look like a burst
of new events ([ADR-0019](adr/0019-events-are-addressable-and-reads-resume.md)).

There is still no global feed: ids order events within one process, not across
them. Delivering events to Slack, a queue, or anywhere else is your
application's job — tailing your own database is simpler than a delivery port
every `Repository` implementation would have to support
([ADR-0012](adr/0012-kernel-hooks-are-composable-middleware.md)).

Payload bytes are stored verbatim; encoding is yours.

## Delivering a result

Three ways to act on a finished Process, in ascending order of what they cost
and what they guarantee ([ADR-0018](adr/0018-durable-delivery-is-built-in-repository-apply.md)):

| | Use when |
|---|---|
| `WithOnFinish` | losing the notification is acceptable — a dev channel ping, a metric, a log line |
| a parent Process on `WaitChildren` | the follow-up is itself agent work |
| an outbox row written inside your `Repository.Apply` | an external system must hear about it, and a duplicate is cheaper than a miss |

The first is best-effort by construction: it runs right after the commit, so a
crash in between loses it and nothing in your store records that anything was
owed ([ADR-0014](adr/0014-completion-handlers-are-best-effort.md)). Reaching for
a durable tier because a notification *might* matter is usually the wrong trade
— most of them do not.

The third works because you already own `Apply`. Add the row to the same write
as the `ChangeSet`:

```go
func (r *Repo) Apply(ctx context.Context, cs agentkit.ChangeSet) error {
	return r.inTx(ctx, func(tx *sql.Tx) error {
		// ... the ChangeSet itself: Guards, Processes (Rev-CAS), Awaits, Events
		for _, p := range cs.Processes {
			if p.Status == agentkit.ProcessSucceeded || p.Status == agentkit.ProcessFailed {
				// Key it by (ProcessID, Rev): at most one Apply succeeds per Rev,
				// so a replayed transition cannot insert a second row.
				if err := insertOutbox(ctx, tx, p); err != nil {
					return err
				}
			}
		}
		return nil
	})
}
```

Two properties of the `Repository` contract carry this, and `repotest` already
checks both: the whole `ChangeSet` commits atomically, so the Process cannot go
terminal without the row; and a `Rev` mismatch writes nothing, so `(ProcessID,
Rev)` names one committed transition and stays stable across a replay.

Write the payload while you are there. `cs.Processes` holds `Output` and
`Failure`, whereas the kernel's `process.finished` event carries none — a relay
that skipped this has to read the Process back.

Then deliver from a separate process: claim a pending row, send, mark it
delivered, and back off on failure. Alert on the age of the oldest pending row.

What you do not get is exactly-once — no tier offers it. An outbox trades silent
loss for duplicates you can see and suppress, so a destination without its own
idempotency (`chat.postMessage`) still needs a dedup key or an update-by-`ts`
scheme of yours.

This needs your own `Repository`. `repository/filesystem` commits by renaming one
snapshot file and `repository/memory` is not durable at all, so neither has
anywhere to put the row.

## Middleware

Five `next`-chain hooks, one per point where the kernel calls out: `Init`,
`Step`, `Generate`, `CallTool`, `SpawnChild`. Registered on the `Kernel` with
`WithInitMiddleware` and its four siblings, so one registration runs for every
agent. A middleware receives the next handler and returns a replacement, which
lets it observe, rewrite the request, refuse by not calling `next`, or call
`next` more than once (`StepMiddleware` excepted — see below):

```go
agentkit.WithToolCallMiddleware(func(next agentkit.ToolCallHandler) agentkit.ToolCallHandler {
    return func(ctx context.Context, req *agentkit.ToolCallRequest) (map[string]any, error) {
        span := tracer.Start(ctx, "tool:"+req.Call.Name,
            attribute.String("process", string(req.Effect.ProcessID)),
            attribute.String("root", string(req.Effect.RootID)),
            attribute.Int("seq", req.Effect.StateSeq),
        )
        res, err := next(ctx, req)
        span.End(err)
        return res, err
    }
}),
```

`EffectContext` is enough to key an audit row and to correlate a whole process
tree: which Process, which agent, which transition, how many attempts it has
already taken, and `Metadata` — a copy of what `WithMetadata` set at spawn, so a
middleware can tag or scope by it without holding a `Repository`. **`Metadata`
is data, not a credential** ([ADR-0011](adr/0011-kernel-has-no-tenancy.md));
reading it is trusting whoever called `Spawn`.

Properties to design around, all detailed in `middleware.go` and
[ADR-0012](adr/0012-kernel-hooks-are-composable-middleware.md):

- **Nothing is persisted by the framework.** Whatever you want kept, you keep.
- **A middleware is a real chokepoint, not a security gate.** `Generate`,
  `CallTool` and `SpawnChild` middleware is the outermost layer of its syscall —
  it wraps the `Limiter` check and, for tool calls, name resolution and argument
  validation — so it can refuse a call fail-closed and see a call the `Limiter`
  later refuses. But `Syscalls.CallTool` is not the only path to a tool: a
  strategy holding a `gollem.Tool` value can call `Run` on it directly.
  Enforcement still belongs inside `Run` ([tools.md](tools.md)).
- **`StepMiddleware` sees the `Step` call, not the commit.** It sits between
  `DecodeState` and `EncodeState`; the commit happens after it returns. A
  transition that fails to commit is re-run, so the middleware runs again — the
  at-least-once model is visible here exactly as it is.
- **`StepMiddleware` must call `next` at most once.** A Step's effects — spawned
  children, emitted events, metrics — buffer per transition, not per call, so a
  retried Step would commit the discarded attempt's effects alongside the
  accepted one's. The second call returns `ErrInvalidRequest`. To decide a
  transition without running the strategy, skip `next` entirely and return a
  `NewStepResult`.
- **`StepRequest.Process` is a copy.** Writing to it changes nothing that is
  committed — which is deliberate, since the commit is built from the original
  and a middleware setting `Rev` would make every `Apply` conflict.
- **A request and a result live only for the handler call.** Nothing is
  deep-copied (the point of a middleware is that it may rewrite them), so their
  payloads, maps and slices are shared with the kernel and with whatever runs
  next. Handing one to a goroutine or a queue that outlives the call means
  copying what you need out of it first.
- **They fire on replays.** Effects run at least once, so the same logical
  operation may run through a middleware more than once. De-duplicate on your
  side if you need to.
- **No static type safety over the type-erased payloads.** A kernel-level
  middleware runs across every agent, so it cannot know a particular strategy's
  input, state or output type. Read one with `InitInput[I]` / `StepState[S]` /
  `SpawnInput[I]` / `ResultState[S]` (or `[any]` to observe without knowing the
  type), and replace one by deriving a new request (`NewInitRequest`,
  `NewStepRequest`, `NewSpawnRequest`) or by building a `NewStepResult`. A wrong
  type compiles and fails at run time with `ErrInvalidRequest` — the nature of a
  cross-cutting layer, not a gap to work around.
- **A Decision is the one payload with no `[any]` escape hatch.**
  `ResultDecision[O]` needs the agent's exact output type, because a Decision
  carries a type witness so that a nil interface output survives erasure. Use
  `DecisionKindOf` to branch on continue/suspend/done/fail without naming `O`.
- **A panic in middleware is not recovered by the chain.** Inside a transition
  the worker's own recovery turns it into a transition error and the Process
  retries; in `Init` it propagates to the `Spawn` caller.

`SpawnRequest.OnCommit(fn)` is the one thing a plain chain cannot express by
itself: child creation is buffered into the transition commit (ADR-0009), so
whether the child exists is only known later. `fn` is called once with that
outcome — `nil` when the transition committed, non-nil otherwise. Its scope is
the *transition*, not the child, and it runs after the commit, outside it, so a
panic in it is recovered and logged rather than becoming a transition error.

## Choosing

- **A durable audit record that must exist before an action happens** → inside
  the tool's `Run`. Middleware runs inline and is not a journal.
- **Progress your application reacts to** → `Emit`, then tail your own store.
- **Telling something outside about a finished run** → pick a tier in
  [Delivering a result](#delivering-a-result). Best-effort is the default answer;
  reach past it only when a miss actually costs something.
- **Distributed tracing / metrics export / tool policy** → middleware.
- **Enforcing a budget** → `Limiter`.
- **Reporting usage** → `Metrics`.

## Logging

`WithLogger(*slog.Logger)` supplies the kernel's logger, used for things like a
recovered `SpawnRequest.OnCommit` panic. It defaults to `slog.Default()`.
