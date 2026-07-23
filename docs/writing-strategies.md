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

## Persisting conversation history

Threading `History` through your own state (above) works, but you write the
carrying, the folding, and the checkpointing by hand. If your strategy just
needs a running conversation — no need to split a tool round across steps —
register with `agentkit.WithHistoryRepository` and call `sys.Session()`
instead; agentkit carries `History` across `Generate` calls for you, and
persists it across steps and crashes too.

```go
import histmem "github.com/gollem-dev/agentkit/historystore/memory"

agent, err := agentkit.Register(reg, "chat", 1, &chatStrategy{},
    agentkit.WithHistoryRepository[Output](histmem.New()),
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

`Session.Generate` injects the carried `History` and `sys.Tools()` for you —
no `WithHistory`/`WithTools` to pass by hand. `Session.History(ctx)` returns the
current history (loading the stored one on first use) if you need to inspect it. `historystore/memory.New()`
(non-persistent) and `historystore/filesystem.New(dir)` (single-process,
persistent) are the reference stores.

Two obligations come with this, and you hit both immediately:

- **Keep a tool round inside one `Step`.** History persisted through
  `sys.Session()` is saved outside the atomic transition commit
  ([ADR-0017](adr/0017-history-is-a-decoupled-best-effort-store.md)). If a
  `tool_use` is committed in one `Step` and its `tool_result` only in a later
  one, a crash in between leaves the stored History with a dangling
  `tool_use`. Run the call and feed its result back to the model within the
  same `Step` whenever you use `sys.Session()`.
- **The option is opt-in, and persistence is best-effort.** Without
  `WithHistoryRepository`, `sys.Session()` still works, but its `History` lives
  only for the duration of one claim — it does not survive a suspend or a
  crash. If your strategy needs to split a tool round across steps, as
  `strategy/simple` does, keep `History` in your own checkpointed state via the
  raw `sys.Generate` + `agentkit.WithHistory(...)` pattern shown above instead
  of `sys.Session()`.

See [ADR-0017](adr/0017-history-is-a-decoupled-best-effort-store.md) for the
save-before-commit ordering and the duplication window it trades against.

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
