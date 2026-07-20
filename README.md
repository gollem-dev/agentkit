# agentkit

A general-purpose, crash-resilient runtime for LLM agents in Go.

An agent loop is easy to write and hard to *operate*. The usual one keeps the
whole run — conversation, tool results, "waiting for the user to click approve" —
in the memory of one process, so a deploy, a crash, or a scale-in ends it. There
is nothing to resume, because nothing was ever written down.

`agentkit` runs that same loop as a **durable state machine**. Every transition
is checkpointed into your store before the next one starts, and any worker in
any process can pick a run up from its last checkpoint. It is built on
[gollem](https://github.com/gollem-dev/gollem) for the LLM client and tool
abstractions, and stays deliberately small: the kernel is a state machine, a
lease, and a wait queue — nothing more.

## Features

- **Checkpointed execution** — a run is a `Process` whose state is committed to
  the store after every transition, and resumed from there by any worker.
- **Durable waits** — `Suspend` parks a run on an await: a question for a human,
  a timer, or a set of child processes. Nothing is held open while it waits.
- **Multi-worker execution** — `Serve` claims runs with a lease and commits with
  a `Rev` CAS, so any number of workers on any number of hosts is safe.
- **Child processes** — a strategy spawns children and waits for their results;
  the children and the parent's new state commit in one atomic write.
- **Usage metering and limits** — token and tool usage accumulates on the
  `Process`, and a `Limiter` decides when a run has had enough.
- **Idempotent spawning** — an idempotency key or a `subject` prevents a retried
  request from starting a second run.
- **Middleware** — a `next`-chain around `Init`, `Step`, `Generate`, `CallTool`
  and `SpawnChild`, registered once on the `Kernel`: audit, tracing, redaction,
  retry, tool policy. A middleware can also refuse a call by not calling `next`.
- **Pluggable persistence** — `Repository` is a small SPI you implement over your
  own store; `repository/repotest` is its contract as a runnable test suite.
- **Typed API** — `Register` returns an `Agent[I]`, so inputs are checked at
  compile time and `any` never appears in the public API.
- **Strategies included** — `strategy/simple` (LLM loop) and `strategy/planexec`
  (plan → parallel children → replan → finalize).

### Why not an in-memory loop?

An in-memory loop is fine until a run outlives the process holding it: a deploy,
a ten-minute LLM step, a human who answers tomorrow. Then you need the run
written down, resumable, and safe for several workers to share — which is what
this is. If your agent finishes inside one request and losing it is acceptable,
you do not need any of that.

## Quick start

Agents fit awkwardly into request/response because they are slower than a
request. So keep the HTTP tier stateless: it only *starts* runs and *reads*
them, while workers do the work elsewhere.

```go
package main

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/gollem-dev/agentkit"
	"github.com/gollem-dev/agentkit/repository/memory"
	"github.com/gollem-dev/agentkit/strategy/simple"
)

func newAPI(kernel *agentkit.Kernel, assistant agentkit.Agent[simple.Input]) http.Handler {
	mux := http.NewServeMux()

	// Start a run. Returns in milliseconds: Spawn only writes a pending Process.
	mux.HandleFunc("POST /jobs", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Prompt string `json:"prompt"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		// A retried POST must not start a second run.
		var opts []agentkit.SpawnOption
		if key := r.Header.Get("Idempotency-Key"); key != "" {
			opts = append(opts, agentkit.WithIdempotencyKey(key))
		}

		pid, err := assistant.Spawn(r.Context(), kernel, simple.Input{Prompt: req.Prompt}, opts...)
		if err != nil {
			http.Error(w, "spawn failed", http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]string{"id": string(pid)})
	})

	// Read a run. Any replica can answer this — the state is in the Repository,
	// not in the replica that happened to accept the POST.
	mux.HandleFunc("GET /jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		proc, err := kernel.GetProcess(r.Context(), agentkit.ProcessID(r.PathValue("id")))
		if errors.Is(err, agentkit.ErrProcessNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		} else if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		res := map[string]any{"status": string(proc.Status)}
		if proc.Status == agentkit.ProcessSucceeded {
			var out simple.Output // the strategy owns this format; the kernel stored bytes
			if err := json.Unmarshal(proc.Output, &out); err != nil {
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			res["texts"] = out.Texts
		}
		writeJSON(w, res)
	})

	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
```

Wiring it up — here the API and one worker share a binary, which is fine for
development. In production they are separate deployments pointing at the same
`Repository`; nothing else changes.

```go
func main() {
	ctx := context.Background()
	client, err := claude.New(ctx, "...api key...") // any gollem LLM client
	if err != nil {
		panic(err)
	}

	reg := agentkit.NewRegistry()
	assistant, err := simple.Register(reg, "assistant", 1) // -> agentkit.Agent[simple.Input]
	if err != nil {
		panic(err)
	}

	// memory.New() is for development. Swap in your own Repository (or
	// repository/filesystem for a single local process) and the workers below
	// can live on other hosts.
	kernel, err := agentkit.New(memory.New(), client, reg)
	if err != nil {
		panic(err)
	}

	go func() {
		// The worker loop: claim a runnable Process, run one transition, commit.
		// Run it in as many processes as you like.
		if err := kernel.Serve(ctx, agentkit.WithConcurrency(4)); err != nil {
			panic(err)
		}
	}()

	_ = http.ListenAndServe(":8080", newAPI(kernel, assistant))
}
```

More in [docs/getting-started.md](./docs/getting-started.md).

## How it fits together

The whole runtime is five moving parts, in the order you meet them:

1. **You write a `Strategy[S, I]`** — the agent's logic, cut into transitions.
   `Init` builds the initial state purely; each `Step` makes *one* move and
   returns a `Decision` (`Continue` / `Suspend` / `Done` / `Fail`). Because a
   `Step` is the unit that gets checkpointed, "how much work per `Step`" is the
   main design decision you make. `EncodeState`/`DecodeState` are yours; the
   kernel only stores the resulting bytes and never looks inside them.
2. **`Register` hands back a typed `Agent[I]`** — the only way to spawn that
   agent, so the input type is checked at compile time and `any` never reaches
   the public API.
3. **`Spawn` creates a `Process`** — one run, with its state, status, metrics and
   lease living in the `Repository`. It is asynchronous: the row is written, the
   id comes back, and nothing has executed yet.
4. **`Serve` workers move Processes forward** — claim one, run a `Step`, commit
   state + emitted events + declared waits + spawned children in a single atomic
   write, repeat. A `Step` reaches the outside world only through **`Syscalls`**
   (`Generate`, `CallTool`, `SpawnChild`, `Await`, `Emit`, `Metrics`, `Now`),
   which is where metering and limits are applied.
5. **`Suspend` parks the run on an `Await`** — a question for a human, a timer, or
   a set of child processes. The `Process` leaves memory entirely and comes back
   when the await is satisfied.

Around all of that, **middleware** wraps the five points where the kernel calls
out (`Init`, `Step`, `Generate`, `CallTool`, `SpawnChild`) — that is where a
concern that spans every agent goes.

Concepts in full: [docs/concepts.md](./docs/concepts.md). Writing your own
strategy: [docs/writing-strategies.md](./docs/writing-strategies.md).

## Waiting for a human

This is the case a plain loop handles worst, so it is worth seeing end to end.
The strategy suspends on a question instead of blocking:

```go
if !st.Confirmed {
	return st, agentkit.Suspend(agentkit.Question("confirm", []byte("run X? (yes/no)"))), nil
}
```

The `Process` is now `waiting` and consumes nothing — no goroutine, no
connection, no worker. Your application shows the pending question and delivers
the answer whenever it arrives:

```go
awaits, _ := kernel.ListAwaits(ctx, pid)          // what is this run waiting for?
err := kernel.Respond(ctx, pid, "confirm", []byte("yes"), agentkit.WithRespondedBy("alice"))
```

`Respond` commits the answer and makes the `Process` runnable again; the next
worker to claim it re-enters `Step` with `Confirmed` set. The human may take an
hour, and the process that asked may be long gone.

> **This is confirmation, not enforcement.** A strategy that is buggy — or
> steered by a prompt injection — can call `CallTool` without ever asking. If you
> need a hard allow/deny gate, put it *inside the tool*: wrap what your
> `ToolFactory` returns so an unauthorized call is refused in `Run`. The kernel
> deliberately has no authorization concept
> ([ADR-0008](docs/adr/0008-three-await-kinds-confirmation-is-a-question.md)).
> A `ToolCall` middleware is the other chokepoint — see below.

## Middleware

Five points where the kernel calls out are wrapped by a `next`-chain, registered
on the `Kernel`: `Init` and `Step` (the strategy boundary) and `Generate`,
`CallTool` and `SpawnChild` (the effects). One registration covers every agent,
which makes this the place for audit, tracing, redaction, retry and tool policy.

An effect middleware is the outermost layer of its syscall — it wraps the
`Limiter` check, tool resolution and argument validation — so a refused call is
visible to it too, and returning without calling `next` stops the call before
any of them:

```go
kernel, _ := agentkit.New(repo, client, reg,
	agentkit.WithToolCallMiddleware(func(next agentkit.ToolCallHandler) agentkit.ToolCallHandler {
		return func(ctx context.Context, req *agentkit.ToolCallRequest) (map[string]any, error) {
			if !allowed[req.Call.Name] {
				audit(req.Effect, req.Call, "denied")
				return nil, errDenied // next is never called: the tool does not run.
			}
			out, err := next(ctx, req)
			audit(req.Effect, req.Call, err) // ErrLimitExceeded reaches here too.
			return out, err
		}
	}),
)
```

Effects run at least once, so a middleware fires on every execution including
re-runs. Nothing it records is persisted by the framework; for an audit that
must be durable *before* the action, record it inside the tool's `Run`.

> Refusing a call is not an authorization gate. A middleware is a real
> chokepoint for calls made through `Syscalls.CallTool`, but a strategy holding
> a `gollem.Tool` value can call `Run` on it directly. Enforcement belongs
> inside `Run`.

`SpawnChild` is buffered into the transition commit, so its middleware gets
`req.OnCommit(fn)` to learn whether the child was actually persisted.

**A kernel middleware runs across all agents, so it does not know any
strategy's input or state type.** The type-erased payloads are read with
`InitInput[I]` / `StepState[S]` / `SpawnInput[I]` and replaced by deriving a new
request (`NewInitRequest` and friends); `ok == false` just means "another
agent's Process — pass it through". Passing `any` as the type argument always
succeeds and is the intended form for generic logging. Nothing here is checked
by the compiler: a wrong type surfaces as `ErrInvalidRequest` at run time. That
is the nature of a cross-cutting layer, and it is why typed manipulation of a
payload is better placed in the strategy's own `Init`.

See [ADR-0012](docs/adr/0012-kernel-hooks-are-composable-middleware.md) for why
this replaced the observation-only `Observer` hooks, and
[docs/observability.md](./docs/observability.md) for the audit and tracing
recipes.

## Bundled strategies

- **strategy/simple** — the LLM loop: generate, run tool calls, feed results
  back, repeat until the model answers. One `Generate` per transition.
- **strategy/planexec** — plan → run tasks as child processes in parallel →
  replan → finalize. Generic over the task agent's input type:
  `Register[T](reg, name, ver, taskAgent, makeInput, ...)`.

Details in [docs/bundled-strategies.md](./docs/bundled-strategies.md).

## Persistence (the `Repository` SPI)

`Repository` is the contract you implement to run agentkit on your store; the
application never calls it directly. It needs no transaction mechanism — only
atomic application of a `ChangeSet` and conditional writes:

1. `Apply(cs)` is all-or-nothing.
2. Each `cs.Processes` row is a `Rev` CAS (stored Rev must equal the row's Rev);
   on success Rev is incremented. This fences stale-worker commits.
3. `cs.Guards` are write-free `Rev` preconditions (used for the children
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

Verify your own against the contract with **repository/repotest**:

```go
func TestMyRepo(t *testing.T) {
	repotest.Run(t, func(t *testing.T) agentkit.Repository { return mystore.New() })
}
```

More in [docs/persistence.md](./docs/persistence.md).

## What is *not* guaranteed

Durability is not exactly-once. An LLM is non-deterministic, so `agentkit`
refuses to pretend replay is:

- **Guaranteed:** a committed transition is never lost, and one transition
  commits atomically.
- **At-least-once, non-deterministic replay:** a transition that crashes before
  committing is re-run from the last committed state. LLM and tool calls may run
  again (an LLM re-charge is accepted), and the re-run may take a different path.
- **Your responsibility:** a side-effecting tool must be **idempotent**. For
  exactly-once effects, commit the decision to state first and execute it in the
  next transition.

There is no effect journal, no operation label, and no deterministic clock — a
worker just re-executes `Step` from the checkpoint. Read
[docs/execution-model.md](./docs/execution-model.md) before you write a tool that
touches the outside world; it is short and it is the part people get wrong.

## Documentation

- [examples/](./examples/) — six runnable programs, one per idea. They are a
  separate module, so the LLM SDK they need stays out of this one's dependency
  graph, and they work offline: `cd examples && go run ./quickstart` needs no
  credentials.
- [docs/](./docs/) — guides: execution model, getting started, concepts, writing
  strategies, tools, persistence, observability.
- [docs/design/](./docs/design/) — architecture, process lifecycle, consistency
  model, responsibility boundaries.
- [docs/adr/](./docs/adr/) — the decisions behind the design, and what was
  rejected.

## Requirements

- Go 1.26+
- `github.com/gollem-dev/gollem`

## License

See [LICENSE](./LICENSE).
