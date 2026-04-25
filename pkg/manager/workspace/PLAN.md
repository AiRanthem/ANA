# workspace/PLAN.md

## Purpose

Own the workspace **lifecycle engine**:

- The `Workspace` data model (per `DESIGN.md` §6.3) and the
  `Repository` interface that persists it.
- The `Status` state machine and the validated transitions.
- The `Controller` that drives `CreateWorkspace` / `DeleteWorkspace`
  end-to-end (calling into `agent.AgentSpec` and `infraops.InfraOps`
  per `DESIGN.md` §4).
- The `ProbeScheduler` that periodically calls `AgentSpec.Probe` on
  active workspaces and updates status.

This package depends on `agent/`, `infraops/`, and `plugin/`. It is
imported by the manager root package and nothing else.

## Public surface (intent only)

- Types (the manager root re-exports these with `Workspace`-prefixed
  names per `DESIGN.md` §6 — `WorkspaceStatus`, `WorkspaceError`):
  - `Workspace` value type per `DESIGN.md` §6.3.
  - `Status` enum: `init`, `healthy`, `failed`.
  - `Error` — diagnostic record per `DESIGN.md` §6.4.
  - `AttachedPlugin` per `DESIGN.md` §6.5.
- `Repository` interface (§3 below).
- `Controller` struct + `ControllerConfig` (§4).
- `ProbeScheduler` struct + `ProbeSchedulerConfig` (§5).
- Sentinel errors:
  - `ErrWorkspaceNotFound`
  - `ErrAliasConflict`
  - `ErrInvalidStatusTransition`
  - `ErrInstallTimeout`
  - `ErrControllerShutdown`
  - `ErrSchedulerShutdown`

## Repository

```go
type Repository interface {
    Insert(ctx context.Context, w Workspace) error
    Get(ctx context.Context, id WorkspaceID) (Workspace, error)
    GetByAlias(ctx context.Context, namespace Namespace, alias Alias) (Workspace, error)
    List(ctx context.Context, opts ListOptions) ([]Workspace, string, error) // rows + nextCursor
    Update(ctx context.Context, w Workspace) error
    UpdateStatus(ctx context.Context, id WorkspaceID, status Status, statusError *Error, lastProbeAt time.Time) error
    Delete(ctx context.Context, id WorkspaceID) error
    Close(ctx context.Context) error
}

type ListOptions struct {
    Namespace Namespace
    AgentType AgentType
    InfraType InfraType
    Status    Status
    Labels    map[string]string
    Limit     int
    Cursor    string
}
```

Behavior rules:

- `Insert` returns `ErrAliasConflict` if `(Namespace, Alias)` is taken;
  the row is unchanged.
- `UpdateStatus` is the **only** write that flips `Status`. All other
  writes (e.g., `Update` for description / labels) MUST refuse to
  change `Status` and return `ErrInvalidStatusTransition` on attempt
  — the controller is the single owner of state changes.
- `UpdateStatus` validates legal transitions per `DESIGN.md` §5.1:
  - `init → healthy` ok.
  - `init → failed` ok.
  - `healthy → failed` ok.
  - `failed → healthy` ok.
  - `init → init` (idempotent on insert) ok.
  - Any other transition returns `ErrInvalidStatusTransition`.
- `List` returns rows sorted by `(Namespace, Alias)` ascending; the
  cursor encodes the last seen row.
- All methods are safe for concurrent use.

Implementations:

- `MemoryRepository` — reference, mutex-protected. Suitable for
  tests and single-instance demos.
- Production deployments supply a SQL-backed implementation and
  inject it through `Builder.WorkspaceRepository`.

## Controller

The Controller is the workhorse that drives a `CreateWorkspace` /
`DeleteWorkspace` from API entry to terminal status. It is owned by
the manager root and reused across all callers.

```go
type Controller struct {
    Repo          Repository
    PluginStorage plugin.Storage
    AgentSpecs    *agent.SpecSet
    Factories     *infraops.FactorySet
    Clock         func() time.Time
    Logger        *slog.Logger

    // tuning
    InstallTimeout time.Duration // default 10 min
    InstallWorkers int           // default 4
    MaxPluginSize  int64         // default 256 MiB; enforced when
                                 // expanding zips during install.
}

func (c *Controller) Start(ctx context.Context) error
func (c *Controller) Stop(ctx context.Context) error

// Submit enqueues a CreateWorkspace install run. The Workspace passed
// in is already persisted in `init`. Submit returns immediately; the
// install runs on a worker. ErrControllerShutdown if Stop has been
// called.
func (c *Controller) Submit(ctx context.Context, w Workspace, params agent.InstallParams) error

// Delete tears down a Workspace synchronously. Errors that occur in
// AgentSpec.Uninstall and InfraOps.Clear are logged but do not block
// the eventual Repository.Delete call. Returns ErrWorkspaceNotFound
// if the row is gone.
func (c *Controller) Delete(ctx context.Context, id WorkspaceID) error

// CountInflight reports the number of in-progress installs (for
// tests and metrics).
func (c *Controller) CountInflight() int
```

### Install worker pipeline

For each `Submit(workspace, params)`:

1. Acquire a slot in the worker pool (bounded by `InstallWorkers`).
2. Build an install context derived from `c.installCtx` (cancelled by
   `Stop`) with deadline `now + InstallTimeout`.
3. Resolve `factory := c.Factories.Get(workspace.InfraType)`. If
   missing, transition workspace to `failed` and return.
4. Build `ops := factory(ctx, workspace.InfraOptions)`. Persist any
   factory error to `failed`.
5. `ops.Init(ctx)`. Persist `ErrAlreadyInitialized` and any other
   `Init` error to `failed`.
6. Resolve `spec := c.AgentSpecs.Get(workspace.AgentType)`. If
   missing, persist `failed` (this should be unreachable because
   the Manager validates the agent type at Submit time, but the
   defensive check stays in case the registry is mutated mid-flight
   by tests).
7. For each `pluginRef` in `workspace.Plugins`:
   a. `body, obj, err := c.PluginStorage.Get(ctx, pluginRef.PluginID)`.
   b. `reader, err := plugin.OpenZipReaderFromStream(ctx, body, obj.Size, c.MaxPluginSize)`
      (the helper buffers the stream and validates the manifest).
   c. `placedPaths, err := spec.PluginLayout().Apply(ctx, ops,
      reader.Manifest(), reader.FS())`.
   d. Append `(pluginRef.PluginID, placedPaths)` to a result slice.
   e. `reader.Close()`; defer `body.Close()`.
   Any error in this loop transitions the workspace to `failed`.
8. `spec.Install(ctx, ops, params)`. On error → `failed`.
9. `result, err := spec.Probe(ctx, ops)`. If `err` or
   `!result.Healthy`, transition to `failed` with the reported error.
10. Persist the AttachedPlugins (with `placedPaths`) via
    `Repository.Update`, then transition to `healthy` via
    `Repository.UpdateStatus`.
11. Release the worker pool slot.

Cancellation:

- `Stop` cancels `installCtx`. In-flight workers see ctx cancel,
  flush the partial state to `failed`, and exit. `Stop` waits for
  the worker pool to drain (bounded by the supplied `ctx`).
- An individual install whose context exceeds `InstallTimeout`
  transitions to `failed` with `ErrInstallTimeout`. The probe
  scheduler also force-flips `init` workspaces older than
  `InstallTimeout` (covers the case where the worker died without
  writing).

### Delete pipeline

`Controller.Delete(ctx, id)`:

1. `w, err := Repo.Get(ctx, id)`. If `ErrWorkspaceNotFound`, return
   it.
2. Build `factory`, then `ops := factory(ctx, w.InfraOptions)`.
3. Resolve `spec := AgentSpecs.Get(w.AgentType)`.
4. `spec.Uninstall(ctx, ops)` — log errors, continue.
5. `ops.Clear(ctx)` — log errors, continue.
6. `Repo.Delete(ctx, id)`. Returns nil even when steps 4/5 logged
   errors.

The Delete is **synchronous** because it is fast and operators want
to know the row is gone before they re-create. If a backend grows
slow `Uninstall` (e.g., draining a long-running daemon), v2 may
introduce an async variant.

## Probe scheduler

```go
type ProbeScheduler struct {
    Repo       Repository
    AgentSpecs *agent.SpecSet
    Factories  *infraops.FactorySet
    Clock      func() time.Time
    Logger     *slog.Logger

    Interval        time.Duration // default 30s
    Workers         int           // default 4
    Timeout         time.Duration // default 5s; per-probe ctx deadline
    InstallTimeout  time.Duration // mirrors Controller; used to flip stuck `init`
}

func (s *ProbeScheduler) Start(ctx context.Context) error
func (s *ProbeScheduler) Stop(ctx context.Context) error
func (s *ProbeScheduler) Stats() ProbeStats
```

### Tick

Every `Interval`:

1. Call `Repo.List(opts={Statuses: [healthy, failed, init]})` in
   pages.
2. For each row:
   a. If `Status == init` and `now - CreatedAt > InstallTimeout`,
      transition to `failed` with `ErrInstallTimeout`.
   b. If `Status in {healthy, failed}`, dispatch to a worker:
      - Build `factory`, `ops := factory(ctx, row.InfraOptions)`.
      - `result, err := spec.Probe(probeCtx, ops)`.
      - Compute `newStatus := healthy` if `err == nil &&
        result.Healthy`, else `failed`.
      - If `newStatus != row.Status` OR result has new `Detail`,
        `Repo.UpdateStatus(...)`.
3. Wait for all worker goroutines for this tick to complete before
   sleeping until the next tick.

`probeCtx` is `ctx` from `Start` plus `WithTimeout(Timeout)`.

### Concurrency

- A workspace is probed by **at most one** worker per tick. The
  scheduler dedups by `WorkspaceID` within a tick.
- Workers across ticks may overlap if a tick takes longer than
  `Interval`. The scheduler drops a tick rather than running two in
  parallel: when the previous tick is still active at tick time,
  log `slog.Warn(probe_overrun)` and skip.

### Stats

`ProbeStats` exposes counters for tests:

```go
type ProbeStats struct {
    TicksRun        uint64
    ProbesAttempted uint64
    ProbesHealthy   uint64
    ProbesFailed    uint64
    Overruns        uint64
}
```

## State machine

Per `DESIGN.md` §5.1. The controller is the only writer of `init →
{healthy, failed}`. The probe scheduler is the only writer of
`{healthy ↔ failed}` and the only writer of `init → failed` via the
install-timeout watchdog.

```
        Controller.Submit
              │
              ▼
        ┌──────┐  install ok    ┌─────────┐
        │ init ├──────────────▶ │ healthy │
        └──┬───┘                └────┬────┘
           │ install fail            │ probe fail
           │     OR                  ▼
           │ install timeout    ┌────────┐
           ▼                    │ failed │
        ┌────────┐ ◀────────────┤        │
        │ failed │   (probe ok) └────────┘
        └────────┘
```

Workspaces never have a "deleted" status; `Delete` removes the row.

## Edge cases & decisions

- **AgentType / InfraType disappears between Insert and install
  worker.** Should not happen because the manager registers types
  before accepting calls, but the worker still handles the missing
  case gracefully by transitioning to `failed` with
  `ErrAgentTypeUnknown` or `ErrInfraTypeUnknown`.
- **PluginStorage.Get returns 404 mid-install.** Plugin was deleted
  between submit and worker pickup. Workspace transitions to
  `failed` with `ErrPluginNotFound`; partial files written so far
  remain (cleanup happens at Delete).
- **Repository write fails during install.** The worker logs the
  error, retries the same write up to two times with exponential
  backoff (250 ms / 750 ms), then gives up. The workspace is left
  in its current persisted status; the next probe tick will pick
  it up.
- **Two CreateWorkspace calls race for the same alias.** The first
  Insert wins; the second returns `ErrAliasConflict`. No worker is
  scheduled for the loser.
- **Probe scheduler started before Controller.** Allowed; the
  scheduler will skip `init` rows until they age out (transitioning
  them to `failed`), which is the correct behavior when the
  controller never finishes them.
- **Stop while installs are in-flight.** Each in-flight install
  transitions its workspace to `failed{Code:"shutdown"}` and exits.
  Workers drain before `Stop` returns. Restarting the manager
  resumes from those `failed` rows; operators delete and recreate.

## Tests to write (no implementation in this pass)

1. `MemoryRepository` round-trip: Insert / Get / GetByAlias / List /
   UpdateStatus / Delete.
2. `Repository.UpdateStatus` rejects illegal transitions.
3. `Controller.Submit` runs the canonical pipeline against a fake
   `infraops` + fake `agent.AgentSpec` and lands `healthy`.
4. `Controller.Submit` lands `failed` when `Install` returns an
   error; `StatusError` populated.
5. `Controller.Submit` honors `InstallTimeout`.
6. `Controller.Delete` is idempotent against a missing row.
7. `Controller.Delete` succeeds even when `Uninstall` errors.
8. `ProbeScheduler` tick flips `healthy → failed` and back.
9. `ProbeScheduler` flips stuck `init → failed` after
   `InstallTimeout`.
10. `ProbeScheduler` skips overrunning ticks and counts overruns.
11. Concurrent `Controller.Submit` + `ProbeScheduler` under `-race`.

## Out of scope

- Async delete with a "deleting" status. v2.
- Multi-instance manager coordination (leases, distributed
  scheduling). v2.
- Workspace **update** beyond labels/description (which `Update`
  supports). Adding plugins to a live workspace is a v2 feature.
- A separate "in-place reinstall" flow. Re-creation is the policy.
