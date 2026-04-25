# pkg/manager/agent AGENTS.md

## Scope

Defines the `AgentSpec` interface — the extension point that binds a
*kind* of agent program (Claude Code, OpenClaw, …) into the manager's
workspace lifecycle. Owns supporting types (`PluginLayout`,
`ProbeResult`, `ProtocolDescriptor`, `InstallParams`,
`WorkspaceSummary`) and the small `SpecSet` registry helper.

Per-agent implementations (e.g., Claude Code) live in subpackages
(`agent/claudecode/`, …) that import this package.

## Rules

- `AgentSpec` is purely an interface — no default implementation in
  this package. Concrete specs ship as separate subpackages so each
  one has a focused review surface.
- `SpecSet` is concurrency-safe; `Register` returns
  `ErrAgentTypeConflict` on duplicate `Type()`.
- All exported types in this package are JSON-serializable wherever a
  field can flow into the workspace record (`ProtocolDescriptor`,
  `ProbeResult`). The package documents the field types but does not
  enforce serialization at compile time.
- Use the cross-module names from `DESIGN.md` §6.1 / §8 verbatim
  (`AgentType`, `PluginLayout`, `ProtocolDescriptor`, …); do not invent
  local synonyms.

## Don'ts

- Do not import `pkg/manager/workspace`, `pkg/manager/plugin`'s
  storage / repository layer, or the manager root. The dependency
  graph only allows `agent → infraops` and `agent → plugin` (for
  `Manifest`); enforce by review.
- Do not embed transport behavior (HTTP clients, CLI runners) in this
  package; that is the job of the per-agent subpackage and, at
  invocation time, the assembly code that translates
  `ProtocolDescriptor` into an `agentio.Agent`.
- Do not call `pkg/manager/infraops.InfraOps.Clear`. Only the
  workspace controller may clear an infra.
- Do not log spec internals here; this is interface code.
