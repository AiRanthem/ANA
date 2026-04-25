# pkg/orchestrator/invoker AGENTS.md

## Scope

Single-Request execution surface. Drives an `agentio.Agent`, drains the
`EventStream`, fans events out to the audit sink (fail-fast) and the event
bus (best-effort), and returns the aggregated outcome to the engine.

This is the only package inside `pkg/orchestrator` that touches
`agentio.Agent` directly.

## Rules

- Audit ordering is normative: `request.created`, `transcript.input`,
  `request.running`, ..., `transcript.output`, `transcript.event_summary`,
  `request.completed | request.failed`. Tests must pin this.
- Every successful path closes the `EventStream` (`defer stream.Close()`).
- Every failure path emits exactly one terminal event (`request.failed`)
  plus enough audit transcript to explain the failure.
- Audit sink writes block; bus publishes do not.
- Honor `ctx.Done()`; cancellation must reach the bridge stream.

## Don'ts

- Do not strip Salutations or interpret agent output. The engine inspects
  `OutputText` after `Run` returns.
- Do not retry. Retry is a v2 wrapper concern.
- Do not own the agent lifecycle (creation, pooling). Receive it as a
  parameter; the registry-supplied factory handles construction.
- Do not log message bodies at info level; log only IDs and counts.
  Bodies belong in the audit transcript, not in process logs.
