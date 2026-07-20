# ADR-0004: Repository SPI is ChangeSet + Rev CAS

## Summary

Persistence is a single interface, `Repository`, whose write path is one method:
`Apply(ctx, ChangeSet)`. A `ChangeSet` is a declarative bundle of process rows,
await rows, event appends, and write-free `Guards`. The contract asks for data
semantics only — atomic application of the whole set, and a per-row `Rev`
compare-and-set — never for a transaction object.

Invariant logic (first-writer-wins on a response, cancel-vs-commit races) lives
in the kernel as a read → build → `Apply` → retry-on-`ErrConflict` loop, not in
the implementation.

## Context

A persistence port can be shaped as `InTx(func(tx Tx) error)`, handing the
implementation a transaction and letting the kernel run arbitrary logic inside
it. That requires the store to *have* transactions, which excludes eventually
consistent key-value stores, and it tends to pull kernel invariants into the
implementation (`Repository.Respond` doing the first-writer-wins check).

agentkit needs to run on whatever the caller already operates: PostgreSQL,
Firestore, DynamoDB, an in-memory map.

## Decision

`Repository` has three finder methods, one claim method, two list methods, and
`Apply`. The full contract:

1. `Apply` applies the whole `ChangeSet` atomically — all or nothing.
2. Each row in `cs.Processes` is a `Rev` CAS: stored `Rev` must equal the row's
   `Rev`, or nothing is written and `ErrConflict` is returned. On success each
   row's `Rev` is incremented (an insert declares `Rev == 0` and is stored as
   `Rev == 1`). This fences stale-worker commits.
3. Each `ProcessGuard` in `cs.Guards` is a read-only `Rev` precondition — checked,
   never written, `Rev` not advanced. It exists to make the `WaitChildren`
   check-then-act atomic (see ADR-0009).
4. `ClaimNextProcess` atomically claims one runnable process and never
   double-claims across concurrent workers. Every claim mints a fresh
   `LeaseToken` (a uuid v7), including a re-claim by the same worker. That token
   is the fence identity.
5. Uniqueness is maintained on `idempotency_key`, on an open process's
   `Subject`, and on `(process_id, await_key)`. A violation writes nothing and
   returns `ErrConflict`.
6. `ListEvents` preserves per-process append order.

Reads deep-copy on the way out: a caller mutating a returned `*Process` must not
be able to reach stored state.

How that is realized is free — an RDB transaction, a Firestore transaction, a
conditional write, or a mutex around an immutable snapshot.

## Alternatives rejected

- **`InTx(func(Tx) error)` callback.** Requires real transactions, excluding
  conditional-write KV stores, and leaks kernel invariants into implementations.
- **Higher-level methods (`Repository.Respond`, `Repository.Finalize`).** Every
  implementation would then re-derive the same invariants, and a subtle
  divergence between two implementations would be undetectable.
- **A separate lease generation column alongside `Rev`.** Two optimistic tokens
  where one suffices; `Rev` covers both CAS and lease-expiry detection, with
  `LeaseToken` carrying claim identity.

## Consequences

- Implementers get a small, testable contract. `repository/repotest` is the
  executable form of the list above — `repotest.Run(t, factory)` covers Rev
  increment, cross-row atomicity, guards, both uniqueness domains, await upsert,
  event ordering, claim eligibility (pending / waiting past `WakeAt` / expired
  lease), fresh-token-per-claim, no-double-claim under 100-way concurrency, and
  deep-copy-on-read. **Run it against any new implementation.**
- The kernel must handle `ErrConflict` everywhere it writes. It does, with
  re-read-and-re-decide loops in `Respond`, `Cancel`, `commitFinal` and the
  worker's commit path.
- Bundled reference implementations: `repository/memory` (mutex + copy-on-write
  snapshot) and `repository/filesystem` (the same snapshot, persisted by
  temp-file → fsync → rename → directory fsync, single process only, enforced by
  a `flock`). Neither is intended for multi-host deployment.
- Batching is natural: one transition is one `Apply`, and a waiting process
  costs zero writes.

## History

| Date | Change |
|---|---|
| 2026-07-20 | Initial record, extracted from the initial implementation spec (D3, D34). |
