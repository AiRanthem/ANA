package storage

import (
	"context"
	"errors"

	"github.com/AiRanthem/ANA/pkg/manager/plugin"
	"github.com/AiRanthem/ANA/pkg/manager/workspace"
)

// Storage is the unified Data Access Layer (DAL) facade for the manager domain.
type Storage interface {
	WorkspaceRepo() workspace.Repository
	PluginRepo() plugin.Repository
	PluginStorage() plugin.Storage

	// Close unifies releasing underlying database connections and object storage client resources.
	Close(ctx context.Context) error
}

type storageImpl struct {
	workspaceRepo workspace.Repository
	pluginRepo    plugin.Repository
	pluginStorage plugin.Storage
}

func (s *storageImpl) WorkspaceRepo() workspace.Repository { return s.workspaceRepo }
func (s *storageImpl) PluginRepo() plugin.Repository       { return s.pluginRepo }
func (s *storageImpl) PluginStorage() plugin.Storage       { return s.pluginStorage }

func (s *storageImpl) Close(ctx context.Context) error {
	var errs []error
	if s.workspaceRepo != nil {
		if err := s.workspaceRepo.Close(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	if s.pluginRepo != nil {
		if err := s.pluginRepo.Close(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	if s.pluginStorage != nil {
		if err := s.pluginStorage.Close(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// Builder orchestrates the strict construction of a Storage instance.
type Builder struct {
	workspaceRepo workspace.Repository
	pluginRepo    plugin.Repository
	pluginStorage plugin.Storage
}

// NewBuilder creates an empty Builder for the Storage facade.
func NewBuilder() *Builder {
	return &Builder{}
}

// WithWorkspaceRepo sets the workspace.Repository implementation.
func (b *Builder) WithWorkspaceRepo(r workspace.Repository) *Builder {
	if b == nil {
		return nil
	}
	b.workspaceRepo = r
	return b
}

// WithPluginRepo sets the plugin.Repository implementation.
func (b *Builder) WithPluginRepo(r plugin.Repository) *Builder {
	if b == nil {
		return nil
	}
	b.pluginRepo = r
	return b
}

// WithPluginStorage sets the plugin.Storage implementation.
func (b *Builder) WithPluginStorage(s plugin.Storage) *Builder {
	if b == nil {
		return nil
	}
	b.pluginStorage = s
	return b
}

// Build validates that all required dependencies are present and returns a Storage instance.
func (b *Builder) Build() (Storage, error) {
	if b == nil {
		return nil, errors.New("storage: nil builder")
	}
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
