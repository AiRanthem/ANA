# events/PLAN.md

## Purpose

Best-effort runtime event bus for live subscribers (IM bridges that want
typing indicators, web SSE, console UIs, etc.). The bus is the
counterpart of the audit sink: never blocks the engine, may drop events
under back-pressure.

## Public surface (intent only)

- `Bus` interface:
  - `Publish(ctx, Event) (delivered int, dropped int)`
  - `Subscribe(ctx, SubscribeOptions) (Subscription, error)`
  - `Close(ctx) error`
- `Subscription` interface:
  - `Events() <-chan Event`
  - `Errors() <-chan error` — surface non-fatal subscriber issues
  - `Close() error`
- `SubscribeOptions`:
  - `TaskID TaskID` — required; subscribers always scope to one task in
    v1. (Cross-task subscriptions are a v2 extension.)
  - `BufferSize int` — per-subscriber channel size; defaults to 256.
  - `IncludeChunks bool` — if false (default), `request.text_chunk` is
    suppressed; useful for slow consumers.
- `Event` struct per `DESIGN.md` §8.2.
- Sentinels: `ErrBusClosed`, `ErrSubscribeUnknownTask`.

## Behavior

### Publish path

- `Publish` enqueues the event to every active subscriber for the given
  `task_id` (subscribers scoped to a different task ignore the publish).
- For each subscriber, if its channel buffer is full, the event is
  dropped for that subscriber and the dropped counter is incremented.
- `Publish` never blocks. The engine and invoker rely on this property.

### Subscribe path

- A new subscription receives events from the moment of subscription
  forward; no replay. Callers that need a complete history use the audit
  sink instead.
- `Events()` is closed when the task reaches a terminal state or when the
  subscriber calls `Close()`.
- `Errors()` carries non-fatal info such as drop notices. Slow subscribers
  see `ErrSubscriberLagged`.

### Close

- `Bus.Close` notifies all subscribers (closes their `Events()` channels)
  and drains in-flight publishes. After `Close`, further `Publish` calls
  return zero counts; further `Subscribe` calls return `ErrBusClosed`.

## Drop policy

- v1 uses pure non-blocking buffered channels per subscriber. No coalescing
  of chunk events.
- Dropped counts are published on `Errors()` periodically (e.g., every 1
  second per subscriber) so consumers know they fell behind.
- The bus does not implement backoff or rate limiting in v1.

## Subscriber concurrency

- Each subscriber runs in its own goroutine started by `Subscribe`.
- The goroutine forwards events from the bus's internal queue to the
  subscriber's channel. On subscriber close, it drains and exits.
- The bus tracks subscribers under an `sync.Map` keyed by `(TaskID,
  subscriberID)`.

## Edge cases & decisions

- Multiple subscribers for the same task: each gets its own buffer and
  drop counter. Drops are independent.
- Subscribing after the task ended: returns `ErrSubscribeUnknownTask`. The
  caller can fall back to `Wait` for the final result.
- Publishing with `task_id == ""`: the bus rejects the publish. All
  events must be task-scoped.
- Engine shutdown: `Bus.Close` is called by the orchestrator's shutdown
  path; existing subscribers see channel close.

## Tests to write (no implementation in this pass)

1. Subscribe → Publish → Events delivered in order.
2. Slow subscriber drops events, fast subscriber does not.
3. `IncludeChunks = false` filters out `request.text_chunk` events.
4. Task terminal closes subscriber channel.
5. Subscribe after terminal returns `ErrSubscribeUnknownTask`.
6. Concurrent publishers and subscribers under `-race`.

## Out of scope

- Cross-task subscriptions (v2; needs auth / multi-tenancy).
- Persistent replay (audit sink is the replay surface).
- Outbound transports (SSE, WebSocket, gRPC) — those live in ingress
  layers above the orchestrator and consume `Subscription`.
