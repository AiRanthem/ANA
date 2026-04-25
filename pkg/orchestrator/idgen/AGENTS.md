# pkg/orchestrator/idgen AGENTS.md

## Scope

ID generation for tasks, sessions, requests, and events. One default
implementation (time-sortable, random-suffixed) and one sequential test
implementation.

## Rules

- IDs are opaque strings. Do not encode parsing logic on the consumer
  side; do not build adapters that decompose IDs.
- All generators are concurrency-safe.
- Test code uses `NewSequential` so audit and event traces are stable.

## Don'ts

- Do not accept caller-supplied IDs in v1. All IDs are minted internally
  by the orchestrator.
- Do not import other orchestrator subpackages; this package sits at the
  bottom of the dependency graph.
- Do not change the default ID format without bumping the audit-record
  schema version.
