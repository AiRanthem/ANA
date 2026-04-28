# ANA Storage Module Design

## 1. Problem Statement
The current `pkg/manager` module tightly couples its domain logic with concrete, in-memory storage implementations (`memory_repository.go`, `memory_storage.go`). This violates the separation of concerns, making the domain layer less pure and complicating the injection of durable persistence backends (like SQLite, PostgreSQL, S3, or LocalFS) required for production deployments. Additionally, initializing the manager without ensuring all storage dependencies are fulfilled can lead to runtime panics or invalid states.

## 2. Rationale
To address these issues, we introduce the `storage` module. This module acts as the Data Access Layer (DAL) abstraction between the `manager` domain and the physical storage engines.
- **Purity:** By moving concrete implementations (e.g., `memory`) into the `storage` module, `pkg/manager` remains focused strictly on workspace lifecycle management and plugin validation.
- **Safety:** A strict `Builder` pattern enforces that the application cannot instantiate the storage layer without explicitly providing all required backends (Workspace metadata, Plugin metadata, Plugin blob storage).
- **Extensibility:** New storage backends (e.g., `sqlite` for metadata, `localfs` for blobs) can be implemented as subpackages within `pkg/storage` without altering the manager's core logic.

## 3. Detailed Design

### 3.1 The `Storage` Facade
The module exposes a unified `Storage` interface that acts as a registry for the underlying repositories.

```go
package storage

import (
	"context"

	"github.com/AiRanthem/ANA/pkg/manager/plugin"
	"github.com/AiRanthem/ANA/pkg/manager/workspace"
)

type Storage interface {
	WorkspaceRepo() workspace.Repository
	PluginRepo() plugin.Repository
	PluginStorage() plugin.Storage

	// Close unifies releasing underlying database connections and object storage client resources
	Close(ctx context.Context) error
}
```

### 3.2 The Strict Builder Pattern
To prevent incomplete initialization, the `Builder` requires three distinct `With*` method calls before `Build()` can succeed.

```go
type Builder struct {
	workspaceRepo workspace.Repository
	pluginRepo    plugin.Repository
	pluginStorage plugin.Storage
}

func NewBuilder() *Builder {
	return &Builder{}
}

func (b *Builder) WithWorkspaceRepo(r workspace.Repository) *Builder {
	b.workspaceRepo = r
	return b
}

func (b *Builder) WithPluginRepo(r plugin.Repository) *Builder {
	b.pluginRepo = r
	return b
}

func (b *Builder) WithPluginStorage(s plugin.Storage) *Builder {
	b.pluginStorage = s
	return b
}

func (b *Builder) Build() (Storage, error) {
	if b.workspaceRepo == nil {
		return nil, errors.New("storage: workspace repository is required")
	}
	if b.pluginRepo == nil {
		return nil, errors.New("storage: plugin repository is required")
	}
	if b.pluginStorage == nil {
		return nil, errors.New("storage: plugin storage is required")
	}

	return &storageImpl{
		workspaceRepo: b.workspaceRepo,
		pluginRepo:    b.pluginRepo,
		pluginStorage: b.pluginStorage,
	}, nil
}
```

### 3.3 Subpackages (Providers)
- `pkg/storage/memory`: Target location for migrated in-memory implementations (`MemoryRepository`, `MemoryStorage`) primarily used for testing and local dev.
- `pkg/storage/sqlite`: Relational database implementation for Workspace and Plugin metadata.
- `pkg/storage/localfs`: Local filesystem implementation for Plugin ZIP blob storage.
