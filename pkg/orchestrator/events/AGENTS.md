# pkg/orchestrator/events AGENTS.md

## Scope

Best-effort runtime event bus for live subscribers. Counterpart to
`audit/`: never blocks the engine, may drop on slow subscribers.

## Rules

- `Publish` is non-blocking. Always.
- Subscriptions are task-scoped; subscribers without a task id are
  rejected.
- Every dropped event must be reflected in the per-subscriber drop
  counter and surfaced via `Errors()`.
- Subscriber goroutines must terminate when the task is terminal or the
  subscriber closes.

## Don'ts

- Do not record audit data here. Use the `audit.Sink` for durable
  records.
- Do not coalesce events in v1; semantics must remain "drop oldest /
  drop new" per channel buffer rule.
- Do not import other orchestrator subpackages except `idgen` (for ID
  types). The bus carries opaque `Event` values constructed by the
  engine; do not pull in `audit`, `task`, or `session` to dereference
  them.
- Do not block on subscriber I/O. If a subscriber is wedged, drop and move
  on.
