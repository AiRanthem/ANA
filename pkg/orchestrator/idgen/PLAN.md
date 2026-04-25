# idgen/PLAN.md

## Purpose

Generate ID values for tasks, sessions, requests, and events. Centralized
so that engine and modules don't reach for `crypto/rand` or `uuid`
directly, which keeps tests deterministic via injection.

## Public surface (intent only)

- ID type definitions, exported here so every other subpackage can import
  them without inducing cycles with the root engine package:
  - `type TaskID string`
  - `type SessionID string`
  - `type RequestID string`
  - (`EventID` stays as plain `string` because audit and events use it
    only as an opaque dedup key.)
- `Generator` interface:
  - `NewTaskID() TaskID`
  - `NewSessionID() SessionID`
  - `NewRequestID() RequestID`
  - `NewEventID() string`
- Constructors:
  - `NewDefault() Generator` — UUIDv7-like (time-sortable) IDs.
  - `NewSequential(prefix string) Generator` — deterministic counter for
    tests; thread-safe.

The root `pkg/orchestrator/types.go` re-exports these IDs as type aliases
(e.g., `type TaskID = idgen.TaskID`) so callers can write
`orchestrator.TaskID` ergonomically. Subpackages MUST import them from
`idgen` to keep the dependency graph acyclic (see `DESIGN.md` §13.1).

## ID format

Default generator uses time-sortable ULIDs / UUIDv7. The exact format is
implementation-private; callers must not parse the ID. Properties to
preserve:

- 26..36 ASCII characters.
- Lexicographically sortable by creation time within the same generator
  instance, so audit logs sorted by ID approximate insertion order.
- Globally unique with negligible collision probability.

For deterministic tests, `NewSequential("T-")` returns `T-0000000001`,
`T-0000000002`, etc. Each `New*ID` method increments an independent
counter; type prefixes encode in the deterministic form (e.g., `T-`,
`S-`, `R-`, `E-`) for readability.

## Concurrency

Both the default and sequential generators are safe for concurrent use.
The default uses entropy from `crypto/rand`; the sequential uses an
`atomic.Uint64` per ID kind.

## Edge cases & decisions

- The orchestrator does not accept caller-supplied IDs in v1. All IDs are
  minted internally. This eliminates a class of input-validation bugs
  (e.g., colliding task ids from buggy clients).
- Generator failures (e.g., `crypto/rand` short read) are fatal because no
  meaningful Task can proceed without IDs. The default generator panics on
  underlying RNG failure; this matches Go stdlib precedent.

## Tests to write (no implementation in this pass)

1. `NewDefault` produces unique, lexicographically sortable IDs.
2. `NewSequential` is deterministic and ordered.
3. Concurrent generation under `-race` produces unique values.

## Out of scope

- Cross-process clocks or distributed ID coordination. v2 may need a
  region-aware generator if multiple orchestrator nodes share an audit
  log; for now, randomness is enough.
- ID parsing or component extraction (timestamp, machine id). Treat IDs as
  opaque.
