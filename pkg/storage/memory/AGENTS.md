# pkg/storage/memory AGENTS.md

## Scope
Thread-safe, in-memory implementations of the `workspace.Repository`, `plugin.Repository`, and `plugin.Storage` interfaces. Primarily used for unit testing, fast local development, and ephemeral instances.

## Rules
- All structures MUST be protected by `sync.RWMutex` to ensure thread safety during concurrent access.
- Must accurately simulate atomic operations (e.g., CAS for workspace status, atomic overwrite for plugin storage).

## Don'ts
- Do not persist data to disk.
- Do not leak memory (ensure Delete operations completely remove references).
