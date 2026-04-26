package claudecode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"sort"
	"strings"
	"text/template"
	"time"

	"github.com/AiRanthem/ANA/pkg/manager/agent"
	"github.com/AiRanthem/ANA/pkg/manager/infraops"
)

const (
	specType        agent.AgentType = "claude-code"
	specDisplayName                 = "Claude Code"
	specDescription                 = "Anthropic's Claude Code CLI as a CLI-only workspace bootstrapped in an InfraOps environment."
	defaultBinary                   = "claude"
)

var (
	// ErrBinaryUnavailable indicates the configured Claude binary is not runnable.
	ErrBinaryUnavailable = errors.New("claudecode: binary unavailable")
	// ErrInvalidPluginLayout is re-exported from agent for layout planning failures.
	ErrInvalidPluginLayout = agent.ErrInvalidPluginLayout
)

// Options configures the Claude Code reference spec.
type Options struct {
	// Binary is the executable used by Install and Probe.
	// Empty means "claude" (resolved from PATH inside the infra).
	Binary string

	// SettingsTemplate overrides the default .claude/settings.json content.
	// The template receives a single field: {Workspace agent.WorkspaceSummary}.
	SettingsTemplate *template.Template

	// ProbeArgs overrides the default probe args ("--version").
	ProbeArgs []string
}

// Spec is the reference implementation of agent.AgentSpec for Claude Code.
type Spec struct {
	binary           string
	settingsTemplate *template.Template
	probeArgs        []string
	layout           *layout
}

// New constructs a Claude Code reference spec.
func New(opts Options) (Spec, error) {
	binary := defaultBinary
	if opts.Binary != "" {
		if strings.TrimSpace(opts.Binary) == "" {
			return Spec{}, errors.New("claudecode new: binary must not be blank")
		}
		if strings.ContainsAny(opts.Binary, ";|&><") {
			return Spec{}, fmt.Errorf("claudecode new: binary contains shell metacharacters: %q", opts.Binary)
		}
		binary = opts.Binary
	}

	probeArgs := []string{"--version"}
	if len(opts.ProbeArgs) > 0 {
		if len(opts.ProbeArgs) > 8 {
			return Spec{}, fmt.Errorf("claudecode new: probe args exceed limit: %d", len(opts.ProbeArgs))
		}
		probeArgs = slices.Clone(opts.ProbeArgs)
		for idx, arg := range probeArgs {
			if arg == "" {
				return Spec{}, fmt.Errorf("claudecode new: probe args[%d] is empty", idx)
			}
		}
	}

	var settingsTemplate *template.Template
	if opts.SettingsTemplate != nil {
		cloned, err := opts.SettingsTemplate.Clone()
		if err != nil {
			return Spec{}, fmt.Errorf("claudecode new: clone settings template: %w", err)
		}
		settingsTemplate = cloned
	}

	return Spec{
		binary:           binary,
		settingsTemplate: settingsTemplate,
		probeArgs:        probeArgs,
		layout:           newLayout(),
	}, nil
}

// Type returns the stable manager identifier for this spec.
func (s Spec) Type() agent.AgentType { return specType }

// DisplayName returns the user-facing name for this spec.
func (s Spec) DisplayName() string { return specDisplayName }

// Description returns a concise description of this spec.
func (s Spec) Description() string { return specDescription }

// PluginLayout returns the canonical Claude Code plugin layout strategy.
func (s Spec) PluginLayout() agent.PluginLayout {
	if s.layout == nil {
		return newLayout()
	}
	return s.layout
}

// Install verifies the binary and writes deterministic workspace seed files.
func (s Spec) Install(ctx context.Context, ops infraops.InfraOps, params agent.InstallParams) error {
	if _, err := s.execVersion(ctx, ops, []string{"--version"}); err != nil {
		return fmt.Errorf("claudecode install verify binary: %w", err)
	}

	settingsJSON, err := s.renderSettings(params.Workspace)
	if err != nil {
		return fmt.Errorf("claudecode install render settings: %w", err)
	}
	if err := ops.PutFile(ctx, path.Join(".claude", "settings.json"), bytes.NewReader(settingsJSON), 0o644); err != nil {
		return fmt.Errorf("claudecode install put settings: %w", err)
	}

	agentsDoc := renderWorkspaceAgents(params.Workspace)
	if err := ops.PutFile(ctx, "AGENTS.md", bytes.NewReader(agentsDoc), 0o644); err != nil {
		return fmt.Errorf("claudecode install put workspace agents: %w", err)
	}

	return nil
}

// Uninstall is a no-op because this spec starts no daemon.
func (s Spec) Uninstall(context.Context, infraops.InfraOps) error { return nil }

// Probe is read-only and checks binary availability/version.
func (s Spec) Probe(ctx context.Context, ops infraops.InfraOps) (agent.ProbeResult, error) {
	start := time.Now()
	version, err := s.execVersion(ctx, ops, s.probeCommandArgs())
	latency := time.Since(start)

	if err != nil {
		return agent.ProbeResult{
			Healthy: false,
			Latency: latency,
			Error: &agent.WorkspaceError{
				Code:       "probe.binary_unavailable",
				Message:    err.Error(),
				Phase:      "probe",
				RecordedAt: time.Now(),
			},
		}, nil
	}

	return agent.ProbeResult{
		Healthy: true,
		Latency: latency,
		Detail: map[string]string{
			"version": version,
		},
	}, nil
}

// ProtocolDescriptor returns the stable invocation descriptor for CLI bridge.
func (s Spec) ProtocolDescriptor() agent.ProtocolDescriptor {
	return agent.ProtocolDescriptor{
		Kind: agent.ProtocolKindCLI,
		Detail: map[string]any{
			"command":             []any{"claude", "code"},
			"resume_flag":         "--resume",
			"cwd_relative_to_dir": "",
			"stdin_input":         true,
		},
	}
}

func (s Spec) execVersion(ctx context.Context, ops infraops.InfraOps, args []string) (string, error) {
	result, err := ops.Exec(ctx, infraops.ExecCommand{
		Program: s.binaryName(),
		Args:    slices.Clone(args),
	})
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrBinaryUnavailable, err)
	}
	if result.ExitCode != 0 {
		reason := firstNonEmptyString(
			strings.TrimSpace(string(result.Stderr)),
			strings.TrimSpace(string(result.Stdout)),
			fmt.Sprintf("exit code %d", result.ExitCode),
		)
		return "", fmt.Errorf("%w: %s", ErrBinaryUnavailable, reason)
	}

	version := firstNonEmptyString(
		firstNonEmptyLine(string(result.Stdout)),
		firstNonEmptyLine(string(result.Stderr)),
		"unknown",
	)
	return version, nil
}

func (s Spec) binaryName() string {
	if s.binary == "" {
		return defaultBinary
	}
	return s.binary
}

func (s Spec) probeCommandArgs() []string {
	if len(s.probeArgs) == 0 {
		return []string{"--version"}
	}
	return slices.Clone(s.probeArgs)
}

func (s Spec) renderSettings(workspace agent.WorkspaceSummary) ([]byte, error) {
	if s.settingsTemplate != nil {
		var buf bytes.Buffer
		data := struct {
			Workspace agent.WorkspaceSummary
		}{
			Workspace: workspace,
		}
		if err := s.settingsTemplate.Execute(&buf, data); err != nil {
			return nil, fmt.Errorf("execute settings template: %w", err)
		}
		return buf.Bytes(), nil
	}

	cfg := struct {
		SchemaVersion int `json:"schema_version"`
		Workspace     struct {
			Alias     string `json:"alias"`
			Namespace string `json:"namespace"`
		} `json:"workspace"`
	}{
		SchemaVersion: 1,
	}
	cfg.Workspace.Alias = string(workspace.Alias)
	cfg.Workspace.Namespace = string(workspace.Namespace)

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal default settings: %w", err)
	}
	return append(data, '\n'), nil
}

func renderWorkspaceAgents(workspace agent.WorkspaceSummary) []byte {
	pluginNames := make([]string, 0, len(workspace.Plugins))
	for _, pluginRef := range workspace.Plugins {
		pluginNames = append(pluginNames, pluginRef.Name)
	}
	sort.Strings(pluginNames)

	var b strings.Builder
	b.WriteString("# Workspace AGENTS\n\n")
	b.WriteString(fmt.Sprintf("alias: %s\n", workspace.Alias))
	b.WriteString(fmt.Sprintf("namespace: %s\n", workspace.Namespace))
	b.WriteString("agent_type: claude-code\n\n")
	b.WriteString("attached_plugins:\n")
	if len(pluginNames) == 0 {
		b.WriteString("- (none)\n")
		return []byte(b.String())
	}
	for _, name := range pluginNames {
		b.WriteString("- ")
		b.WriteString(name)
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

func firstNonEmptyLine(value string) string {
	for _, line := range strings.Split(value, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
