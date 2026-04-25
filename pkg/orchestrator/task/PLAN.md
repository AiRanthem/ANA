# task/PLAN.md

## Purpose

Own the Task data model, its lifecycle state machine, the public `Submit /
Wait / Cancel / Snapshot` integration points, and the `Store` interface that
persists task records.

The engine in the orchestrator root drives Tasks; this package supplies the
durable representation, the wait primitive, and the per-task cancellation
plumbing.

## Public surface (intent only)

- `TaskID` type (string alias re-exported from root `types.go`).
- `Status` enum: `pending`, `running`, `completed`, `failed`, `cancelled`.
- `Task` struct:
  - `TaskID`, `Status`, `Envelope MessageEnvelope`, `RootSessionID`,
    `OpenedSessions int`, `CreatedAt`, `StartedAt`, `FinishedAt`,
    `*ResultError`, `FinalOutput string`.
- `Store` interface:
  - `Create(ctx, Task) error`
  - `Update(ctx, Task) error`
  - `Get(ctx, TaskID) (Task, error)`
  - `List(ctx, ListOptions) ([]Task, error)` — for ops endpoints; v1 may
    leave this empty.
- `Handle` interface — internal-only, returned to the engine when it
  registers a Task (NOT exposed on the public Orchestrator API). The
  engine keeps a `map[TaskID]*Handle` for its lifetime and uses it to
  satisfy the public `Wait`, `Cancel`, and `Snapshot` methods.
  - `ID() TaskID`
  - `Wait(ctx) (Result, error)` — backs `Orchestrator.Wait`
  - `Cancel() error` — backs `Orchestrator.Cancel`
  - `Snapshot() Snapshot` — backs `Orchestrator.Snapshot`
  - `Done() <-chan struct{}` — closed on terminal state; consumed by the
    engine's runtime goroutines and by the event bus to close
    subscriptions.
- Sentinels: `ErrTaskNotFound`, `ErrTaskAlreadyTerminal`,
  `ErrTaskCancelled`.

## Lifecycle

State machine per `DESIGN.md` §5.1.

| From       | To         | Trigger |
|------------|------------|---------|
| (new)      | pending    | `Store.Create` after `Submit` validation |
| pending    | running    | Engine begins driving the root session |
| running    | completed  | Stack drained successfully |
| running    | failed     | Engine reports terminal error |
| running    | cancelled  | `Cancel` called or root ctx done |
| pending    | cancelled  | `Cancel` called before engine started |
| pending    | failed     | Validation failure during `Submit` |

`Update` rejects illegal transitions.

## Wait semantics

- `Handle.Wait(ctx)` blocks until the Task reaches a terminal state.
- Cancelling `ctx` detaches the waiter; the Task continues. This matches
  `DESIGN.md` §3.
- Multiple waiters share the same terminal `Result`; broadcast via a
  `sync.WaitGroup` or closed channel.
- After terminal state, subsequent `Wait` calls return immediately.

## Cancellation

- Each Task owns a `context.CancelFunc` derived from its parent ctx.
- `Cancel()` calls the cancel func and updates state to `cancelled` (if
  not already terminal). The engine observes ctx cancellation in the
  invoker and bridge calls and stops issuing new Requests.
- Cancellation is idempotent. `Cancel` after a terminal state returns nil.
- `Cancel` does not wait for in-flight bridge IO to drain; callers may
  follow up with `Wait` to observe the final state.

## Persistence

- Default `Store` is in-memory. Backed by a `sync.Map` or a sharded map
  guarded by an `sync.RWMutex`.
- The interface is shaped so a Redis/Postgres backend can drop in. A
  durable Store does not, by itself, allow a restarted orchestrator to
  resume in-flight tasks; resumption is a v2 feature that requires
  hydrating the session stack from the audit log.

## Snapshot

- `Snapshot` returns a struct per `DESIGN.md` §6.6 by reading task and
  session stores. It is non-blocking and consistent at the moment of read.
- Includes the current `Stack`, the partial chain tree, and the most
  recent error if any.

## Edge cases & decisions

- `Submit` for an envelope whose body has no Salutation: the orchestrator
  may elect to route to the default workspace via `registry.Default`. This
  policy lives in the engine, not in `task/`. The Store sees the envelope
  unchanged.
- `Cancel` racing with completion: serialize via the Store's `Update`. The
  loser observes `ErrTaskAlreadyTerminal` and returns nil.
- `Wait` after the Task expires from the Store (future v2 GC): return
  `ErrTaskNotFound`. v1 keeps tasks indefinitely.

## Tests to write (no implementation in this pass)

1. Submit → Wait → completed terminal Result.
2. Submit → Cancel → Wait returns cancelled Result with partial tree.
3. Multiple concurrent waiters all observe terminal state.
4. Snapshot before completion reflects in-flight state.
5. Idempotent Cancel.
6. Store round-trip with all fields preserved.
7. Concurrent submission of unrelated tasks under `-race`.

## Out of scope

- Streaming subscription (`events/`).
- Audit recording (`audit/`).
- Engine driver loop (root package).
- Distributed task scheduling.
