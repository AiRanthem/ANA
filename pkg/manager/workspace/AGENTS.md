# pkg/manager/workspace AGENTS.md

## Scope

Workspace lifecycle engine: the `Workspace` data model, the
`Repository` interface (and a `MemoryRepository` reference impl), the
status state machine, the `Controller` that drives `CreateWorkspace`
and `DeleteWorkspace` end-to-end, and the `ProbeScheduler` that
periodically probes active workspaces.

This package is the only writer of `Workspace.Status`. The state
machine in `DESIGN.md` §5.1 is the source of truth.

## Rules

- The `Controller` is the **only** writer of the `init → {healthy,
  failed}` transition. The `ProbeScheduler` is the **only** writer
  of `{healthy ↔ failed}` and the `init → failed` install-timeout
  watchdog. Repository implementations enforce this with
  `UpdateStatus`-only transitions and reject status mutations from
  the generic `Update` method.
- Persist before side effects: `init` is written before any infra
  call; terminal status is written after the side effects complete.
  This invariant lets the manager recover after restart.
- `Controller.Submit` returns immediately after queuing; the install
  runs on a worker. `Controller.Delete` is synchronous.
- `ProbeScheduler` skips `init` rows except for the install-timeout
  watchdog. Probes are non-mutating by spec; never trigger an
  install retry from within a probe.
- Use the cross-module names from `DESIGN.md` §6 verbatim
  (`Status`, `WorkspaceID`, `Namespace`, `Alias`, …).
- Run `go test -race` for any change in this package — the
  controller, scheduler, and repository all coordinate under
  contention.

## Don'ts

- Do not perform retries with policy here. The v1 controller writes
  `failed` and lets the operator decide. Retry/back-off lives in v2
  with explicit knobs.
- Do not start goroutines outside the documented pools (install
  workers, scheduler workers). New goroutines need a Builder option
  and a deterministic shutdown path.
- Do not import the manager root package or `pkg/agentio`. The
  dependency graph allows `workspace → {agent, infraops, plugin}`,
  nothing more.
- Do not log secrets from `InstallParams` or `InfraOptions`. Log
  structural fields only (`workspace_id`, `workspace_alias`,
  `namespace`, `agent_type`, `infra_type`, `phase`,
  `latency_ms`, `err`).
- Do not delete plugin storage records from this package. The
  workspace controller copies plugin contents at install time and
  never reaches back into `plugin.Storage` afterwards.
