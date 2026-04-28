# pkg/storage/sqlite AGENTS.md

## Scope
Durable relational storage implementation for Workspace and Plugin metadata using SQLite. Implements `workspace.Repository` and `plugin.Repository`.

## Rules
- Use standard `database/sql` with a robust SQLite driver.
- Ensure complex domain structs (e.g., `Manifest`, `InfraOptions`, `Labels`) are properly serialized to/from JSON/JSONB.
- Enforce unique constraints at the database schema level (e.g., `namespace` + `alias` for workspaces, `namespace` + `name` for plugins).
- `UpdateStatusCAS` MUST be implemented using atomic SQL updates (`UPDATE ... WHERE id = ? AND status = ?`).

## Don'ts
- Do not store Plugin ZIP blobs here.
- Do not expose database connection details outside this subpackage.
