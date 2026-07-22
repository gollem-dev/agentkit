# Examples

Six programs you can run. Each one is a complete `main` package, and each one
is about a single thing agentkit does.

> These live in their own Go module. Running them pulls in a Vertex AI client
> and its transitive dependencies, and none of that belongs in the dependency
> graph of anyone who merely imports agentkit. **Run every command below from
> this directory**, not from the repository root.

| Example | What it shows |
|---|---|
| [`quickstart`](quickstart) | register → construct → spawn → serve, and nothing else |
| [`tools`](tools) | supplying tools, and the idempotency and authorization they oblige you to |
| [`human-in-the-loop`](human-in-the-loop) | a strategy that suspends on a question and resumes from the answer |
| [`durable-worker`](durable-worker) | submitting and executing in separate processes, and surviving a crash |
| [`fanout`](fanout) | a planner that runs its tasks as parallel child processes |
| [`middleware`](middleware) | wrapping all five kernel hooks with one registration each |

## Running them

```bash
cd examples
go run ./quickstart
```

The module has a `replace` pointing at `../`, so the examples always exercise
the agentkit in this working tree rather than a published version of it.

**They work offline.** With no model configured, the LLM is a stub replaying a
script, so every example completes without network access, without credentials,
and with the same output every time. The stub is what the tests use, and its
output is the one described below.

To run against a real model, point them at Vertex AI:

```bash
gcloud auth application-default login
export GEMINI_PROJECT_ID=your-project
export GEMINI_LOCATION=us-central1
go run ./quickstart
```

Both variables are required, and the caller needs `roles/aiplatform.user` (or
equivalent) on the project. Setting only one is an error rather than a silent
fall back to the stub, because "I thought I was running live" is an expensive
thing to be wrong about.

gollem's Gemini client is Vertex-only and authenticates through Application
Default Credentials; there is no API-key mode. To use a different provider,
replace the `gemini.New` call in [`internal/demo`](internal/demo/demo.go) with
`claude.New` or `openai.New` — nothing else in the examples depends on it.

With a live model the output stops being deterministic: the model may call a
tool the script did not anticipate, or word an answer differently. The
descriptions below are of the offline run.

## quickstart

The four steps, always in this order: register, construct, spawn, serve.

Worth noticing that `Spawn` returns before anything executes. It writes a
pending `Process` and stops there; work begins when a `Serve` worker claims it —
with `Serve` already running, that is usually eager and immediate rather than
waiting for the next poll. The example runs the worker in a goroutine only so
that one program can show both halves.

## tools

The model is told to ship a release. It tries production first, the policy
refuses it inside `Run`, the error goes back to the model as a function
response, and it retries against staging.

Two obligations are on display:

- **Idempotency.** `deployTool` derives a key from the meaning of its
  arguments, because a transition can run more than once and agentkit does not
  hand out a key that would survive a non-deterministic replay.
- **Authorization.** `guardedTool` refuses before the inner tool can act.
  `Run` is the only path to the effect, so it is the only place a refusal
  cannot be routed around. A `ToolCallMiddleware` can refuse a call as well,
  and one registration covers every agent — but it is a chokepoint for calls
  made through `Syscalls.CallTool`, not a gate, so enforcement stays in `Run`.

Only half of the first one is shown here, and the example says so: the key is
the part a tool author writes, but the store that key is checked against has to
outlive the worker. This one is a map in memory, so a replay coming from a
different process after a crash would deploy a second time. In a real system
that is the deploy API's own idempotency support, or a unique constraint in a
database — hence the `deployments` interface the tool depends on.

The output reports two tool calls but only one deployment: the kernel meters the
attempt, not the outcome. A tool's error is returned to the strategy, not fatal
to the Process.

See [tools.md](../docs/tools.md).

## human-in-the-loop

A hand-written `Strategy` that suspends on a question, and an operator that
answers it.

```bash
go run ./human-in-the-loop -answer no
```

The Process parks in `waiting` and the question goes into the Repository. The
strategy holds no goroutine, no stack and no live handle while it waits — the
state that resumes it is entirely in the store, which is why the answer can
arrive long after the transition that asked for it.

How long "long" can be is the Repository's business, not the kernel's. This
example uses `memory`, so the question lives exactly as long as the program.
Back it with a shared store and the two halves can be separate programs on
separate hosts; `durable-worker` shows that split.

What approval leads to here is a generated line of text, not a deletion. The
effect belongs in a tool, with the obligations `tools` covers. Refusing skips it
entirely, which the metrics confirm: no LLM call happens at all.

This is a *confirmation, not a security gate*. Nothing prevents a strategy from
calling a tool without asking first. When you need a decision that cannot be
bypassed, put it in the tool, as `tools` does.

See [ADR-0008](../docs/adr/0008-three-await-kinds-confirmation-is-a-question.md).

## durable-worker

The one example with more than one command, because the point is that the
process submitting work and the process doing it are different.

```bash
go run ./durable-worker submit -topic durability -rounds 4
go run ./durable-worker work -pid <id> -crash-after 2   # exits mid-round
go run ./durable-worker status -pid <id>                # partial progress
go run ./durable-worker work -pid <id>                  # resumes, finishes
```

The simulated crash exits after the LLM call but before the transition commits,
so that round runs again on resume — the committed sequence number after the
crash shows one fewer round than the LLM was asked for. That is at-least-once
execution, and it is the reason a side-effecting tool has to be idempotent.

The resume may pause before it picks the work up: a Process left `running` by a
dead worker only becomes claimable once its lease expires. That lease started
when the dead worker claimed it, so what is left to wait out is whatever remains
of it — at most the 5 seconds this demo uses, and nothing at all if you spent
longer than that reading the `status` output. A real deployment sets the lease
well above how long one transition takes.

The store is `repository/filesystem`, which holds an exclusive lock on its
directory, so these commands run one at a time. It is a single-process reference
implementation — real workers on more than one host need a `Repository` of your
own, verified with `repotest`. See [persistence.md](../docs/persistence.md).

## fanout

A planner decomposes the goal, each task runs as its own child `Process`, and a
summarizer folds the results back together.

The children are inserted as part of the parent's transition commit, so a crash
between "decided to fan out" and "children exist" is not possible — either both
happened or neither did.

The planner and the summarizer are bound to named model roles, and the task
workers fall through to the kernel's default model — which is how you point a
stronger model at planning and a cheaper one at the tasks. Offline, giving each
its own client also keeps the run deterministic, since children execute in
parallel and would otherwise race over one script.

Note what the budget in this example does **not** do: a `Limiter` sees one
Process's metrics, so it caps the planner and each child separately, not the
tree as a whole. A tree-wide budget needs your own accounting keyed by
`proc.RootID`.

The comparison is `>=`, because the limiter runs *before* the call it is
deciding about. With `>`, a budget of N would allow N+1 calls — an off-by-one
that only shows up once someone relies on the number.

See [bundled-strategies.md](../docs/bundled-strategies.md) and
[observability.md](../docs/observability.md).

## middleware

A `next`-chain on each of the five points where the kernel calls out: `Init` and
`Step` at the strategy boundary, and `Generate`, `CallTool` and `SpawnChild` for
the effects. The program's output is the trail they wrote.

```
audit trail
  init      coordinator (left alone)
  generate  coordinator role=middleware.coordinator tokens=64/16
  tool      coordinator lookup ok
  tool      coordinator purge DENIED by policy
  step      coordinator seq=1 -> continue (state readable: true)
  init      reporter    prompt prefixed with the house style
  step      coordinator seq=2 -> suspend (state readable: true)
  spawn     reporter    persisted with the transition
  ...
```

Four things that trail is showing:

- **Refusing.** The tool policy returns without calling `next`, so the tool
  never runs. Effect middleware is the *outermost* layer of its syscall — it
  wraps the `Limiter` check, tool resolution and argument validation — so it
  stops a call before any of those, and still sees a call refused deeper in.
- **Rewriting.** The `Init` middleware prefixes a house style onto the prompt.
  It runs for every agent and cannot know any one strategy's input type:
  `InitInput` reporting `ok == false` means "another agent's Process, pass it
  through", which is what happens to the coordinator's own input.
- **Whether a child really exists.** `SpawnChild` only buffers a child into the
  transition's commit, so the returned id is not proof of anything yet.
  `req.OnCommit` reports the transition's outcome — note that the two "persisted"
  lines appear *after* the `seq=2 -> suspend` that committed them.
- **What `Step` middleware can see.** It observes the Step call, not the commit;
  a transition that fails to commit is re-run and traced again. It must call
  `next` at most once, because a Step's buffered effects accumulate per
  transition rather than per call.

Refusing a call is not an authorization gate. This is a real chokepoint for
calls made through `Syscalls.CallTool`, but a strategy holding a `gollem.Tool`
value can call `Run` on it directly — which is why `tools` puts the enforcement
inside `Run`.

See [ADR-0012](../docs/adr/0012-kernel-hooks-are-composable-middleware.md).

## Where to go next

The examples show the API in use; the guides explain the model behind it. Start
with [execution-model.md](../docs/execution-model.md) before shipping anything
that touches the outside world.
