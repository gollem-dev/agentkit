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
        return st, agentkit.Decision[myOutput]{}, err
    }
    st.Plan = plan
    return st, agentkit.Continue[myOutput](), nil
}

// transition N+1: the plan is durable, so any replay executes the same one.
res, err := sys.CallTool(ctx, st.Plan.Call)
```

That splits the decision from the act. What it does not tell you is whether the
*act* already happened — a crash between `CallTool` returning and the commit
leaves no trace in state. For that, ask whether this attempt is a replay:

```go
// transition N+1, continued: has a previous attempt at THIS transition run?
if a := sys.Attempt(); a.IsReplay() {
    // A previous attempt may have executed the call. Check before repeating it,
    // if the operation can be checked at all.
    if done, err := alreadyApplied(ctx, st.Plan.OperationID); err == nil && done {
        st.Result = ...
        return st, agentkit.Continue[myOutput](), nil
    }
}
res, err := sys.CallTool(ctx, st.Plan.Call)
```

`AttemptInfo` reports the two origins separately, because they are not equally
informative. `Errors` counts previous attempts that returned an error, so you
know roughly how far they got. `UncleanReclaims` counts previous claims that
*vanished*, and those tell you nothing: the transition may have completed every
effect and died immediately before its commit. Worse, a reclaim caused by an
expired lease may be running **concurrently** with a predecessor that has not
yet noticed it lost — so `UncleanReclaims > 0` is not a promise that you are
alone.

This is the tool for the obligation this page places on you. It narrows the
window; it does not close it. An operation that must never run twice still needs
an idempotency key the tool itself enforces
([tools.md](tools.md)), and how many times a crashed transition may be retried
at all is bounded by `WithMaxUncleanReclaims`
([ADR-0015](adr/0015-unclean-reclaims-are-counted-and-bounded.md)).

### `Step` must be re-runnable and must not block

`Step` runs from the top every time it is called. Anything that must not happen
twice belongs in your state, not in a local variable.

Waiting is expressed by suspending on an await, never by sleeping. A blocked
`Step` holds a claim and a lease and stops the process from being picked up
elsewhere.

This now cuts closer to home than it used to. An instance running `Serve`
dispatches a newly-runnable Process eagerly, on a goroutine driven from the
`Spawn`/`Respond` call itself ([ADR-0016](adr/0016-eager-dispatch-is-a-scheduling-optimization.md)) — so a
blocking `Step` no longer only delays some other process's poll; it can also
sit directly on the interactive request path, and it occupies one of
`WithMaxConcurrent`'s hard-limit slots the whole time.

Two `ServeOption`s bound how much of this runs at once: `WithPollConcurrency`
is a soft limit on the number of poll loops, `WithMaxConcurrent` is the hard
limit on claims driven at once by polling and eager dispatch combined. Eager
dispatch itself returns no result to the caller — `Spawn` and `Respond` still
only report that the transition was scheduled or resumed; observe the outcome
through `GetProcess` or `WithOnFinish`, same as with polling.

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
