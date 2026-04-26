package agent

import (
	"context"
	"errors"
	"io/fs"
	"reflect"
	"strings"
	"testing"

	"github.com/AiRanthem/ANA/pkg/manager/infraops"
	"github.com/AiRanthem/ANA/pkg/manager/plugin"
)

type fakeLayout struct{}

func (fakeLayout) Apply(context.Context, infraops.InfraOps, plugin.Manifest, fs.FS) ([]string, error) {
	return []string{".agent/plugins/example"}, nil
}

type fakeSpec struct {
	agentType   AgentType
	displayName string
	layout      PluginLayout
	desc        ProtocolDescriptor
}

func (s fakeSpec) Type() AgentType { return s.agentType }
func (s fakeSpec) DisplayName() string {
	return s.displayName
}
func (s fakeSpec) Description() string { return "test spec" }
func (s fakeSpec) PluginLayout() PluginLayout {
	return s.layout
}
func (s fakeSpec) Install(context.Context, infraops.InfraOps, InstallParams) error { return nil }
func (s fakeSpec) Uninstall(context.Context, infraops.InfraOps) error              { return nil }
func (s fakeSpec) Probe(context.Context, infraops.InfraOps) (ProbeResult, error) {
	return ProbeResult{Healthy: true}, nil
}
func (s fakeSpec) ProtocolDescriptor() ProtocolDescriptor { return s.desc }

func TestSpecSetRegister_DuplicateType(t *testing.T) {
	t.Parallel()

	set := NewSpecSet()
	spec := fakeSpec{
		agentType:   "claude-code",
		displayName: "Claude Code",
		layout:      fakeLayout{},
		desc:        ProtocolDescriptor{Kind: ProtocolKindCLI},
	}

	if err := set.Register(spec); err != nil {
		t.Fatalf("first register failed: %v", err)
	}

	err := set.Register(spec)
	if !errors.Is(err, ErrAgentTypeConflict) {
		t.Fatalf("expected ErrAgentTypeConflict, got %v", err)
	}
}

func TestSpecSetTypes_Sorted(t *testing.T) {
	t.Parallel()

	set := NewSpecSet()
	cases := []fakeSpec{
		{agentType: "openclaw", displayName: "OpenClaw", layout: fakeLayout{}, desc: ProtocolDescriptor{Kind: ProtocolKindCLI}},
		{agentType: "claude-code", displayName: "Claude Code", layout: fakeLayout{}, desc: ProtocolDescriptor{Kind: ProtocolKindCLI}},
		{agentType: "my-agent", displayName: "My Agent", layout: fakeLayout{}, desc: ProtocolDescriptor{Kind: ProtocolKindREST}},
	}

	for _, c := range cases {
		if err := set.Register(c); err != nil {
			t.Fatalf("register %q failed: %v", c.agentType, err)
		}
	}

	got := set.Types()
	want := []AgentType{"claude-code", "my-agent", "openclaw"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("types mismatch: got=%v want=%v", got, want)
	}
}

func TestValidateAgentType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		agentType AgentType
		wantErr   bool
	}{
		{name: "valid single char", agentType: "a"},
		{name: "valid kebab", agentType: "claude-code"},
		{name: "valid max length", agentType: AgentType(strings.Repeat("a", 32))},
		{name: "empty", agentType: "", wantErr: true},
		{name: "starts with digit", agentType: "1agent", wantErr: true},
		{name: "contains underscore", agentType: "my_agent", wantErr: true},
		{name: "uppercase", agentType: "Agent", wantErr: true},
		{name: "too long", agentType: AgentType(strings.Repeat("a", 33)), wantErr: true},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := ValidateAgentType(tc.agentType)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected nil error, got %v", err)
			}
		})
	}
}

func TestValidateProtocolDescriptor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		desc    ProtocolDescriptor
		wantErr bool
	}{
		{
			name: "valid cli detail",
			desc: ProtocolDescriptor{
				Kind: ProtocolKindCLI,
				Detail: map[string]any{
					"command": []any{"claude", "code"},
				},
			},
		},
		{
			name:    "empty kind",
			desc:    ProtocolDescriptor{},
			wantErr: true,
		},
		{
			name: "unsupported kind",
			desc: ProtocolDescriptor{
				Kind: "grpc",
			},
			wantErr: true,
		},
		{
			name: "unsupported detail value",
			desc: ProtocolDescriptor{
				Kind:   ProtocolKindREST,
				Detail: map[string]any{"not_json": chan int(nil)},
			},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := ValidateProtocolDescriptor(tc.desc)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected nil error, got %v", err)
			}
		})
	}
}
