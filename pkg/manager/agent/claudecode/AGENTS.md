# pkg/manager/agent/claudecode AGENTS.md

## Scope

Reference `agent.AgentSpec` implementation for the **Claude Code** CLI.
Owns the install routine, plugin layout strategy, probe routine, and
`ProtocolDescriptor` for Claude Code workspaces. CLI-only — no daemon
process is started; per-request invocation is the orchestrator CLI
bridge's responsibility.

This package is one of potentially many concrete agent specs. Other
agents (OpenClaw, custom HTTP-shaped agents, …) live in sibling
subpackages.

## Rules

- The `Type()` value is `"claude-code"` and never changes; workspace
  records depend on this id.
- Install is idempotent within a single call; the manager calls it
  exactly once per workspace.
- Probe is non-mutating. Read-only commands only (`claude --version`).
- Plugin layout must reject two plugins that sanitize to the same
  `<name>` segment **before** any `PutFile` call, so the working
  directory remains coherent on failure.
- Use `agent.PluginLayout` and `agent.ProtocolDescriptor` types
  verbatim from the parent package; do not duplicate the type
  definitions here.

## Don'ts

- Do not embed transport runtime. Per-request invocation belongs to
  `pkg/bridge/cli`; this spec only describes how to set the workspace
  up and how to reach it.
- Do not import `pkg/manager/workspace` or the manager root. The
  dependency graph allows `agent/claudecode → agent` and
  `agent/claudecode → infraops` and `agent/claudecode → plugin`,
  nothing more.
- Do not log secrets from `InstallParams.UserParams`; treat the map
  as opaque.
- Do not download `claude` from the internet inside `Install`.
  Operator-supplied binaries via PATH or `Options.Binary` only; the
  manager is not a package manager.
