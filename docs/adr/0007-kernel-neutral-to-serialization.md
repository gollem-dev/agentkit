# ADR-0007: The kernel is neutral to serialization

## Summary

agentkit contains no `json.Marshal` or `json.Unmarshal` of caller data. User
payloads cross the API as `[]byte` and are stored verbatim; the kernel's only
check is that they are non-nil. Strategy state is encoded and decoded by the
strategy author via `EncodeState`/`DecodeState`, and a strategy's output is
encoded by `EncodeOutput`, in whatever format they choose. Kernel-owned data
lives in typed row fields, and turning a row into bytes is the `Repository`
implementation's job.

Serialization therefore exists in exactly two places: the caller's own code, and
the `Repository` implementation.

## Context

Every kernel-side marshal is a format decision imposed on the caller and a
failure mode the caller cannot see. An internally marshalled envelope hides
types that break in transit — a field that fails to round-trip is discovered at
runtime, in a worker, after a checkpoint.

Several intermediate designs tried to keep type safety while marshalling
internally: typed helpers over `json.RawMessage`, generic
`DefineQuestion[Q, A]`/`DefineEvent[P]` constructors, reflective type audits at
definition time. All of them left agentkit standing in for `encoding/json`'s
semantics on the caller's behalf.

## Decision

Push all encoding out.

- **Caller data is `[]byte`**: `Spawn` output, `Respond` response, `Done` output,
  `Question` payload, `Emit` payload, `Process.State`, `Process.Output`. Not
  `json.RawMessage` — the type would imply JSON, and the kernel does not assume
  a format. There is no `json.Valid` check, only a nil check.
- **Strategy state is the author's**: `EncodeState(S) ([]byte, error)` and
  `DecodeState(version int, raw []byte) (S, error)`. `DecodeState` receives the
  version that wrote the bytes, so schema migration is ordinary code inside it
  (there is no separate `Migrate` hook). JSON, gob, protobuf — all fine.
- **Kernel-owned data is typed on the row**: `Await.Children`, `Await.Results`,
  `Await.Fired`, `Event.Type`, `Event.Key`, `Process.Metrics`, and so on. The
  `Repository` decides how a row becomes bytes.
- **`Strategy.Init` takes `I`, not `[]byte`.** It runs synchronously inside
  `Spawn`, so no persistence boundary is crossed and a serialization round trip
  there would be pure waste.

The bundled strategies choose JSON, but that is `strategy/simple` and
`strategy/planexec` picking a contract for their own types — not a kernel rule.

## Alternatives rejected

- **Kernel-built envelopes / `json.RawMessage` fixed as the type.** Fixes a
  format while claiming neutrality, and re-imports the type-breakage problem.
- **`DefineQuestion[Q, A]` and friends with internal marshalling.** Nicer at the
  call site, but agentkit ends up owning `encoding/json` semantics for caller
  types, which is the thing being avoided.
- **An `Output` contract on the agent definition, validated by the kernel.**
  Validation requires parsing, which requires knowing the format. Still
  rejected. Note what this does *not* cover: `Strategy.EncodeOutput` turns the
  value passed to `Done` into bytes, exactly as `EncodeState` does for state.
  The kernel calls it and checks the result is non-nil; it never parses the
  bytes and never judges their shape. Adding a kernel-side check of what is
  *inside* those bytes is the thing that stays out.

## Consequences

- Callers write their own `Marshal`/`Unmarshal`. This is deliberate friction: it
  keeps the failure at the caller's own call site where the types are visible.
- `Done(output)` takes the typed output; `EncodeOutput` produces the bytes, and
  a nil result is a transition error. Non-nil is the only thing the kernel can
  meaningfully check. There is no `DecodeOutput`, because nothing reads those
  bytes back as a type: a completion handler is handed the value `Done` received
  (ADR-0014) and a parent treats a child's `Output` as opaque bytes.
- A `Repository` may store `State`/`Output` as `bytea`, base64 in JSON, or a
  blob reference — the kernel never inspects them.
- Cross-version state reads are `DecodeState`'s problem, and version bumps are
  the strategy's `Version()`.
- `GenerateResult` carries `*gollem.History` precisely so the strategy can fold
  conversation state into a form its own `EncodeState` handles (see ADR-0001).

## History

| Date | Change |
|---|---|
| 2026-07-20 | Initial record, extracted from the initial implementation spec (D36, D39, D40, D41, D42). |
| 2026-07-20 | `Done` now takes the typed output and `Strategy.EncodeOutput` produces the bytes (ADR-0014). The decision is unchanged — the kernel still marshals nothing and parses nothing — so the rejected "Output contract validated by the kernel" was clarified to say what it does and does not cover. |
