# invoker/PLAN.md

## Purpose

Execute one Request: take a built `agentio.InvokeRequest`, drive an
`agentio.Agent`, drain the resulting `EventStream`, fan out events to the
audit sink and event bus, and return the aggregated outcome to the engine.

This is the only place inside `pkg/orchestrator` that touches
`agentio.Agent` directly. Everything else operates on session ids and
records.

## Public surface (intent only)

- `Invoker` interface:
  - `Run(ctx, RunInput) (RunOutput, error)`
- `RunInput`:
  - `IDs IDTriple` — `(TaskID, SessionID, RequestID)`.
  - `Workspace registry.Workspace` — for audit context.
  - `Agent agentio.Agent` — already obtained from the registry.
  - `Request agentio.InvokeRequest` — already produced by `prompt/`.
  - `Audit audit.Sink` — fail-fast audit channel.
  - `Bus events.Bus` — best-effort live channel.
- `RunOutput`:
  - `OutputText string` — aggregated text content (Salutation included if
    any; the engine strips it).
  - `Usage agentio.Usage` — if reported by the stream.
  - `EventCount int`.
  - `FinishedAt time.Time`.
- Sentinels: `ErrAgentInvoke`, `ErrAgentStream`, `ErrAuditSink`,
  `ErrCancelled`.

## Behavior

1. Emit `request.created` to audit then bus.
2. Persist the `transcript.input` record to audit (the user-visible
   payload sent to the agent — extracted from `RunInput.Request` parts in
   text form). On error, fail the Request.
3. Call `Agent.Invoke(ctx, Request)`. On error: emit `request.failed`,
   fail.
4. On the first event: emit `request.running` to audit then bus.
5. For each event:
   - `EventTextDelta`: append to `OutputText`, increment `EventCount`,
     emit `request.text_chunk` to bus only.
   - `EventToolCall`, `EventToolResult`: pass through to bus only as
     opaque payloads. Audit gets a `transcript.event_summary` JSON entry
     when the request closes.
   - `EventUsage`: copy to `Usage`.
   - `EventError`: short-circuit; emit `request.failed`, fail.
6. On stream EOF: emit `transcript.output` (the aggregated `OutputText`)
   and a `transcript.event_summary` (JSON of usage + finish reason) to
   audit. Then emit `request.completed` to audit then bus.
7. Always close the `EventStream` (via `defer stream.Close()`).
8. Always honor `ctx.Done()`: a cancelled ctx aborts further reads,
   triggers `request.failed`, and returns `ErrCancelled`.

### Audit ordering

Per Request, the audit sink receives the following sequence (skipping
chunk-level events):

```
request.created
transcript.input
request.running
transcript.output
transcript.event_summary
request.completed
```

On failure the trailing `transcript.output` may be partial and
`request.completed` is replaced by `request.failed`.

### Bus ordering

Per Request, the bus receives:

```
request.created
request.running
request.text_chunk*
[other event passthroughs]
request.completed | request.failed
```

`request.text_chunk` carries the cumulative offset and the new fragment so
subscribers can reconstruct partial text.

## Concurrency

- The invoker does not spawn additional goroutines for a single Request.
  It reads from the `EventStream` synchronously.
- Calls to `audit.Sink.Write` are blocking and must succeed; failures
  propagate as Request failures.
- Calls to `events.Bus.Publish` are non-blocking (`Bus` enqueues to per-
  subscriber buffers). Slow subscribers do not block the invoker.

## Cancellation

- Single source of truth: the engine-supplied `ctx`.
- The invoker MUST honor `ctx.Done()` even if the bridge stalls; the
  bridge contract guarantees that closing the stream returns goroutines.
- A cancelled ctx propagates `ErrCancelled` and lets the engine decide
  whether the Task is `cancelled` or `failed` (cancelled if the task ctx
  was cancelled; failed for any other ctx-derived error).

## Edge cases & decisions

- Stream EOF without any text: a Request can still complete; `OutputText`
  is empty. The engine treats empty plain output as a no-op pop, which is
  legitimate (the agent had nothing to say).
- Bridge returns nil stream and nil error: treated as `ErrAgentInvoke`.
  The agent contract says non-nil stream on success.
- Audit sink saturates: invoker blocks until the sink accepts or returns
  an error. If the engine's audit sink is a queue with a deadline, the
  caller can wrap it with their own timeout policy.
- Bus full: bus drops, invoker proceeds.

## Tests to write (no implementation in this pass)

1. Happy path: text deltas aggregated, audit sequence emitted in order,
   bus sees the same plus chunk events.
2. Bridge invoke error: audit sees `request.created` then
   `request.failed`; transcript records absent except possibly an empty
   `transcript.input`.
3. Mid-stream error event: aggregated text up to that point preserved;
   `request.failed` emitted with error code/message.
4. Context cancellation mid-stream: `request.failed` with cancelled error
   code; stream closed.
5. Audit sink failure: invoker returns `ErrAuditSink`; bus may have
   already received `request.created` (best-effort) — no rollback
   guarantees.
6. Bus saturated: invoker still completes; bus reports drops via metrics.

## Out of scope

- Routing decisions (engine).
- Building the `InvokeRequest` (`prompt/`).
- Stripping Salutations from `OutputText` (engine).
- Retries (v2 wrapper around this `Invoker`).
