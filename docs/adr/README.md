# Architecture Decision Records

This directory records **why agentkit is shaped the way it is**. Each file
captures one decision that is expensive to reverse: a boundary, a contract, a
guarantee, or a naming convention that propagates through the public API.

Anyone designing a change to agentkit — human or agent — reads this directory
first. A design that contradicts an ADR is not automatically wrong, but it is
never silently allowed: **update the ADR in the same change**, or don't make the
change.

## Index

| ADR | Title |
|---|---|
| [0001](0001-depend-on-gollem-directly.md) | Depend on gollem directly |
| [0002](0002-flat-root-package.md) | Flat root package plus subpackages |
| [0003](0003-at-least-once-replay-no-effect-journal.md) | At-least-once replay, no effect journal |
| [0004](0004-repository-changeset-rev-cas.md) | Repository SPI: ChangeSet + Rev CAS |
| [0005](0005-required-positional-optional-functional.md) | Required arguments positional, optional as functional options |
| [0006](0006-typed-handles-from-register.md) | Define/Register returns a typed handle |
| [0007](0007-kernel-neutral-to-serialization.md) | The kernel is neutral to serialization |
| [0008](0008-three-await-kinds-confirmation-is-a-question.md) | Three await kinds; confirmation is a question |
| [0009](0009-child-processes-committed-atomically.md) | Child processes commit atomically with the transition |
| [0010](0010-limiter-is-one-function.md) | Execution limits are one Limiter function |
| [0011](0011-kernel-has-no-tenancy.md) | The kernel has no tenancy concept |
| [0012](0012-kernel-hooks-are-composable-middleware.md) | Kernel hooks are composable middleware |
| [0013](0013-os-metaphor-naming.md) | OS-metaphor naming |

## How to write one

### File name

`NNNN-kebab-case-title.md`, where `NNNN` is the next unused zero-padded number.
Numbers are never reused, even if an ADR is later superseded.

### Structure

Every ADR uses exactly these sections, in this order:

```markdown
# ADR-NNNN: <imperative title, the decision itself>

## Summary

<The current decision, stated as fact, in 3-8 lines. Self-contained: a reader
who stops here must come away with the correct mental model. If the decision
was later revised, this section describes the CURRENT state, never the history.>

## Context

<What forced a decision. Constraints, the property that had to hold.>

## Decision

<What was chosen, in enough detail to be checkable against the code.>

## Alternatives rejected

<Each rejected option with the reason it lost. This is the section that stops
the same debate from being reopened.>

## Consequences

<What this costs, what it now forbids, what responsibility it pushes onto the
caller.>

## History

| Date | Change |
|---|---|
| YYYY-MM-DD | Initial record. |
```

### Rules

- **The summary is the contract.** Write it so the rest of the file is optional.
  Someone scanning ten ADRs before a design session reads ten summaries and
  nothing else — that has to be enough to keep them out of trouble.
- **State the decision, not its status.** There is no `Status:` field. An ADR
  present in this directory is in force. A decision that no longer holds is
  rewritten in place, with the change noted in `History`.
- **Rewrite in place; do not append.** When a decision changes, edit `Summary`,
  `Decision`, and `Alternatives rejected` so they describe today, then add a
  `History` row saying what changed and why. A reader must never have to
  reconstruct the current state by replaying a diff log.
- **Keep the abandoned path visible.** If a mechanism was built and then removed
  (for example the effect journal in ADR-0003), say so in `Context` or
  `Alternatives rejected`. The reason it was removed is the most valuable thing
  in the file — without it, someone reinvents it.
- **Cite the code, not the intention.** Claims must be checkable: name the type,
  function, or file. If an ADR and the code disagree, one of them is a bug.
- **One decision per file.** If the summary needs the word "also", split it.
- **Do not restate the code.** An ADR explains why a boundary exists, not what
  the API is. API shape belongs in `docs/`, structural overviews in
  `docs/design/`.
