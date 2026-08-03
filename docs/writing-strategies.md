# Writing a strategy

A strategy is a checkpointable state machine. agentkit calls `Step` once per
transition, persists whatever you return, and calls it again — possibly on a
different machine, possibly after a crash.

## The shape

```go
type myState struct {
    Prompt string          `json:"prompt"`
    Phase  string          `json:"phase"`
    Rounds int             `json:"rounds"`
    Hist   *gollem.History `json:"hist,omitempty"`
}

type myInput struct {
    Prompt string
}

type myOutput struct {
    Answer string `json:"answer"`
}

type myStrategy struct {
    systemPrompt string
}

func (s *myStrategy) Version() int { return 1 }

func (s *myStrategy) Init(in myInput) (myState, error) {
    if in.Prompt == "" {
        return myState{}, goerr.New("prompt is required")
    }
    return myState{Prompt: in.Prompt, Phase: "start"}, nil
}

func (s *myStrategy) Step(ctx context.Context, sys agentkit.Syscalls, st myState) (myState, agentkit.Decision[myOutput], error) {
    // ... one transition ...
}

func (s *myStrategy) EncodeState(st myState) ([]byte, error) {
    return json.Marshal(st)
}

func (s *myStrategy) DecodeState(version int, raw []byte) (myState, error) {
    var st myState
    if err := json.Unmarshal(raw, &st); err != nil {
        return myState{}, goerr.Wrap(err, "decode state")
    }
    return st, nil
}

func (s *myStrategy) EncodeOutput(out myOutput) ([]byte, error) {
    return json.Marshal(out)
}
```

Register it, and keep the handle. The three type parameters are inferred from
the strategy:

```go
agent, err := agentkit.Register(reg, "my-agent", 1, &myStrategy{})
pid, err := agent.Spawn(ctx, kernel, myInput{Prompt: "..."})
```

To react when a run finishes, wire a handler at registration. It receives the
value you passed to `Done`, and fires for failures and cancellations too —
delivery is best-effort, so see
[ADR-0014](adr/0014-completion-handlers-are-best-effort.md) before relying on it:

```go
agent, err := agentkit.Register(reg, "my-agent", 1, &myStrategy{},
    agentkit.WithOnFinish(func(ctx context.Context, pid agentkit.ProcessID,
        res agentkit.FinishResult[myOutput]) error {
        if res.Status != agentkit.ProcessSucceeded {
            return nil
        }
        return notify(ctx, res.Output.Answer)
    }),
)
```

## Five rules

### 1. `Step` runs from the top, every time

After a crash, another worker decodes the last committed state and calls `Step`
again. Everything not in your state is gone.

So: anything that must not happen twice belongs in the state.

```go
// Wrong — a crash after the effect replays it with no record.
res, _ := sys.CallTool(ctx, call)
st.Result = res
return st, agentkit.Continue(), nil

// Right — commit the intent, act in the next transition.
if st.PendingCall == nil {
    st.PendingCall = &call
    return st, agentkit.Continue(), nil
}
res, err := sys.CallTool(ctx, *st.PendingCall)
```

See [execution-model.md](execution-model.md) for when this matters and when
plain tool idempotency suffices.

### 2. Never block

Waiting means suspending on an await. A `time.Sleep` or a blocking channel
receive holds the claim and the lease and stops anyone else from making progress
on the process.

```go
// Wrong
time.Sleep(time.Hour)

// Right
return st, agentkit.Suspend(agentkit.Timer("wake", sys.Now().Add(time.Hour))), nil
```

### 3. One `Generate` per transition

A crash then costs at most one LLM round. Both bundled strategies follow this;
use a phase field to enforce it:

```go
switch st.Phase {
case "plan":     return s.plan(ctx, sys, st)      // one Generate
case "collect":  return s.collect(sys, st)        // zero
case "finalize": return s.finalize(ctx, sys, st)  // one
}
```

### 4. `Init` is pure

No context, no syscalls — the signature gives you, the strategy author, no
path to an effect. (Whoever configures the `Kernel` can still wrap `Init` with
an `InitMiddleware`, which does receive a `ctx` — see
[observability.md](observability.md) — but that is a decision made outside
your strategy.) `Init` runs synchronously inside `Spawn`, so validate the input
here and return an error; the caller gets it directly instead of discovering a
`failed` process later.

### 5. `DecodeState` owns migration

It receives the version that wrote the bytes. Old checkpoints are real: a
process spawned before a deploy still has to run after it.

```go
func (s *myStrategy) DecodeState(version int, raw []byte) (myState, error) {
    switch version {
    case 1:
        var old v1State
        if err := json.Unmarshal(raw, &old); err != nil {
            return myState{}, goerr.Wrap(err, "decode v1")
        }
        return migrateV1(old), nil
    case 2:
        var st myState
        ...
    default:
        return myState{}, goerr.New("unknown state version", goerr.V("version", version))
    }
}
```

Bump `Version()` when the encoding changes, and keep reading the old ones until
no process can still hold them.

## Calling the LLM

```go
res, err := sys.Generate(ctx, []gollem.Input{gollem.Text(st.Prompt)},
    agentkit.WithHistory(st.Hist),
    agentkit.WithTools(sys.Tools()...),
    agentkit.WithSystemPrompt(s.systemPrompt),
    agentkit.WithRole(RolePlanner),
    agentkit.WithSchema(mySchema),
)
if err != nil {
    return st, agentkit.Decision{}, err
}
st.Hist = res.History          // fold history into state for the next round
```

Keeping `res.History` in your state is what makes the conversation survive a
checkpoint. `WithSchema` requests structured JSON output; `WithLLMOptions` passes
gollem's own generate options (temperature and friends) through.

For settings that belong to the gollem *session* rather than to one generate —
`gollem.WithSessionPromptCache`, the content-block middlewares — use
`WithLLMSessionOptions`. They are appended after the options agentkit derives
from `WithSystemPrompt`, `WithTools` and friends, so a scalar setting passed
here overrides the typed one and a list setting adds to it. That ordering is
what lets a `GenerateMiddleware` turn prompt caching on for every agent from a
single `agentkit.New` registration:

```go
agentkit.WithGenerateMiddleware(func(next agentkit.GenerateHandler) agentkit.GenerateHandler {
    return func(ctx context.Context, req *agentkit.GenerateRequest) (*agentkit.GenerateResult, error) {
        req.LLMSessionOptions = append(req.LLMSessionOptions, gollem.WithSessionPromptCache(true))
        return next(ctx, req)
    }
})
```

## Persisting conversation history

Threading `History` through your own state (above) works, but you write the
carrying, the folding, and the checkpointing by hand. If your strategy just needs
a running conversation, register with `agentkit.WithHistoryStore` and use
`sys.Session()` instead; agentkit carries `History` across calls for you, and
persists it across steps and crashes too.

```go
import histmem "github.com/gollem-dev/agentkit/historystore/memory"

agent, err := agentkit.Register(reg, "chat", 1, &chatStrategy{},
    agentkit.WithHistoryStore[Output](histmem.New()),
)
```

```go
func (s *chatStrategy) Step(ctx context.Context, sys agentkit.Syscalls, st chatState) (chatState, agentkit.Decision[Output], error) {
    res, err := sys.Session().Generate(ctx, []gollem.Input{gollem.Text(st.Prompt)})
    if err != nil {
        return st, agentkit.Decision[Output]{}, err
    }
    st.Turn++
    if st.Turn >= 3 {
        return st, agentkit.Done(Output{Texts: res.Texts}), nil
    }
    return st, agentkit.Continue[Output](), nil
}
```

`Session().Generate` injects the carried `History` and `sys.Tools()` for you — no
`WithHistory`/`WithTools` to pass by hand. `Session().History(ctx)` returns the
current history (loading the committed one on first use) if you need to inspect
it. `historystore/memory.New()` (non-persistent) and
`historystore/filesystem.New(dir)` (single-process, persistent) are the reference
stores; a production one implements `agentkit.HistoryStore` and is checked
against `historystore/historytest.Run`.

Without `WithHistoryStore`, every `Session()` method returns
`ErrHistoryNotConfigured` — the managed conversation is never silently run
without persistence.

### A tool round may span several `Step`s

You do not have to finish a tool round before returning. A `Step` can commit with
the conversation ending on a `tool_use` nobody has answered yet, and a later
`Step` — on another worker, after a crash, or after a human replied — answers it.
That works because a committed History is named by `Process.HistoryRef`, which is
written in the same `Apply` as your state, so the two always roll back together
([ADR-0017](adr/0017-history-is-an-immutable-versioned-store.md)).

`Session().CallTool` is what closes the round: it runs the call like
`sys.CallTool` and appends the result to the conversation, so the next `Generate`
needs no input at all.

```go
func (s *approve) Step(ctx context.Context, sys agentkit.Syscalls, st state) (state, agentkit.Decision[Output], error) {
    // Resumed: a human answered, and the conversation still ends on the call.
    if aw, ok := sys.Await("approve"); ok && aw.Response != nil {
        if string(aw.Response) == "yes" {
            if _, err := sys.Session().CallTool(ctx, *st.Pending); err != nil {
                return st, agentkit.Decision[Output]{}, err
            }
        }
        res, err := sys.Session().Generate(ctx, nil) // the result is already in the History.
        if err != nil {
            return st, agentkit.Decision[Output]{}, err
        }
        return st, agentkit.Done(Output{Texts: res.Texts}), nil
    }

    res, err := sys.Session().Generate(ctx, []gollem.Input{gollem.Text(st.Prompt)})
    if err != nil {
        return st, agentkit.Decision[Output]{}, err
    }
    if len(res.FunctionCalls) > 0 {
        st.Pending = res.FunctionCalls[0]
        return st, agentkit.Suspend[Output](agentkit.Question("approve", []byte(st.Pending.Name))), nil
    }
    return st, agentkit.Done(Output{Texts: res.Texts}), nil
}
```

Use `Session().CallTool` only for a call the **model** asked for. For a call your
strategy makes on its own — fetching something to build a prompt, say — use
`sys.CallTool`: there is no `tool_use` for it, and appending a `tool_result`
without one corrupts the conversation. The kernel cannot tell the two apart, so
this one is on you.

Keeping `History` in your own checkpointed state via raw `sys.Generate` +
`agentkit.WithHistory(...)` is still fully supported, and is what
`strategy/simple` does.

### Continuing a finished Process's conversation

One long conversation does not have to be one Process. Spawn a new one per turn
and pass the previous Process's id:

```go
next, err := chat.Spawn(ctx, kernel, chatInput{Prompt: "and what about Friday?"},
    agentkit.WithInheritedHistory(previous))
```

The new Process starts its first `Session().Generate` from the conversation
`previous` committed — including the results of tools it ran — while its
`Metrics`, its `Limit` budget and its cancellation are its own. That is the point
of doing it this way: one question is one unit of work you can bound and stop
without touching the ones before it.

What crosses is the conversation and nothing else. State does not: the new
Process runs `Init` on the input you give it, like any other.

**Inherit from a Process that is finished.** agentkit pins the version at `Spawn`
and never releases it, but it cannot keep the *issuing* Process from releasing it:
if `previous` is still running, its next commit reports that version as
superseded, and reclaiming it is then the store's decision
([ADR-0017](adr/0017-history-is-an-immutable-versioned-store.md)). Inheriting
from a running Process is allowed — branching off its current conversation is a
reasonable thing to want — but the version can be gone by the time it is read,
which surfaces as a failing first transition, not as an empty conversation.

`Spawn` fails synchronously if `previous` does not exist, or has committed no
conversation yet, or the agent was registered without `WithHistoryStore`. It is
not available on `SpawnChild`: a strategy has no `HistoryRef` to name.

## Running tools

```go
for _, call := range res.FunctionCalls {
    out, err := sys.CallTool(ctx, *call)
    // A tool's own error is returned to you, not fatal to the Process.
    // Feed it back to the model as a FunctionResponse, or handle it.
}
```

Arguments are validated against the tool's spec before it runs. An unknown tool
name is `ErrToolNotFound`.

## Reading the process-scoped map

`sys.Metadata()` returns a copy of what `WithMetadata` set at spawn — the same
map a `ToolFactory` reads. Reach for it when the value is infrastructure-facing
(which workspace, which toolset) and the `ToolFactory` already keys off it;
anything the strategy's own logic turns on belongs in its typed `Input` instead.
**It is data, not a credential** — see
[ADR-0011](adr/0011-kernel-has-no-tenancy.md) and [tools.md](tools.md).

## Waiting for a human

Declare the wait in one transition, read the answer in the next:

```go
if st.Phase == "ask" {
    st.Phase = "answered"
    return st, agentkit.Suspend(
        agentkit.Question("confirm", []byte("Deploy to production? (yes/no)"),
            agentkit.WithDeadline(sys.Now().Add(24*time.Hour))),
    ), nil
}

if st.Phase == "answered" {
    aw, ok := sys.Await("confirm")
    if !ok {
        return st, agentkit.Decision{}, goerr.New("missing await")
    }
    switch aw.Status {
    case agentkit.AwaitResponded:
        if string(aw.Response) != "yes" {
            return st, agentkit.Done([]byte(`{"result":"declined"}`)), nil
        }
    case agentkit.AwaitExpired:
        return st, agentkit.Fail(agentkit.FailureStrategyError, "nobody answered"), nil
    }
}
```

Remember what this is and is not: a confirmation, not enforcement. Real
authorization lives inside the tool ([tools.md](tools.md)).

## Spawning children

```go
// in the plan phase
var ids []agentkit.ProcessID
for _, task := range tasks {
    id, err := s.taskAgent.SpawnChild(ctx, sys, taskInput{Prompt: task.Prompt})
    if err != nil {
        return st, agentkit.Fail(agentkit.FailureStrategyError, err.Error()), nil
    }
    ids = append(ids, id)
}
st.RoundKey = agentkit.AwaitKey(fmt.Sprintf("tasks:%d", st.Round))
return st, agentkit.Suspend(agentkit.WaitChildren(st.RoundKey, ids...)), nil

// in the collect phase
aw, ok := sys.Await(st.RoundKey)
for _, r := range aw.Results {   // r.Status, r.Output, r.Failure
    ...
}
```

The children are inserted as part of this transition's commit, so a crash before
the commit leaves no orphans. If every child happens to be finished already, the
suspend is elided and the next transition runs straight away — you do not need
to handle that case specially.

`WaitChildren` only accepts your own direct children.

## Emitting events

```go
sys.Emit(ctx, "my.progress", []byte(`{"round":2}`))
```

Events are buffered and written durably with the transition commit, in order.
Delivering them anywhere (Slack, a queue) is your application's job — read them
back with `kernel.ListEvents`.

## Testing

`BindStrategy` is exported so you can drive a fake strategy directly, and
gollem's `mock` package (`LLMClientMock`, `SessionMock`, `ToolMock`) covers the
LLM side. `repository/memory` gives you a real `Repository` in-process.

The behaviour worth testing is the machine, not the mechanics:

- `Init` rejects bad input.
- Each phase returns the `Decision` it should.
- **`Step` is safe to re-run** — call it twice from the same committed state and
  check nothing doubles.
- `DecodeState` reads every version you still claim to support.
