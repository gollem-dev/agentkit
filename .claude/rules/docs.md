---
paths:
  - "docs/**"
  - "README.md"
  - "CLAUDE.md"
---

# Documentation

Three directories with three different jobs. Putting content in the wrong one is
the most common mistake here.

| Directory | Answers | Audience | Changes when |
|---|---|---|---|
| `docs/` | how do I use it | someone building on agentkit | the public API changes |
| `docs/design/` | how do the pieces fit | someone changing agentkit | the structure changes |
| `docs/adr/` | why is it this way | someone questioning a decision | a decision is revisited |

All documentation is written in **English**, matching `README.md`.

## Universal rules

- **Never document what the code already states.** Type definitions, field
  lists, exhaustive option catalogues, function signatures. Duplicated reference
  goes stale silently and then actively misleads. Name the type and let the
  reader read it.
- **Every claim must be checkable against the code.** If you cannot point at the
  function that implements it, do not write it. When something is unverified,
  say so explicitly — "not verified" is a legitimate and useful statement.
- **Never overstate a guarantee.** This codebase deliberately declines several
  properties (exactly-once effects, approval enforcement, tenant isolation).
  Documentation that softens a refusal into a "mostly works" is a correctness
  bug, because someone will build on it. When describing a boundary, say who
  owns the responsibility instead.
- **Cross-link instead of repeating.** A concept is explained in one place; every
  other mention links to it. Guides link down to ADRs for rationale; ADRs do not
  explain usage.
- **Update docs in the same change as the code.** Not afterwards.

## `docs/` — SDK documentation

For someone building an application on agentkit.

- Lead with the task, not the type. "Waiting for a human", not "the Await type".
- Show working code. Examples must compile against the real API — check
  signatures before writing them, and prefer copying a shape from the tests.
- State obligations where the reader will hit them, not only in a distant
  reference page. Idempotency belongs in `tools.md` *and* wherever tools are
  first mentioned; "confirmation is not enforcement" belongs everywhere
  confirmation appears.
- Keep the entry path short: `README.md` → `execution-model.md` →
  `getting-started.md` → `concepts.md`. Anything new fits into that sequence or
  hangs off it as a guide.

## `docs/design/` — conceptual design

For someone changing agentkit.

- Only structures that span several files and are invisible from any one of
  them: layering, invariants and their enforcement mechanism, failure windows,
  responsibility boundaries.
- **No duplicate management of anything the code expresses.** The test: *if a
  pure rename would force an edit here, it belongs in the code, not here.*
- Prefer diagrams for state machines and flows (mermaid). A `stateDiagram-v2` or
  `sequenceDiagram` carries more than a paragraph.
- Say what is *not* guaranteed as prominently as what is.

## `docs/adr/` — decision records

The full convention, including the required section structure, is in
[`docs/adr/README.md`](../../docs/adr/README.md). Read it before adding or
editing an ADR. The essentials:

- File name `NNNN-kebab-case-title.md`; numbers are never reused.
- Sections, in order: `Summary`, `Context`, `Decision`, `Alternatives rejected`,
  `Consequences`, `History`.
- **`Summary` is the contract.** Written so the rest of the file is optional,
  and always describing the *current* state — never the history.
- **No `Status` field.** An ADR present in the directory is in force.
- **Rewrite in place, never append.** When a decision changes, edit the body to
  describe today and add a `History` row. A reader must never reconstruct the
  current state by replaying a diff log.
- **Keep abandoned mechanisms visible** in `Context` or `Alternatives rejected`.
  Why something was removed is the most valuable content in the file — without
  it, someone reinvents it.
- One decision per file. If the summary needs "also", split it.
- Add the new ADR to the index table in `docs/adr/README.md`.

## When a design contradicts an ADR

Update the ADR in the same change, with a `History` entry. Do not implement
around it and do not leave the record stale. If the contradiction is not
intentional, the design is the thing that is wrong.
