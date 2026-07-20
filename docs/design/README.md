# Design notes

Conceptual design for agentkit: the structures and invariants that sit *above*
the code and are not visible from any single file.

Read this directory before designing a change, together with [`../adr/`](../adr/).
The split between them:

| Directory | Answers | Changes when |
|---|---|---|
| [`../adr/`](../adr/) | *Why* is it this way, and what was rejected | a decision is revisited |
| `design/` | *How do the pieces fit together* | the structure changes |
| [`../`](../) | *How do I use it* | the public API changes |

## Contents

- [architecture.md](architecture.md) — layers, the four extension points, and how
  one transition flows through them.
- [process-lifecycle.md](process-lifecycle.md) — the process state machine, the
  anatomy of a transition, and what a claim does.
- [consistency-model.md](consistency-model.md) — what is guaranteed under crashes
  and concurrent workers, and by which mechanism.
- [responsibility-boundaries.md](responsibility-boundaries.md) — who is
  accountable for what, especially for correctness properties the framework
  explicitly does *not* provide.

## What belongs here

- Structures spanning several files or packages, invisible from any one of them.
- Invariants and their enforcement mechanism.
- Responsibility boundaries, particularly the ones the framework declines.
- Diagrams of flow and state.

## What does not

- **Anything the code already states.** Type definitions, field lists, function
  signatures, option catalogues. Duplicated API reference goes stale silently and
  then actively misleads. Name a type and let the reader read it.
- Usage instructions and examples — those are [`docs/`](../).
- Decision rationale and rejected alternatives — those are [`docs/adr/`](../adr/).

The test: **if the sentence would need editing after a pure rename, it probably
belongs in the code, not here.**
