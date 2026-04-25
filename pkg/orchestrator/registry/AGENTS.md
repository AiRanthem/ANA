# pkg/orchestrator/registry AGENTS.md

## Scope

Catalog of Workspaces (alias, runtime kind, runtime config, agent factory)
plus alias and id indexes. The only place that maps a Salutation alias to a
concrete `agentio.Agent`.

## Rules

- The default in-memory implementation must be safe for concurrent reads
  and writes; verify with `go test -race`.
- `AgentFactory` is opaque to this package; do not instantiate or cache
  bridge clients here.
- Reject bad input at `Register`/`Update`. Engine code assumes lookups
  return well-formed `Workspace` values.
- Disabled workspaces are invisible to lookups but their ID stays reserved
  to avoid recycling collisions inside an audit history.

## Don'ts

- Do not import `pkg/bridge/...`. Bridge bindings flow in through the
  `AgentFactory` callback supplied by the caller.
- Do not read configuration from the environment, files, or remote
  services in this package; those are caller responsibilities.
- Do not silently mutate a workspace's alias mid-flight; replace via
  `Disable` + `Register` to avoid races with running tasks.
- Do not import `protocol/`. Alias regex validation is duplicated here at
  the boundary because the parser and the registry both need to be
  authoritative for their own surface.
