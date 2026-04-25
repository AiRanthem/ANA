# registry/PLAN.md

## Purpose

Hold the mutable catalog of registered Workspaces, including alias indexing,
runtime metadata, and per-workspace bridge factories. Resolves an alias to a
ready-to-use `agentio.Agent` for the invoker.

## Public surface (intent only)

- `Workspace` — fields per `DESIGN.md` §6.3.
- `RuntimeKind` enum: `chat_api`, `resumable_cli`, `socket_session`. Used by
  `prompt/` and `invoker/` to choose role-aware injection and bridge
  features.
- `Registry` interface:
  - `Register(ctx, Workspace, AgentFactory) error`
  - `Update(ctx, Workspace) error`
  - `Disable(ctx, workspaceID string) error`
  - `Enable(ctx, workspaceID string) error`
  - `LookupByAlias(ctx, alias string) (Workspace, AgentFactory, error)`
  - `LookupByID(ctx, workspaceID string) (Workspace, AgentFactory, error)`
  - `Default(ctx) (Workspace, AgentFactory, error)`
  - `List(ctx) ([]Workspace, error)`
- `AgentFactory func(ctx context.Context, ws Workspace) (agentio.Agent, error)`
  - Constructs (or returns a cached) `agentio.Agent` for the given
    workspace. Implementations decide whether to reuse a single Agent
    instance per workspace or create one per call.
- Sentinel errors:
  - `ErrAliasNotFound`
  - `ErrWorkspaceNotFound`
  - `ErrWorkspaceDisabled`
  - `ErrAliasConflict`
  - `ErrInvalidWorkspace`
  - `ErrNoDefaultWorkspace`

## Behavior rules

### Aliases

- Aliases are unique within a registry instance.
- Comparison is case-sensitive at lookup. `Register` normalizes by trimming
  surrounding whitespace; otherwise the alias is taken verbatim.
- Allowed character set per `protocol/PLAN.md` §Alias rules.
- A registry may carry at most one `IsDefaultEntry == true` workspace. If a
  registration sets `IsDefaultEntry = true`, any prior default is demoted.

### Disable / enable

- A disabled workspace returns `ErrWorkspaceDisabled` from `LookupByAlias`
  and `LookupByID`.
- Disable is idempotent and does not abort in-flight tasks. Tasks already
  routed before disable continue to completion; new lookups fail.

### AgentFactory contract

- Must be safe for concurrent calls.
- Returned `agentio.Agent` is treated as opaque; closure semantics are the
  factory's responsibility (e.g., a long-lived socket Agent is reused across
  Requests; a CLI Agent that owns a process pool is a single instance).
- Errors from the factory propagate up unchanged so the engine can wrap with
  task/session/request context.

## Data model

### Workspace

Fields per `DESIGN.md` §6.3. Validation rules:

- `WorkspaceID` non-empty, unique.
- `Alias` non-empty, validated by the alias rule above.
- `RuntimeType` non-empty.
- `RuntimeKind` is one of the enum values.
- `Description` ≤ 256 chars (used in the Notes block; long descriptions
  hurt cache and are rejected).
- `RuntimeConfig` opaque to the registry; passed through to the factory.

### Indexes

Two indexes maintained in lock-step:

- `byID map[string]Workspace`
- `byAlias map[string]string` — alias → workspace id.

Indexes are protected by an `sync.RWMutex`. Reads (lookups) are served under
the read lock; writes (register/update/disable) acquire the write lock.

### Persistence

The default implementation is in-memory. The interface is shaped so a
durable backend can be plugged in later by reusing the same `Registry`
interface. v1 does not persist registry state.

## Edge cases & decisions

- Alias collision: `Register` returns `ErrAliasConflict` and leaves state
  unchanged.
- Updating a workspace's alias: handled as `Disable` + `Register` from the
  caller's perspective. The registry does NOT silently rewrite the alias
  mapping mid-flight to avoid race conditions with active tasks.
- Multiple defaults: `Register`/`Update` enforces single-default invariant
  by demoting the previous default.
- A factory returning nil agent without error: validated and treated as
  `ErrInvalidWorkspace`.

## Tests to write (no implementation in this pass)

1. Register / lookup happy path.
2. Alias collision returns `ErrAliasConflict`, state unchanged.
3. Disable hides workspace from alias and id lookups.
4. Re-enable restores lookups.
5. Default election: single default invariant under multiple registrations.
6. Concurrent register + lookup race-free under `-race`.
7. AgentFactory error surfaces unchanged.
8. Validation rejects oversized `Description` and bad aliases.

## Out of scope

- Workspace discovery (DNS, gateway pings, cluster membership). The
  registry takes whatever it is given.
- Bridge construction (`AgentFactory` is provided by the caller assembling
  the orchestrator).
- Authorization between workspaces (v2; insertion point is between
  `LookupByAlias` and the engine push step).
