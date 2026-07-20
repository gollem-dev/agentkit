# ADR-0002: Flat root package plus subpackages

## Summary

The kernel and every port it defines live in one flat root package, `agentkit`.
Subpackages exist only for things a caller opts into: bundled strategies
(`strategy/simple`, `strategy/planexec`), reference persistence
(`repository/memory`, `repository/filesystem`), and the persistence conformance
suite (`repository/repotest`). Shared implementation between the two reference
repositories lives in `repository/internal/store`.

## Context

The obvious layered layout — `kernel/`, `llm/`, `tool/` — puts the state machine
in one package and the ports it uses in others. But the ports reference kernel
types (`Process`, `Metrics`, `Syscalls`) and the kernel references the ports, so
`kernel → tool → kernel` is an import cycle. Breaking it needs a fourth "types"
package that every layer imports, which is layering in name only.

## Decision

One package, `agentkit`, holds the kernel (`Kernel`, worker loop, transition
commit) and every extension point (`Repository`, `Strategy`, `ToolFactory`,
`Limiter`, and the middleware chains of ADR-0012). A caller writes one import to
use the runtime.

`repository/internal/store` is the one `internal/` package in the tree. It
exists because the boundary genuinely spans two sibling packages (`memory` and
`filesystem` must satisfy an identical contract); unexported helpers cannot
cross that boundary.

## Alternatives rejected

- **`kernel/` + `llm/` + `tool/` split.** Creates the import cycle above, and
  the cycle-breaking types package makes the split cosmetic.
- **A `types/` or `model/` package holding shared structs.** Callers would then
  import two packages to write one strategy, and the "which package owns this
  type" question recurs on every addition.

## Consequences

- Adding a type to the runtime means adding a file to the root package, not
  deciding a layer. File names carry the structure instead: `process.go`,
  `await.go`, `decision.go`, `syscalls.go`, `kernel.go`, `worker.go`.
- The root package is large by file count. That is accepted; gollem itself is
  laid out the same way, and the alternative is worse.
- Everything exported from the root package is public API. Keep the surface
  small deliberately — an unexported helper is the default, `export_test.go` is
  how tests reach internals.

## History

| Date | Change |
|---|---|
| 2026-07-20 | Initial record, extracted from the initial implementation spec (D2, D3). |
