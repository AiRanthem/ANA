# ANA Manager Design

> **Status:** v1 design, no implementation yet. Implementation tasks should
> follow the per-module `PLAN.md` files together with this document.
>
> **Audience:** ANA contributors implementing or extending the workspace
> management plane.

## 1. Background & Goals

ANA is an agent orchestration system. The orchestrator (`pkg/orchestrator`)
is the runtime control plane that routes user input through a tree of
workspaces and produces a final answer. Before a workspace can be routed
to, somebody has to **provision** it: pick an agent program (Claude Code,
OpenClaw, …), drop the right plugins into the right directories, install
the program, optionally start a long-running gateway, give the workspace a
human-readable alias, and persist enough state to find it again later.
That provisioning lifecycle is the job of `pkg/manager`.

`pkg/manager` is the workspace **lifecycle plane**. It owns:

- A catalog of **Plugins** (zip packages of skills/rules/hooks/subagents)
  with pluggable storage and a manifest format we control.
- A registry of **Agent specs** (interface implementations describing how
  to install, configure, probe, and reach a particular kind of agent
  program).
- A registry of **InfraOps factories** (interface implementations
  describing how to perform atomic file/exec/network operations against a
  particular kind of execution environment — local directory, Docker
  container, E2B sandbox, etc.).
- A repository of **Workspaces** — concrete agent instances bootstrapped
  inside an infra, attached to plugins, and tracked by status with a
  liveness probe.

The manager is **decoupled** from the orchestrator: it does not import
`pkg/agentio` or `pkg/orchestrator/...`. The application assembly layer
that builds the binary is the only place that bridges the two planes
(typically by reading a manager `Workspace` plus its `AgentSpec`'s
protocol descriptor and synthesizing an `agentio.Agent` factory for the
orchestrator's `registry`). This boundary is documented in §11.

### 1.1 Goals

- One vocabulary (Agent, Plugin, Workspace, Infra) used consistently
  across the package and its module docs.
- Pluggable backends for each capability (DB, plugin storage, infra
  type, agent type) so deployments can swap implementations.
- A small, documented file format for plugin packages so users can build
  plugins by hand or with tooling without reading code.
- Asynchronous workspace provisioning with an observable status machine
  (`init` / `healthy` / `failed`) and a built-in liveness probe loop.
- Crash-safe operations: every state transition that the user can
  observe is persisted before any side effect that the user cannot
  reverse.
- A clear extension surface (`AgentSpec`, `infraops.Factory`,
  `PluginStorage`, `Repository` interfaces) so adding a new agent type
  or a new infra type is a localized change.
- A registration API that lets a future component (orchestrator
  registry, scheduler, dashboard) consume manager state without
  reaching into internals.

### 1.2 Non-Goals (v1)

- No multi-node coordination. A manager instance owns its database; if
  you want HA you front it with a SQL backend and run a single writer.
  Cross-instance coordination is v2.
- No plugin **versioning**. A plugin is identified by a stable ID; new
  uploads overwrite the stored zip; previously-attached workspaces are
  unaffected because they extracted their content at attach time.
- No automatic plugin upgrade for existing workspaces. The user
  re-creates a workspace (or runs a re-attach flow that we deliberately
  defer to v2).
- No multi-tenancy isolation beyond namespacing of workspace aliases and
  plugin names. Authentication, authorization, and quota are out of
  scope.
- No general-purpose secret store. Workspace creation may take secrets
  through `infraops.Options` or `AgentSpec.InstallParams`, but the
  manager neither stores nor rotates them.
- No retention or GC of audit / log artifacts. The manager records
  status; logs are emitted via **`pkg/logs`** (`logs.FromContext(ctx)` on
  contexts that carry a logger) and consumed by the deployment.
- No long-running orchestration of agent **runs**. Driving an agent
  through a task is the orchestrator's job; the manager only ensures the
  workspace is reachable.

## 2. Concepts & Vocabulary

### 2.1 Agent

An **Agent** is the metadata that describes a *kind* of agent program:
its identifier (e.g., `claude-code`, `openclaw`), its display name, the
recipe to install it inside an `InfraOps`-shaped environment, the recipe
to lay plugin contents into the agent's expected directory structure,
the liveness probe routine, and a structured **protocol descriptor**
that downstream consumers (e.g., the orchestrator) read to learn how to
talk to a workspace built from this Agent.

In this codebase Agent is represented by the `agent.AgentSpec` interface
(see `agent/PLAN.md`). The manager keeps an in-memory map keyed by
`Type()` (the agent type id) populated through `Builder.RegisterAgentSpec`.
Workspaces persist `agent_type` so the controller can find the spec
again on restart.

> Disambiguation: `pkg/manager/agent` is **not** `pkg/agentio`. The
> former describes how to bring an agent program *into existence inside
> an infra*; the latter is the canonical request/event contract used at
> *invocation time*. The two planes meet only in application assembly
> code.

### 2.2 Plugin

A **Plugin** is a packaged collection of agent-portable resources
(skills, rules, hooks, subagents, MCP configs, AGENTS.md, …) stored as a
zip file with a small manifest. Plugins are agent-agnostic in their
on-disk layout: `skills/`, `rules/`, `hooks/`, etc. Each `AgentSpec`
declares a `PluginLayout` that maps the canonical layout onto the
agent's specific filesystem expectations (for example, Claude Code
expects `~/.claude/plugins/<plugin-name>/skills/...`).

Plugins live in two places:

- **Plugin metadata** in the manager database (id, name, description,
  size, content hash, attached count, timestamps).
- **Plugin content** in a `PluginStorage` backend (default: local
  directory; extensible to S3, GCS, …) keyed by plugin id.

See `plugin/PLAN.md` for the manifest format, on-disk layout, and the
`PluginStorage` and `PluginRepository` interfaces.

### 2.3 Workspace

A **Workspace** is a concrete instantiation of an Agent inside an Infra,
optionally with a set of attached Plugins, identified by a globally
unique opaque id and a human-readable alias scoped within a namespace.

A workspace owns:

- `WorkspaceID` — opaque, manager-generated, stable.
- `Namespace` — string (default: `"default"`). Aliases collide only
  within a namespace.
- `Alias` — human-friendly, namespace-scoped, immutable.
- `AgentType` — the registered `AgentSpec.Type()`; pins the workspace
  to one agent kind for life.
- `InfraType` — the registered infra type; pins the workspace to one
  infra kind for life.
- `InfraOptions` — opaque-to-manager bag persisted verbatim, used to
  reconstruct an `InfraOps` instance whenever the controller needs to
  touch the workspace (probe, delete, etc.).
- `Plugins []AttachedPlugin` — snapshot of which plugins were attached
  at creation time and where they were placed (see §6.5).
- `Status` — `init`, `healthy`, `failed` (state machine in §5).
- `LastProbeAt`, `LastError`, `CreatedAt`, `UpdatedAt`, …

Workspaces **never share** an alias within a namespace, never change
their `AgentType` or `InfraType`, and never rebuild plugins on demand.
"Update" is "delete + create" with the same alias.

### 2.4 Infra

An **Infra** is the runtime environment a workspace lives in. The
manager treats infras as opaque file-and-process surfaces accessed
through the `infraops.InfraOps` interface (§7). v1 ships one
implementation, `infraops/localdir`, that targets a local directory on
the host running the manager. Future implementations (Docker, E2B,
serverless) plug in through the same interface.

`InfraOps` instances are created on demand. The manager does **not**
keep a long-lived `InfraOps` per workspace; instead it constructs one
each time it needs to act, using the persisted `InfraType` +
`InfraOptions`. This keeps state small and crash-safe.

### 2.5 Manager

The **Manager** is the public façade — one Go interface that the host
binary depends on. It owns:

- A `PluginRepository` (database) and a `PluginStorage` (blob backend)
  that together back the Plugin CRUD API.
- A `WorkspaceRepository` that backs the Workspace CRUD API.
- A `Clock` and `IDGenerator` (injected for testability).
- An immutable `agentSpecs` registry built from `RegisterAgentSpec`.
- An immutable `infraFactories` registry built from `RegisterInfraType`.
- A background **probe scheduler** that periodically calls
  `AgentSpec.Probe` on every active workspace.
- A `logs.Logger` for observability, injected on the context at `Build`
  time via `logs.IntoContext` and read everywhere else with
  `logs.FromContext(ctx)`.

The Manager exposes:

- Plugin CRUD (`CreatePlugin`, `GetPlugin`, `ListPlugins`,
  `DeletePlugin`, `GetPluginDownloadURL`).
- Infra factory access (`NewInfraOps`).
- Workspace CRUD + lifecycle
  (`CreateWorkspace`, `GetWorkspace`, `GetWorkspaceByAlias`,
  `ListWorkspaces`, `DeleteWorkspace`).
- Process lifecycle (`Start`, `Stop`) for the probe scheduler.

### 2.6 Namespace

A **Namespace** is a labeling string (default `"default"`) that scopes
**workspace alias** and **plugin name** uniqueness. Namespaces are
declarative: there is no "create namespace" API; a namespace exists by
virtue of having a row in the workspace or plugin repository. v1 does
not enforce ACLs across namespaces; the namespace is a soft partition
useful for organizing tenants or environments.

## 3. Public API (Sketch)

Concrete Go signatures are settled in code; the shape below is fixed.

```go
package manager

type Manager interface {
    // Plugin CRUD
    CreatePlugin(ctx context.Context, req CreatePluginRequest) (Plugin, error)
    GetPlugin(ctx context.Context, id PluginID) (Plugin, error)
    GetPluginByName(ctx context.Context, namespace Namespace, name string) (Plugin, error)
    ListPlugins(ctx context.Context, opts ListPluginsOptions) (rows []Plugin, nextCursor string, err error)
    DeletePlugin(ctx context.Context, id PluginID) error
    GetPluginDownloadURL(ctx context.Context, id PluginID, opts DownloadURLOptions) (string, error)

    // Infra factory
    NewInfraOps(ctx context.Context, infraType InfraType, options infraops.Options) (infraops.InfraOps, error)
    InfraTypes() []InfraType

    // Workspace CRUD + lifecycle
    CreateWorkspace(ctx context.Context, req CreateWorkspaceRequest) (Workspace, error)
    GetWorkspace(ctx context.Context, id WorkspaceID) (Workspace, error)
    GetWorkspaceByAlias(ctx context.Context, namespace Namespace, alias Alias) (Workspace, error)
    ListWorkspaces(ctx context.Context, opts ListWorkspacesOptions) (rows []Workspace, nextCursor string, err error)
    DeleteWorkspace(ctx context.Context, id WorkspaceID) error
    AgentTypes() []AgentType

    // Process lifecycle
    Start(ctx context.Context) error
    Stop(ctx context.Context) error
}
```

The constructor takes a `Builder` so all collaborators are explicit and
swappable in tests:

```go
type Builder struct {
    // immutable backends (required)
    PluginRepository    plugin.Repository
    PluginStorage       plugin.Storage
    WorkspaceRepository workspace.Repository

    // optional collaborators (defaults provided)
    Clock        func() time.Time
    IDGenerator  IDGenerator   // see §6.8
    Logger       logs.Logger   // optional; wired into ctx at Build via logs.IntoContext

    // install worker tuning (optional)
    InstallTimeout time.Duration // default 10 min
    InstallWorkers int           // default 4

    // probe scheduler tuning (optional)
    ProbeInterval time.Duration  // default 30 s
    ProbeWorkers  int            // default 4
    ProbeTimeout  time.Duration  // default 5 s

    // registered extensions (populated via RegisterAgentSpec / RegisterInfraType)
    // unexported fields, mutated by the methods below
}

func NewBuilder() *Builder
func (b *Builder) RegisterAgentSpec(spec agent.AgentSpec) *Builder
func (b *Builder) RegisterInfraType(infraType InfraType, factory infraops.Factory) *Builder
func (b *Builder) Build(ctx context.Context) (Manager, error)
```

`Build(ctx)` returns an error if:

- a required backend is nil,
- two `AgentSpec` values declare the same `Type()`,
- two infra factories share the same `infraType`.

A manager with zero registered agent types or zero registered infra
types is allowed (a "plugin-only" manager that just hosts the plugin
catalog still validates) but emits **`logs.FromContext(ctx).Warn`**
so the operator can spot misconfiguration.

The `Builder` is **single-use**: `Build` consumes it. Re-using the
builder after `Build` returns is an error.

## 4. End-to-End Walkthrough

The canonical flows ground the abstractions. Triples in the form
`(workspace_id, agent_type, infra_type)` accompany each step.

### 4.1 Upload a plugin

```
User → Manager.CreatePlugin({
    Namespace: "default",
    Name:      "trading-research",
    Description: "Stock-research skills + hooks",
    Content:   <io.Reader of plugin.zip>,
})

Manager validates the manifest (manifest.toml at the zip root, see §6.4),
computes a content hash, asks PluginStorage.Put(planned_id, body) to
store the bytes, and inserts a Plugin row in PluginRepository.

Result: Plugin{ID, Namespace, Name, ContentHash, Size, ...}.
```

### 4.2 Provision a workspace

```
User → Manager.CreateWorkspace({
    Namespace: "default",
    Alias:     "Alice",
    AgentType: "claude-code",
    InfraType: "localdir",
    InfraOptions: infraops.Options{
        "dir": "/var/lib/ana/workspaces/alice",
    },
    Plugins: []PluginRef{
        {ID: "plg-trading-research"},
    },
    InstallParams: map[string]any{
        "claude_binary": "/usr/local/bin/claude",
    },
})

1. Manager validates: namespace+alias unused, agent_type registered,
   infra_type registered, plugin ids exist in repo.
2. Manager allocates WorkspaceID, persists Workspace{Status: init} to
   WorkspaceRepository in one transaction.
3. Manager dispatches the install to a worker goroutine and returns the
   in-flight Workspace to the caller. The caller polls Get* to observe
   status.
4. Worker:
   a. Constructs InfraOps via infraops.Factory(infraOptions).
   b. Calls InfraOps.Init() — guarantees an empty working directory.
   c. Resolves AgentSpec by AgentType.
   d. For each PluginRef: PluginStorage.Get(id) → io.ReadCloser; the
      worker streams the zip into AgentSpec.PluginLayout().Apply(ops, ...)
      which writes files via InfraOps.PutFile(...) into the agent's
      expected paths.
   e. Calls AgentSpec.Install(ctx, ops, InstallParams) which installs
      the agent program (e.g., copies the binary, writes a config
      file) and, if applicable, starts the daemon process.
   f. Calls AgentSpec.Probe(ctx, ops) once to confirm.
   g. On success: WorkspaceRepository.UpdateStatus(id, healthy).
   h. On error at any step: WorkspaceRepository.UpdateStatus(id, failed)
      with the error message and code; the working directory is left in
      place for diagnosis (cleanup happens at delete time).

The probe scheduler picks the workspace up at the next tick and keeps
status in sync going forward.
```

### 4.3 Probe loop

```
Every ProbeInterval (default 30s):
  for each workspace in WorkspaceRepository.ListByStatus(healthy|failed):
      ops    := factory(workspace.InfraOptions)
      result := AgentSpec.Probe(ctx, ops)
      newStatus := healthy if result.Healthy else failed
      if newStatus != workspace.Status:
          WorkspaceRepository.UpdateStatus(workspace.ID, newStatus, result.Error)

The scheduler uses a worker pool (ProbeWorkers, default 4). Workspaces
in init are skipped — the install worker owns them.
```

### 4.4 Delete a workspace

```
User → Manager.DeleteWorkspace(workspace_id)

1. Manager loads the Workspace; if not found, returns ErrWorkspaceNotFound.
2. Reconstructs InfraOps via factory(workspace.InfraOptions).
3. Calls AgentSpec.Uninstall(ctx, ops) to stop any daemons cleanly
   (best-effort; errors logged but continue).
4. Calls InfraOps.Clear(ctx) to drop the working directory.
5. WorkspaceRepository.Delete(workspace_id).
6. Returns success even if step 3 or 4 emitted recoverable errors,
   provided step 5 succeeded; partial cleanup is reported through the
   logger.

Idempotent: deleting a non-existent workspace returns
ErrWorkspaceNotFound without side effects.
```

### 4.5 Delete a plugin

```
User → Manager.DeletePlugin(plugin_id)

1. PluginRepository.Get(plugin_id) → Plugin or ErrPluginNotFound.
2. PluginRepository.Delete(plugin_id) → removes the metadata row first so
   callers never see a row whose blob is already gone if storage delete fails.
3. PluginStorage.Delete(plugin_id) → removes the zip from the backend.
   A failed storage delete after a successful metadata delete leaves an orphan
   blob, which is preferable to queryable metadata without bytes.

Workspaces that previously attached this plugin keep their on-disk
plugin files (the manager does not enforce referential integrity at
delete). The workspace metadata still records AttachedPlugin{ID:...},
which is now a dangling reference; consumers must tolerate this.
```

## 5. State Machines

### 5.1 Workspace

```
                ┌──────┐
CreateWorkspace │ init │
        ────────▶      │
                └──┬───┘
                   │ install worker succeeds
                ┌──▼─────┐
                │healthy │◀─── probe ok
                └──┬─────┘
                   │ probe / install fail
                   ▼
                ┌──────┐
                │failed│ ◀─── probe still failing
                └──┬───┘
                   │ probe ok
                   └──▶ healthy
```

Terminal: workspaces never reach a terminal state at the data layer;
"deleted" is the absence of the row. Cancellation in the install worker
flips `init → failed` if the operator cancels the manager while a
workspace is being created.

`init` is special:

- Newly persisted state right after `CreateWorkspace` returns to the
  caller.
- Skipped by the probe scheduler (the install worker is the only writer).
- A workspace stuck in `init` for longer than `InstallTimeout`
  (configurable, default 10 minutes) is force-flipped to `failed` by
  the probe scheduler with `ErrInstallTimeout`. This protects against
  install-worker crashes.

### 5.2 Plugin

Plugins have no status machine. They exist or they do not. Uploading a
new zip with the same `(namespace, name)` overwrites the row and the
content (atomic at the manager level); see §10.2 for the concurrency
contract.

## 6. Core Data Model

This section is the cross-module source of truth for field names. Each
module may add internal fields but MUST NOT rename or repurpose those
listed below.

### 6.1 Identifier types

| Type             | Shape                            | Notes |
|------------------|----------------------------------|-------|
| `PluginID`       | string                           | Opaque; generated by manager. Convention: `plg_<base32>`. |
| `WorkspaceID`    | string                           | Opaque. Convention: `wsp_<base32>`. |
| `Namespace`      | string                           | `[a-z0-9-]{1,32}`. Default `"default"`. |
| `Alias`          | string                           | `[A-Za-z][A-Za-z0-9_-]{0,63}`. |
| `AgentType`      | string                           | Lowercase kebab. Owned by `AgentSpec.Type()`. |
| `InfraType`      | string                           | Lowercase kebab. Owned by registration. |

All ID types are aliases over `string` declared in
`pkg/manager/types.go` to keep call sites readable.

### 6.2 `Plugin`

| Field         | Type        | Notes |
|---------------|-------------|-------|
| `ID`          | `PluginID`  | Primary key. |
| `Namespace`   | `Namespace` | Scope for `Name` uniqueness. |
| `Name`        | `string`    | Unique within namespace. `[a-z0-9-]{1,64}`. |
| `Description` | `string`    | ≤ 1024 chars. Free-form. |
| `Manifest`    | `plugin.Manifest` | Structured copy of `manifest.toml`. |
| `ContentHash` | `string`    | `sha256:<hex>` of the zip body. |
| `Size`        | `int64`     | Bytes. |
| `CreatedAt`   | `time.Time` | UTC. |
| `UpdatedAt`   | `time.Time` | UTC. |

The full structure of `Manifest` lives in `plugin/PLAN.md` §Manifest.

### 6.3 `Workspace`

| Field             | Type                   | Notes |
|-------------------|------------------------|-------|
| `ID`              | `WorkspaceID`          | Primary key. |
| `Namespace`       | `Namespace`            | Scope for `Alias` uniqueness. |
| `Alias`           | `Alias`                | Immutable; namespace-unique. |
| `AgentType`       | `AgentType`            | Immutable. |
| `InfraType`       | `InfraType`            | Immutable. |
| `InfraOptions`    | `infraops.Options`     | Persisted as JSON; opaque to manager. |
| `InstallParams`   | `map[string]any`       | Persisted as JSON; opaque to manager; consumed by `AgentSpec.Install`. |
| `Plugins`         | `[]AttachedPlugin`     | Snapshot at creation time. |
| `Status`          | `WorkspaceStatus`      | `init` / `healthy` / `failed`. |
| `StatusError`     | `*WorkspaceError`      | Non-nil when `Status == failed` or transient install error. |
| `Description`     | `string`               | Free-form, ≤ 1024 chars. |
| `Labels`          | `map[string]string`    | Optional user labels for ListWorkspaces filtering. |
| `LastProbeAt`     | `time.Time`            | Zero until first probe completes. |
| `CreatedAt`       | `time.Time`            | UTC. |
| `UpdatedAt`       | `time.Time`            | UTC. |

Secrets in `InstallParams` and `InfraOptions`: these maps are stored
verbatim by the repository. Operators that handle secret material
choose a repository implementation that encrypts at rest, or supply
references (e.g., `{"vault_path":"…"}`) instead of inline secrets. The
manager logger NEVER logs option/param values — only their keys (see
§9).

### 6.4 `WorkspaceError`

| Field      | Type     | Notes |
|------------|----------|-------|
| `Code`     | `string` | Machine-friendly. e.g. `install.exec_failed`, `probe.unhealthy`. |
| `Message`  | `string` | Human-readable. |
| `Phase`    | `string` | `install`, `probe`, `uninstall`. |
| `RecordedAt` | `time.Time` |  |

### 6.5 `AttachedPlugin`

| Field          | Type        | Notes |
|----------------|-------------|-------|
| `PluginID`     | `PluginID`  | Reference to `Plugin`; may dangle after the plugin is deleted. |
| `Name`         | `string`    | Plugin name at attach time, captured for diagnostic display. |
| `ContentHash`  | `string`    | Plugin content hash at attach time. |
| `PlacedPaths`  | `[]string`  | Paths inside the workspace dir that received plugin files (relative to `InfraOps.Dir()`). |

`AttachedPlugin` is a snapshot. Re-attaching a plugin to an existing
workspace is not supported in v1 (see §1.2); replacements go through
delete-and-recreate.

### 6.6 Request shapes

```go
type CreatePluginRequest struct {
    Namespace   Namespace
    Name        string
    Description string
    Content     io.Reader   // exactly the zip body
}

type ListPluginsOptions struct {
    Namespace Namespace // empty: all namespaces
    NameLike  string    // optional substring match on Name
    Limit     int
    Cursor    string    // opaque pagination cursor
}

type DownloadURLOptions struct {
    TTL time.Duration // optional; defaults to PluginStorage default
}

type CreateWorkspaceRequest struct {
    Namespace     Namespace
    Alias         Alias
    AgentType     AgentType
    InfraType     InfraType
    InfraOptions  infraops.Options
    InstallParams map[string]any
    Plugins       []PluginRef
    Description   string
    Labels        map[string]string
}

type PluginRef struct {
    ID PluginID
}

type ListWorkspacesOptions struct {
    Namespace Namespace
    AgentType AgentType
    InfraType InfraType
    Status    WorkspaceStatus
    Labels    map[string]string
    Limit     int
    Cursor    string
}
```

### 6.7 `WorkspaceStatus`

```go
type WorkspaceStatus string

const (
    StatusInit    WorkspaceStatus = "init"
    StatusHealthy WorkspaceStatus = "healthy"
    StatusFailed  WorkspaceStatus = "failed"
)
```

### 6.8 `IDGenerator`

```go
type IDGenerator interface {
    PluginID() PluginID
    WorkspaceID() WorkspaceID
}
```

The default implementation produces opaque base32 ids prefixed with
`plg_` and `wsp_` respectively, seeded by `crypto/rand`. Tests inject
deterministic generators. The package does NOT mint other id types
here — `EventID`, `RequestID`, etc. belong to the orchestrator and
are not part of the manager surface.

## 7. InfraOps Contract

The full specification lives in `infraops/PLAN.md`; this section fixes
the cross-module shape and the v1 method set.

### 7.1 v1 method set

```go
type InfraOps interface {
    // Type returns the infra type id (e.g., "localdir").
    Type() string
    // Dir returns the absolute working directory inside the infra.
    Dir() string

    // Init prepares a newly-created backing state (empty directory for
    // localdir). Calling Init on non-empty or already-initialized state
    // returns ErrAlreadyInitialized / ErrDirNotEmpty as documented in
    // infraops/PLAN.md.
    Init(ctx context.Context) error

    // Open attaches to existing backing state. It must not create or clear
    // state. The probe scheduler calls Open before Probe; the install worker
    // uses Init for create-time setup then operates on the same instance.
    Open(ctx context.Context) error

    // Exec runs a structured command relative to Dir(). Stdout/Stderr
    // are buffered into ExecResult unless the caller supplies streamers
    // on ExecCommand.
    Exec(ctx context.Context, cmd ExecCommand) (ExecResult, error)

    // PutFile writes `path` (relative to Dir()) with `content`.
    // Intermediate directories are created.
    PutFile(ctx context.Context, path string, content io.Reader, mode fs.FileMode) error

    // GetFile reads `path` (relative to Dir()) as a stream.
    GetFile(ctx context.Context, path string) (io.ReadCloser, error)

    // Request opens an HTTP-shaped channel to a daemon listening on
    // `port` inside the infra. Implementations translate `port` into
    // a host:port reachable from the manager process (e.g., the local
    // loopback for localdir; published ports for Docker; tunneled URLs
    // for E2B). Caller closes the response body.
    Request(ctx context.Context, port int, req *http.Request) (*http.Response, error)

    // Clear drops the working directory and any infra-side resources
    // (containers, sandboxes, …). Idempotent.
    Clear(ctx context.Context) error
}
```

`ExecCommand` and `ExecResult` are defined in `infraops/PLAN.md`. The
shape is fixed: program + args + env + stdin + optional stdout/stderr
writers, plus a working-directory override relative to `Dir()`.

### 7.2 Why these methods

- **Why structured `Exec` (no shell string)?** Eliminates injection.
  Implementations that ride a shell on the other side (Docker, SSH)
  quote the args. Callers that need shell features pass
  `Program: "sh", Args: []string{"-c", "..."}` deliberately.
- **Why explicit `PutFile`/`GetFile`?** Binary-safe transfer is
  required for plugin zips and CLI binaries. Encoding through `Exec`
  has been tried elsewhere; it always regrets.
- **Why HTTP-shaped `Request` (instead of WS / arbitrary TCP)?** The
  v1 agent set runs HTTP-style gateways. Adding a `Dial(port)
  (net.Conn, error)` for socket / WS support is a backward-compatible
  extension.
- **Why no `Stat`/`Remove`/`Mkdir`?** They're consequences of
  `Exec("test", ...)`, `Exec("rm", ...)`, etc. on the platforms we
  support, and `PutFile` already creates intermediate directories.
  Future infras that benefit from native directory operations can add
  them as an extension interface checked via type assertion.

### 7.3 Options & Factory

```go
type Options map[string]any   // JSON-serializable

type Factory func(ctx context.Context, opts Options) (InfraOps, error)
```

Required option for every infra: `"dir"` — the absolute working
directory. Implementations document additional keys in their own
`PLAN.md`. The manager validates only that `dir` is non-empty before
calling the factory; the factory validates everything else and returns
typed errors.

The factory is invoked in three situations:

1. `Manager.NewInfraOps` — caller-driven, for ad-hoc operations.
2. Workspace install worker — once per provisioning.
3. Probe scheduler — once per probe tick per workspace.

Therefore factories MUST be cheap to call (do real work in `Init` /
`Open`, not in the constructor). Callers that attach to existing disk
state (probes, deletes) use `Open`; provisioning uses `Init` then
continues on the same `InfraOps` without a second `Open`.

## 8. AgentSpec Contract

Specification lives in `agent/PLAN.md`; cross-module surface fixed
here.

### 8.1 Interface shape

```go
type AgentSpec interface {
    Type() AgentType
    DisplayName() string
    Description() string

    // PluginLayout returns the strategy that maps canonical plugin
    // contents into agent-specific paths inside an InfraOps.
    PluginLayout() PluginLayout

    // Install bootstraps the workspace inside ops. Steps are
    // implementation-defined but MUST be idempotent so the install
    // worker can resume from any post-PutFile state if it crashes.
    // Install MAY start long-running processes (gateway, daemons);
    // CLI-style agents typically do not.
    Install(ctx context.Context, ops infraops.InfraOps, params InstallParams) error

    // Uninstall stops anything Install started. Best-effort.
    // Errors are logged but do not abort delete.
    Uninstall(ctx context.Context, ops infraops.InfraOps) error

    // Probe checks that a previously-installed workspace is reachable.
    // Probe MUST NOT mutate state.
    Probe(ctx context.Context, ops infraops.InfraOps) (ProbeResult, error)

    // ProtocolDescriptor returns a structured description of the
    // workspace's invocation surface. Consumers (e.g., orchestrator
    // assembly code) read this to construct an agentio.Agent
    // factory. The manager itself never reads the descriptor at
    // runtime.
    ProtocolDescriptor() ProtocolDescriptor
}
```

### 8.2 PluginLayout

```go
type PluginLayout interface {
    // Apply walks `pluginRoot` (the canonical fs.FS view of an
    // unpacked plugin zip) and writes files into ops at the
    // agent-specific paths. Returns the list of placed paths so the
    // workspace record can audit what was written.
    Apply(ctx context.Context, ops infraops.InfraOps, manifest plugin.Manifest, pluginRoot fs.FS) ([]string, error)
}
```

The layout is purely a path-translation policy; it does not read or
write anything beyond what `pluginRoot` exposes. Implementations live
next to the agent spec (e.g., `agent/claudecode/layout.go`) so each
agent type owns its mapping.

### 8.3 ProbeResult

```go
type ProbeResult struct {
    Healthy bool
    // Latency is the round-trip latency of the probe call.
    Latency time.Duration
    // Detail is a small, key→value summary surfaced in workspace
    // status (e.g., "version": "claude-code 1.4.2").
    Detail map[string]string
    // Error, when non-nil, explains why Healthy is false.
    Error *WorkspaceError
}
```

### 8.4 ProtocolDescriptor

A stable, JSON-friendly description of how to invoke the workspace.
The manager treats it as opaque. The shape is:

```go
type ProtocolDescriptor struct {
    Kind   ProtocolKind   // "cli" / "rest" / "socket" / future
    Detail map[string]any // kind-specific
}
```

Conventional `Detail` keys per kind are documented in `agent/PLAN.md`
§ProtocolDescriptor. Examples:

- `kind: "cli"` → `{ "command": ["claude", "code"], "resume_flag": "--resume" }`
- `kind: "rest"` → `{ "base_url_template": "http://{host}:{port}/v1", "port": 8080 }`

Application assembly code (in the host binary, outside `pkg/manager`)
maps `ProtocolDescriptor` + the workspace's resolved `InfraOps` to an
`agentio.Agent`. This bridge is intentionally outside the manager (see
§11).

### 8.5 InstallParams

```go
type InstallParams struct {
    Workspace WorkspaceSummary // namespace, alias, id, plugins (read-only)
    UserParams map[string]any  // verbatim from CreateWorkspaceRequest.InstallParams
}
```

`UserParams` keys are agent-specific and documented per agent's PLAN.

## 9. Logging & Observability

`pkg/manager` follows the repository logging conventions (`AGENTS.md`).

### 9.1 Structured logging

Every log line uses **`logs.FromContext(ctx)`** (falling back to
`logs.Default()` when no logger is stored on `ctx`) with stable keys.
Optional `Builder.Logger` is applied once at **`Build`** with
`logs.IntoContext` on the `ctx` passed into `workspace.Controller.Start`
and `workspace.ProbeScheduler.Start`. Those `Start` methods derive a
long-lived root with **`context.WithoutCancel`** so worker cancellation
is independent of the `Build` caller's deadline/cancel, while logger
values still propagate from that `ctx`.

| Key                | Description |
|--------------------|-------------|
| `op`               | Operation name (`manager.create_workspace`, `manager.probe`, …) |
| `component`        | `"manager"`, `"manager.workspace"`, `"manager.plugin"`, `"manager.probe"` |
| `workspace_id`     | When the line is workspace-scoped |
| `workspace_alias`  | Same |
| `namespace`        | Workspace or plugin namespace |
| `agent_type`       | `claude-code`, … |
| `infra_type`       | `localdir`, … |
| `plugin_id`        | When plugin-scoped |
| `latency_ms`       | Operation latency |
| `err`              | Error string (only if `level >= warn`) |

Sensitive values (secrets in `InstallParams`, `InfraOptions`) are
NEVER logged. Logging the **keys** of those maps is allowed; logging
**values** is forbidden.

### 9.2 Metrics & traces

v1 emits log-only observability. Metrics endpoints and tracing are not
in scope. The probe scheduler exposes its inner counters via `Stats()`
on the scheduler component for tests; production builds attach to it
through the logger.

### 9.3 Auditing

The manager does not provide a separate audit sink (the orchestrator
has its own). State transitions in the database (`Workspace.Status`
moves, `Plugin` create/delete) are the operational record. If
operators want a separate audit trail, they wrap the repositories.

## 10. Concurrency, Cancellation, Crash Safety

### 10.1 Concurrency model

- The Manager is safe for concurrent use by multiple callers.
- Plugin CRUD and Workspace CRUD share no locks beyond what the
  underlying repositories enforce. The default in-memory repositories
  use one mutex per resource family (one for plugins, one for
  workspaces).
- The probe scheduler runs in its own goroutine with a fixed-size
  worker pool. Workers operate on different workspaces by id; never on
  the same workspace concurrently.
- `CreateWorkspace` returns immediately after persisting the `init`
  row; the install runs in a worker goroutine owned by the manager.
  Workers are tracked in a `sync.WaitGroup`; `Stop` blocks until all
  install workers and probe workers terminate.

### 10.2 Cancellation

- Every method takes `context.Context`; ctx cancellation propagates to
  the underlying `InfraOps` calls (which propagate to subprocess /
  HTTP calls).
- Cancelling `CreateWorkspace`'s caller ctx does **not** cancel the
  install: the install runs under a manager-owned context. To cancel
  an in-flight install, call `DeleteWorkspace`. The DESIGN deliberately
  decouples caller lifetime from install lifetime to keep the API
  ergonomic.
- `Stop(ctx)` cancels all install / probe contexts and waits for the
  pool to drain, bounded by the supplied `ctx`. It then calls
  `Close` on the workspace repository, plugin repository, and plugin
  storage in that order; close errors are logged but do not abort
  shutdown. After `Stop`, the Manager rejects new operations with
  `ErrShutdown`. The manager does NOT close the configured
  `logs.Logger` / underlying handler, `agent.SpecSet`, or
  `infraops.FactorySet` — those are caller-owned.

### 10.3 Crash safety

- Every observable state transition writes to the repository **before**
  the side effect that the user cannot reverse:
  - `init` is persisted before any infra side effect.
  - `failed` is persisted before any cleanup that destroys evidence.
  - `healthy` is persisted only after the post-install probe succeeds.
- Plugin uploads are a two-phase commit:
  1. The manager resolves the existing `(namespace, name)` row via
     `PluginRepository.GetByName`; if absent, it allocates a new
     `PluginID`. Either way, this id is the storage key.
  2. `PluginStorage.Put(id, body)` writes the blob (overwriting any
     prior body at the same id atomically per the storage contract).
  3. `PluginRepository.Insert(plugin)` for fresh rows or
     `PluginRepository.Update(plugin)` for overwrites; the metadata
     write is the commit point.
  If step 3 fails for a fresh upload, the manager calls
  `PluginStorage.Delete(id)` best-effort, then returns the error.
  For overwrites, step 3 failure leaves the new blob in place behind
  the old metadata — a future re-upload re-overwrites; v1 does not
  reconcile.
  Orphan blobs (step-3 cleanup also failed) are tolerated — a future
  GC sweep reconciles. v1 does not ship a GC; operators can run a
  manual sweep using the `PluginStorage.List` (optional, see
  `plugin/PLAN.md`).

### 10.4 Manager restart

After a crash or restart:

- Workspaces in `init` may be orphaned (the install worker died). The
  probe scheduler force-flips `init` workspaces older than
  `InstallTimeout` to `failed`.
- Workspaces in `healthy`/`failed` are picked up by the probe loop on
  the next tick.
- Plugin orphans (zip with no metadata) are detectable via
  `PluginStorage.List` minus `PluginRepository.List`; see §10.3.

### 10.5 Loop protection

There is no loop in the manager runtime; every operation is
finite-step. Probe cadence is bounded by `ProbeInterval`. Install
attempts are not retried automatically — the operator deletes and
re-creates if needed.

## 11. Boundary with the Orchestrator

The manager and the orchestrator share **no Go imports**. The
boundary is enforced at the package level:

- `pkg/manager/...` MUST NOT import `pkg/agentio` or
  `pkg/orchestrator/...`.
- `pkg/orchestrator/...` MUST NOT import `pkg/manager/...` (already
  enforced by orchestrator AGENTS.md).

Integration happens in the **host binary**, outside both packages, by:

1. Reading `Workspace` records from the manager repository (or via
   `Manager.ListWorkspaces`).
2. Resolving the workspace's `AgentSpec` (via `Manager.AgentSpec(type)`,
   to be added if needed; or via the assembly code's own registry).
3. Calling `agentSpec.ProtocolDescriptor()` to learn the invocation
   shape.
4. Using `Manager.NewInfraOps(infraType, infraOptions)` to obtain a
   live `InfraOps`.
5. Synthesizing an `agentio.Agent` factory and registering it with
   `pkg/orchestrator/registry`.

This adapter layer lives in operator code (e.g., `cmd/ana/wiring.go`).
The manager's role ends at "I can hand you a `ProtocolDescriptor` and
an `InfraOps`."

> Future change: if multiple binaries need this glue, extract it into
> a thin package (e.g., `pkg/managerorchestrator`) that imports both
> `pkg/manager` and `pkg/orchestrator`. The manager itself stays
> agnostic.

## 12. Module Map

Detailed responsibilities live in each `PLAN.md`. Quick reference:

| Path                                       | Responsibility |
|--------------------------------------------|----------------|
| `pkg/manager/manager.go` (root)            | `Manager` interface, `Builder`, top-level types, install worker, probe scheduler wiring |
| `pkg/manager/types.go` (root)              | Cross-module ID types, request/option shapes, status enum |
| `pkg/manager/agent/`                       | `AgentSpec` interface + `PluginLayout`, `ProbeResult`, `ProtocolDescriptor`, `InstallParams` |
| `pkg/manager/agent/claudecode/`            | Reference `AgentSpec` for Claude Code |
| `pkg/manager/plugin/`                      | `Plugin` data model, `Manifest` schema, on-disk plugin format, `Storage` and `Repository` interfaces, default reference impls |
| `pkg/manager/infraops/`                    | `InfraOps` interface, `Options`, `Factory`, common types (`ExecCommand`, `ExecResult`) |
| `pkg/manager/infraops/localdir/`           | Reference `InfraOps` for a local directory |
| `pkg/manager/workspace/`                   | `Workspace` data model, `Repository` interface (default in-memory impl), status state machine, install worker, probe scheduler |

### 12.1 Dependency graph

```
                          ┌────────────────────────────────┐
   Layer 3 (root)         │ pkg/manager (Manager, Builder, │
                          │ install worker)                │
                          └────────────┬───────────────────┘
                                       │ imports all below
   ┌───────────────────────────────────┴────────────────────┐
   │                                                        │
   │  Layer 2:   workspace                                  │
   │             (→ agent, infraops, plugin)                │
   │                                                        │
   │  Layer 1:   agent/claudecode (→ agent, infraops,       │
   │             plugin)                                    │
   │             infraops/localdir (→ infraops)             │
   │                                                        │
   │  Layer 0:   agent      infraops      plugin            │
   │             (→ infraops, plugin)  (no internal imports)│
   │             (no other internal imports)                │
   │                                                        │
   └────────────────────────────────────────────────────────┘
```

Concrete import rules:

- `plugin/` and `infraops/` import nothing from `pkg/manager/...`.
- `agent/` imports `infraops/` (for `InfraOps` in `Install`/`Probe`)
  and `plugin/` (for `Manifest` in `PluginLayout`). It MUST NOT
  import `workspace/` or the root.
- `agent/claudecode/`, `infraops/localdir/` import only their parent
  package and the standard library; specifically they MUST NOT depend
  on `workspace/` or the root.
- `workspace/` imports `agent/`, `infraops/`, `plugin/`. It MUST NOT
  import the root package.
- The root package imports every subpackage to wire the manager.
- **No transitive import of `pkg/agentio` or `pkg/orchestrator/...`
  is allowed inside `pkg/manager/...`.** Enforce with a lint check or
  reviewer discipline.

## 13. v2 Extension Points

The design preserves clean insertion points for the following features.
None block v1.

- **Plugin versioning.** Add `Plugin.Version` (semver), allow multiple
  rows per `(namespace, name)`, store one zip per `(id, version)`,
  add `WorkspaceRepository` migration to track attached versions.
- **Plugin re-attach / hot upgrade.** New `Manager.UpdateWorkspace`
  method. Requires AgentSpec to declare `SupportsReinstall()`.
- **Multi-instance manager.** Replace in-memory repositories with a
  SQL backend; add a per-row leader election token; the probe
  scheduler holds a lease.
- **Authorization / namespacing with ACLs.** Insert a policy hook
  before `CreatePlugin` and `CreateWorkspace`; manager keeps no
  identity but accepts `caller` metadata in the request shapes (TBD
  in v2 design).
- **Workspace pause / resume.** Add states `paused` and `resuming` to
  the state machine. Operators stop the daemon without deleting the
  filesystem state.
- **Push delivery of status changes.** Adapter consuming a future
  `StatusEventBus` field on `Builder`; v1 does not emit status
  events.
- **Custom probe schedulers.** Allow `Builder.Scheduler` to be set to
  a user-supplied implementation when the default loop is unsuitable
  (e.g., distributed deployments).

## 14. Open Questions (deferred)

- **AgentSpec discovery via metadata APIs.** Should `Manager` expose
  `AgentSpec(type AgentType) (agent.AgentSpec, error)` for assembly
  code? Today the assembly code carries its own registry alongside
  the manager. Adding the lookup is convenient but couples consumers
  to the manager surface. Tentatively yes; finalize during
  implementation.
- **Plugin authoring tooling.** Out of scope for the design but
  influential: a CLI to lint a plugin directory, build the zip, and
  validate the manifest will materially affect the manifest shape we
  pin in `plugin/PLAN.md`. v1 ships the format; tooling is separate.
- **Manager-orchestrator glue.** Whether to extract the integration
  package now or after observing two consumers. v1 tolerates inline
  glue.
- **InfraOps.Stat / Remove / Mkdir.** Pure ergonomic addition; revisit
  once a second `InfraOps` implementation lands and we know the real
  duplication cost.

## 15. Glossary

- **Agent** — a *type* of agent program (Claude Code, OpenClaw, …).
  Represented by `agent.AgentSpec`.
- **Workspace** — an *instance* of an Agent inside an Infra, persisted
  in the manager database with a status machine.
- **Infra** — the runtime environment (local dir, container, sandbox)
  that hosts a workspace's files and processes.
- **InfraOps** — the abstract interface the manager uses to act on an
  infra: `Init`, `Exec`, `PutFile`, `GetFile`, `Request`, `Clear`.
- **Plugin** — a portable bundle of skills/rules/hooks/subagents in a
  zip with a manifest, stored in `PluginStorage`.
- **PluginLayout** — the per-Agent strategy that maps canonical plugin
  paths to agent-specific paths.
- **ProtocolDescriptor** — the structured, JSON-friendly description
  of how to invoke a workspace, returned by `AgentSpec`.
- **Namespace** — soft partition for alias / plugin-name uniqueness.
- **Probe** — a non-mutating health check executed by the scheduler.
