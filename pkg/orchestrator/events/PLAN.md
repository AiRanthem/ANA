# events/PLAN.md

## Purpose

Best-effort runtime event bus for live subscribers (IM bridges that want
typing indicators, web SSE, console UIs, etc.). The bus is the
counterpart of the audit sink: it receives events only after the owning
component has written the same event to the audit sink. The bus never blocks
the engine or invoker, and it drops events under back-pressure.

## Public surface (intent only)

- `Bus` interface:
  - `Publish(ctx, Event) (delivered int, dropped int)`
  - `Subscribe(ctx, SubscribeOptions) (Subscription, error)`
  - `Close(ctx) error`
- `Subscription` interface:
  - `Events() <-chan Event`
  - `Errors() <-chan error` — surface non-fatal subscriber issues
  - `Dropped() uint64` — cumulative count of events dropped for this subscriber
  - `Close() error`
- `SubscribeOptions`:
  - `TaskID TaskID` — required; subscribers always scope to one task in
    v1. (Cross-task subscriptions are a v2 extension.)
  - `SessionID SessionID` — optional; when set, only matching session
    events are delivered.
  - `RequestID RequestID` — optional; when set, only matching request
    events are delivered. This is normally paired with `SessionID`.
  - `BufferSize int` — per-subscriber channel size; defaults to 256.
  - `IncludeChunks bool` — if false (default), `request.text_chunk` is
    suppressed; useful for slow consumers.
- `Event` struct per `DESIGN.md` §8.2. Event ownership is defined in
  `DESIGN.md` §8: engine owns `task.*`, `session.*`, and `route.directive`;
  invoker owns `request.*`, including streaming chunks.
- Sentinels: `ErrBusClosed`, `ErrSubscribeUnknownTask`.

## Behavior

### Publish path

- The caller of `Publish` is the owning component after a successful
  `audit.Sink.WriteEvent`. `Publish` does not write audit records itself.
- `Publish` enqueues the event to every active subscriber for the given
  `task_id` whose optional `session_id` and `request_id` filters also
  match.
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
- A drop produces a `SubscriberLaggedError` pushed inline onto the
  subscriber's `Errors()` channel. When that channel (buffer size 1) is
  full, the bus drops the older notification and replaces it with the
  newer one — consumers always see the latest cumulative `Dropped()`
  count, never a stale one. They may miss intermediate counts.
- The bus does not implement backoff or rate limiting in v1.

## Subscriber concurrency

- Each subscriber owns a bounded event channel. `Publish` uses
  non-blocking sends to those channels and never waits for consumers to
  read.
- The bus tracks subscribers under a mutex keyed by `(TaskID,
  subscriberID)`. Subscription close is idempotent and safe to race with
  publish.

## Edge cases & decisions

- Multiple subscribers for the same task: each gets its own buffer and
  drop counter. Drops are independent.
- Subscribing after the task ended: returns `ErrSubscribeUnknownTask`. The
  caller can fall back to `Wait` for the final result.
- Publishing with `task_id == ""`: the bus ignores the publish (zero
  delivered, zero dropped). The `Bus.Publish` signature has no error
  return because publishers are best-effort; callers MUST validate
  `task_id` before invoking. Same for an already-cancelled `ctx`.
- Engine shutdown: `Bus.Close` is called by the orchestrator's shutdown
  path; existing subscribers see channel close.

## Tests

1. Subscribe → Publish → Events delivered in order.
2. Slow subscriber drops events, fast subscriber does not.
3. `IncludeChunks = false` filters out `request.text_chunk` events.
4. Task terminal closes subscriber channel.
5. Subscribe after terminal returns `ErrSubscribeUnknownTask`.
6. Session/request scoped subscribers receive only matching events.
7. Concurrent publishers and subscribers under `-race`.

## Out of scope

- Cross-task subscriptions (v2; needs auth / multi-tenancy).
- Persistent replay (audit sink is the replay surface).
- Outbound transports (SSE, WebSocket, gRPC) — those live in ingress
  layers above the orchestrator and consume `Subscription`.
