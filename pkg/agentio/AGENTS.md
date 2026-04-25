# AGENTS.md

## Scope
- `pkg/agentio` owns the canonical request, event, stream, and session contracts.
- This package is transport-neutral and orchestration-neutral.

## Responsibilities
- Define `InvokeRequest`, `InputPart`, `Event`, `EventStream`, `Agent`, `SessionAgent`, and `Session`.
- Provide canonical JSON-safe request encoding helpers.
- Provide stream helpers such as `CollectText` and `TextReaderAdapter`.

## Non-Responsibilities
- No REST, CLI, or socket transport code.
- No routing, workspace lookup, orchestration, persistence, or delivery logic.

## Parallel Implementation Boundary
- One worker may own `types.go`.
- One worker may own `stream.go`.
- One worker may own `canonical.go`.
- Coordinate on exported type names before editing across files.
