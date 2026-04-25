# pkg/manager AGENTS.md

## Scope

`pkg/manager` is the workspace **lifecycle plane**. It owns the catalog
of Plugins, the registry of Agent specs and Infra factories, and the
repository of Workspaces. It bootstraps agent programs inside infras,
attaches plugins, persists workspace records, and drives a periodic
liveness probe.

This package depends on the standard library plus its own subpackages.
It is consumed by the host binary's assembly code (e.g., `cmd/ana/...`)
and is **decoupled from the orchestrator**: it MUST NOT import
`pkg/agentio` or `pkg/orchestrator/...`. Integration with the
orchestrator happens in operator code that reads workspace records and
synthesizes `agentio.Agent` factories from the agent's
`ProtocolDescriptor`.

## Authoritative documents

- `DESIGN.md` (this directory) — the comprehensive design and the
  source of truth for cross-module decisions. Any change to the
  workspace state machine, the Plugin/Workspace data model, the
  InfraOps method set, the AgentSpec contract, or the
  manager-orchestrator boundary MUST update `DESIGN.md` first.
- Per-module `PLAN.md` files — module-level details (interfaces, data
  shapes, edge cases). They MUST stay consistent with `DESIGN.md`.

If a module needs to deviate from `DESIGN.md`, update `DESIGN.md` in
the same change.

## Module conventions

- Every subpackage is small and exports a focused interface plus a
  default reference implementation when one is shipped.
- Subpackages follow the dependency graph in `DESIGN.md` §12.1. Adding
  a new edge requires updating that section.
- Cross-module integration tests live in this directory (root package),
  not in subpackages, to avoid circular references.
- Identifier types (`PluginID`, `WorkspaceID`, `Namespace`, `Alias`,
  `AgentType`, `InfraType`) are aliases over `string` declared in
  `types.go`. Use the named types at function signatures; raw strings
  are reserved for serialization boundaries.

## Naming

- Logger keys: `op`, `component`, `workspace_id`, `workspace_alias`,
  `namespace`, `agent_type`, `infra_type`, `plugin_id`, `latency_ms`,
  `err`. New keys require updating `DESIGN.md` §9.1.
- Sentinel errors: `ErrPluginNotFound`, `ErrWorkspaceNotFound`,
  `ErrAliasConflict`, `ErrAgentTypeUnknown`, `ErrInfraTypeUnknown`,
  `ErrShutdown`, `ErrInstallTimeout`, etc. Owned by the originating
  subpackage; the root package wraps them with operation context using
  `fmt.Errorf("<manager op>: %w", err)`.

## Things to avoid

- No transitive import of `pkg/agentio` or `pkg/orchestrator/...`. The
  manager is decoupled from the runtime control plane by design (see
  `DESIGN.md` §11). Adapter code lives in operator binaries, not here.
- No silent retries on install. A failed install moves the workspace to
  `failed` with a structured error; the operator decides whether to
  delete and recreate.
- No background work outside the documented loops (install worker pool,
  probe scheduler). New goroutines require a Builder option and a
  shutdown path through `Stop`.
- No reading or writing of secrets in this package. `InstallParams` and
  `InfraOptions` are stored verbatim and never logged at value level.
- No reach-around imports between subpackages. `agent/` does not see
  `workspace/`; `infraops/` does not see `agent/`. Stay inside the
  layered graph.
