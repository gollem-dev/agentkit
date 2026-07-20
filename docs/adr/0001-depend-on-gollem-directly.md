# ADR-0001: Depend on gollem directly

## Summary

agentkit takes a module dependency on `github.com/gollem-dev/gollem` and uses
`gollem.LLMClient`, `gollem.Session`, `gollem.Tool`, `gollem.History`,
`gollem.Input`, `gollem.Parameter` and `gollem.FunctionCall` as its own types.
There is no agentkit-owned LLM port, no tool wrapper, and no adapter layer.
A caller passes any gollem LLM client to `agentkit.New` and any `gollem.Tool`
through a `ToolFactory`; both flow to the strategy unchanged.

## Context

agentkit needs an LLM client abstraction and a tool abstraction. gollem already
defines both, and every provider implementation (Anthropic, OpenAI, Gemini)
targets gollem's named types.

Go uses nominal typing. A structurally identical interface declared in agentkit
would be a *different* type: a `gollem.Session` returned by a provider could not
satisfy an `agentkit.Session`, because the method signatures mention
`gollem.Input` and `gollem.Response`, not agentkit's copies. "Compatible by
shape" does not exist here.

## Decision

Depend on gollem and use its types verbatim across the public API. The two
places agentkit does define its own type are deliberate exceptions:

- `GenerateResult` (`syscalls.go`) instead of `*gollem.Response`, because
  `Response.Error` is an `error` and therefore not round-trippable through a
  checkpoint. `GenerateResult` carries the `*gollem.History` so a strategy can
  fold conversation state into its own checkpointed state.
- `agentkit.GenerateOption` instead of `gollem.GenerateOption`, because agentkit
  options configure session construction as well as generation. gollem's own
  options pass through via `WithLLMOptions`.

## Alternatives rejected

- **Define agentkit-owned ports of the same shape, plus adapters.** Impossible
  without an adapter for every provider, and the adapter set is exactly the
  maintenance burden that a port was supposed to avoid. Nominal typing makes the
  "just match the signature" plan unimplementable.
- **Vendor provider clients into agentkit.** Duplicates gollem's work and forces
  agentkit to track every provider API change.

## Consequences

- agentkit's minimum Go version follows gollem's (`go 1.26.0`).
- gollem's breaking changes are agentkit's breaking changes. This is accepted:
  both live under the `gollem-dev` organization and move together.
- Tests use gollem's own mocks (`gollem/mock`: `LLMClientMock`, `SessionMock`,
  `ToolMock`) rather than agentkit-local fakes.
- `*gollem.History` is the conversation type everywhere. gollem validates its
  version on unmarshal, so a format mismatch fails a transition deterministically
  rather than corrupting state. Passing a `History` produced by one provider to a
  different provider's session is **not** verified — treat mid-run provider
  swapping as unsupported until tested.

## History

| Date | Change |
|---|---|
| 2026-07-20 | Initial record, extracted from the initial implementation spec (D1). |
