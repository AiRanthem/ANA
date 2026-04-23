# AGENTS.md

## Scope
- `pkg/orchestrator` owns ANA's synchronous routing and handoff layer.
- It depends on `pkg/agentio` contracts and the `AgentGateway` interface in `pkg/orchestrator/gateway.go`, not on transport details.

## File Responsibilities
- `types.go`: orchestration domain types only
- `normalize.go`: ingress normalization only
- `route.go`: first-line route extraction only
- `registry.go`: workspace alias registry and target lookup only
- `router.go`: routing policy only
- `record.go`: persistence mode and recorder implementations only
- `gateway.go`: invocation handoff into `agentio.Agent`
- `result.go`: caller-facing result assembly only
- `orchestrator.go`: session/invocation state machine only

## Non-Responsibilities
- No HTTP handlers or IM bridge server code.
- No REST/SSE/stdout/socket frame parsing.
- No retry queue, fallback scheduler, or push delivery in v1.

## Parallel Implementation Boundary
- One worker may own normalization and route extraction.
- One worker may own registry and router.
- One worker may own recorder and result assembly.
- One worker may own the orchestrator state machine after the shared types are merged.
