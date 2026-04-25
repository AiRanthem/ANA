# pkg/orchestrator/audit AGENTS.md

## Scope

Fail-fast, durable audit interface for the orchestrator. Defines the
`Sink` contract, the record shapes, and a `MemorySink` reference
implementation for tests.

## Rules

- `WriteEvent` and `WriteTranscript` are blocking until the record is
  accepted; either return nil (durable) or return an error.
- Implementations must preserve per-task ordering. Cross-task ordering is
  not required.
- Use the `EventID` and `(TaskID, SessionID, RequestID, Kind, Seq)` keys
  for dedup; do not invent new keys.
- `Multi` is fail-fast by design; for best-effort fan-out, wrap secondary
  sinks externally.

## Don'ts

- Do not implement business logic (validation, filtering, transforms) in a
  sink. Sinks are pipes; the orchestrator emits well-formed records.
- Do not import other orchestrator subpackages except `idgen` (for ID
  types). Audit must be free of cycles; the engine at the root must be
  free to import `audit`.
- Do not assume a specific transport; the package only defines interfaces
  and an in-memory reference. Concrete sinks live in operator code.
- Do not silently drop records. Always either block or error.
