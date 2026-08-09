# ADR-0010: Execution limits are the strategy's Limit method

## Summary

The kernel **measures**; the strategy **decides**. Measurement is `Metrics`:
eight `int64` counters the kernel maintains itself, which are struct fields and
a closed set, plus any number of counters the caller defined, which are not.
Two of the eight are a prompt-cache breakdown: components of input tokens, not
additions to it — `InputTokens` stays the true total, and
`InputTokens - CacheReadInputTokens` is the input not served from cache
(uncached input plus any cache write, not a single price tier).

A caller-defined counter is named by a `MetricKey`, a sealed interface whose only
values come from `DefineMetricKey`, so a counter cannot be addressed by a string
literal at the point of use. Two things write one: `Syscalls.Count`, for what a
strategy knows itself, and the optional `MeteredTool` interface, for what only a
tool knows. Both feed the same counters, which are read back through
`Metrics.Count` and roll up into the parent exactly like the eight.

The decision is a required method on `Strategy`:

```go
Limit(ctx context.Context, proc *Process, metrics Metrics) LimitDecision
```

The `Limiter` function type is that method's shape, and is what the bundled
strategies take through their own `WithLimiter` option to build the method from a
caller's closure. There is no Kernel-wide limiter.

A `LimitDecision` is one of three verdicts, built by `LimitPass`, `LimitNotice`
or `LimitStop` and read back through `Kind()` and `Message()`. `LimitPass()` is
how a strategy says "unlimited"; there is no way to skip answering.

- **`LimitStop(reason)`** refuses. Before an effect the reason reaches the
  strategy wrapped in `ErrLimitExceeded`; at a transition boundary it finalizes
  the process as `failed` with `FailureLimitExceeded` and that reason as the
  message.
- **`LimitNotice(msg)`** continues, carrying a message the strategy can read
  through `Syscalls.LimitStatus()` and middleware through `EffectContext.Limit`.
  The kernel never interprets it, never injects it anywhere, and nothing reads it
  unless the caller asks.
- **`LimitPass()`** continues with nothing to report.

The method runs at each transition boundary, before every `Generate`,
`CallTool` and `SpawnChild`, and again after each of those has been counted. The
first two refuse the work when it says stop; the third cannot, because the work
is done, and only updates what `LimitStatus()` reports. Being called that often
makes two demands on it: it must be **read-only** with respect to whatever it
consults — an enquiry, never an acquisition — and it must not block.

The boundary call happens before the state is decoded and outside the recover in
`runTransition`, so it goes through `callLimit`, which converts a panic into a
transition error rather than letting it reach the worker goroutine.

`Process.Metrics` counts a Process's own effects **plus every child that has
terminated, once each**, so one closure expresses a subtree budget as well as a
per-Process one. It is a budget veto, not admission control: it decides whether
to continue, and has no way to wait.

## Context

The original design had two layers: a static `Limits map[Metric]int64` table,
overridable per agent definition and per spawn, plus a `Governor` port for
dynamic throttling. Between them they expressed only what a table can express,
while each new policy shape (rate limits, monetary budgets, per-agent ceilings,
whole-tree budgets) demanded new structure.

Both were replaced by a single closure injected on the Kernel
(`WithLimiter`), which fixed the shape problem but put the policy one scope too
high. One Kernel meant one limiter, so a per-agent cap was written as a `switch`
on the `proc.Agent` string — where a missing case falls through to `default` and
grants no limit at all. The runtime that hands out `Agent[I]`, a typed handle, was
answering "what may this agent spend" with untyped string matching that fails
open.

## Decision

Delete all of it. Ship the counters, and make answering the question part of
being a `Strategy`.

A static cap is a few lines in `Limit`. A rate limit consults the caller's own
limiter from there. A budget that varies per tenant reads `proc.Metadata`. All
through one method, and none of it in agentkit.

**The method is required, which is the whole point.** An optional slot — a
`KernelOption`, a `RegisterOption` — can be left empty, and an empty budget slot
reads as no budget. A method on the interface cannot be left out: a strategy that
wants no limit writes `return LimitPass()` and has said so on the record.

**One limiter, at one scope.** Keeping a Kernel-wide one alongside would need a
composition rule (which verdict wins, whose message lands in `Failure.Message`)
and would double the call count, to deliver something the method already covers:
`Process.Metrics` folds in every terminated child, so a whole-tree budget is the
root's own `Limit` reading its own `metrics` argument. Two mechanisms for one
question is the `Governor` mistake this record already rejected once.

`metrics` is a live snapshot: committed cumulative (`proc.Metrics`) plus what the
current run has accumulated so far. There is no way for an effect to consume
budget without the next check seeing it.

**The kernel's own counters are struct fields, not map entries.** That set is
closed by this record, so `map[Metric]int64` advertised a key space that does not
exist and made every read an index into something that might be missing. The
wire names reproduce the map's former keys, so a snapshot written before the
change reads back without a migration — not that the bytes are identical:
all-zero metrics moved from `null` to `{}`, and a key outside the original six is
dropped on read. Adding a field since grows what the struct declares, never what
a caller can address.

**Caller-defined counters are a map, because that set is open — and the key type
is what keeps it safe.** The kernel can only measure what it performs itself:
tokens, calls, spawns. Documents fetched, credits charged, rows written are
knowledge the caller has and the kernel does not, and a budget over them was
previously expressible only outside agentkit, where it lost the one thing the
kernel provides — a total that survives a crash and folds children into their
parent.

A `map[string]int64` would have brought back exactly the defect this record
removed: a name is written at each use site, and a typo produces a second
counter that reads zero rather than a compile error. So the key is `MetricKey`, a
sealed interface (`.claude/rules/go.md`: a marker method blocks construction
*and* external implementation, which an opaque struct does not). Its only
implementation is a comparable **value** type, so identity is the name — two
`DefineMetricKey` calls with the same name are equal and index the same entry.
That is the opposite of `ModelRole`, deliberately: a role is never persisted,
while a counter is, and a key rebuilt from a stored name under pointer identity
would never match the one the caller holds.

There is no registry behind `DefineMetricKey`, so two packages choosing the same
name get the same counter. Detecting that would need mutable package state and
would make a name no code in this binary defines — which is exactly what a
Repository reads back — impossible to reconstruct. Namespace the name instead.

The counters live in an unexported field read through `Count` / `Counters` and
rebuilt through `WithCount`, and nothing mutates the map in place. An exported
map would be handed to a `Limiter` directly at the transition boundary
(`callLimit` passes `proc.Metrics`), where writing to it would corrupt the row —
and read-only-ness is something this record asks of a `Limiter` in prose, not in
the type. Keeping it unexported also means a Repository's clone needs no deep
copy. The cost is that `Metrics` owns its own `MarshalJSON`; it already declared
its wire names here, and a map keyed by an interface cannot be encoded from a
struct tag at all.

**Two write paths, because they know different things.** A strategy cannot see
inside a tool, and a tool cannot reach the strategy's syscalls. `Syscalls.Count`
covers the first, `MeteredTool` the second, and both land in the same counters —
this is one mechanism with two inputs, not two mechanisms answering one question.
`Count` re-evaluates `Limit` like a metered effect so `LimitStatus()` and
`Metrics()` still describe the same moment, and cannot be refused: what it
records has already happened. A `MeteredTool` report is taken after `Run`,
including a `Run` that failed, and an entry the kernel cannot count — a nil key,
a negative value — is dropped with a log line rather than failing a call whose
effect already happened.

**The verdict is three-valued, because stopping is not the only useful answer.**
A budget that can only refuse forces a run to end at the cap with no chance for
the agent to wrap up on its own terms. `LimitNotice` lets a budget say "nearly
out" and leaves what to do about it — shorten the plan, drop expensive tools,
answer now — to `Step`. The kernel carries the message and does nothing else with
it: acting on it would mean the kernel owning vocabulary for talking to a model
(ADR-0011).

With `Limit` on the same interface, a strategy reads back a verdict it produced
itself. That is not circular. `Limit` states a policy against counters the kernel
owns and the strategy cannot see any other way; `Step` decides what to do about
the answer. Splitting the two is what lets "nearly out" mean something different
from "stop", and a strategy is free to make `Limit` a pure function of the budget
and keep every reaction in `Step`.

Reading it is a pull, never a push. A strategy calls `LimitStatus()`, or a
`GenerateMiddleware` reads `EffectContext.Limit` and appends to the system
prompt itself; agentkit injects nothing.

**The verdict carries one kind and one message, not a bool and two strings.**
The three verdicts are mutually exclusive, so a shape that can express "stopped,
and also here is a notice" is a shape with unreachable states in it. `Kind()`
plus `Message()` makes the exclusion structural, and matches `Failure{Code,
Message}` and `DecisionKind`.

**`LimitStop` takes a string, not an error.** Both call sites discarded the
error type anyway — one wrapped `err.Error()` into `ErrLimitExceeded`, the other
put it in `Failure.Message` — so accepting an `error` only invited callers to
believe their type survived. With a string, every refusal a strategy observes is
an `ErrLimitExceeded` and `errors.Is` is sufficient to recognise one.

**The re-evaluation after an effect exists so the verdict and the counters
describe the same moment.** `Metrics()` already moves as effects run; a verdict
frozen at the transition boundary would report "plenty left" while `Metrics()`
showed the budget nearly gone.

A refusal returned by that post-effect evaluation is stored like any other. It
does not fail the effect — that one has run and its cost cannot be taken back —
but it is exactly the case worth seeing: the `Generate` that crossed the cap is
also the one whose result the strategy could finish with, and hiding the refusal
until the next effect would spend another one to learn it. Enforcement stays
with the next pre-effect check and the next boundary.

This means a stored `LimitKindStop` says "`Limit` is refusing", not "this
Process has stopped". The same reading already applied after a pre-effect
refusal that a strategy caught and chose to continue past, so there is one rule
rather than two.

**A whole-tree budget is the same method, because the counters roll up.** A
child adds its own `Metrics` to its parent in its terminal transition
(`reportToParent`, `worker.go`), and since it had already absorbed its own
children the same way, a root ends up holding the tree. This originally required
the caller to key its own accounting by `proc.RootID`; a fan-out strategy like
`planexec` made that mandatory for anyone who cared about total spend, which is a
lot of machinery to demand for a question the kernel was already measuring the
parts of.

**The fold is keyed to the child, not to an await.** A Process terminates exactly
once, so counting there is correct by construction — no dedup check, no set of
already-counted ids. Keying it to await resolution instead was the first attempt
and it was wrong twice over: a child named by two await keys was counted twice,
and a child nobody waited on — which ADR-0009 permits — was never counted at all.
`ChildResult.Metrics` still carries the figure to the parent's strategy, which is
a separate concern from the accounting.

The fold rides the child's own terminal `ChangeSet`, which already writes the
parent row, so it stays one write per transition. What it does not do is track a
child still running; a parent's view of its subtree is as of the last child that
finished.

`limit_exceeded` is not a process status. It is `failed` with
`FailureCode == FailureLimitExceeded`, which keeps the state machine at six
statuses and puts the reason where every other failure reason already lives.

## Alternatives rejected

- **`Limits map[Metric]int64` plus per-agent and per-spawn overrides.** Fixes the
  shape of a policy to "a number per counter", which most real policies are not.
- **A `Governor` port for dynamic throttling.** A second mechanism for the same
  question; the method already covers it.
- **A Kernel-wide `WithLimiter`, which is what this used to be.** One per Kernel
  meant a per-agent budget was a `switch` on `proc.Agent`, and the case you forget
  to write grants no limit. An optional slot cannot express "every agent has
  answered this".
- **A `RegisterOption` instead of an interface method.** Same scope, and it reads
  better next to `WithOnFinish` — but it is still optional, so it still fails open
  when it is left out. That was the defect being fixed.
- **A per-spawn limiter.** A `Limiter` is a closure, and the `Process` row a
  `Repository` persists is data — it has no contract for representing executable
  code, let alone reconstructing it. A worker picking the Process up after a crash
  would have nothing to rebuild the closure from. Per-agent works precisely
  because the `Registry` is in-process and every worker builds the same one. A
  per-spawn budget expressed as data is the `Limits` table again.
- **Giving `Limit` the strategy state `S`.** The boundary evaluation runs before
  `DecodeState`, so this would move decoding earlier and mix "the state would not
  parse" into "the budget refused". A limit that needs the algorithm's own state
  is a branch in `Step`; what `Limit` buys over that branch is only the ability to
  refuse without entering `Step` at all.
- **Cost metrics in the kernel** (`cost_micro_usd` and friends). Pricing depends
  on model, date and contract — knowledge the kernel does not have and cannot
  acquire. Emitting a number it cannot compute correctly would be worse than
  emitting nothing. Callers derive cost from the token counters.
- **Changing `Tool.Run` so it can return usage.** `gollem.Tool.Run` returns
  `map[string]any` and conforming to that signature (ADR-0001) matters more, so
  a tool reports through the separate optional `MeteredTool` interface. A tool
  that does not implement it still counts as `tool_calls` and nothing else.
- **`map[string]int64` for the caller-defined counters.** The defect this record
  removed, in another form: the name is written at each use site, and a typo
  makes a second counter that reads zero instead of a compile error.
- **An opaque struct (`MetricKey struct{ name string }`) instead of a sealed
  interface.** It blocks construction but not the zero value, so
  `var k agentkit.MetricKey` is a key that looks valid and addresses nothing. The
  interface has exactly one invalid form, `nil`.
- **A registry inside `DefineMetricKey` that rejects a duplicate name.** Needs
  mutable package state, and makes a name no code in this binary defines
  impossible to reconstruct — which is what a Repository does on every read.
  Panicking at init because another library chose the same name is also not a
  failure a library gets to impose.
- **Exporting the counter map on `Metrics`.** It would reach a `Limiter`
  writable, and at the transition boundary that map is the Process row's own.
- **Making `Metrics` itself an interface, so a caller supplies the accumulation
  logic.** It collides with three things at once: a Repository would have to
  serialize a type it cannot know (ADR-0007); a worker resuming after a crash
  would have nothing to rebuild the accumulator from, which is why a per-spawn
  `Limiter` was rejected above; and `reportToParent` folds a child's metrics into
  a parent running a *different* strategy, so there would be no defined meaning
  for the fold. A strategy that wants accumulation the kernel does not provide
  already has a place for it — its own checkpointed state — and what it gives up
  there is precisely the cross-process roll-up.
- **Giving the `Limiter` a second argument for subtree totals**, or widening it
  into a request struct. Both reopen the shape question this record closed, to
  deliver something the existing `Metrics` argument can carry once the counters
  roll up. This is about the *arguments*: what a limiter needs in order to
  decide is already there. Widening the *return* was a separate question, since
  `error | nil` could not express "continue, and here is why you should hurry"
  at all.
- **A severity or a remaining-budget fraction on the verdict.** Both need a
  denominator, and the kernel does not have one — a budget may be tokens, money,
  a rate, or a whole-tree total. Putting a scale in the type is the static
  `Limits` table returning in another form; a limiter that wants degrees can say
  so in the notice text.
- **A second port for the advisory channel** (`WithBudgetNotifier` or similar).
  Exactly the `Governor` mistake again: two mechanisms answering one question.
- **Injecting the notice into the prompt from the kernel.** It would make the
  kernel own vocabulary for addressing a model, and the plumbing already exists
  — `GenerateRequest.SystemPrompt` is writable from a `GenerateMiddleware`. What
  was missing was only a way to read the verdict.
- **Evaluating the verdict lazily, when `LimitStatus()` is called.** Attractive
  because only readers would pay for it, but a limiter is allowed to consult a
  rate limiter (this file's own example does), and a read that consumes a token
  is a trap.
- **Returning a bare `string` from `LimitStatus()`.** `Syscalls` is a public
  interface, so both changing that signature and adding a second method are
  breaking changes; returning an opaque struct keeps later additions to
  accessors, which are not.
- **A `Repository` query by `RootID`, so a caller can total the tree itself.**
  Another SPI method every implementer owes, to recompute on demand what the
  kernel can accumulate as it goes — and racy besides, since a sibling can
  finish between the query and the decision.
- **Letting a `Limiter` block until budget frees up.** It runs on the transition
  hot path while the claim holds its lease; waiting there converts a throttle
  into a lease expiry and an unclean reclaim (ADR-0015). Work that must wait
  belongs in a timer await.

## Consequences

- Reading `Metrics` is meaningful; interpreting it is not the kernel's job. Cost,
  quota and fairness all live in strategy code.
- **Every `Strategy` implementation owes a `Limit` method**, and a new one cannot
  be registered without writing it. Most write `return LimitPass()`.
- `Limit` must be cheap and non-blocking. It runs on the transition hot path,
  **before and after every effect** — `1 + 2×effects` calls per attempt — so the
  cost of a slow one is now paid roughly twice as often.
- **`Limit` must not consume what it consults.** Drawing a rate-limit token
  or charging a quota inside it over-charges every effect and refuses work that
  has already happened. This was implicit while it ran once per effect and is
  not implicit now; it is stated on the type.
- **There is no "no limiter" fast path.** A run with no budget still pays a method
  call at every check, because "unlimited" is now an answer rather than an absent
  configuration.
- **A panic in `Limit` at the transition boundary becomes a transition error**
  (`callLimit`), retried and eventually `retry_exhausted` like any other strategy
  failure. Inside an effect it is already covered by `runTransition`'s recover.
- A refusal mid-transition surfaces as `ErrLimitExceeded` to the
  strategy, which may handle it (checkpoint what it has and `Suspend`) or
  propagate it. That is the strategy author's call. The verdict stays readable
  through `LimitStatus()` afterwards, so a strategy that catches the error can
  see the reason without parsing it out of the message.
- `LimitStatus()` and `EffectContext.Limit` are not deterministic across a
  replay: they depend on how far an attempt got, like `Now()` and `Metrics()`.
  Folding one into checkpointed state is a bug.
- A `Limit` that only ever returns `LimitNotice` cannot end a run. That is the
  point, but it means a misconfigured budget fails open rather than closed. What
  no longer fails open is forgetting to configure one at all.
- Metrics from a failed attempt are still folded in on requeue and on
  `retry_exhausted`, so a crash-looping process cannot consume unbounded budget
  invisibly.
- **A parent's recorded counters can exceed its own cap.** It passes the check,
  spawns children that spend the rest of the budget, and trips on its next
  transition — by which point the subtree really has spent that much. The cap
  still binds what the parent goes on to do; it is not a bound on what is
  recorded. `examples/fanout` is written around this case.
- Reading "what did this one Process spend" is no longer a single field once it
  has children. The per-Process figure is recoverable from the children's own
  rows, which keep their own totals.
- **`Metrics` is no longer comparable with `==`**, because of the map. Compare
  with `reflect.DeepEqual`. Empty is normalised to a nil map so two values
  carrying the same numbers compare equal however each was built.
- **A `Repository` must carry counters whose names it cannot know in advance.**
  One storing JSON gets this for free; one storing discrete columns enumerates
  them with `Counters()` and rebuilds with `WithCount`. `repotest.Run` covers it.
- A snapshot written before caller-defined counters existed loads as "none", and
  one written after them is read by an older binary with the counters dropped —
  the same way an unknown key has always been dropped. Rolling back loses them;
  the kernel's eight are unaffected.
- **A caller-defined counter is at-least-once, like every other.** A retried
  transition counts again, and the failed attempt's count is folded on requeue
  rather than lost (ADR-0003).

## History

| Date | Change |
|---|---|
| 2026-07-20 | Initial record, extracted from the initial implementation spec (D6, D9, D10, D33). |
| 2026-07-26 | `Process.Metrics` now includes every terminated child, counted once each, so one `Limiter` expresses a subtree budget. Previously a tree budget needed the caller's own accounting keyed by `RootID`, which a fan-out strategy made mandatory. The signature is unchanged. The fold is keyed to the child's terminal transition rather than to await resolution — the first attempt keyed it to the await, which double-counted a child named by two await keys and missed one nobody waited on. |
| 2026-07-26 | The `Limiter` returns a `LimitDecision` instead of an `error`, adding a third verdict: continue while telling the strategy the budget is running out. A two-valued return could only end a run at the cap, giving an agent no chance to wrap up on its own terms. The message is read through `Syscalls.LimitStatus()` or `EffectContext.Limit` and the kernel does nothing with it — injecting it would put model-facing vocabulary in the kernel. `LimitStop` takes a string because both call sites already discarded the error type. The verdict is re-evaluated after each effect is counted so it and `Metrics()` describe the same moment; a refusal there is stored but does not fail the effect, which is what lets a strategy finish with the result that crossed the cap. Being called twice per effect makes read-only-ness a stated requirement rather than an implicit one. `Metrics` became a struct: the counter set is closed by this record, so a map advertised keys that never existed. Its json tags reproduce the map's keys, so old snapshots read back without a migration — though all-zero metrics moved from `null` to `{}` and an unknown key is dropped. |
| 2026-07-26 | The decision moved from a Kernel-wide injected closure (`WithLimiter`) to a required `Strategy.Limit` method, and the Kernel option was deleted rather than kept alongside. One limiter per Kernel forced a per-agent budget to be a `switch` on the `proc.Agent` string, where a missing case falls through to "no limit" — an optional slot cannot express that every agent has answered, and a required method can. Keeping both would have needed a composition rule and doubled the call count to deliver what the method already covers, since `Process.Metrics` folds in terminated children and a whole-tree budget is the root's own `Limit`. The method takes no `S`: the boundary evaluation runs before `DecodeState`. Per-spawn was rejected because a closure cannot be persisted on the Process row. The boundary call is wrapped by `callLimit` so a panic there becomes a transition error instead of killing the worker, and the bundled strategies gained their own `WithLimiter` option. |
| 2026-08-06 | `Metrics` grew two counters, `CacheReadInputTokens` and `CacheCreationInputTokens`, so a caller can separate the input a Generate call served from the prompt cache from the input it did not. Both are components of `InputTokens`, not additions to it: `InputTokens` keeps meaning the true total gollem reports, so no existing `Limit` implementation changes what it sees. `InputTokens - CacheReadInputTokens` is the input not served from cache — uncached input plus any cache write — not a single price tier, since a cache write is commonly billed at a premium over uncached input and a cache read at a discount; agentkit carries the three counts and leaves pricing to the caller. Only Claude reports cache writes; the field is 0 for a provider that does not, indistinguishable from "no caching" and left uncorrected rather than teaching the kernel a provider-capability flag. The set is still closed at eight fields — a caller still cannot add a ninth. |
| 2026-08-10 | `Metrics` gained caller-defined counters, so the set is no longer closed. The kernel's own eight still are — a caller cannot add a ninth of those — but it can now name counters of its own. They exist because the kernel can only measure what it performs itself, while a budget over documents fetched or credits charged had to live outside agentkit and lost the roll-up and the crash-durable total by doing so. The key is `MetricKey`, a sealed interface whose sole implementation is a comparable value type, so a counter cannot be named by a string literal at the point of use (the `map[string]int64` defect this record already removed once) and a key rebuilt from a stored name still matches the caller's — the opposite of `ModelRole`'s pointer identity, because a counter is persisted and a role is not. There is no registry, so a duplicate name is one counter; namespace it. Two paths write one, `Syscalls.Count` for what a strategy knows and the optional `MeteredTool` for what only a tool knows, both landing in the same counters — one mechanism with two inputs, not two mechanisms for one question. The map is unexported and never mutated in place, since `callLimit` hands a `Limiter` the row's own `Metrics` and read-only-ness is asked of it in prose rather than in the type; that costs `Metrics` its own `MarshalJSON` (a map keyed by an interface cannot be encoded from a struct tag) and its comparability with `==`. Making `Metrics` itself an interface was rejected: a Repository could not serialize it, a resuming worker could not rebuild it, and the parent/child fold crosses strategies. |
