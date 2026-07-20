# Tools

agentkit uses `gollem.Tool` directly. There is no wrapper type, no side-effect
classification, and no approval gate — a tool is used exactly as gollem defines
it ([ADR-0001](adr/0001-depend-on-gollem-directly.md)).

That means the two properties agentkit cannot provide are yours to provide:
**idempotency** and **authorization**.

## Supplying tools

A `ToolFactory` is called once per claim and returns the tools that process may
use:

```go
kernel, err := agentkit.New(repo, model, reg,
    agentkit.WithToolFactory(func(ctx context.Context, proc *agentkit.Process) ([]gollem.Tool, error) {
        switch proc.Agent {
        case "researcher":
            return []gollem.Tool{searchTool, fetchTool}, nil
        case "operator":
            return []gollem.Tool{deployTool, rollbackTool}, nil
        }
        return nil, nil
    }),
)
```

It is a function type, not an interface, because building per claim is the
normal case and a closure is the natural form. A stateful implementation passes
a method value.

The factory receives the `*Process`, so the agent kind itself is the selector.
agentkit has no separate vocabulary for tool selection — one that the kernel
does not interpret would just be a type with no specification behind it
([ADR-0011](adr/0011-kernel-has-no-tenancy.md)).

Dependencies that do not vary per process (an HTTP client, a connection pool)
can be captured in the closure or carried on the context passed to `Serve`.

Inside a strategy, `sys.Tools()` returns what the factory built — declare them to
the model with `agentkit.WithTools(sys.Tools()...)` and execute the resulting
calls with `sys.CallTool`.

## Idempotency is your responsibility

Transitions run at least once. **A tool with an external side effect will
eventually run twice** ([execution-model.md](execution-model.md)).

Derive an idempotency key from the *meaning* of the arguments:

```go
func (t *notifyTool) Run(ctx context.Context, args map[string]any) (map[string]any, error) {
    // The key identifies the logical operation, not the call site.
    key := fmt.Sprintf("notify:%v:%v", args["channel"], args["incident_id"])
    if err := t.notifier.SendOnce(ctx, key, args); err != nil {
        return nil, goerr.Wrap(err, "send", goerr.V("key", key))
    }
    return map[string]any{"sent": true}, nil
}
```

agentkit deliberately does not pass you a framework-generated key. Under
non-deterministic replay a positional key (`process/seq/label`) would name a
different logical call than the original, which makes it wrong exactly when it
matters ([ADR-0003](adr/0003-at-least-once-replay-no-effect-journal.md)).

If a side effect genuinely cannot be made idempotent, keep it out of the same
transition as the decision that triggers it — checkpoint the intent, then act.

## Authorization goes inside `Run`

There is no approval gate in the kernel. A strategy can suspend on a question to
ask a human first, and that is genuinely useful, but it is a *confirmation*: a
buggy or manipulated strategy can call `CallTool` without asking, and nothing
stops it.

A hard allow/deny decision must sit where there is no path around it:

```go
type guardedTool struct {
    inner gollem.Tool
    authz Authorizer
}

func (t *guardedTool) Spec() gollem.ToolSpec { return t.inner.Spec() }

func (t *guardedTool) Run(ctx context.Context, args map[string]any) (map[string]any, error) {
    if err := t.authz.Check(ctx, t.inner.Spec().Name, args); err != nil {
        return nil, goerr.Wrap(err, "authorization denied")  // fail closed
    }
    return t.inner.Run(ctx, args)
}
```

Wrap in the `ToolFactory` and every call is checked, because `Run` is the only
way to reach the effect.

The same holds for audit. `Observer` hooks are best-effort and not persisted by
the framework ([ADR-0012](adr/0012-observer-is-best-effort-observation.md)); if
something must be durably recorded *before* it happens, record it in `Run`.

## Scoping tools per tenant

`proc.Metadata` is the intended channel for infrastructure-facing scope:

```go
agentkit.WithToolFactory(func(ctx context.Context, proc *agentkit.Process) ([]gollem.Tool, error) {
    tenant := proc.Metadata["tenant"]
    db, err := pool.For(tenant)
    if err != nil {
        return nil, goerr.Wrap(err, "tenant db", goerr.V("tenant", tenant))
    }
    return []gollem.Tool{newQueryTool(db)}, nil
})
```

**`Metadata` is not a credential.** It is caller-supplied data stored verbatim,
and the kernel neither interprets nor validates it. Reading it here means
trusting whoever called `Spawn` — which is only safe if the application derived
that value server-side from an already-validated principal *before* spawning.
Validate first, then establish scope; never load a scope from input and check it
afterwards.

## Errors and validation

Arguments are validated against `Spec().ValidateArgs` before `Run` is called; a
failure returns to the strategy without running the tool. An unregistered name
is `ErrToolNotFound`.

A tool's own error is returned to the strategy — it does **not** fail the
process. Most strategies feed it back to the model as a `FunctionResponse` error
so the model can recover, which is what `strategy/simple` does.

Every call counts one `tool_calls` metric. Tools cannot report metrics of their
own, because `gollem.Tool.Run` returns a fixed `map[string]any` and conforming to
that signature matters more; this can be added later as an optional interface
without a breaking change ([ADR-0010](adr/0010-limiter-is-one-function.md)).
