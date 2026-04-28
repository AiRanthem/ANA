package manager

import (
	"errors"
	"fmt"
	"io"
	"regexp"
	"time"

	"github.com/AiRanthem/ANA/pkg/manager/agent"
	"github.com/AiRanthem/ANA/pkg/manager/infraops"
	"github.com/AiRanthem/ANA/pkg/manager/plugin"
	"github.com/AiRanthem/ANA/pkg/manager/workspace"
)

const defaultNamespace Namespace = "default"

var (
	namespacePattern = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)
	aliasPattern     = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,63}$`)
)

// Identifier types exposed by the manager root package.
type (
	PluginID    string
	WorkspaceID string
	Namespace   string
	Alias       string
	AgentType   string
	InfraType   string
)

// Plugin is the public plugin metadata value returned by Manager methods.
type Plugin struct {
	ID          PluginID
	Namespace   Namespace
	Name        string
	Description string
	Manifest    plugin.Manifest
	ContentHash string
	Size        int64
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// WorkspaceStatus is the public workspace lifecycle state.
type WorkspaceStatus string

const (
	StatusInit    WorkspaceStatus = "init"
	StatusHealthy WorkspaceStatus = "healthy"
	StatusFailed  WorkspaceStatus = "failed"
)

// WorkspaceError is the public persisted workspace failure payload.
type WorkspaceError struct {
	Code       string    `json:"code"`
	Message    string    `json:"message"`
	Phase      string    `json:"phase"`
	RecordedAt time.Time `json:"recorded_at"`
}

// AttachedPlugin is the public workspace plugin snapshot.
type AttachedPlugin struct {
	PluginID    PluginID `json:"plugin_id"`
	Name        string   `json:"name"`
	ContentHash string   `json:"content_hash"`
	PlacedPaths []string `json:"placed_paths,omitempty"`
}

// Workspace is the public workspace record returned by Manager methods.
type Workspace struct {
	ID            WorkspaceID       `json:"id"`
	Namespace     Namespace         `json:"namespace"`
	Alias         Alias             `json:"alias"`
	AgentType     AgentType         `json:"agent_type"`
	InfraType     InfraType         `json:"infra_type"`
	InfraOptions  infraops.Options  `json:"infra_options"`
	InstallParams map[string]any    `json:"install_params,omitempty"`
	Plugins       []AttachedPlugin  `json:"plugins,omitempty"`
	Status        WorkspaceStatus   `json:"status"`
	StatusError   *WorkspaceError   `json:"status_error,omitempty"`
	Description   string            `json:"description,omitempty"`
	Labels        map[string]string `json:"labels,omitempty"`
	LastProbeAt   time.Time         `json:"last_probe_at,omitempty"`
	CreatedAt     time.Time         `json:"created_at"`
	UpdatedAt     time.Time         `json:"updated_at"`
}

// CreatePluginRequest uploads or overwrites a plugin package.
type CreatePluginRequest struct {
	Namespace   Namespace
	Name        string
	Description string
	Content     io.Reader
}

// ListPluginsOptions filters plugin listing.
type ListPluginsOptions struct {
	Namespace Namespace
	NameLike  string
	Limit     int
	Cursor    string
}

// DownloadURLOptions controls plugin download URL generation.
type DownloadURLOptions struct {
	TTL time.Duration
}

// CreateWorkspaceRequest provisions a new workspace.
type CreateWorkspaceRequest struct {
	Namespace     Namespace
	Alias         Alias
	AgentType     AgentType
	InfraType     InfraType
	InfraOptions  infraops.Options
	InstallParams map[string]any
	Plugins       []PluginRef
	Description   string
	Labels        map[string]string
}

// PluginRef references an existing plugin by ID.
type PluginRef struct {
	ID PluginID
}

// ListWorkspacesOptions filters workspace listing.
type ListWorkspacesOptions struct {
	Namespace Namespace
	AgentType AgentType
	InfraType InfraType
	Status    WorkspaceStatus
	Labels    map[string]string
	Limit     int
	Cursor    string
}

// IDGenerator produces opaque manager IDs.
type IDGenerator interface {
	PluginID() PluginID
	WorkspaceID() WorkspaceID
}

// Common manager-facing sentinels.
var (
	ErrPluginNotFound         = plugin.ErrPluginNotFound
	ErrPluginNameConflict     = plugin.ErrPluginNameConflict
	ErrDuplicatePluginRef     = errors.New("manager: duplicate plugin ref")
	ErrWorkspaceDirConflict   = errors.New("manager: workspace dir conflict")
	ErrWorkspaceNotFound      = workspace.ErrWorkspaceNotFound
	ErrAliasConflict          = workspace.ErrAliasConflict
	ErrAgentTypeUnknown       = agent.ErrAgentTypeUnknown
	ErrInfraTypeUnknown       = infraops.ErrInfraTypeUnknown
	ErrInstallTimeout         = workspace.ErrInstallTimeout
	ErrShutdown               = errors.New("manager: shutdown")
	ErrUnsupportedDownloadURL = errors.New("manager: unsupported download URL")
)

func normalizeNamespace(namespace Namespace) Namespace {
	if namespace == "" {
		return defaultNamespace
	}
	return namespace
}

func validateNamespace(namespace Namespace) error {
	if !namespacePattern.MatchString(string(namespace)) {
		return fmt.Errorf("invalid namespace %q", namespace)
	}
	return nil
}

func validateAlias(alias Alias) error {
	if !aliasPattern.MatchString(string(alias)) {
		return fmt.Errorf("invalid alias %q", alias)
	}
	return nil
}

func validateDescription(description string) error {
	if len(description) > 1024 {
		return fmt.Errorf("description exceeds 1024 chars")
	}
	return nil
}

func clonePluginValue(p Plugin) Plugin {
	p.Manifest = cloneManifest(p.Manifest)
	return p
}

func cloneWorkspaceValue(w Workspace) Workspace {
	w.InfraOptions = cloneMapAny(w.InfraOptions)
	w.InstallParams = cloneMapAny(w.InstallParams)
	w.Plugins = cloneAttachedPlugins(w.Plugins)
	w.StatusError = cloneWorkspaceError(w.StatusError)
	w.Labels = cloneLabels(w.Labels)
	return w
}

func cloneAttachedPlugins(in []AttachedPlugin) []AttachedPlugin {
	if len(in) == 0 {
		return nil
	}
	out := make([]AttachedPlugin, len(in))
	for i := range in {
		out[i] = in[i]
		if len(in[i].PlacedPaths) > 0 {
			out[i].PlacedPaths = append([]string(nil), in[i].PlacedPaths...)
		}
	}
	return out
}

func cloneWorkspaceError(err *WorkspaceError) *WorkspaceError {
	if err == nil {
		return nil
	}
	out := *err
	return &out
}

func cloneManifest(m plugin.Manifest) plugin.Manifest {
	m.Plugin.Metadata = cloneMapAny(m.Plugin.Metadata)
	m.Skills = cloneManifestEntries(m.Skills)
	m.Rules = cloneManifestEntries(m.Rules)
	m.Hooks = cloneManifestEntries(m.Hooks)
	m.Subagents = cloneManifestEntries(m.Subagents)
	m.MCPs = cloneManifestEntries(m.MCPs)
	return m
}

func cloneManifestEntries(in map[string]plugin.ManifestEntry) map[string]plugin.ManifestEntry {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]plugin.ManifestEntry, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneLabels(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneMapAny[T ~map[string]any](in T) T {
	if len(in) == 0 {
		return nil
	}
	out := make(T, len(in))
	for k, v := range in {
		out[k] = deepCloneAny(v)
	}
	return out
}

func deepCloneAny(v any) any {
	switch x := v.(type) {
	case map[string]any:
		return cloneMapAny(x)
	case []any:
		out := make([]any, len(x))
		for i := range x {
			out[i] = deepCloneAny(x[i])
		}
		return out
	default:
		return v
	}
}
