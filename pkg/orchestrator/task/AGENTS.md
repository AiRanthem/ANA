# pkg/orchestrator/task AGENTS.md

## Scope

Task data model, state transitions, `Store` interface, per-task handle
(`Wait`, `Cancel`, `Snapshot`), and root cancellation plumbing.

## Rules

- Handle every `Wait` call as a potentially long-blocking operation. Never
  hold per-task locks across the wait; signal completion via a closed
  channel or a `sync.WaitGroup`.
- `Cancel` is idempotent. Multiple callers must observe consistent state.
- `Update` enforces state transitions; reject illegal moves with
  `ErrTaskAlreadyTerminal`.
- Snapshot reads must be consistent with the latest committed state but
  must not block writers.

## Don'ts

- Do not record audit events or push events to the bus from this package;
  those are the engine's responsibility.
- Do not subscribe to `agentio.Agent` streams. The Task package never
  touches transport.
- Do not introduce automatic GC in v1. Operators control retention via
  audit-tier storage.
- Do not import `session/` or `events/`. Use IDs and the `Store`
  interfaces defined here.
