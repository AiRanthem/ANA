# agent/PLAN.md

## Purpose

Define the **`AgentSpec` interface** — the single extension point that
binds a kind of agent program (Claude Code, OpenClaw, …) into the
manager's workspace lifecycle. This package owns:

- The `AgentSpec` interface itself and the supporting types
  (`PluginLayout`, `ProbeResult`, `ProtocolDescriptor`,
  `InstallParams`, `WorkspaceSummary`).
- The narrow registry helper (`SpecSet`) used by the root package to
  collect and look up registered specs by `Type()`.

It does **not** own concrete implementations. Each implementation
lives in its own subpackage (`agent/claudecode/`,
`agent/openclaw/`, …).

## Public surface (intent only)

- `AgentType` — string alias for the spec id, also re-exported from
  the manager root.
- `AgentSpec` interface per `DESIGN.md` §8.1.
- `PluginLayout` interface per `DESIGN.md` §8.2.
- `ProbeResult` struct per `DESIGN.md` §8.3.
- `ProtocolDescriptor` struct + `ProtocolKind` enum per
  `DESIGN.md` §8.4.
- `InstallParams` struct per `DESIGN.md` §8.5, including the read-only
  `WorkspaceSummary` view.
- `SpecSet` helper:
  - `NewSpecSet() *SpecSet`
  - `(s *SpecSet) Register(spec AgentSpec) error` — rejects duplicate
    `Type()`.
  - `(s *SpecSet) Get(t AgentType) (AgentSpec, bool)`
  - `(s *SpecSet) Types() []AgentType`
  - `(s *SpecSet) Len() int`
- Sentinel errors:
  - `ErrAgentTypeConflict`
  - `ErrAgentTypeUnknown`
  - `ErrInvalidProtocolDescriptor`
  - `ErrInvalidPluginLayout`

## Behavior rules

### `Type()` is the spec identity

- `Type()` MUST return a non-empty lowercase kebab string,
  `[a-z][a-z0-9-]{0,31}`. The constructor of the spec validates this.
- The id is global within a manager and serves as the join key in
  workspace records (`Workspace.AgentType`).

### `DisplayName()` and `Description()`

- `DisplayName()` returns the human-friendly label shown in operator
  tooling. Required, non-empty.
- `Description()` returns up to 1024 characters of markdown-friendly
  free text. Optional; the empty string is allowed.

### `Install(ctx, ops, params)`

- Caller (the workspace install worker, see `workspace/PLAN.md`) calls
  `ops.Init` **before** `Install`. Specs MAY assume the working
  directory exists and is empty.
- The order of operations within `Install` is implementation-defined,
  but specs SHOULD:
  1. Place agent binary / entrypoint (via `ops.PutFile` or
     `ops.Exec("curl", …)` etc.).
  2. Write configuration files.
  3. Optionally start a daemon (HTTP gateway, socket server).
- Specs MUST be **idempotent under retry within the same call** —
  i.e., `PutFile`-ing the same target twice MUST NOT fail. They are
  **not** required to be idempotent across calls; the manager calls
  `Install` exactly once per workspace.
- A failure SHOULD return an error wrapped with the phase
  (`"install: <step>: %w"`). The workspace controller maps the error
  to a `WorkspaceError{Phase: "install"}` record.
- `Install` MUST NOT mutate state outside `ops`. Specs that need
  off-infra side effects (e.g., registering a workspace with a remote
  service) MUST receive those targets through `params.UserParams` and
  document the keys.

### `Uninstall(ctx, ops)`

- Best-effort cleanup of anything `Install` started. Stop daemons,
  release sockets, log out of remote services, etc.
- The manager calls `ops.Clear` after `Uninstall`, so file-system
  cleanup is **not** the spec's responsibility.
- Errors are logged but do NOT block workspace deletion. Specs SHOULD
  return errors that the operator can act on (e.g., "container kill
  failed: ...") rather than swallow them.
- Implementations MUST tolerate the partial-install case: if `Install`
  failed halfway, `Uninstall` may run against an environment where
  some state never existed. Treat missing artifacts as success.

### `Probe(ctx, ops)`

- MUST be non-mutating. Side effects break the v2 multi-instance story
  and confuse operators.
- SHOULD complete within `ProbeTimeout` (default 5 seconds, set by
  the manager). Implementations that need longer probes return
  `Healthy: false` with `ErrTimeout` and the operator tunes
  `ProbeTimeout` upward.
- The returned `Detail` map is small (~10 keys, values ≤ 256 chars
  each). Big payloads belong in operator-supplied logging, not in the
  workspace record.

### `ProtocolDescriptor()`

- Returns a static, value-typed descriptor. Implementations SHOULD
  return the same value on every call (the manager treats it as
  cached). It does not change per-workspace; per-workspace details
  flow through `InfraOps.Dir()` / `Request(port, ...)` resolution.
- The descriptor is JSON-serializable: only types `bool`, `string`,
  `int64`, `float64`, `[]any`, `map[string]any` are allowed in
  `Detail`. The package validates this lazily; tests SHOULD assert it.

### `PluginLayout()`

- Returns the strategy that knows where in the workspace dir each
  canonical plugin path goes. Implementations SHOULD be value-typed
  (no goroutines, no file handles).
- The strategy MUST handle a plugin manifest with arbitrary subsets
  of canonical sections (`skills/`, `rules/`, `hooks/`, `subagents/`,
  `mcps/`, `assets/`); missing sections are not errors.

## Data model

### `AgentType`

```go
type AgentType string
```

Validated at registration. Allowed character set:
`^[a-z][a-z0-9-]{0,31}$`.

### `AgentSpec` (interface, repeating §8.1 for locality)

```go
type AgentSpec interface {
    Type() AgentType
    DisplayName() string
    Description() string
    PluginLayout() PluginLayout
    Install(ctx context.Context, ops infraops.InfraOps, params InstallParams) error
    Uninstall(ctx context.Context, ops infraops.InfraOps) error
    Probe(ctx context.Context, ops infraops.InfraOps) (ProbeResult, error)
    ProtocolDescriptor() ProtocolDescriptor
}
```

### `ProtocolDescriptor` and `ProtocolKind`

```go
type ProtocolKind string

const (
    ProtocolKindCLI    ProtocolKind = "cli"
    ProtocolKindREST   ProtocolKind = "rest"
    ProtocolKindSocket ProtocolKind = "socket"
)

type ProtocolDescriptor struct {
    Kind   ProtocolKind
    Detail map[string]any
}
```

Conventional `Detail` keys per kind (NOT enforced here; consumers in
adapter code rely on these by convention, agents SHOULD NOT invent
incompatible alternatives without coordinating):

- `cli`:
  - `command` (`[]string`) — the program + base args, e.g.
    `["claude", "code"]`.
  - `cwd_relative_to_dir` (`string`, optional) — subpath under
    `InfraOps.Dir()` where the CLI must be run; default `""`.
  - `resume_flag` (`string`, optional) — flag name for session
    resumption (e.g., `--resume`).
  - `env` (`[]string`, optional) — extra `KEY=VALUE` pairs.
- `rest`:
  - `port` (`int64`) — the port inside the infra; consumed via
    `InfraOps.Request(port, ...)`.
  - `base_path` (`string`, optional) — URL path prefix (e.g., `/v1`).
  - `auth` (`string`, optional) — `"none"` / `"bearer"` /
    `"basic"`. Token sources are caller-supplied; the manager never
    stores tokens.
- `socket`:
  - `port` (`int64`).
  - `path` (`string`, optional).

### `InstallParams`

```go
type InstallParams struct {
    Workspace  WorkspaceSummary
    UserParams map[string]any
}

type WorkspaceSummary struct {
    ID        WorkspaceID
    Namespace Namespace
    Alias     Alias
    AgentType AgentType
    InfraType InfraType
    Plugins   []AttachedPluginRef
}

type AttachedPluginRef struct {
    PluginID    PluginID
    Name        string
    ContentHash string
}
```

`WorkspaceSummary` is read-only; mutating it has no effect because the
controller writes the row separately. The summary is provided so the
spec can, for example, name the daemon process after the alias.

### `ProbeResult`

```go
type ProbeResult struct {
    Healthy bool
    Latency time.Duration
    Detail  map[string]string
    Error   *WorkspaceError
}
```

`WorkspaceError` is the same struct documented in `DESIGN.md` §6.4 and
re-exported from the root.

## SpecSet

The `SpecSet` helper is owned here so that any package that needs to
look up specs (the root manager, hypothetical orchestrator-glue
packages, tests) gets a consistent collection type.

```go
type SpecSet struct {
    // unexported map[AgentType]AgentSpec under a sync.RWMutex
}

func NewSpecSet() *SpecSet
func (s *SpecSet) Register(spec AgentSpec) error // ErrAgentTypeConflict on duplicate
func (s *SpecSet) Get(t AgentType) (AgentSpec, bool)
func (s *SpecSet) Types() []AgentType            // sorted ascending
func (s *SpecSet) Len() int
```

The Manager `Builder` uses `SpecSet` internally; consumers may
construct one directly for tests.

## Edge cases & decisions

- **Two specs with the same Type:** Rejected at `Register`; the second
  call returns `ErrAgentTypeConflict` and the registry is unchanged.
- **Spec returns empty Type / DisplayName:** The Manager builder
  validates these at `Build()` time and refuses to construct.
- **Missing PluginLayout:** Disallowed. Specs MUST always return a
  non-nil `PluginLayout`, even if it places no files (see
  `agent/claudecode/PLAN.md` for what an "empty layout" looks like —
  it never actually happens in practice but the type makes it
  expressible).
- **AgentSpec doing async work in `Install`:** Discouraged. The
  controller treats `Install` as synchronous. If a spec must launch a
  daemon, it should do so synchronously (start the process, wait for
  the readiness signal, return) and rely on `Probe` for liveness
  checks afterward.
- **AgentSpec calling `ops.Clear`:** Forbidden. Only the controller
  calls `Clear`, and only in delete flows. Specs that want to
  scratch-and-rebuild must re-create files; they MUST NOT clear the
  directory.

## Tests to write (no implementation in this pass)

1. `SpecSet.Register` rejects duplicate `Type()` with
   `ErrAgentTypeConflict`.
2. `SpecSet.Types` returns sorted output.
3. `ProtocolDescriptor` JSON round-trip preserves `Kind` and a
   reasonable `Detail` payload.
4. A fake spec used in tests passes a smoke `Install → Probe →
   Uninstall` cycle against a fake `infraops.InfraOps`.
5. Spec with empty `Type()` is rejected at the boundary chosen
   (registration time, per the rule above).

## Out of scope

- Concrete agent implementations (`claudecode/`, future `openclaw/`,
  …). They are independent subpackages that import this one.
- Plugin **manifest** schema definitions; those live in `plugin/`.
- Any glue with `pkg/agentio` or the orchestrator; per
  `DESIGN.md` §11, the manager does not import either.
