# Getting started

## Requirements

- Go 1.26 or later
- A [gollem](https://github.com/gollem-dev/gollem) LLM client

```bash
go get github.com/gollem-dev/agentkit
```

## The smallest working program

```go
package main

import (
	"context"
	"log"

	"github.com/gollem-dev/agentkit"
	"github.com/gollem-dev/agentkit/repository/memory"
	"github.com/gollem-dev/agentkit/strategy/simple"
	"github.com/gollem-dev/gollem/llm/claude"
)

func main() {
	ctx := context.Background()

	client, err := claude.New(ctx, "...api key...")
	if err != nil {
		log.Fatal(err)
	}

	// 1. Register the agents. Register returns a typed handle.
	reg := agentkit.NewRegistry()
	assistant, err := simple.Register(reg, "assistant", 1)
	if err != nil {
		log.Fatal(err)
	}

	// 2. Build the kernel: repository, default model, registry.
	kernel, err := agentkit.New(memory.New(), client, reg)
	if err != nil {
		log.Fatal(err)
	}

	// 3. Spawn. This writes a pending Process and returns immediately.
	pid, err := assistant.Spawn(ctx, kernel, simple.Input{Prompt: "Summarize the news"})
	if err != nil {
		log.Fatal(err)
	}
	log.Println("spawned", pid)

	// 4. Serve. Run this in as many processes as you like.
	if err := kernel.Serve(ctx); err != nil {
		log.Fatal(err)
	}
}
```

Four steps, always in this order: **register → construct → spawn → serve.**

`Register` must complete before any `Spawn` or `Serve`; afterwards the
`Registry` is read-only.

## Spawning is asynchronous

`Spawn` runs the strategy's `Init` synchronously (so a bad input is an error you
get right there), writes a `pending` process row, and returns its ID. Nothing
executes until a `Serve` worker claims it.

That separation is the point: the process that accepts a request and the process
that runs the agent do not have to be the same one, or even be alive at the same
time.

## Reading results

```go
proc, err := kernel.GetProcess(ctx, pid)
// proc.Status  — pending / running / waiting / succeeded / failed / cancelled
// proc.Output  — the strategy's Done output, when succeeded
// proc.Failure — the code and message, when failed
```

`Output` is whatever the strategy's `EncodeOutput` produced — agentkit never
parses it (see [ADR-0007](adr/0007-kernel-neutral-to-serialization.md)). The
bundled strategies use JSON:

```go
var out simple.Output
if err := json.Unmarshal(proc.Output, &out); err != nil { ... }
```

## Being told when a run finishes

Polling is not the only option. Wire a handler at registration and it runs once
the terminal state is committed, with the output already typed:

```go
agent, err := simple.Register(reg, "assistant", 1,
    simple.WithOnFinish(func(ctx context.Context, pid agentkit.ProcessID,
        res agentkit.FinishResult[simple.Output]) error {
        switch res.Status {
        case agentkit.ProcessSucceeded:
            return post(ctx, res.Output.Texts)
        case agentkit.ProcessFailed:
            return alert(ctx, res.Failure.Code, res.Failure.Message)
        default: // cancelled
            return nil
        }
    }),
)
```

Two things to know before you build on it.

**Delivery is best-effort.** The handler never fires twice, but if the worker
dies between committing the terminal state and calling it, nothing retries and
it never fires at all. If the follow-up work must not be lost, model it as a
parent process waiting on `WaitChildren` instead — every step there is part of a
committed transition. See
[ADR-0014](adr/0014-completion-handlers-are-best-effort.md).

**It runs synchronously, on whichever instance committed the transition.** A
slow handler delays that worker's next claim, so keep it short or hand off. For
a `cancelled` process the committing instance is whoever called `Cancel`, which
is usually your application rather than a worker.

## Answering a question

A strategy that suspends on a question parks the process in `waiting`. Find the
open awaits and respond:

```go
awaits, err := kernel.ListAwaits(ctx, pid)
for _, aw := range awaits {
    if aw.Status == agentkit.AwaitOpen && aw.Kind == agentkit.AwaitQuestion {
        log.Println("asked:", string(aw.Question))
    }
}

err = kernel.Respond(ctx, pid, "confirm", []byte("yes"),
    agentkit.WithRespondedBy("user:alice"))
```

`Respond` flips the process back to `pending` so a worker picks it up. Only an
open await accepts a response — the second responder gets `ErrAwaitClosed`.

## Adding tools

Tools are plain `gollem.Tool`s, built per claim by a `ToolFactory`:

```go
kernel, err := agentkit.New(memory.New(), client, reg,
    agentkit.WithToolFactory(func(ctx context.Context, proc *agentkit.Process) ([]gollem.Tool, error) {
        return []gollem.Tool{searchTool, weatherTool}, nil
    }),
)
```

The factory receives the `*Process`, so it can vary tools by `proc.Agent` or
`proc.Metadata`. Read [tools.md](tools.md) before wiring anything with side
effects — replay makes idempotency your responsibility.

## Choosing storage

| Implementation | Use it for |
|---|---|
| `repository/memory` | tests, development, one-shot runs |
| `repository/filesystem` | a single local process that should survive a restart |
| your own | anything with more than one host |

```go
repo, err := filesystem.New("/var/lib/myagent")
defer repo.Close()
```

`filesystem` holds an exclusive lock on its directory, so only one process may
open it. Neither bundled implementation is meant for multi-host workers — see
[persistence.md](persistence.md) to write your own, and verify it with
`repotest`.

## Running workers

```go
err := kernel.Serve(ctx,
    agentkit.WithWorkerID("worker-1"),
    agentkit.WithConcurrency(4),
    agentkit.WithPollInterval(200*time.Millisecond),
    agentkit.WithLease(30*time.Second),
)
```

`Serve` blocks until the context is cancelled. Run it in as many processes as
you like — claims are exclusive, and a worker that dies has its processes picked
up once the lease expires.

An application that only submits work simply never calls `Serve`.

### Sizing the lease

A lease is **not** a timeout on a transition. Nothing is cancelled when it
expires; it only marks the point from which another worker may assume this one
died. So a lease that is too short does not fail — it lets a second worker start
the same transition again, LLM call and tool calls included.

Size it against **the slowest single transition** — one `Generate` plus that
round's tool calls — not against the whole run. The lease is renewed on every
commit, so a claim running sixteen transitions never needs a lease covering all
sixteen. Nothing extends it while `Step` is executing, so leave margin.

### The two retry bounds

They look similar and mean different things:

| Option | Bounds | Says |
|---|---|---|
| `WithMaxStepAttempts` | attempts that returned an **error** | the strategy keeps failing — read its code |
| `WithMaxUncleanReclaims` | claims that **died mid-transition** | workers keep dying — look at the workers |

They are separate because an error tells you how far the last attempt got, while
a vanished claim tells you nothing: it may have completed every side effect and
died just before committing. If duplicated side effects are unacceptable for an
agent, set `WithMaxUncleanReclaims(0)` — the Process then fails as
`unclean_reclaim` rather than being re-run after a crash.

Details in [process-lifecycle.md](design/process-lifecycle.md) and
[ADR-0015](adr/0015-unclean-reclaims-are-counted-and-bounded.md).

## Next

- [examples/](../examples/) — the same shape as a program you can run, plus one
  each for tools, human confirmation, crash recovery, child processes and
  middleware. They are a separate module: `cd examples && go run ./quickstart`.
- [Execution model](execution-model.md) — **read this before shipping anything
  with side effects.**
- [Concepts](concepts.md) — the vocabulary.
- [Writing a strategy](writing-strategies.md) — when the bundled ones are not
  enough.
