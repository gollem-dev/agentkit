# agentkit

A general-purpose, crash-resilient runtime for LLM agents in Go.

`agentkit` runs an agent as a **durable, resumable state machine**. A `Process`
is checkpointed after every transition and can be picked up by any worker after
a crash. It is built on [gollem](https://github.com/gollem-dev/gollem) for the
LLM client and tool abstractions, and it stays deliberately small: the kernel is
a state machine, a lease, and a wait queue — nothing more.

## Execution model (read this first)

An LLM is inherently non-deterministic, so `agentkit` does **not** pretend to
give exactly-once execution. Instead it makes two honest guarantees and pushes
the rest to where it belongs:

- **Guaranteed:** a committed transition is never lost, and one transition
  commits atomically (state + declared waits + emitted events + spawned children
  in a single `Apply`).
- **At-least-once, non-deterministic replay:** if a transition crashes before it
  commits, another worker re-runs it from the last committed state. LLM and tool
  calls may run again (an LLM re-charge is accepted), and the re-run may take a
  different path.
- **Your responsibility:** a side-effecting tool must be **idempotent** (derive
  an idempotency key from its own arguments). If you need exactly-once /
  atomic side effects, use a *checkpoint-before-effect* pattern: commit the
  decision (a semantic operation id) to state first, then execute it in the next
  transition. See "Isolated external effects" below.

There is no effect journal, no operation label, and no deterministic clock — a
worker just re-executes `Step` from the checkpoint.

## Quick start

```go
package main

import (
	"context"

	"github.com/gollem-dev/agentkit"
	"github.com/gollem-dev/agentkit/repository/memory"
	"github.com/gollem-dev/agentkit/strategy/simple"
	"github.com/gollem-dev/gollem/llm/claude" // any gollem LLM client
)

func main() {
	ctx := context.Background()
	client, _ := claude.New(ctx, "...api key...")

	reg := agentkit.NewRegistry()
	assistant, _ := simple.Register(reg, "assistant", 1) // -> agentkit.Agent[simple.Input]

	kernel, _ := agentkit.New(memory.New(), client, reg)

	pid, _ := assistant.Spawn(ctx, kernel, simple.Input{Prompt: "Summarize the news"})
	_ = pid

	// A worker loop. Run it in as many processes as you like.
	_ = kernel.Serve(ctx)
}
```

`Spawn` is asynchronous: it writes a pending `Process` and returns immediately;
execution begins when a `Serve` worker claims it.

## Concepts

- **Process** — one execution unit. Its full state (`State` bytes, status,
  metrics, lease, ...) lives in the `Repository`. It moves through
  `pending → running → waiting/succeeded/failed/cancelled`.
- **Strategy[S, I]** — a checkpointable typed state machine you implement.
  `Init(I) (S, error)` builds the initial state purely (inside `Spawn`); `Step`
  runs one transition and returns a `Decision` (`Continue` / `Suspend` /
  `Done` / `Fail`); `EncodeState`/`DecodeState` own serialization (the kernel
  only stores bytes). Register with `Register[S, I]`, which returns a typed
  `Agent[I]` — the only way to spawn, so the input type is compile-time-checked
  and `any` never appears in the public API.
- **Syscalls** — the one path a strategy uses to touch the world: `Generate`
  (LLM), `CallTool`, `SpawnChild` (via the typed handle), `Await` (read a wait),
  `Emit` (event), `Metrics`, `Now`. It meters usage and runs the `Limiter`.
- **Await** — a durable "wait": `Question` (a human answer — confirmation is a
  yes/no question), `Timer`, or `WaitChildren` (child processes). Declared via
  `Suspend(...)`, answered via `Kernel.Respond`.
- **Kernel** — the lifecycle API (`Agent[I].Spawn` / `Respond` / `Cancel` /
  `GetProcess` / `ListAwaits` / `ListEvents`) and the worker loop (`Serve`).

## Human confirmation (not a security gate)

There is no built-in approval gate. To ask a human before a sensitive action, a
strategy suspends on a question and resumes on the answer:

```go
if !st.Confirmed {
	return st, agentkit.Suspend(agentkit.Question("confirm", []byte("run X? (yes/no)"))), nil
}
```

> This is *confirmation*, not enforcement. A buggy or manipulated strategy could
> call `CallTool` directly, so this does **not** guarantee "a human stops every
> action". If you need a hard allow/deny gate, wrap the tools your `ToolFactory`
> returns so an unauthorized call is refused inside `Run`.

## Auditing & tracing (Observer)

`agentkit` does not persist effects, but it exposes observation points for audit
trails and tracing via `WithObserver`. Each hook is a span (start with the
intent, end with the result); it is best-effort (never persisted, never blocks
execution, panics are recovered) and fires on every execution including re-runs:

```go
kernel, _ := agentkit.New(repo, client, reg, agentkit.WithObserver(agentkit.Observer{
	ToolCall: func(ctx context.Context, ec agentkit.EffectContext, call gollem.FunctionCall) func(map[string]any, error) {
		auditStart(ec, call) // record intent
		return func(res map[string]any, err error) { auditEnd(ec, res, err) }
	},
}))
```

For an audit that must be durable *before* the action, wrap the tool instead.

## Isolated external effects

Because replay is non-deterministic, a strategy that performs a side effect in
the same transition as the LLM decision can, after a crash, produce an effect
that is not reflected in the committed state (e.g. the model decided differently
on re-run). Idempotency stops *duplicate* effects, not *divergent* ones. For
exactly-once semantics, commit the intent first and execute it in the next
transition, keyed by that intent.

## Bundled strategies

- **strategy/simple** — LLM loop: generate, run tool calls, feed results back,
  repeat until the model answers. One `Generate` per transition.
- **strategy/planexec** — plan → run tasks as child processes in parallel →
  replan → finalize. Generic over the task agent's input type:
  `Register[T](reg, name, ver, taskAgent, makeInput, ...)`.

## Repository (persistence SPI)

`Repository` is the persistence contract you implement to run agentkit on your
store. The application never calls it directly. It requires no transaction
mechanism — only atomic application of a `ChangeSet` and conditional writes:

1. `Apply(cs)` is all-or-nothing.
2. Each `cs.Processes` row is a `Rev` CAS (stored Rev must equal the row's Rev);
   on success Rev is incremented. This fences stale-worker commits.
3. `cs.Guards` are write-free `Rev` preconditions (used for the WaitChildren
   check-then-act).
4. Uniqueness is maintained: `idempotency_key`, an open Process's `subject`, and
   `(process_id, await_key)`. A violation writes nothing and returns
   `ErrConflict`.
5. `ClaimNextProcess` atomically claims one runnable Process, mints a fresh
   `LeaseToken` on every claim (the fence identity), and never double-claims.
6. `ListEvents` preserves append order.

Two reference implementations are bundled:

- **repository/memory** — in-process, for tests, development, and one-shot runs.
- **repository/filesystem** — a single local process, one atomic `state.json`
  snapshot (single-process only; not for multi-host workers).

Verify your own implementation against the contract with
**repository/repotest**:

```go
func TestMyRepo(t *testing.T) {
	repotest.Run(t, func(t *testing.T) agentkit.Repository { return mystore.New() })
}
```

## Documentation

- [examples/](./examples/) — five runnable programs, one per idea. They are a
  separate module, so the LLM SDK they need stays out of this one's dependency
  graph, and they work offline: `cd examples && go run ./quickstart` needs no
  credentials.
- [docs/](./docs/) — guides: execution model, getting started, concepts,
  writing strategies, tools, persistence, observability.
- [docs/design/](./docs/design/) — architecture, process lifecycle, consistency
  model, responsibility boundaries.
- [docs/adr/](./docs/adr/) — the decisions behind the design, and what was
  rejected.

## Requirements

- Go 1.26+
- `github.com/gollem-dev/gollem`

## License

See [LICENSE](./LICENSE).
