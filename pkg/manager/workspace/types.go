package workspace

import (
	"context"
	"errors"
	"time"

	"github.com/AiRanthem/ANA/pkg/manager/agent"
	"github.com/AiRanthem/ANA/pkg/manager/infraops"
)

// Cross-module identifier aliases stay aligned with the manager design.
type (
	WorkspaceID = agent.WorkspaceID
	PluginID    = agent.PluginID
	Namespace   = agent.Namespace
	Alias       = agent.Alias
	AgentType   = agent.AgentType
	InfraType   = agent.InfraType
)

// Status is the persisted workspace lifecycle state.
type Status string

const (
	StatusInit    Status = "init"
	StatusHealthy Status = "healthy"
	StatusFailed  Status = "failed"
)

// StatusWriter identifies which component performs a CAS status transition.
type StatusWriter string

const (
	// StatusWriterController may transition init -> {healthy, failed}.
	StatusWriterController StatusWriter = "controller"
	// StatusWriterScheduler may transition init -> failed (watchdog), healthy <-> failed.
	StatusWriterScheduler StatusWriter = "scheduler"
)

// Error is the persisted workspace error record.
type Error = agent.WorkspaceError

// AttachedPlugin captures the plugin snapshot that was attached at workspace creation time.
type AttachedPlugin struct {
	PluginID    PluginID `json:"plugin_id"`
	Name        string   `json:"name"`
	ContentHash string   `json:"content_hash"`
	PlacedPaths []string `json:"placed_paths,omitempty"`
}

// Workspace is the manager's persisted workspace record.
type Workspace struct {
	ID            WorkspaceID       `json:"id"`
	Namespace     Namespace         `json:"namespace"`
	Alias         Alias             `json:"alias"`
	AgentType     AgentType         `json:"agent_type"`
	InfraType     InfraType         `json:"infra_type"`
	InfraOptions  infraops.Options  `json:"infra_options"`
	InstallParams map[string]any    `json:"install_params,omitempty"`
	Plugins       []AttachedPlugin  `json:"plugins,omitempty"`
	Status        Status            `json:"status"`
	StatusError   *Error            `json:"status_error,omitempty"`
	Description   string            `json:"description,omitempty"`
	Labels        map[string]string `json:"labels,omitempty"`
	LastProbeAt   time.Time         `json:"last_probe_at,omitempty"`
	CreatedAt     time.Time         `json:"created_at"`
	UpdatedAt     time.Time         `json:"updated_at"`
}

// ListOptions controls workspace listing.
type ListOptions struct {
	Namespace Namespace
	AgentType AgentType
	InfraType InfraType
	Status    Status
	Labels    map[string]string
	Limit     int
	Cursor    string
}

// Repository persists workspaces.
type Repository interface {
	Insert(ctx context.Context, w Workspace) error
	Get(ctx context.Context, id WorkspaceID) (Workspace, error)
	GetByAlias(ctx context.Context, namespace Namespace, alias Alias) (Workspace, error)
	List(ctx context.Context, opts ListOptions) ([]Workspace, string, error)
	Update(ctx context.Context, w Workspace) error
	UpdateStatus(ctx context.Context, id WorkspaceID, status Status, statusError *Error, lastProbeAt time.Time) error
	UpdateStatusCAS(
		ctx context.Context,
		id WorkspaceID,
		writer StatusWriter,
		expect Status,
		next Status,
		statusError *Error,
		lastProbeAt time.Time,
	) error
	Delete(ctx context.Context, id WorkspaceID) error
	Close(ctx context.Context) error
}

var (
	// ErrWorkspaceNotFound indicates a missing workspace row.
	ErrWorkspaceNotFound = errors.New("workspace: workspace not found")
	// ErrPluginContentHashMismatch indicates stored plugin bytes disagree with the workspace snapshot hash.
	ErrPluginContentHashMismatch = errors.New("workspace: plugin content hash mismatch")
	// ErrAliasConflict indicates that a namespace-scoped alias is already taken.
	ErrAliasConflict = errors.New("workspace: alias conflict")
	// ErrInvalidStatusTransition indicates a status transition outside the documented state machine.
	ErrInvalidStatusTransition = errors.New("workspace: invalid status transition")
	// ErrStatusPreconditionFailed indicates the row status did not match the expected value for a CAS write.
	ErrStatusPreconditionFailed = errors.New("workspace: status precondition failed")
	// ErrInstallTimeout marks workspaces that stayed in init beyond the configured install timeout.
	ErrInstallTimeout = errors.New("workspace: install timeout")
	// ErrControllerShutdown indicates the controller has been stopped and rejects new submissions.
	ErrControllerShutdown = errors.New("workspace: controller shutdown")
	// ErrSchedulerShutdown indicates the probe scheduler has been stopped.
	ErrSchedulerShutdown = errors.New("workspace: scheduler shutdown")
)
