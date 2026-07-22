# ADR-0013: OS-metaphor naming

## Summary

agentkit's core vocabulary is borrowed from operating systems, consistently:

| Concept | Name | OS analogue |
|---|---|---|
| execution unit | `Process` | a process |
| the driving core | `Kernel` | the kernel |
| the effect gateway | `Syscalls` (parameter name `sys`) | system calls |
| launching | `Spawn` / `SpawnChild` | `fork` |
| dispatching a ready process | `dispatch` / `dispatcher` | the scheduler's dispatcher (ready → CPU) |
| identity / clock | `ProcessID()` / `Now()` | `getpid` / clock |
| a registered program | `AgentName` + strategy | a program on disk |
| launch input | `Input` | `argv` |

The metaphor is not decoration. It states the design: a `Strategy` is a user
program that cannot touch the outside world except through system calls into the
kernel.

## Context

The design proposal used `Run`, `Engine`, `Runtime`, `Meter`, `Store`, `Tx`,
`AgentKind`, `ModelSlot` — names drawn from several unrelated metaphors. Each was
locally defensible and collectively they gave no model of how the pieces relate.
`Runtime` in particular is a word that means everything and therefore nothing.

## Decision

Adopt the OS metaphor throughout, and let it decide naming questions:

- `Run` → **`Process`**, with `ProcessID`, `ProcessStatus`, `process.*` events.
- `Engine` (+ a separate `Worker` type) → **`Kernel`**, one type with a `Serve`
  method. The two shared every dependency; splitting them only duplicated
  wiring. Process separation is expressed by whether a deployment calls `Serve`,
  not by which type it constructs.
- `Runtime` → **`Syscalls`**, the sole path from strategy to world.
- `Meter` → **`Metric`** (key) and **`Metrics`** (set).
- `Store` + `Tx` → **`Repository`** + `ChangeSet` + `Apply` (see ADR-0004).
- `AgentKind` → **`AgentName`**. "Kind" implies a category; the value is a unique
  registry name. "ID" would suggest a generated opaque identifier, which is what
  `ProcessID` is.
- `ModelSlot` → **`ModelRole`** (see ADR-0006).
- `Start` → **`Spawn`**. Launching is asynchronous — it writes a pending row and
  returns. "Spawn" describes creation, which is what actually happens; "start"
  would suggest execution has begun. Execution is a separate event: a worker's
  claim, or — on an instance running `Serve` — eager dispatch, which may begin
  driving the Process on a goroutine before `Spawn` even returns
  ([ADR-0016](0016-eager-dispatch-is-a-scheduling-optimization.md)). `Spawn`
  still only creates; it does not itself run the strategy.
- **`dispatch`** names the in-process scheduler step that hands a ready Process to
  a goroutine to run, mirroring an OS dispatcher handing a ready process to a CPU
  (ADR-0016). It is the one scheduler-metaphor name; there is still no scheduler
  *priority*.

## Alternatives rejected

- **Keep the proposal's vocabulary.** Mixed metaphors, and `Runtime` is
  uninformative.
- **Separate `Engine` and `Worker` types.** Duplicated wiring for a distinction
  a method call already makes.

## Consequences

- New API should be checked against the metaphor. If a name has no OS analogue,
  that is a hint the concept may not belong in the kernel — which is the same
  filter ADR-0011 applies from a different direction.
- Renaming later is expensive: `Process` and `AgentName` are persisted wire
  values, and `process.created` / `process.finished` / `await.created` are event
  type strings a caller may already be matching on.
- The metaphor has limits worth stating out loud. There is no scheduler
  priority, no signal delivery, and no memory isolation — `Syscalls` is a
  gateway, not a privilege boundary. A strategy runs in the same address space
  as the kernel and is trusted code (which is exactly why confirmation is not
  enforcement — ADR-0008).

## History

| Date | Change |
|---|---|
| 2026-07-20 | Initial record, extracted from the initial implementation spec (D5, D22, D23, D24, D25, D30, D37). |
| 2026-07-22 | Added `dispatch`/`dispatcher` to the vocabulary and clarified that, with eager dispatch, execution may begin before `Spawn` returns (ADR-0016). |
