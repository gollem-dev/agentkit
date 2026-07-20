# agentkit documentation

agentkit runs an LLM agent as a **durable, resumable state machine**. A
`Process` is checkpointed after every transition and can be picked up by any
worker after a crash.

## Start here

1. **[Execution model](execution-model.md)** — what agentkit guarantees, what it
   does not, and what that makes your job. Read this before writing any code
   that touches the outside world.
2. **[Getting started](getting-started.md)** — from an empty `main` to a running
   agent.
3. **[Concepts](concepts.md)** — `Process`, `Strategy`, `Syscalls`, `Await`,
   `Kernel`, `Repository`, and how they relate.

## Guides

- **[Writing a strategy](writing-strategies.md)** — implementing
  `Strategy[S, I, O]`: state design, transitions, waiting on humans and children.
- **[Bundled strategies](bundled-strategies.md)** — `strategy/simple` and
  `strategy/planexec`.
- **[Tools](tools.md)** — supplying `gollem.Tool`s, and the idempotency and
  authorization obligations that come with them.
- **[Persistence](persistence.md)** — implementing `Repository`, and verifying it
  with `repotest`.
- **[Observability](observability.md)** — metrics, limits, events, and
  middleware.

## Design and decisions

- **[docs/design/](design/)** — how the pieces fit together: architecture,
  process lifecycle, consistency model, responsibility boundaries.
- **[docs/adr/](adr/)** — why it is this way, and what was rejected.

If you are designing a change to agentkit itself, read both before you start.
