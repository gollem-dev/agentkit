# Execution model

Read this before writing anything that touches the outside world. Most mistakes
with agentkit come from assuming a guarantee it deliberately does not make.

## What agentkit guarantees

**A committed transition is never lost.** Once a transition commits, its state
survives any crash.

**One transition is atomic.** State, declared awaits, emitted events, spawned
children and metrics land in a single `Repository.Apply` — all of it, or none.

That is the whole list.

## What it does not guarantee

**Exactly-once execution.** A transition that crashes before committing is
re-run by another worker from the last checkpoint. Any LLM call or tool call it
made may run again.

**Deterministic replay.** An LLM is not deterministic. A re-run may produce
different text, call a different tool, or call the same tool with different
arguments. There is no effect journal, no operation key, and no deterministic
clock — a worker simply re-executes `Step`.

An LLM re-charge on replay is accepted as a cost of the design.

## What that makes your job

### Side-effecting tools must be idempotent

A tool that changes the world will eventually run twice. Derive an idempotency
key from the **meaning of the arguments** — the invoice being paid, the message
being sent — not from anything positional:

```go
func (t *transferTool) Run(ctx context.Context, args map[string]any) (map[string]any, error) {
    key := fmt.Sprintf("invoice:%v", args["invoice_id"])
    return t.payments.TransferOnce(ctx, key, args)
}
```

agentkit deliberately does not hand you a framework-generated key. Under
non-deterministic replay, a positional key would identify the wrong thing
precisely when it matters.

### Idempotency stops duplicates, not divergence

Suppose a transition asks the model what to do and then does it. A crash after
the effect but before the commit means the effect happened, but the committed
state has no record of it — and the replay may decide on a *different* action.
No idempotency key helps, because it is genuinely a different operation.

For exactly-once semantics, **checkpoint the decision, then act on it**:

```go
// transition N: decide, commit the intent, do nothing else.
if st.Plan == nil {
    plan, err := decide(ctx, sys)
    if err != nil {
        return st, agentkit.Decision{}, err
    }
    st.Plan = plan
    return st, agentkit.Continue(), nil
}

// transition N+1: the plan is durable, so any replay executes the same one.
res, err := sys.CallTool(ctx, st.Plan.Call)
```

### `Step` must be re-runnable and must not block

`Step` runs from the top every time it is called. Anything that must not happen
twice belongs in your state, not in a local variable.

Waiting is expressed by suspending on an await, never by sleeping. A blocked
`Step` holds a claim and a lease and stops the process from being picked up
elsewhere.

### Bound the work per transition

One `Generate` per transition means a crash costs at most one LLM round. Both
bundled strategies follow this rule; yours should have a reason not to.

## Where the guarantees do apply

Child processes are framework objects, so agentkit *does* coordinate them.
`SpawnChild` buffers the child into the transition's commit — if the transition
does not commit, the child never existed, so there are no orphans. When a child
finishes, waking the parent is part of the child's own terminal commit, so there
is no window in which a child is done and the parent never hears about it.

See [design/consistency-model.md](design/consistency-model.md) for the full set
of failure windows and how each is closed.

## Confirmation is not a security gate

A strategy can ask a human before acting:

```go
if !st.Confirmed {
    return st, agentkit.Suspend(agentkit.Question("confirm", []byte("run X? (yes/no)"))), nil
}
```

This is a *confirmation*, not enforcement. There is no approval gate in the
kernel, and a buggy or manipulated strategy can call `Syscalls.CallTool`
directly.

For a hard allow/deny decision, check inside the tool, where nothing can bypass
it:

```go
func (t *deployTool) Run(ctx context.Context, args map[string]any) (map[string]any, error) {
    if !t.authz.Allows(ctx, args) {
        return nil, goerr.New("not authorized")
    }
    ...
}
```

## Further reading

- [ADR-0003](adr/0003-at-least-once-replay-no-effect-journal.md) — why the effect
  journal was removed.
- [design/responsibility-boundaries.md](design/responsibility-boundaries.md) —
  the full map of who owns which correctness property.
