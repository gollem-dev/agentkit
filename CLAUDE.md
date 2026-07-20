# agentkit

A general-purpose, crash-resilient runtime for LLM agents in Go. It runs an
agent as a durable, resumable state machine: a `Process` is checkpointed after
every transition and can be picked up by any worker after a crash. It is built
on [gollem](https://github.com/gollem-dev/gollem) and stays deliberately small —
the kernel is a state machine, a lease, and a wait queue.

## Design workflow — read this before designing anything

**Before proposing or implementing any design, read [`docs/adr/`](docs/adr/) and
[`docs/design/`](docs/design/).** They are not background reading; they are the
constraints.

- Every ADR opens with a `Summary`. Scanning the summaries is usually enough.
- **If a design would contradict an ADR, stop.** Either change the design, or
  update the ADR in the same change with a `History` entry explaining why. A
  silent deviation is not an option — an ADR that no longer matches the code is
  worse than no ADR at all.
- If a change alters how components fit together, update `docs/design/`.
- If a change alters the public API or how it is used, update `docs/`.

Documentation is part of the implementation, not a follow-up.

## The invariants

These recur throughout the codebase. Violating one is almost always a bug, not
a new idea:

1. **No `any` in the public API.** Type erasure is confined to unexported code.
   `Register` returns a typed `Agent[I]`; `DefineModelRole` returns a sealed
   `ModelRole`. → [ADR-0006](docs/adr/0006-typed-handles-from-register.md)
2. **The kernel never marshals or unmarshals caller data.** User payloads are
   `[]byte` stored verbatim. Serialization lives in exactly two places: the
   caller's code and the `Repository` implementation. →
   [ADR-0007](docs/adr/0007-kernel-neutral-to-serialization.md)
3. **Required arguments are positional; optional ones are functional options.**
   No config struct mixes the two. →
   [ADR-0005](docs/adr/0005-required-positional-optional-functional.md)
4. **No effect journal, no deterministic replay.** The only guarantees are "a
   committed transition is never lost" and "one transition commits atomically".
   Do not add machinery that implies more. →
   [ADR-0003](docs/adr/0003-at-least-once-replay-no-effect-journal.md)
5. **One transition is one `Apply`.** State, awaits, events, spawned children and
   metrics commit together. A second write to close a window is a design smell —
   fold it into the change set. →
   [ADR-0009](docs/adr/0009-child-processes-committed-atomically.md)
6. **No vocabulary the kernel does not interpret.** No tenancy, no scope keys, no
   capability lists, no tool-selection language. A type with no specification
   behind it is a footgun. → [ADR-0011](docs/adr/0011-kernel-has-no-tenancy.md)
7. **The kernel does not enforce authorization.** Confirmation is a question a
   strategy asks; enforcement belongs inside tools. Never describe it as a
   security gate. →
   [ADR-0008](docs/adr/0008-three-await-kinds-confirmation-is-a-question.md)
8. **OS-metaphor naming.** `Process`, `Kernel`, `Syscalls`, `Spawn`, `Agent`. A
   new name with no OS analogue is a hint the concept may not belong here. →
   [ADR-0013](docs/adr/0013-os-metaphor-naming.md)

## Layout

```
agentkit/                  root package: kernel + every port definition
  process.go await.go decision.go event.go metric.go id.go   the model
  kernel.go                lifecycle API (Spawn/Respond/Cancel/Get/List)
  worker.go                Serve, claims, transitions, commits
  syscalls.go              the effect gateway
  strategy.go repository.go tool.go middleware.go             the ports
  strategy/simple/         LLM loop
  strategy/planexec/       plan -> children -> replan -> finalize
  repository/memory/       reference impl (in-process)
  repository/filesystem/   reference impl (single process, one snapshot file)
  repository/repotest/     the Repository contract, executable
  repository/internal/store/  shared state machine behind the two references

examples/                  a SEPARATE module (its own go.mod, replace ../)
  internal/demo/           live-or-stub model, and a Process poll helper
  quickstart/ tools/ human-in-the-loop/ durable-worker/ fanout/ middleware/
```

`examples/` is a second module on purpose: it needs an LLM provider's SDK, and
that dependency must not reach anyone who imports agentkit. The cost is that
`./...` from the root does not cover it — CI runs test, lint and gosec inside
`examples/` separately, and those steps are the only thing keeping the examples
compiling against the current API.

The root package is flat on purpose — a layered split creates an import cycle
([ADR-0002](docs/adr/0002-flat-root-package.md)). File names carry the structure.

## Commands

```bash
go vet ./...          # compile check — never use `go build` here
gofmt -l .            # formatting
go test ./...         # all tests must pass
```

## Testing

- Test files are `package agentkit_test` (black box). Internals needed by tests
  are exposed through `export_test.go`.
- `xyz.go` → `xyz_test.go`. No `_e2e_test.go` or `_integration_test.go` files.
- Use `github.com/m-mizutani/gt` for assertions and gollem's own `mock` package
  (`LLMClientMock`, `SessionMock`, `ToolMock`) for the LLM side.
- **Any change to a `Repository` implementation must keep `repotest.Run`
  passing**, and any new implementation must call it.
- The behaviour that matters most for a strategy is **re-runnability**: running
  `Step` twice from the same committed state must not double anything.

## Rules

Detailed conventions live in [`.claude/rules/`](.claude/rules/) and load when the
matching files are touched:

- [`docs.md`](.claude/rules/docs.md) — writing `docs/`, `docs/adr/`, `docs/design/`
- [`go.md`](.claude/rules/go.md) — Go conventions specific to this repository
