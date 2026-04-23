# ANA Unified Message Routing Design

## Background

ANA needs one orchestration path for multiple ingress forms:

- IM bridges
- OpenAI / Anthropic-shaped Web APIs
- scheduled jobs
- webhooks

The system should normalize these requests into one internal message shape, resolve the target workspace, invoke the target agent through a unified gateway, and return a traceable call chain plus final output.

The current repository already defines a canonical agent invocation abstraction in `pkg/bridge`:

- `InvokeRequest`
- `InputPart`
- `Agent` / `SessionAgent`
- `EventStream`

This design builds on that abstraction instead of introducing a second protocol layer for agent execution.

## Goals

- Provide one synchronous invocation path for all first-party ingress types.
- Support prompt-native routing through a strict in-band directive such as `to #Alice`.
- Support agent-to-agent handoff through the same directive.
- Track a full call chain with a shared `session_id` and per-hop `invocation_id`.
- Allow configurable persistence of inputs, outputs, and execution events.
- Leave clean extension points for ACL, push delivery, retries, and richer routing signals.

## Non-Goals

- No event bus or workflow engine in v1.
- No async queue-based orchestration in v1.
- No push/result-delivery subsystem design in v1.
- No retry, fallback, or circuit-breaker strategy in v1.
- No agent-to-agent ACL enforcement in v1, but the design must preserve the insertion point.

## V1 Scope

V1 handles:

1. Ingress normalization
2. Prompt-header route extraction
3. Alias-to-workspace resolution
4. Synchronous invocation chaining
5. Handoff tracking
6. Optional transcript persistence
7. Returning call-chain status and final leaf output

## Recommended Architecture

The system uses a thin layered orchestration pipeline:

`IngressAdapter -> MessageNormalizer -> RouteExtractor -> TargetResolver -> Router -> InvocationOrchestrator -> AgentGateway -> bridge.Agent/SessionAgent -> EventRecorder / ResultAssembler`

### Layer Responsibilities

#### IngressAdapter

Accepts transport-specific requests from IM, Web API, cron, and webhook entrypoints.

Responsibilities:

- auth and source validation
- basic request shaping
- idempotency key extraction
- source metadata collection

It does not parse routing directives and does not invoke agents directly.

#### MessageNormalizer

Converts ingress-specific payloads into one internal `MessageEnvelope`.

Responsibilities:

- normalize text input
- normalize attachment references
- attach source metadata
- map external conversation identifiers to internal session context when available

#### RouteExtractor

Parses only the first non-empty line of the message body.

Valid v1 route directive:

```text
to #Alice
```

or:

```text
to #Alice: <rest of first line message>
```

Rules:

- routing is triggered only by the first non-empty line
- free-form matching in arbitrary body text is not supported
- the directive line is removed from the forwarded body
- if no directive is found, routing falls back to the default entry workspace if configured

This keeps the prompt-native experience without relying on fuzzy text interpretation.

#### TargetResolver

Maps `target_alias` to a stable `workspace_id`.

Responsibilities:

- resolve the alias through a global registry
- return runtime information needed for invocation
- preserve the distinction between explicit route and default entry route

This mapping step is explicit and separate from route parsing.

#### Router

Applies routing policy.

Responsibilities:

- choose explicit alias target when a directive exists
- otherwise choose the configured default entry workspace
- fail fast if neither explicit target nor default entry is available

The router does not manage retries, timeouts, queues, or concurrency policy.

#### InvocationOrchestrator

Drives the synchronous call chain.

Responsibilities:

- create or attach the `session`
- create the root `invocation`
- call the selected target through `AgentGateway`
- inspect completed output for handoff
- create child invocations on handoff
- terminate when a leaf invocation produces a final non-handoff output

This is the layer that corresponds to the desired first-phase scheduler behavior.

#### AgentGateway

Bridges ANA workspace execution to `pkg/bridge`.

Responsibilities:

- choose the runtime adapter by workspace type
- construct `bridge.InvokeRequest`
- open `bridge.Agent` or `bridge.SessionAgent`
- expose results back as canonical `EventStream`

The platform layer never depends on transport details such as REST, SSE, CLI stdout, or WebSocket frames.

#### EventRecorder

Records execution lifecycle data.

Responsibilities:

- invocation status changes
- handoff events
- optional full transcript persistence
- optional summarized transcript persistence

#### ResultAssembler

Builds the caller-facing synchronous response.

Responsibilities:

- return the visible call chain
- return the final leaf output
- optionally include system status lines such as `routed to Alice -> handed off to Bob`

## Routing Protocol

### Directive Form

V1 supports a single explicit route directive in-band:

```text
to #<alias>
```

The directive must appear at the start of the first non-empty line.

Examples:

```text
to #Alice
Please review this patch.
```

```text
to #Alice: Please review this patch.
```

### Alias Rules

- alias is globally unique in v1
- alias is user-facing; internal routing always uses `workspace_id`
- one workspace has one primary alias in v1
- future alias expansion may support multiple aliases per workspace

### Missing Route Behavior

- if an explicit alias is present but cannot be resolved, fail the request
- if no explicit alias is present and a default entry workspace is configured, route to it
- if no explicit alias is present and no default entry workspace is configured, fail the request

## Agent Handoff Semantics

V1 handoff uses the same directive format on agent output.

If a completed agent output starts with:

```text
to #Bob
```

the system treats this as a pure handoff:

- the current invocation is marked `handed_off`
- the directive line is stripped
- the remaining body becomes the input of the next invocation
- a child invocation is created for `Bob`
- the upstream invocation output is not treated as the final user-visible answer

The user sees:

- the call chain
- the final leaf agent output
- optional status messages describing route transitions

This model matches a control-flow transfer instead of a normal reply.

## Execution Model

V1 uses synchronous serial execution.

### Why Serial

- easier to reason about
- easier to debug
- no event bus or work queue required
- matches the current focus on invocation correctness over throughput optimization

### Handoff Detection Strategy

Each invocation output is buffered until that invocation completes.

Then the orchestrator runs `RouteExtractor` against the complete output text to determine whether it is:

- a final answer, or
- a handoff instruction

Because handoff is decided from the completed output header, v1 should not expose raw token streaming from a single invocation directly to the caller.

This is acceptable for v1 and leaves room for future designs that split control events from user-visible deltas.

## Core Data Model

### MessageEnvelope

Represents a normalized inbound request before target resolution.

Suggested fields:

- `message_id`
- `source_type`
- `source_message_id`
- `session_id`
- `input_text`
- `attachments`
- `metadata`
- `received_at`

### RouteDirective

Represents the parsed routing intent. This may remain in-memory.

Suggested fields:

- `target_alias`
- `body_text`
- `is_explicit`
- `raw_header`

### ResolvedTarget

Represents the resolved invocation target after alias mapping.

Suggested fields:

- `workspace_id`
- `workspace_alias`
- `agent_runtime_type`
- `entry_mode`
- `invoke_config_ref`

### Session

Represents one end-to-end call chain triggered by one external request.

Suggested fields:

- `session_id`
- `entry_message_id`
- `origin`
- `status`
- `created_at`
- `finished_at`

### Invocation

Represents one concrete agent execution step.

Suggested fields:

- `invocation_id`
- `session_id`
- `parent_invocation_id`
- `workspace_id`
- `agent_type`
- `input_text`
- `status`
- `started_at`
- `finished_at`
- `error_code`
- `error_message`

### Handoff

Represents one explicit invocation-to-invocation transfer.

Suggested fields:

- `session_id`
- `from_invocation_id`
- `to_invocation_id`
- `target_alias`
- `created_at`

### Transcript

Stores persisted input, output, event, or summary data for an invocation.

Suggested fields:

- `invocation_id`
- `kind`
- `content`
- `content_type`
- `seq`
- `created_at`

### Workspace

Represents a callable runtime target.

Suggested fields:

- `workspace_id`
- `name`
- `runtime_type`
- `runtime_config`
- `enabled`
- `created_at`
- `updated_at`

### WorkspaceAliasBinding

Represents the global alias registry.

Suggested fields:

- `alias`
- `workspace_id`
- `is_primary`
- `created_at`
- `updated_at`

## State Model

### Session State

V1 session states:

- `running`
- `completed`
- `failed`

### Invocation State

V1 invocation states:

- `created`
- `running`
- `handed_off`
- `completed`
- `failed`

### Execution Rules

1. Create or attach a `session` after normalization.
2. Extract explicit route from the incoming message body.
3. Resolve the target through `TargetResolver` or default entry routing.
4. Create the root `invocation`.
5. Invoke the target through `AgentGateway`.
6. Buffer the completed output.
7. Re-run `RouteExtractor` against the completed output.
8. If a handoff directive exists:
   - mark current invocation `handed_off`
   - create `Handoff`
   - resolve next target
   - create child invocation
   - continue synchronously
9. If no handoff directive exists:
   - mark current invocation `completed`
   - mark session `completed`
   - return chain plus final output
10. On any unrecoverable resolution or execution error:
   - mark current invocation `failed`
   - mark session `failed`
   - return error plus visible call-chain state

## Persistence Strategy

Persistence is configurable by deployment.

Recommended v1 options:

- `none`
- `summary`
- `full`

Behavior:

- `none`: keep only minimal execution metadata
- `summary`: store lifecycle events plus summarized input/output
- `full`: store full invocation transcript and relevant events

This keeps storage policy independent from routing and execution logic.

## Error Handling

V1 failures should be explicit and diagnostic.

Examples:

- invalid route directive format
- unresolved alias
- no default entry workspace configured
- workspace disabled
- runtime invocation failure

Errors should include enough context to locate the failure point:

- `session_id`
- `invocation_id` when available
- alias or `workspace_id`
- operation name
- failure reason

## Extension Points Reserved for V2+

The v1 design should preserve clear insertion points for:

- agent-to-agent ACL enforcement between `TargetResolver` and `InvocationOrchestrator`
- hop limits and loop protection in `InvocationOrchestrator`
- retry / timeout / fallback policy as a separate execution-control layer
- push delivery on top of `EventRecorder` / `ResultAssembler`
- additional route signals such as headers or structured metadata
- richer alias models and namespace scopes

## Why This Design

This design intentionally stays small:

- no event-driven over-architecture
- no duplicate agent protocol abstraction
- no fuzzy prompt parsing
- no premature delivery-system design

It is still flexible enough to support the intended ANA direction:

- one front door
- many ingress styles
- prompt-native routing
- unified workspace execution
- traceable multi-agent handoff chains
