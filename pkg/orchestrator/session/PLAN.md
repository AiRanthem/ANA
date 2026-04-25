# session/PLAN.md

## Purpose

Own the in-memory model of session lifecycle, the call-stack per task, and
the persistence interface for sessions and requests.

## Public surface (intent only)

- `SessionID` and `RequestID` types (string aliases re-exported from root
  `types.go`).
- `Status` enums:
  - `SessionStatus`: `running`, `paused`, `closed`, `failed`.
  - `RequestStatus`: `created`, `running`, `completed`, `failed`.
- `RequestKind` enum per `DESIGN.md` §6.7: `root`, `delegation`,
  `resume`. Owned by this package because it is a `Request` field.
  Consumers (`prompt/`, `invoker/`) import this package for the type.
- `Session` struct per `DESIGN.md` §2.3 / §6.4.
- `Request` struct per `DESIGN.md` §6.4. The `Kind` field carries
  `RequestKind`.
- `Stack` interface — task-scoped call stack:
  - `Push(ctx, Session) error`
  - `Pop(ctx) (Session, error)`
  - `Peek(ctx) (Session, error)`
  - `Depth(ctx) int`
- `Store` interface — task-scoped session and request persistence:
  - `CreateSession(ctx, Session) error`
  - `UpdateSession(ctx, Session) error`
  - `GetSession(ctx, taskID, sessionID) (Session, error)`
  - `ListSessions(ctx, taskID) ([]Session, error)`
  - `CreateRequest(ctx, Request) error`
  - `UpdateRequest(ctx, Request) error`
  - `GetRequest(ctx, taskID, sessionID, requestID) (Request, error)`
  - `ListRequests(ctx, taskID, sessionID) ([]Request, error)`
- Sentinel errors: `ErrSessionNotFound`, `ErrSessionClosed`,
  `ErrInvalidTransition`, `ErrMaxSessionsExceeded`.

The `Stack` is created per task by the engine and lives only in memory; the
`Store` is shared across tasks.

## Lifecycle & transitions

State machine per `DESIGN.md` §5.2. Allowed transitions:

| From      | To        | Trigger |
|-----------|-----------|---------|
| (new)     | running   | `Stack.Push` after `Store.CreateSession` |
| running   | paused    | Salutation detected in agent output |
| paused    | running   | Callee popped; `UpdateSession` flips state |
| running   | closed    | Plain output; engine calls `Stack.Pop` |
| paused    | failed    | Cancellation while paused, or callee subtree failed unrecoverably |
| running   | failed    | Request inside this session terminally failed |
| closed    | (none)    | Terminal |
| failed    | (none)    | Terminal |

`UpdateSession` rejects illegal transitions with `ErrInvalidTransition`.

## Stack semantics

- The Stack is an in-memory `[]SessionID` guarded by a mutex. It is owned
  by one Task; the engine creates a fresh Stack on `Submit`.
- The Stack is mechanical bookkeeping only. The cumulative session count
  per task and the `MaxSessions` limit (see `DESIGN.md` §10.3) are
  enforced by the engine *before* it calls `Stack.Push`. When the engine
  observes the limit, it surfaces `ErrMaxSessionsExceeded` to the caller
  and fails the task without touching the Stack.
- Stack mutations happen *after* the corresponding session record reaches
  the Store. Crash invariant: even if the orchestrator dies mid-loop, the
  Store reflects the last committed state.

## Resume semantics

- When a session moves from `paused` to `running`, the engine creates a
  *new* `Request` under the same `SessionID`. This is how a single Alice
  session ends up with multiple Requests in the spec example.
- `Request.InputText` for the resume Request is the callee's plain output.
- The engine sets `RequestKind = resume` for the prompt builder so Notes
  are not regenerated.

## Request lifecycle

This package owns the data model and the `Store` for Requests; the actual
state transitions are driven by the engine and the invoker. The expected
sequence:

- `Request.created` → `Request.running` when the invoker receives the
  first event (or successfully dispatches the call).
- `Request.running` → `Request.completed` on stream EOF without failure
  events. `OutputText` is the aggregated text (Salutation included if
  any; the engine strips it later when deciding push vs pop).
- `Request.running` → `Request.failed` on stream `EventError`, transport
  failure, or context cancellation.

The audit sink and event bus see each transition, but those emissions are
the invoker's responsibility (see `invoker/PLAN.md`); this package
exposes only `UpdateRequest` to record the new state.

## Persistence

- Default `Store` is in-memory, backed by maps under a single mutex per
  task. This is sufficient for v1; multi-task contention is rare because
  each task touches its own slice of the maps.
- The `Store` interface is shaped so a Redis or SQL backend can be plugged
  in without touching the engine.
- Sessions and requests are NOT pruned automatically. The audit sink is
  the canonical long-term store; the in-memory Store is operational state.

## Concurrency

- Within a task: serial by definition (only one running session at a time).
  No internal concurrency hazards inside the Stack.
- Across tasks: per-task locks; the Store implementation may use sharded
  maps to keep contention low.

## Edge cases & decisions

- A session's only Request fails before any output: session moves directly
  to `failed`. Engine pops it and propagates failure to the parent session
  (which itself fails) and so on up to the task.
- Callee returns plain output but its session is in `failed`: parent
  session inherits `failed`. The engine emits a `session.failed` event for
  each affected ancestor.
- `Stack.Pop` on an empty stack returns `ErrSessionNotFound` (engine
  treats this as the "task done" signal).
- Re-pushing a `closed` session is rejected with
  `ErrInvalidTransition`. Repeating a workspace requires a brand new
  session id (matching the spec: a new Salutation creates a new session).

## Tests to write (no implementation in this pass)

1. Push/Pop happy path matching the spec example.
2. Pause/Resume invariants: same session id, multiple requests.
3. Engine emits `ErrMaxSessionsExceeded` when cumulative session count
   exceeds the configured limit; the Stack remains in its prior state.
4. Illegal transitions blocked (e.g., closed → running).
5. `Store` round-trips persisted state.
6. Concurrent task workloads under `-race`.
7. Cancellation flips `running` to `failed` after the in-flight Request
   propagates cancellation.

## Out of scope

- Cross-orchestrator session migration (v2).
- Session GC policies; in-memory Store keeps everything for the lifetime of
  the orchestrator process.
- The actual stack drive loop (engine concern, lives in
  `pkg/orchestrator/engine.go`).
