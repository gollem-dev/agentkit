---
paths:
  - "**/*.go"
  - "go.mod"
---

# Go conventions in agentkit

These are specific to this repository. General Go rules (goerr wrapping, gt for
tests, no `strings.Contains(err.Error(), ...)`) apply as usual.

## Public API

- **`any` must not appear in an exported signature.** Type erasure is confined
  to unexported closures — `BindStrategy` is the sanctioned boundary. If a new
  API needs `any` at the surface, make registration return a typed handle
  instead (the `Register` → `Agent[I]`, `DefineModelRole` → `ModelRole` motif).
- **Required arguments are positional; optional ones are `WithX` options.** A
  new required dependency is a breaking change by construction — that is
  correct, and it should force the question of whether it is really required.
- **Option types are per-call-site** (`KernelOption`, `SpawnOption`,
  `ServeOption`, `GenerateOption`, `RespondOption`, `AwaitOption`) so an option
  cannot be passed where it is meaningless.
- Keep the exported surface minimal. The root package is flat, so everything
  exported there is public API. Unexported is the default; `export_test.go` is
  how tests reach internals.
- Sealed types use an unexported marker method (`ModelRole.modelRole()`), which
  blocks both construction and external implementation. An opaque struct only
  blocks construction and is therefore weaker.

## Serialization

- **No `json.Marshal` / `json.Unmarshal` of caller data anywhere in the root
  package.** User payloads are `[]byte`, stored verbatim; the only check is
  non-nil. Strategy state is the author's via `EncodeState`/`DecodeState`.
- Kernel-owned data lives in typed row fields. Turning a row into bytes is the
  `Repository` implementation's job.
- The bundled strategies choose JSON for their own types. That is their
  contract, not a kernel rule — do not generalize it into the kernel.

## Persistence and concurrency

- **One transition is one `Apply`.** Anything a transition produces — state,
  awaits, events, spawned children, metrics — goes into that single change set.
  If you find yourself adding a second write to close a race window, fold it in
  instead.
- Distinguish the three mechanisms and do not conflate them:
  - `Rev` CAS — *did this row change?*
  - `ChangeSet.Guards` — *did a row I read but did not write change?*
  - `LeaseToken` — *do I still own this claim?* Compared on conflict to decide
    retry-vs-abandon. `LeaseOwner` is diagnostic only and must never be used as
    a fence.
- **A stale worker never rebases its `Rev`.** On conflict: same lease token →
  rebuild against fresh state and retry; different token → abandon.
- **`ErrConflict` must propagate.** The kernel's retry loops depend on seeing
  it. Never swallow it or translate it into another error.
- No cross-request state in process memory, with two explicit exceptions. The
  `Registry` is one: long-lived, read-only after registration. The second is
  `Kernel.dispatcher` (the eager scheduler, ADR-0016): an `atomic.Pointer`
  installed by `Serve` and cleared when it returns. It holds no business state —
  losing it only degrades to polling — so it does not violate the rule's intent
  (durable business state must not live only in memory). Any further mutable
  Kernel state needs the same justification and an ADR.

## Errors

- Sentinel errors are declared in `errors.go` with `goerr.New`, discriminated by
  `errors.Is`, and wrapped with `goerr.Wrap` plus `goerr.V` context at the call
  site.
- A tool's `Run` error is returned to the strategy, not fatal to the process.
- A strategy panic is recovered and converted into a transition error; it must
  never take down a worker.

## Tests

- `package agentkit_test` (black box), with `export_test.go` exposing what the
  tests genuinely need.
- `xyz.go` → `xyz_test.go`. No `_e2e_test.go` / `_integration_test.go` files.
- Use `github.com/m-mizutani/gt` and gollem's `mock` package.
- **A `Repository` implementation must pass `repotest.Run`.** Changing one means
  re-running it; adding one means calling it.
- For anything touching the transition machinery, test the **crash path**, not
  just the happy path: re-run `Step` from the same committed state and assert
  nothing doubles.
- Random ids in repository tests (the suite uses a nanosecond timestamp plus a
  counter) — never hardcoded ids, which collide under parallel runs.

## Verification

`go vet ./...` for compile checks — **never `go build`**. `gofmt -l .` for
formatting. `go test ./...` must be fully green before a task is complete.
