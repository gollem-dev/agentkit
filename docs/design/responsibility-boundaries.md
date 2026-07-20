# Responsibility boundaries

agentkit declines several properties a runtime could plausibly promise. Each
refusal is deliberate, and each one hands a real obligation to someone else. A
boundary that is not written down is a boundary nobody honours — so this page
exists to name them.

## The map

| Property | Owner | Why not the kernel |
|---|---|---|
| Committed state survives a crash | **kernel** | — |
| One transition is atomic | **kernel** | — |
| Child creation and parent wakeup are atomic | **kernel** | a child process is a framework object |
| No double-claim, no stale commit | **kernel** + `Repository` | — |
| Side effects are not applied twice | **tool author** | only the tool knows what "the same operation" means |
| Effects are not applied at all after a divergent replay | **strategy author** | requires checkpointing intent before acting |
| A human approves a sensitive action | **tool author** | a syscall-level gate is not enforceable |
| Data formats round-trip correctly | **caller / strategy author** | the kernel never parses caller data |
| Tenant isolation | **application** | the kernel has no tenancy concept |
| Cost limits | **application** (`Limiter`) | pricing is not kernel knowledge |
| Audit durability before an action | **tool author** | observers are best-effort |
| Event delivery to channels | **application** | delivery couples to the store |

## Idempotency belongs to tool authors

Transitions run at least once (ADR-0003). A tool with an external side effect
**will** eventually run twice.

The idempotency key must come from the *meaning* of the tool's own arguments —
"transfer $50 to account X for invoice Y" is identified by the invoice, not by
its position in a call sequence. The framework deliberately does not hand out a
positional key (`process/seq/label`), because under non-deterministic replay the
second run may be a different call entirely, and a key that claims otherwise is
wrong exactly when it matters.

```go
func (t *transferTool) Run(ctx context.Context, args map[string]any) (map[string]any, error) {
    key := fmt.Sprintf("invoice:%v", args["invoice_id"]) // derived from meaning
    return t.payments.TransferOnce(ctx, key, args)
}
```

**Idempotency prevents duplicates, not divergence.** A replay may have the model
decide differently, producing an effect the committed state does not reflect. If
that matters, the strategy must commit the intent first and act on it in the
next transition:

```go
// transition N: decide and checkpoint. No effect yet.
if st.Plan == nil {
    plan, err := decide(ctx, sys)
    if err != nil { return st, agentkit.Decision{}, err }
    st.Plan = plan
    return st, agentkit.Continue(), nil
}
// transition N+1: the plan is committed, so a replay executes the same one.
res, err := sys.CallTool(ctx, st.Plan.Call)
```

## Confirmation is not enforcement

A strategy can ask a human before acting, by suspending on a question and
resuming on the answer (ADR-0008). This is genuinely useful and it is genuinely
not a security control.

The kernel has no approval gate. A strategy that skips the question and calls
`Syscalls.CallTool` is not stopped, because `Syscalls` is a gateway, not a
privilege boundary — the strategy runs in the same address space as the kernel.

A hard allow/deny decision goes **inside the tool**:

```go
func (t *deployTool) Run(ctx context.Context, args map[string]any) (map[string]any, error) {
    if !t.authz.Allows(ctx, args) {
        return nil, goerr.New("not authorized")   // fail closed, in Run
    }
    ...
}
```

Wrap the tools your `ToolFactory` returns and the check cannot be routed around,
because there is no path to the effect that does not pass through `Run`.

The same reasoning applies to audit: an `Observer` cannot stop anything and its
records are not persisted by the framework (ADR-0012). If something must be
durably recorded *before* it happens, record it in `Run`, before the effect.

## Metadata is data, not a credential

`Process.Metadata` is caller-supplied and stored verbatim. The kernel does not
interpret it and cannot validate it (ADR-0011).

A `ToolFactory` reading `proc.Metadata["tenant"]` to scope a database client is
trusting whoever called `Spawn`. That is fine **only** if the application
derived that value server-side from an already-validated principal before
spawning. Validate first, then establish scope — never load a scope from input
and verify it afterwards.

## The strategy author's obligations

Beyond the above:

- **`Step` must be re-runnable from the last checkpoint.** It is called from the
  top every time, including after a crash. Anything that must not repeat is
  recorded in state.
- **`Step` must not block.** Waiting is expressed by suspending on an await, not
  by sleeping. A blocked `Step` holds a claim and a lease.
- **`Init` must be pure.** It gets no context and no syscalls — structurally,
  there is no path to an effect — and it runs synchronously inside `Spawn`, so
  its error surfaces to the caller instead of becoming an asynchronous failure.
- **`DecodeState` owns migration.** It receives the version that wrote the bytes.
  Old checkpoints exist in production; refusing to read them strands processes.
- **Bound the LLM work per transition.** One `Generate` per transition means a
  crash costs at most one round. Both bundled strategies follow this.

## The application's obligations

- **Verify your `Repository` with `repotest.Run`.** The kernel's correctness
  rests on that contract. An implementation that has not been run against the
  suite is unverified, however plausible it looks.
- **Keep the `Limiter` cheap.** It runs before every effect and at every
  transition boundary.
- **Keep observers non-blocking and duplicate-tolerant.** They run inline and
  fire on replays.
- **Ship events yourself.** They are written durably and in order; delivering
  them to Slack, a queue, or anywhere else is the application's job.
