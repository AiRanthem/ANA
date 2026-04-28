# ANA Project TODOs

## Storage Module Implementation Plan

The goal is to decouple the `manager` domain from concrete storage implementations by introducing a dedicated `storage` module. This Data Access Layer (DAL) will unify Workspace and Plugin persistence under a single `Storage` facade.

### Phase 1: Architecture & Design (Completed)
- [x] Design the `Storage` facade interface and `Builder` pattern.
- [x] Establish the directory structure for `pkg/storage` and its providers (`memory`, `sqlite`, `localfs`).
- [x] Document the architecture in `pkg/storage/DESIGN.md`.
- [x] Create scoped context files (`AGENTS.md`) for all storage subpackages to guide Coding Agents.

### Phase 2: Core Interfaces & Memory Migration (Pending)
- [ ] Create `pkg/storage/storage.go` containing the `Storage` interface and the strict `Builder`.
- [ ] Migrate in-memory metadata repositories and blob storage to `pkg/storage/memory`:
  - [ ] Move `workspace.MemoryRepository` -> `pkg/storage/memory/workspace_repo.go` (or similar).
  - [ ] Move `plugin.MemoryRepository` -> `pkg/storage/memory/plugin_repo.go`.
  - [ ] Move `plugin.MemoryStorage` -> `pkg/storage/memory/plugin_storage.go`.
- [ ] Refactor `pkg/manager/manager.go` and associated tests to inject storage dependencies via `storage.Builder` rather than instantiating memory providers directly.

### Phase 3: Durable Providers Implementation (Pending)
- [ ] Implement `pkg/storage/sqlite` for durable, relational storage of Workspace and Plugin metadata.
  - Ensure complex fields (e.g., `Manifest`, `InfraOptions`) are correctly serialized as JSON/JSONB.
  - Implement atomic `UpdateStatusCAS` using SQL `UPDATE ... WHERE id = ? AND status = ?`.
- [ ] Implement `pkg/storage/localfs` for file-system-backed storage of Plugin ZIP blobs.
  - Ensure atomic `Put` operations via temporary files and `os.Rename`.
  - Validate all paths to prevent directory traversal escapes.

### Phase 4: Integration & Testing (Pending)
- [ ] Write integration tests for the `sqlite` and `localfs` providers.
- [ ] Verify that `Manager` works seamlessly with both `memory` and durable storage providers.
