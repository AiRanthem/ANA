# ANA Orchestrator Design

> **Status:** v1 design, no implementation yet. Implementation tasks should follow
> the per-module `PLAN.md` files together with this document.
>
> **Audience:** ANA contributors implementing or extending the orchestration layer.

## 1. Background & Goals

ANA is a Token-native multi-agent system. Workspaces (concrete agent instances
such as `Alice` or `Bob`) collaborate by emitting and consuming an in-band
delegation directive named the **Epistolary protocol** — an output that begins
with `{to #<alias>}` is treated as a delegation to the named partner. ANA's job
is to receive such Epistolary inputs from the user, route them to the correct
workspace, drive the resulting delegation chain, return the leaf output back up
the stack, and finally deliver it to the user.

`pkg/orchestrator` owns this routing and chaining loop. It sits above
`pkg/agentio` (canonical request/event/session contracts) and `pkg/bridge/...`
(transport adapters), and below the ingress layer (IM bridges, web APIs,
schedulers, webhooks).

### 1.1 Goals

- One internal pipeline that handles every ingress form once it is normalized
  into `MessageEnvelope`.
- Strict, prompt-native routing via the first non-empty line directive
  `{to #<alias>}` (the **Salutation**).
- A first-class **Task** for every user request, with traceable
  `(task_id, session_id, request_id)` triples for every transport call.
- A first-class **Session** lifecycle that mirrors a workspace's continuous
  reasoning thread within a task, including correct stack-based resume
  semantics on no-Salutation returns.
- Asynchronous task creation with **streaming** lifecycle events, so callers
  (IM bridges, web SSE, console UI) can surface intermediate progress.
- Two distinct observability tiers:
  - A **best-effort runtime event bus** for live subscribers (drops slow
    consumers).
  - A **fail-fast audit sink** that durably records every transport call and
    every transcript boundary.
- Configurable loop / depth protection.
- Cooperative cancellation that propagates to in-flight bridge calls via
  `context.Context`.
- A clean upgrade path for v2 features (ACL between agents, retries, push
  delivery, distributed task store, multi-alias workspaces).

### 1.2 Non-Goals

- No new transport adapters (those live in `pkg/bridge/...`).
- No event bus / workflow engine inside the orchestrator (out-of-process).
- No multi-tenancy controls in v1 (one orchestrator instance == one tenant).
- No per-agent ACL enforcement in v1 (insertion point preserved between the
  registry and the engine).
- No retry, fallback, or circuit-breaker policy in v1.
- No durable task survival across orchestrator restarts (the audit sink is the
  external persistence path).

## 2. Concepts & Vocabulary

### 2.1 Epistolary protocol

A **Salutation** is the literal token sequence `{to #<alias>}` that appears at
the start of the **first non-empty line** of a message body. The body that
follows the directive line is the **payload**.

Inputs are classified as:

- **Epistolary input** — a body whose first non-empty line is a Salutation.
  ANA accepts Epistolary inputs only from the **user**; agent-to-agent
  delegation appears as Epistolary output that ANA promotes to the next
  agent's Epistolary input.
- **Epistolary output** — an agent's reply whose first non-empty line is a
  Salutation. The orchestrator consumes the Salutation; downstream agents
  never see it.
- **Plain output** — an agent reply that does not start with a Salutation.
  Treated as a return up the call stack.

The orchestrator is the only component that mints Epistolary inputs for
workspaces. Workspaces never see another workspace's Salutation; the directive
is always stripped before forwarding.

### 2.2 Task

A **Task** is the entire call tree triggered by one user Epistolary input. It
ends when the engine produces the final plain output addressed back to the
caller (the user). A Task owns:

- A unique `task_id`.
- A `MessageEnvelope` (the original normalized user input).
- A session tree (sessions plus parent-session edges).
- A stream of lifecycle events (live + audit).
- A final `Result` (or terminal error) once finished.
- A cancellation `context.Context` rooted at submission time.

### 2.3 Session

A **Session** is a workspace's continuous reasoning thread inside one Task.
The orchestrator decides session boundaries as follows:

- **Open** when the orchestrator routes a *new* Salutation to the workspace.
  This includes the root user input and any peer-to-peer `{to #X}` from a
  different workspace. Each opening creates a new session distinct from
  previous sessions of the same workspace.
- **Resume** when a callee returns plain output and the call stack pops back
  to this session. Resuming reuses the same `session_id`; a new `Request` is
  issued under it carrying the callee's plain output as input.
- **Close** when the workspace produces a plain output that pops the stack
  (the session has produced its final answer for this thread).

Each session has:

- `task_id` and `session_id`.
- `workspace_id` and `workspace_alias`.
- `parent_session_id` — the session that delegated to this one (empty for the
  root session of a task).
- `initiator_alias` — the workspace alias that delegated *to* this session
  (`""` if root, the user-facing alias otherwise).
- `status` — see §5.2.
- `Requests []RequestRef` — the list of transport calls executed within this
  session.

A session is the unit of "context continuity" for a workspace. Implementations
that resume native CLI sessions (`claude code --resume <id>`) MUST map their
native session id 1:1 to the orchestrator `session_id`. See §7.

### 2.4 Request

A **Request** is one transport-level invocation. It owns:

- `task_id`, `session_id`, `request_id` — the auditable triple.
- The constructed `agentio.InvokeRequest` (post prompt-builder).
- The collected stream output (text + events).
- Status, start/finish times, error fields.

Within a Session there can be multiple Requests because the agent may emit a
Salutation, wait for a callee, then resume with the callee's plain output.
Each "agent inference round" is one Request. A Request never spans a
Salutation handoff: as soon as the agent's stream completes, the engine
inspects the final text to decide the next move.

### 2.5 Workspace

A **Workspace** is the registered metadata for a callable agent instance.
Owned by the registry module. Holds:

- `workspace_id`, `alias`, `runtime_type` (e.g., `cli`, `rest`, `socket`).
- `runtime_kind` — declared by the workspace, see §7.2:
  - `chat_api` (stateless, role-aware messages, full history each call).
  - `resumable_cli` (stateless, `--resume <session_id>`).
  - `socket_session` (stateful long-lived connection).
- `enabled`, `is_default_entry`.
- `description` — short partner blurb used in Notes.
- `runtime_config` — opaque map consumed by the bridge factory.

### 2.6 Call stack

For every active Task the engine maintains a **call stack** of sessions. The
top-of-stack session is the only one currently running. When a session
produces:

- A **Salutation output** — the engine pushes a new session for the target,
  with `parent_session_id = current_session_id`. The current session
  transitions to `paused` until the callee's reply arrives.
- A **plain output** — the engine pops the current session, marks it
  `closed`, and resumes the new top-of-stack session by issuing a new Request
  to its workspace, carrying the popped session's plain output as the input.
  When the stack is empty, the plain output is the final user-visible result.

This is the literal interpretation of "no Salutation = return to the previous
speaker" from the spec.

## 3. Public API (Sketch)

The orchestrator exposes four methods. Concrete signatures are settled in
`orchestrator.go` during implementation; the shape is fixed here.

```go
// MessageEnvelope is the normalized inbound message handed to the
// orchestrator by ingress adapters. Defined in pkg/orchestrator types.go.
type MessageEnvelope struct { /* see §6.1 */ }

// TaskID uniquely identifies a Task within an orchestrator instance.
type TaskID string

// Result is the final caller-facing output of a completed Task.
type Result struct { /* see §6.5 */ }

// Snapshot is a read-only view of a Task at a point in time.
type Snapshot struct { /* see §6.6 */ }

// Orchestrator is the public surface.
type Orchestrator interface {
    // Submit creates a Task, wires up its event stream, and returns
    // immediately. The Task continues to run in the background until it
    // completes, fails, or is cancelled.
    Submit(ctx context.Context, env MessageEnvelope) (TaskID, error)

    // Subscribe attaches a best-effort live event subscriber. Slow
    // subscribers may miss events. Multiple subscribers per Task are
    // supported.
    Subscribe(ctx context.Context, id TaskID) (events.Subscription, error)

    // Wait blocks until the Task reaches a terminal state and returns
    // the final Result. Cancelling ctx detaches Wait but does not cancel
    // the Task; use Cancel for that.
    Wait(ctx context.Context, id TaskID) (Result, error)

    // Cancel transitions the Task toward cancelled. In-flight transport
    // calls are interrupted via ctx propagation.
    Cancel(ctx context.Context, id TaskID) error

    // Snapshot returns the current state of a Task without blocking.
    Snapshot(ctx context.Context, id TaskID) (Snapshot, error)
}
```

The constructor takes an explicit dependency bag so that every collaborator is
visible and replaceable in tests:

```go
type Dependencies struct {
    Registry      registry.Registry
    PromptBuilder prompt.Builder
    Invoker       invoker.Invoker
    AuditSink     audit.Sink         // required, fail-fast
    EventBus      events.Bus         // required, best-effort
    TaskStore     task.Store         // required (default: in-memory)
    SessionStore  session.Store      // required (default: in-memory)
    IDs           idgen.Generator    // required
    Clock         func() time.Time   // optional, defaults to time.Now
    Logger        *slog.Logger       // optional, defaults to slog.Default
    MaxSessions   int                // 0 → use orchestrator default (32); cumulative cap, see §10.3
}

func New(deps Dependencies) (Orchestrator, error)
```

## 4. Walkthrough — The User's Example End-to-End

Below is the canonical flow from the spec, annotated with the `(task_id,
session_id, request_id)` triple and the call-stack mutations.

> User → ANA: `{to #Alice} check stock prices`

```
Submit creates Task T1 with envelope {body: "{to #Alice} check stock prices"}.
Engine extracts directive {alias=Alice, payload="check stock prices"}.
Stack: []                                       # empty
push S1{ws=Alice, parent=∅}
Stack: [S1]

R1 ← invoker.Run(T1, S1, payload="check stock prices")
        prompt-builder for Alice (assume Alice is resumable_cli):
            user-text "check stock prices"
            preamble: Notes block listing Bob (excluding Alice)
        bridge.cli.Agent.Invoke(req: {session_id=S1.id, ...})
        ← agent output: "{to #Bob} What stocks has the user purchased?"

Engine inspects R1 final text → Salutation detected (alias=Bob).
S1 → paused.
push S2{ws=Bob, parent=S1.id}
Stack: [S1, S2]
```

> Engine routes Bob's input via R2.

```
R2 ← invoker.Run(T1, S2, payload="What stocks has the user purchased?")
        prompt-builder for Bob (assume Bob is chat_api):
            system message: Notes block listing Alice (excluding Bob)
            user message: payload
        bridge.rest.Agent.Invoke(req: {session_id=S2.id, ...})
        ← agent output: "NVIDIA, NVDA"

Engine inspects R2 final text → no Salutation (plain output).
S2 → closed.
pop S2
Stack: [S1]                                     # S1 will resume

S1 resume: R3 ← invoker.Run(T1, S1, payload="NVIDIA, NVDA")
        bridge.cli.Agent.Invoke(req: {session_id=S1.id, ...})
            cli.Agent.BuildArgs sees session_id=S1.id and adds --resume
        ← agent output: "NVIDIA's stock price today is roughly $208 per share."

Engine inspects R3 final text → no Salutation.
S1 → closed.
pop S1
Stack: []                                       # task done
T1 → completed; Result.FinalOutput = "NVIDIA's stock price today is ..."
```

Observations:

- Alice has one session (`S1`) but **two** Requests (`R1`, `R3`). The
  `--resume` flag on `R3` is what gives Alice's CLI process the same
  reasoning context.
- Bob has one session (`S2`) and one Request (`R2`).
- The triple `(T1, S1, R1)`, `(T1, S2, R2)`, `(T1, S1, R3)` uniquely locates
  every transport call.

## 5. State Machines

### 5.1 Task

```
            ┌──────────┐
Submit() →  │ pending  │
            └────┬─────┘
                 │ engine starts
            ┌────▼─────┐    cancel       ┌───────────┐
            │ running  │────────────────▶│ cancelled │
            └─┬──┬───┬─┘                 └───────────┘
              │  │   │ engine error
              │  │   └────────▶  ┌────────┐
              │  │               │ failed │
              │  │               └────────┘
              │  │ stack drains
              │  ▼
              │ ┌────────────┐
              │ │ completed  │
              │ └────────────┘
              │
              │ stack still has frames AND ctx done
              ▼
              ┌───────────┐
              │ cancelled │
              └───────────┘
```

Terminal states: `completed`, `failed`, `cancelled`.

### 5.2 Session

```
                  Engine pushes new session
   ┌────────────┐      ┌────────────────┐
   │ (does not  │ ──▶  │   running      │
   │  exist)    │      └─┬──────────┬───┘
   └────────────┘        │          │ Salutation in output → push child
                         │          ▼
                         │       ┌──────────┐  child closes
                         │       │ paused   │ ◀──────────┐
                         │       └────┬─────┘            │
                         │            │ child popped     │
                         │            ▼                  │
                         │       ┌──────────┐            │
                         │       │ running  │ ───────────┘
                         │       └─┬────────┘
                         │ plain output
                         ▼
                       ┌──────────┐
                       │ closed   │
                       └──────────┘

         Any Request error inside the session
         → session transitions to "failed" and propagates to Task.
```

Terminal states: `closed`, `failed`.

### 5.3 Request

```
   Engine starts Request
   ┌─────────────────────┐
   │ created             │
   └─────┬───────────────┘
         │ invoker dispatches
   ┌─────▼─────┐
   │ running   │──▶ stream events ─┐
   └─────┬─────┘                   │
         │ Stream EOF               │
         ▼                          │
   ┌────────────┐                   │
   │ completed  │                   │
   └────────────┘                   │
                                    │ stream error / ctx cancel
                                    ▼
                              ┌─────────┐
                              │ failed  │
                              └─────────┘
```

A Request never re-enters running; retries (out of v1) would be a different
Request id under the same session.

## 6. Core Data Model

Concrete fields are owned by individual modules; the table below is the
authoritative source for cross-module field names. Modules may add internal
fields but must not rename or repurpose the ones below.

### 6.1 `MessageEnvelope` (root types.go)

| Field             | Type                       | Notes |
|-------------------|----------------------------|-------|
| `MessageID`       | `string`                   | Stable id from ingress |
| `SourceType`      | `SourceType`               | im / openai_api / anthropic_api / schedule / webhook |
| `SourceMessageID` | `string`                   | External id for audit cross-reference |
| `BodyText`        | `string`                   | Raw text body, untrimmed |
| `Attachments`     | `[]agentio.InputPart`      | Optional non-text parts forwarded to the first session |
| `Metadata`        | `map[string]string`        | Source-specific metadata |
| `ReceivedAt`      | `time.Time`                | Ingress timestamp |

The orchestrator does NOT own ingress normalization. It accepts already-
normalized envelopes. Ingress adapters live elsewhere and call `Submit`.

### 6.2 `RouteDirective` (`protocol/`)

| Field         | Type     | Notes |
|---------------|----------|-------|
| `TargetAlias` | `string` | Alias from the Salutation; empty if no directive |
| `Payload`     | `string` | Body without the Salutation line |
| `IsExplicit`  | `bool`   | True when a valid Salutation was found |
| `RawHeader`   | `string` | The original Salutation line for audit |

Routing rules (`protocol/PLAN.md` for parser specifics):

- Only the first non-empty line is inspected.
- `{to #<alias>}` MUST be the very first non-whitespace token of that line.
- Whitespace and any inline payload after the directive on the same line are
  trimmed and prepended to the rest of the body.
- A first-line beginning with `to ` (loose form) but failing the strict
  Salutation regex is rejected as `ErrInvalidRouteDirective`. We do not fall
  back to "treat as plain text" to avoid silent route failures.

### 6.3 `Workspace` (`registry/`)

| Field            | Type              | Notes |
|------------------|-------------------|-------|
| `WorkspaceID`    | `string`          | Stable internal id |
| `Alias`          | `string`          | Globally unique, user-facing |
| `RuntimeType`    | `string`          | `cli` / `rest` / `socket` |
| `RuntimeKind`    | `RuntimeKind`     | `chat_api` / `resumable_cli` / `socket_session` |
| `Description`    | `string`          | Used in Notes preamble |
| `Enabled`        | `bool`            | Disabled workspaces fail routing |
| `IsDefaultEntry` | `bool`            | At most one per registry |
| `RuntimeConfig`  | `map[string]any`  | Opaque to orchestrator, consumed by AgentFactory |

### 6.4 `Session` and `Request` (`session/`)

`Session` fields per §2.3.

`Request`:

| Field          | Type             | Notes |
|----------------|------------------|-------|
| `TaskID`       | `TaskID`         |       |
| `SessionID`    | `SessionID`      |       |
| `RequestID`    | `RequestID`      |       |
| `WorkspaceID`  | `string`         | For audit cross-reference |
| `Kind`         | `RequestKind`    | `root` / `delegation` / `resume` (see §6.7) |
| `InputText`    | `string`         | Engine-level user payload (the post-Salutation text). The fully-rendered `agentio.InvokeRequest` (with Notes preamble) is NOT held here; it lives only in the audit transcript |
| `OutputText`   | `string`         | Aggregated plain text (Salutation NOT yet stripped; the engine inspects it to decide push vs pop) |
| `EventCount`   | `int`            |       |
| `Status`       | `RequestStatus`  | created / running / completed / failed |
| `StartedAt`    | `time.Time`      |       |
| `FinishedAt`   | `time.Time`      |       |
| `ErrorCode`    | `string`         |       |
| `ErrorMessage` | `string`         |       |

The full `agentio.InvokeRequest` and the canonical event stream go through
the audit sink as transcript records (§9), not into the in-memory request
struct, to keep the orchestrator memory budget bounded.

### 6.5 `Result`

| Field          | Type           | Notes |
|----------------|----------------|-------|
| `TaskID`       | `TaskID`       |       |
| `Status`       | `TaskStatus`   | completed / failed / cancelled |
| `FinalOutput`  | `string`       | Empty when not completed |
| `Tree`         | `[]ChainStep`  | Pre-order traversal of the session tree |
| `Error`        | `*ResultError` | Non-nil when `Status != completed` |
| `Hops`         | `int`          | Total sessions opened during the task |
| `FinishedAt`   | `time.Time`    |       |

`ChainStep` mirrors the session tree:

| Field             | Type        | Notes |
|-------------------|-------------|-------|
| `SessionID`       | `SessionID` |       |
| `ParentSessionID` | `SessionID` | Empty for root |
| `WorkspaceAlias`  | `string`    |       |
| `Status`          | `SessionStatus` |   |
| `Requests`        | `int`       | Number of Requests in this session |

### 6.6 `Snapshot`

A `Snapshot` is `Result` plus the live state for an in-flight Task:

| Field           | Type              | Notes |
|-----------------|-------------------|-------|
| `TaskID`        | `TaskID`          |       |
| `Status`        | `TaskStatus`      |       |
| `Stack`         | `[]SessionID`     | Bottom-up call stack |
| `Tree`          | `[]ChainStep`     |       |
| `LastError`     | `*ResultError`    | Latest error if any |
| `UpdatedAt`     | `time.Time`       |       |

### 6.7 `RequestKind`

`RequestKind` records why a Request exists. The type is defined in
`session/` (next to `Request`). It is set by the engine when issuing the
Request and consumed by the prompt builder (`prompt/PLAN.md`) to decide
whether to re-emit the Notes preamble.

| Value         | Meaning |
|---------------|---------|
| `root`        | First Request of the Task on the root session. Notes preamble is included. |
| `delegation`  | First Request on a freshly-pushed session (peer-to-peer Salutation). Notes preamble is included. |
| `resume`      | Subsequent Request on a session that just had a callee pop. Notes preamble is omitted to preserve prefix-cache continuity; bridges retain prior turns via their session-state stores (see §7.3). |

### 6.8 ID types

`TaskID`, `SessionID`, `RequestID`, and the loose `EventID string` are
defined in `pkg/orchestrator/idgen` and re-exported by the root
`pkg/orchestrator/types.go` for ergonomics. Subpackages that need IDs
import `idgen`; they MUST NOT import the root package to obtain them
(prevents cycles between the engine and audit/events).

## 7. Required Upstream Contract Changes

The orchestrator design depends on three concrete extensions to existing
packages. These are NOT implemented in this design pass; they are surfaced
here so the orchestrator implementation can rely on them, and so a separate
work-package can land them.

### 7.1 `pkg/agentio` — Per-part role on `InputPart`

**Why:** The Prompt Builder needs to express "this part is system, that part
is user" in one `InvokeRequest` so chat-API bridges can map it to OpenAI /
Anthropic role-tagged messages and benefit from prefix cache hits. The
current `InvokeRequest.Role` is request-level, not part-level.

**Recommended shape:**

- Add a `Role() Role` method on the `InputPart` interface, defaulting to
  `RoleUser` when not implemented.
- Provide a new struct `RoledTextPart{Role Role; Text string}` for the common
  case.
- Existing `TextPart`, `JSONPart`, `BlobPart`, `ToolResultPart` keep their
  signatures; they implicitly return `RoleUser`.

**Adapter behavior:**

- `bridge/rest`: when serializing for chat APIs, group consecutive parts by
  role into messages; map `RoleSystem` to the system message.
- `bridge/cli`: ignores role; `RoledTextPart` flattens to text in the order
  given (the prompt-builder is responsible for placing system content first).

### 7.2 `pkg/bridge/cli` — Dynamic args via `BuildArgs`

**Why:** Resumable CLI agents (`claude code --resume <id>`) need session-
specific arguments. The current `cli.Agent.Args` is fixed at construction.

**Recommended shape:**

- Add `BuildArgs func(*agentio.InvokeRequest) ([]string, error)` to
  `cli.Agent`. When non-nil it overrides `Args`.
- Default behavior unchanged when `BuildArgs` is nil.
- The orchestrator constructs `InvokeRequest.SessionID` from the
  orchestrator session id, so the CLI adapter can read it directly.

### 7.3 `pkg/bridge/rest` and `pkg/bridge/socket` — Session-state stores

**Why:** With `BRIDGE_OWNS` resumption, stateless transports need to remember
session state across calls. Specifically:

- Chat-API REST adapters need a per-session message history so the next call
  prepends prior turns.
- Socket adapters need a connection pool keyed by session id so the same
  physical connection serves the same logical session.

**Recommended shape:**

- New `bridge/rest.HistoryStore` interface:

  ```go
  type HistoryStore interface {
      Load(ctx context.Context, sessionID string) ([]agentio.InputPart, error)
      Append(ctx context.Context, sessionID string, parts []agentio.InputPart, output string) error
      Drop(ctx context.Context, sessionID string) error
  }
  ```

  Default in-memory implementation `rest.MemoryHistoryStore`.

- New `bridge/socket.SessionPool` interface:

  ```go
  type SessionPool interface {
      Acquire(ctx context.Context, sessionID string, dial func(context.Context) (JSONSocket, error)) (JSONSocket, error)
      Release(sessionID string) error
  }
  ```

  Default in-memory implementation `socket.MemorySessionPool`.

These interfaces let the orchestrator pass the same `session_id` into bridge
calls and trust that the adapter handles continuity.

### 7.4 Migration order

1. Land `pkg/agentio` per-part role.
2. Land `pkg/bridge/cli` `BuildArgs`.
3. Land `pkg/bridge/rest.HistoryStore` and `pkg/bridge/socket.SessionPool`.
4. Implement `pkg/orchestrator` per the per-module PLANs.

The orchestrator implementation can mock these contracts during early tests
but the integration tests against real bridges block on (1)–(3) shipping.

### 7.5 Bridge transport invariants (correctness guarantees)

These are behavioral requirements on `pkg/bridge/...` implementations so the
orchestrator and ingress layers can rely on deterministic teardown and
subprocess hygiene. They complement the structural extensions in §7.2–§7.3.

**Socket (`pkg/bridge/socket`) — drain before terminal close**

- Implementations of `agentio.Session` backed by a socket read loop MUST NOT
  let `Recv` return a terminal stream-closed error while events are still
  queued in the session's delivery channel.
- Concretely: when the read side finishes and signals shutdown, `Recv` must
  prefer delivering buffered events (including final `error` / `done`-style
  frames) over reporting `ErrStreamClosed` (or equivalent), so a `select`
  that races `closed` against the event channel does not drop terminal
  diagnostics nondeterministically.

**CLI (`pkg/bridge/cli`) — environment inheritance**

- When `cli.Agent` (or equivalent) accepts caller-supplied `Env` entries, the
  child process environment MUST be formed by merging those entries onto the
  parent’s inherited environment (for example `os.Environ()`), with
  caller-supplied keys overriding duplicates.
- Building `cmd.Env` from an empty base when overrides are present is
  forbidden: it strips `PATH`, `HOME`, TLS trust roots, and other inherited
  variables and breaks typical CLI tools and nested subprocesses.

## 8. Event Taxonomy

Two channels share an event-type namespace. All events are emitted by the
engine and dispatched in this order:

1. `audit.Sink` (synchronous, must succeed).
2. `events.Bus` (asynchronous, best-effort).

Failure semantics:

- If `audit.Sink` returns an error, the engine fails the current Request and
  transitions the session and task to `failed`. This is the primary
  back-pressure path.
- If `events.Bus` drops a message (e.g., subscriber buffer full), the bus
  records a metric counter but the engine proceeds.

### 8.1 Event types

| Type                   | Trigger                                                           |
|------------------------|-------------------------------------------------------------------|
| `task.created`         | Synchronously inside `Submit`, before it returns the TaskID; if the audit sink rejects, `Submit` returns an error and no TaskID is exposed |
| `task.running`         | When the engine begins driving the first session                  |
| `task.completed`       | Stack drained with success                                        |
| `task.failed`          | Any unrecoverable error                                           |
| `task.cancelled`       | `Cancel` called or context expired                                |
| `session.opened`       | Engine pushes a session onto the stack                            |
| `session.paused`       | Salutation found in current session output                        |
| `session.resumed`      | Engine re-issues a Request to a previously paused session         |
| `session.closed`       | Engine pops the session                                           |
| `session.failed`       | Request inside the session terminally failed                      |
| `request.created`      | Right before the invoker dispatches                               |
| `request.running`      | Invoker received the first event from the bridge                  |
| `request.completed`    | Stream EOF with success                                           |
| `request.failed`       | Stream EOF with error / ctx cancel                                |
| `request.text_chunk`   | Aggregated text delta (best-effort live, not on audit)            |
| `route.directive`      | Salutation parsed from incoming envelope or agent output          |

The `request.text_chunk` event is **bus-only** to avoid flooding the audit
log with per-token chunks. Audit gets the aggregated `OutputText` once the
request completes (see §9).

### 8.2 Event schema

Every event carries:

```go
type Event struct {
    EventID    string
    TaskID     TaskID
    SessionID  SessionID  // empty when type == task.*
    RequestID  RequestID  // empty when type == task.* / session.*
    Type       EventType
    OccurredAt time.Time
    Payload    EventPayload // typed per Type
}
```

The full type list, payload shapes, and ordering guarantees live in
`events/PLAN.md` and `audit/PLAN.md`.

## 9. Audit Sink Contract

The audit sink is the canonical record of "every transport call and every
transcript boundary". It is required (no nullable). Implementations may write
to file, database, or a remote pipeline, but must satisfy:

- **Completeness:** every event listed in §8.1 except `request.text_chunk`
  MUST be delivered.
- **Ordering:** events for the same `task_id` are delivered in the order they
  were generated. Cross-task ordering is best-effort.
- **Synchronous semantics:** `Sink.WriteEvent` and `Sink.WriteTranscript`
  MUST return only after the implementation has accepted responsibility
  for durability. Errors are fatal to the in-flight Request.
- **Idempotence:** the orchestrator MUST NOT generate duplicate events for
  the same logical step, but implementations are encouraged to dedup on
  `EventID` for safety.

In addition to event records, the audit sink receives transcript records:

| Field           | Type             | Notes |
|-----------------|------------------|-------|
| `TaskID`        | `TaskID`         |       |
| `SessionID`     | `SessionID`      |       |
| `RequestID`     | `RequestID`      |       |
| `Kind`          | `TranscriptKind` | input / output / event_summary |
| `Content`       | `[]byte`         | UTF-8 text or JSON depending on Kind |
| `ContentType`   | `string`         | `text/plain` / `application/json` |
| `Seq`           | `int`            | Monotonic per (task, session, request) |
| `Schema`        | `string`         | Schema version label for forward compat (`v1`) |
| `CreatedAt`     | `time.Time`      |       |

The orchestrator persists three transcript records per Request:

1. `input` — the user-visible payload sent to the agent (no Notes preamble;
   that is reproducible from registry state).
2. `output` — the agent's raw output text (Salutation included if any).
3. `event_summary` — a JSON blob summarizing usage, error, finish reason.

The audit sink interface is defined in `audit/PLAN.md`.

## 10. Concurrency, Cancellation, Loop Protection

### 10.1 Concurrency

- Tasks run in parallel; the orchestrator instance is safe for concurrent
  callers.
- Within one Task, sessions execute serially by definition (call-stack model);
  there is exactly one running session per task at any instant.
- Internal stores (`task.Store`, `session.Store`) protect their state with
  per-task locks, not a global mutex.
- The engine never blocks on `events.Bus`. If a subscriber stalls the bus,
  the bus drops events for that subscriber.

### 10.2 Cancellation

- `Submit` derives a per-task `context.Context` from the caller's ctx that is
  cancelled when:
  - the caller's ctx is done, or
  - `Cancel(taskID)` is called explicitly.
- The per-task ctx is passed through the invoker into bridge calls, so
  `bridge.cli`, `bridge.rest`, and `bridge.socket` interrupt their I/O.
- `Wait(ctx, taskID)` decouples the caller's ctx from task lifetime; the
  caller can stop waiting without cancelling the task.

### 10.3 Loop protection

- Every Task carries a cumulative counter `OpenedSessions`. The engine
  increments it on every `session.opened` event and never decrements it.
- When the counter exceeds `Dependencies.MaxSessions` (default `32`), the
  engine refuses to push a new session, fails the current session with
  `ErrMaxSessionsExceeded`, and propagates to Task `failed` with the
  visible call tree intact.
- A cumulative counter is preferred over a peak-stack-depth counter
  because pathological loops `A → B → A → B → ...` and breadth-first
  fan-outs `A → B (pop) → C (pop) → D ...` both grow the cumulative count
  even when stack depth stays small.
- v2 may add cycle detection (alias revisits, structural patterns). The
  current limit is intentionally simple and forgiving.

## 11. Error Model

### 11.1 Sentinel errors

Defined at the package boundaries:

- `protocol.ErrInvalidRouteDirective`
- `registry.ErrAliasNotFound`
- `registry.ErrWorkspaceDisabled`
- `registry.ErrNoDefaultWorkspace`
- `session.ErrMaxSessionsExceeded`
- `session.ErrSessionNotFound`
- `task.ErrTaskNotFound`
- `task.ErrTaskAlreadyTerminal`
- `invoker.ErrAgentInvoke`
- `invoker.ErrAgentStream`
- `audit.ErrSinkClosed`
- `audit.ErrSinkBackpressure`

Module-level rules: every wrapper MUST attach context via
`fmt.Errorf("<op>: %w", err)`. Engine logs include the
`(task_id, session_id, request_id)` triple plus the workspace alias.

### 11.2 Failure propagation

| Source                   | Effect on Request | Effect on Session | Effect on Task |
|--------------------------|-------------------|-------------------|----------------|
| Bridge transport error   | failed            | failed            | failed         |
| Stream `EventFailure`    | failed            | failed            | failed         |
| Audit sink write error   | failed            | failed            | failed         |
| Loop protection trip     | n/a               | failed            | failed         |
| Alias not found (route)  | n/a               | failed            | failed         |
| Engine programmer bug    | failed            | failed            | failed         |

A failed task always emits a `Result` with the partial tree assembled so far
plus the `*ResultError` describing the failure point.

## 12. Persistence

- Default deployment uses in-memory `task.Store`, `session.Store`, and an
  in-memory audit sink (suitable for tests and single-node demos).
- Production deployments are expected to replace the audit sink with a
  durable implementation. Restart survival of in-flight tasks is **not** a
  v1 goal; consumers that need it can rebuild from the audit log.
- The store interfaces are pluggable (see `task/PLAN.md` and
  `session/PLAN.md`) so a Redis or Postgres backend can be supplied without
  touching the engine.

## 13. Module Map

Detailed responsibilities live in each `PLAN.md`. Quick reference:

| Path                                | Responsibility |
|-------------------------------------|----------------|
| `pkg/orchestrator/orchestrator.go`  | Public `Orchestrator` API and constructor |
| `pkg/orchestrator/engine.go`        | Engine loop: pop/push session stack, drive invoker, parse outputs, dispatch events |
| `pkg/orchestrator/types.go`         | Cross-module IDs, `MessageEnvelope`, `Result`, `Snapshot`, `Dependencies` |
| `pkg/orchestrator/protocol/`        | Salutation parsing + Notes templates |
| `pkg/orchestrator/registry/`        | Workspace metadata + alias index |
| `pkg/orchestrator/prompt/`          | Role-aware Notes injection (transport-aware) |
| `pkg/orchestrator/session/`         | Session lifecycle, call stack, store |
| `pkg/orchestrator/task/`            | Task lifecycle, snapshots, store, result reads |
| `pkg/orchestrator/invoker/`         | Single-Request execution against `agentio.Agent` |
| `pkg/orchestrator/audit/`           | Audit sink interface, default sinks, transcript records |
| `pkg/orchestrator/events/`          | Best-effort event bus + Subscription |
| `pkg/orchestrator/idgen/`           | ID generators (TaskID, SessionID, RequestID) |

### 13.1 Dependency graph

Layered, bottom-up. Higher layers may import any lower layer; same-layer
edges are forbidden unless explicitly listed below.

```
                            ┌────────────────────────────────┐
   Layer 3 (root)           │   pkg/orchestrator (engine,     │
                            │   orchestrator.go, types.go)    │
                            └───────────┬────────────────────┘
                                        │ imports all below
   ┌────────────────────────────────────┴───────────────────────┐
   │                                                            │
   │  Layer 2:   prompt              invoker                    │
   │             (→ protocol,        (→ registry, audit,        │
   │              registry, session)  events, idgen, session,   │
   │                                  agentio)                  │
   │                                                            │
   │  Layer 1:   audit    events    session    task             │
   │             (→ idgen) (→ idgen) (→ idgen)  (→ idgen)       │
   │                                                            │
   │  Layer 0:   protocol     registry     idgen                │
   │             (no orch     (no orch     (no orch             │
   │              imports)     imports)     imports)            │
   │                                                            │
   └────────────────────────────────────────────────────────────┘
```

Concrete import rules:

- `protocol`, `registry`, `idgen` import nothing from `pkg/orchestrator/...`.
- `audit`, `events`, `session`, `task` import `idgen` only (for ID types).
  They MUST NOT import the root package, even for shared types — keeps
  Layer 3 free to import them without cycles.
- `prompt` imports `protocol`, `registry`, and `session` (for
  `RequestKind`).
- `invoker` imports `registry`, `audit`, `events`, `idgen`, `session`
  (for `RequestKind` and `Request` shape), and external `pkg/agentio`.
  The fully-built `agentio.InvokeRequest` is supplied by the engine, so
  `invoker` does NOT import `prompt`.
- The root package imports every subpackage to wire the engine.
- **No transitive import of `pkg/bridge/...` is allowed inside
  `pkg/orchestrator`**; transport access is mediated by `agentio.Agent`
  factories supplied via `Dependencies`.

## 14. v2 Extension Points

The design preserves clean insertion points for these v2 features. None of
them block v1.

- **Agent-to-agent ACL.** Insert between `registry` resolution and the engine
  push step. ACL receives `(task, source_session, target_alias)` and may
  reject the push.
- **Hop count limits per workspace pair.** Implement on top of the existing
  `OpenedSessions` counter and the per-Task session tree.
- **Retries / fallbacks.** Wrap `invoker.Invoker`. The engine treats the
  wrapped invoker as opaque.
- **Push delivery.** Subscribe to `events.Bus` from the IM/Web tier and write
  events out to user channels.
- **Multi-alias workspaces.** Extend `registry` only; `protocol` does not
  need changes if aliases remain globally unique strings.
- **Distributed task store.** Replace `task.Store` and `session.Store`. The
  engine API is store-agnostic.

## 15. Open Questions (deferred)

- Where do we place per-workspace static system prompts (e.g., a Claude Code
  `CLAUDE.md`)? They likely live in `prompt/` as workspace-scoped templates,
  but the exact ownership boundary with the agent runtime is a v2 question.
- How should structured (`agentio.JSONPart`) or binary (`agentio.BlobPart`)
  inputs be carried across handoffs? v1 only forwards text payloads; v2 may
  forward attachments by reference.
- Cycle detection beyond the simple `MaxSessions` cap; needs more telemetry
  first.

## 16. Glossary

- **Salutation** — the literal `{to #<alias>}` directive at the head of a
  message.
- **Epistolary** — the protocol named after letter etiquette; an Epistolary
  message is one whose first non-empty line is a Salutation.
- **Alias** — the user-facing name of a Workspace (e.g., `Alice`).
- **Notes block** — a stable preamble injected by the prompt builder listing
  the partner workspaces.
- **Hop** — one session opening within a task.
- **Triple** — `(task_id, session_id, request_id)`, the unique identifier of
  a transport call.
