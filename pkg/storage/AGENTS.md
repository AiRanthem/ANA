# pkg/storage AGENTS.md

## Scope
The unified Data Access Layer (DAL) for ANA. It provides the `Storage` facade interface and a strict `Builder` to assemble underlying storage providers (metadata repositories and blob storage) before injecting them into `pkg/manager`.

## Rules
- Maintain strict separation from domain logic; this module only handles data persistence and retrieval.
- The `Builder` MUST enforce that all three dependencies (WorkspaceRepo, PluginRepo, PluginStorage) are provided before successfully building the `Storage` instance.
- Do NOT import `pkg/manager/workspace` or `pkg/manager/plugin` internals, only their public interfaces.

## Don'ts
- Do not implement business logic (e.g., manifest validation, status state machines) here.
- Do not bypass the `Builder` constraints.
