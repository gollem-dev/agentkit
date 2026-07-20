# Concepts

agentkit borrows its vocabulary from operating systems, and the metaphor is
load-bearing: a `Strategy` is a user program that reaches the outside world only
through system calls into the kernel.

```
Kernel     ── the runtime: lifecycle API + worker loop
 └ Process ── one execution unit, fully persisted
    └ Strategy ── your state machine, checkpointed after every transition
       └ Syscalls ── the only path to LLM, tools, children, waits, events
```

## Process

One execution unit. Everything about it lives in the `Repository`: its state
bytes, status, metrics, lease, parent and root ids, metadata.

It moves through six statuses — `pending`, `running`, `waiting`, and the three
terminal ones `succeeded`, `failed`, `cancelled`. `pending` is the parking
state: a process becomes claimable by returning to it, whether it was just
created, answered, or requeued after an error.

`Process.Output` (on success) and `Process.Failure` (on failure) are how a
result leaves the system. `RootID` is inherited by every descendant, so a whole
tree correlates under one id.

`Process.Metadata` is an optional, kernel-opaque `map[string]string` set at
spawn, meant for infrastructure — a `ToolFactory` deciding which database client
to hand out, for instance. **It is data, not a credential:** derive it
server-side from a validated principal before spawning, never trust it as proof
of anything ([ADR-0011](adr/0011-kernel-has-no-tenancy.md)).

## Strategy

Your state machine, and the only code agentkit runs on your behalf.

```go
type Strategy[S, I, O any] interface {
    Version() int
    Init(input I) (S, error)
    Step(ctx context.Context, sys Syscalls, state S) (S, Decision[O], error)
    EncodeState(state S) ([]byte, error)
    DecodeState(version int, raw []byte) (S, error)
    EncodeOutput(out O) ([]byte, error)
}
```

Three types: `S` is what you checkpoint, `I` is what launches a run, `O` is what
a finished run produces.

- **`Init`** builds the initial state, purely. It receives no context and no
  syscalls — structurally, there is no path to an effect — and runs
  synchronously inside `Spawn`, so a bad input is an error the caller sees
  immediately rather than an asynchronous failure later.
- **`Step`** runs one transition and returns a `Decision[O]`. It is called from
  the top every time, including after a crash.
- **`EncodeState` / `DecodeState`** own serialization entirely. The format is
  yours; agentkit stores bytes. `DecodeState` receives the version that wrote
  them, so migration is ordinary code inside it.
- **`EncodeOutput`** turns what you pass to `Done` into the bytes stored on the
  Process. There is no `DecodeOutput`: a completion handler receives the value
  you passed, and a parent reads a child's output as bytes.

Register with `Register[S, I, O]`, which returns a typed `Agent[I]` — the only
way to spawn, so the input type is checked at compile time and `any` never
appears in the public API ([ADR-0006](adr/0006-typed-handles-from-register.md)).
All three types are inferred from the strategy, so the call site writes none of
them.

## Decision

What one transition returns.

| Decision | Meaning |
|---|---|
| `Continue[O]()` | commit and run the next transition |
| `Suspend[O](specs...)` | commit and park until an await resolves |
| `Done(output)` | finish successfully with this output |
| `Fail[O](code, msg)` | finish as failed |

`Done` infers `O` from its argument. The other three take no argument that
mentions `O`, and Go does not infer a type argument from the return context, so
they need it written out: `agentkit.Continue[MyOutput]()`.

`Suspend` that leaves no open await is rejected (`ErrSuspendWithoutAwait`) —
that combination would park the process with nothing able to wake it.

## Syscalls

The single gateway from strategy to world. Because everything goes through it,
metering and limiting have no path around them.

| Group | Calls |
|---|---|
| context | `ProcessID`, `RootID`, `ParentID`, `Agent`, `Now` |
| LLM | `Tools`, `Generate` |
| tools | `CallTool` |
| children | `Agent[I].SpawnChild(ctx, sys, input)` |
| waits | `Await(key)` — reading only; declaring is the `Decision`'s job |
| observation | `Emit`, `Metrics` |

`Generate` returns a `GenerateResult` rather than a raw gollem response, because
it carries the conversation `History` in a form you can fold into your
checkpointed state.

`Now()` is the kernel's clock (injectable for tests via `WithClock`). It is
**not** deterministic across a replay.

## Await

A durable wait, persisted as a row. Three kinds only:

| Kind | Declared with | Resolved by |
|---|---|---|
| question | `Question(key, payload, ...)` | `Kernel.Respond`, or its deadline expiring |
| timer | `Timer(key, until)` | the deadline passing |
| children | `WaitChildren(key, ids...)` | all named children reaching a terminal state |

Human confirmation is **not** a fourth kind — it is a yes/no question that your
strategy chooses to ask. There is no approval gate in the kernel
([ADR-0008](adr/0008-three-await-kinds-confirmation-is-a-question.md)).

Declaring and reading are separate on purpose. You declare through the
`Decision` (waiting is control flow) and read through `sys.Await(key)` (reading
is a syscall):

```go
// transition N — declare
return st, agentkit.Suspend(agentkit.Question("confirm", []byte("proceed?"))), nil

// transition N+1 — read
aw, ok := sys.Await("confirm")
if ok && aw.Status == agentkit.AwaitResponded {
    approved := string(aw.Response) == "yes"
}
```

A question may carry a `WithDeadline`. Reaching it marks the await `expired`,
not `responded`, so your strategy can tell the difference between "they said no"
and "nobody answered".

## Kernel

The runtime. Two halves that happen to share dependencies:

- **The lifecycle API** — `Agent[I].Spawn`, `Respond`, `Cancel`, `GetProcess`,
  `ListAwaits`, `ListEvents`.
- **The worker loop** — `Serve`.

They are one type because they need identical wiring. A deployment that only
submits work calls the API and never calls `Serve`; a worker deployment calls
`Serve`. That is the whole of the process-separation story.

Built with required dependencies positional and optional ones as functional
options ([ADR-0005](adr/0005-required-positional-optional-functional.md)):

```go
kernel, err := agentkit.New(repo, defaultModel, registry,
    agentkit.WithModelRole(planexec.RolePlanner, strongModel),
    agentkit.WithToolFactory(factory),
    agentkit.WithLimiter(limiter),
    agentkit.WithObserver(observer),
)
```

## ModelRole

A named model slot. `DefineModelRole` returns an opaque value; a nil role means
the default model passed to `New`.

Roles are compared by identity, not by name, and the type is sealed — you cannot
construct one any other way. A misspelled string cannot silently fall back to
the default model, because there are no strings involved. Strategies export
their roles as package variables (`planexec.RolePlanner`,
`planexec.RoleSummarizer`); an unbound role falls back to the default model.

## Repository

The persistence contract you implement. The application never calls it directly
— reads go through the kernel.

Its write path is one method, `Apply(ctx, ChangeSet)`, applying a declarative
bundle atomically with `Rev`-based compare-and-set. No transaction mechanism
appears in the interface, so it can be implemented on an RDB, a document store,
or a conditional-write KV store ([ADR-0004](adr/0004-repository-changeset-rev-cas.md)).

See [persistence.md](persistence.md).

## Metrics, Limiter, Event, Observer

- **`Metrics`** — counters the kernel maintains: input/output tokens, LLM calls,
  tool calls, steps, spawns.
- **`Limiter`** — your closure deciding whether to continue, called before every
  effect and at every transition boundary. The kernel measures; you decide.
- **`Event`** — an append-only record written durably inside the transition
  commit. Read per process with `ListEvents`; delivering them anywhere is your
  job.
- **`Observer`** — best-effort span hooks for audit and tracing. Not persisted,
  cannot stop execution, and fires on replays too.

See [observability.md](observability.md).
