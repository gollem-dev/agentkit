# ADR-0020: A cancel waits for the conversation to close

## Summary

When a worker observes `CancelRequested` on a Process it is driving, it does not
finalize while the **managed conversation** the last transition committed still
holds a tool call nobody answered. It runs the strategy's next transition and
looks again, bounded by `WithMaxCancelDeferrals` (default: the rest of the
claim), after which it finalizes whatever the conversation looks like.

The kernel derives this from the conversation it already owns. There is no
declaration for a strategy to write, no field on `Process`, and no change to any
existing signature or to the `Repository` / `HistoryStore` contracts.

The scope is exactly "stopping points the kernel chooses". A cancel of a
`pending` or `waiting` Process still finalizes inline; a crash, a spent
`WithMaxStepAttempts` budget, and the `WithMaxUncleanReclaims` bound still end a
Process wherever its last commit left it. An agent that keeps `History` in its
own state has no conversation the kernel can read, so its cancels are unchanged.

## Context

`Cancel` on a `running` Process sets a flag, and the worker finalizes at its next
re-read ([process-lifecycle](../design/process-lifecycle.md#cancellation)). Which
boundary that is has nothing to do with the strategy's own structure.

A strategy is explicitly allowed to commit mid-round: `Session().CallTool` exists
so a round *can* be closed at a boundary, not because it must be, and
[ADR-0017](0017-history-is-an-immutable-versioned-store.md) makes the split safe
by rolling `History` back with `State`. `strategy/simple` splits exactly this
way — one `Generate` per transition, its tool calls answered by the next one.

So the committed conversation legitimately ends on an unanswered tool call for
whole transitions at a time, and a cancel landing there stores that as the final
transcript. No provider accepts it as a message sequence, which makes it useless
as a seed for another Process and useless to anything that would send it on.

Three shapes were considered before this one.

**Forbid the state.** Require every transition to commit a closed conversation.
This is the only approach that would cover the crash paths too, and it is not
implementable: an LLM loop that answers its calls in the next transition would
have to fold the whole round into one, and a loop is not a bounded round. It
would also delete the guarantee ADR-0017 exists to provide.

**Let the strategy declare it.** Add a marker to `Decision` saying "not a
stopping point". Correct in principle — only the strategy knows why — but it puts
the obligation on every strategy author, and a missing declaration fails
silently. For the managed conversation the kernel is not missing the
information; it is not looking at it.

**Normalize on the way out.** Drop the trailing unanswered turn when the stored
transcript is read. This is complementary, not alternative: it is the only thing
that can cover the paths where no stopping point is chosen at all. It is also
outside this ADR, because it needs public API (an extraction path and a way to
seed a Spawn) that this change does not add.

## Decision

`hasOpenToolCall` walks a `gollem.History` and reports whether any tool call is
missing its response. `Kernel.conversationClosed` gets the committed version from
the claim's `historyState` and answers with the negation. `driveClaim` tests the
deferral bound first, then that, before finalizing a cancel.

**The kernel reads conversation structure here.** It already builds conversation
messages in `Session().CallTool` — the one exception
[ADR-0007](0007-kernel-neutral-to-serialization.md) names — and this stays on the
same side of that line: typed `gollem` fields, no marshalling of caller data, and
no interpretation of what the conversation *says*. The judgement is structural.

**The answer is derived, never stored.** The committed `HistoryRef` already names
the version, and `historyState` already caches it per claim
(`ensureLoaded` loads once; `commitHistory` advances it), so a worker that takes
over after a crash re-derives the same answer from the same row. Storing a flag
would have meant a `Process` field and a `Repository` obligation for a value that
is a pure function of state the store already holds.

**Whenever the kernel cannot tell, the cancel proceeds.** No store, nothing
committed yet, a version that will not load — each answers "closed". The
alternative is holding a cancelled Process hostage to a version nobody can read.
A content that will not decode is the one inversion: it counts as open, because
reading a corrupt record as "answered" asserts something the kernel cannot see,
and the bound limits what that costs.

**The bound is claim-local and clamped below `maxStepsPerClaim`.** A counter that
survived the claim would need to be persisted; one that could exceed the claim
would restart on the next one, and a conversation that never closes would keep a
cancel pending forever. Clamping is what makes "the cancel lands inside the claim
that observed it" true without any persisted state. The default is
`maxStepsPerClaim - 1` rather than a separate number: a strategy that closes its
rounds needs one transition, one that does not would not close in five either, so
a smaller default only cuts short the legitimate shape that answers one call per
transition.

**A `waiting` Process is left alone.** Deferring there would make `Cancel`
asynchronous for a case it cannot help anyway: the await is still open, so the
strategy re-suspends and the conversation never closes. The honest outcome is the
current one, recorded as a known gap rather than papered over.

## Alternatives rejected

- **A marker on `Decision`.** See Context. Kept viable for a strategy that
  manages `History` itself, where the kernel genuinely cannot see the
  conversation, but nothing needs it yet and an unused declaration is vocabulary
  the kernel does not interpret.
- **Refusing the commit instead of the stopping point.** Turns a scheduling
  question into a restriction on how a strategy may split its work, and breaks
  the split ADR-0017 was built for.
- **A `Process` field.** A `Repository` contract change, `repotest` coverage, and
  a migration obligation for third-party stores — for a derivable value.
- **A new `EventType` for the deferral.** `Event` is public, addressable and
  persisted ([ADR-0019](0019-events-are-addressable-and-reads-resume.md)). How
  many transitions a worker spent waiting is an operational detail; the logger is
  where it belongs.
- **A wall-clock grace period instead of a transition count.** Every other bound
  in the worker is a count, and a count is what makes the behaviour testable
  without timing.
- **Treating an unreadable version as open.** Fails closed in the wrong
  direction: a broken `HistoryStore` would stop cancels from landing promptly,
  which is worse than an imperfect transcript.

## Consequences

- **`Cancel` on a running Process is observably slower.** Up to
  `maxCancelDeferrals` further transitions run, their tools and LLM calls really
  happen, and their usage lands in `Process.Metrics`. `WithMaxCancelDeferrals(0)`
  restores the previous behaviour exactly.
- **A strategy that never closes its rounds gets the worst of it** — a slower
  cancel *and* a transcript that ends mid-round. The obligation this places on
  strategy authors is stated in
  [writing-strategies](../writing-strategies.md#where-a-cancel-can-stop).
- **The guarantee is partial by construction.** It says nothing about a Process
  that crashes, exhausts its retries, or is cancelled while waiting. Anything
  that needs a usable transcript from *those* needs the normalization described
  in Context.
- Third-party `Repository` and `HistoryStore` implementations are unaffected. An
  agent registered without `WithHistoryStore` behaves exactly as before.

## History

| Date | Change |
|---|---|
| 2026-08-02 | Initial record. |
