package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"regexp"
	"time"

	"github.com/AiRanthem/ANA/pkg/manager/infraops"
	"github.com/AiRanthem/ANA/pkg/manager/plugin"
)

// AgentType identifies a registered agent spec.
type AgentType string

// ProtocolKind identifies how a workspace should be invoked.
type ProtocolKind string

const (
	ProtocolKindCLI    ProtocolKind = "cli"
	ProtocolKindREST   ProtocolKind = "rest"
	ProtocolKindSocket ProtocolKind = "socket"
)

// ProtocolDescriptor describes a workspace invocation surface.
type ProtocolDescriptor struct {
	Kind   ProtocolKind   `json:"kind"`
	Detail map[string]any `json:"detail,omitempty"`
}

// WorkspaceError is a machine-readable workspace failure.
type WorkspaceError struct {
	Code       string    `json:"code"`
	Message    string    `json:"message"`
	Phase      string    `json:"phase"`
	RecordedAt time.Time `json:"recorded_at"`
}

// ProbeResult reports the health status of a workspace probe.
type ProbeResult struct {
	Healthy bool              `json:"healthy"`
	Latency time.Duration     `json:"latency"`
	Detail  map[string]string `json:"detail,omitempty"`
	Error   *WorkspaceError   `json:"error,omitempty"`
}

// InstallParams are passed into AgentSpec.Install.
type InstallParams struct {
	Workspace  WorkspaceSummary `json:"workspace"`
	UserParams map[string]any   `json:"user_params,omitempty"`
}

// WorkspaceSummary is a read-only workspace view passed into AgentSpec.
type WorkspaceSummary struct {
	ID        WorkspaceID         `json:"id"`
	Namespace Namespace           `json:"namespace"`
	Alias     Alias               `json:"alias"`
	AgentType AgentType           `json:"agent_type"`
	InfraType InfraType           `json:"infra_type"`
	Plugins   []AttachedPluginRef `json:"plugins,omitempty"`
}

// AttachedPluginRef captures plugin metadata at attach time.
type AttachedPluginRef struct {
	PluginID    PluginID `json:"plugin_id"`
	Name        string   `json:"name"`
	ContentHash string   `json:"content_hash"`
}

// These cross-module identifier aliases are kept local here so this package
// remains independent from manager root package implementation details.
type (
	WorkspaceID string
	PluginID    string
	Namespace   string
	Alias       string
	InfraType   string
)

// PluginLayout maps canonical plugin content into an agent-specific path plan.
type PluginLayout interface {
	Apply(ctx context.Context, ops infraops.InfraOps, manifest plugin.Manifest, pluginRoot fs.FS) ([]string, error)
}

// AgentSpec describes how to install/probe/uninstall a concrete agent type.
type AgentSpec interface {
	Type() AgentType
	DisplayName() string
	Description() string
	PluginLayout() PluginLayout
	Install(ctx context.Context, ops infraops.InfraOps, params InstallParams) error
	Uninstall(ctx context.Context, ops infraops.InfraOps) error
	Probe(ctx context.Context, ops infraops.InfraOps) (ProbeResult, error)
	ProtocolDescriptor() ProtocolDescriptor
}

var (
	// ErrAgentTypeConflict indicates duplicate registration for the same type.
	ErrAgentTypeConflict = errors.New("agent: agent type conflict")
	// ErrAgentTypeUnknown indicates a lookup miss for an unregistered type.
	ErrAgentTypeUnknown = errors.New("agent: agent type unknown")
	// ErrInvalidProtocolDescriptor indicates a malformed protocol descriptor.
	ErrInvalidProtocolDescriptor = errors.New("agent: invalid protocol descriptor")
	// ErrInvalidPluginLayout indicates a spec returned an unusable PluginLayout.
	ErrInvalidPluginLayout = errors.New("agent: invalid plugin layout")
)

var agentTypePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// ValidateAgentType enforces the manager-wide type format.
func ValidateAgentType(agentType AgentType) error {
	if !agentTypePattern.MatchString(string(agentType)) {
		return fmt.Errorf("%w: %q", ErrAgentTypeUnknown, agentType)
	}
	return nil
}

// ValidateProtocolDescriptor validates the descriptor shape and JSON payload.
func ValidateProtocolDescriptor(desc ProtocolDescriptor) error {
	if desc.Kind == "" {
		return fmt.Errorf("%w: empty kind", ErrInvalidProtocolDescriptor)
	}

	switch desc.Kind {
	case ProtocolKindCLI, ProtocolKindREST, ProtocolKindSocket:
	default:
		return fmt.Errorf("%w: unsupported kind %q", ErrInvalidProtocolDescriptor, desc.Kind)
	}

	for k, v := range desc.Detail {
		if k == "" {
			return fmt.Errorf("%w: empty detail key", ErrInvalidProtocolDescriptor)
		}
		if err := validateJSONCompatibleValue(v); err != nil {
			return fmt.Errorf("%w: detail[%q]: %w", ErrInvalidProtocolDescriptor, k, err)
		}
	}

	return nil
}

func cloneProbeDetail(detail map[string]string) map[string]string {
	if len(detail) == 0 {
		return nil
	}
	out := make(map[string]string, len(detail))
	maps.Copy(out, detail)
	return out
}

func validateJSONCompatibleValue(v any) error {
	switch x := v.(type) {
	case nil, bool, string,
		float64, float32,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64:
		return nil
	case []any:
		for idx, elem := range x {
			if err := validateJSONCompatibleValue(elem); err != nil {
				return fmt.Errorf("array[%d]: %w", idx, err)
			}
		}
		return nil
	case map[string]any:
		for key, value := range x {
			if key == "" {
				return errors.New("empty map key")
			}
			if err := validateJSONCompatibleValue(value); err != nil {
				return fmt.Errorf("map[%q]: %w", key, err)
			}
		}
		return nil
	default:
		return fmt.Errorf("unsupported type %T", v)
	}
}
