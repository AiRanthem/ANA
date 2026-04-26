package agent

import (
	"context"
	"errors"
	"io/fs"
	"math"
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
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if !errors.Is(err, ErrInvalidAgentType) {
					t.Fatalf("expected ErrInvalidAgentType, got %v", err)
				}
				return
			}
			if err != nil {
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
			name: "valid go typed collections",
			desc: ProtocolDescriptor{
				Kind: ProtocolKindCLI,
				Detail: map[string]any{
					"command": []string{"claude", "code"},
					"env": map[string]string{
						"A": "1",
					},
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

func TestValidateProtocolDescriptor_RejectsNonFiniteFloats(t *testing.T) {
	t.Parallel()

	cases := []ProtocolDescriptor{
		{Kind: ProtocolKindCLI, Detail: map[string]any{"x": math.NaN()}},
		{Kind: ProtocolKindCLI, Detail: map[string]any{"x": math.Inf(1)}},
		{Kind: ProtocolKindCLI, Detail: map[string]any{"x": math.Inf(-1)}},
		{Kind: ProtocolKindCLI, Detail: map[string]any{"x": []any{1, math.Inf(-1)}}},
		{Kind: ProtocolKindCLI, Detail: map[string]any{"x": map[string]any{"y": float32(math.NaN())}}},
	}

	for _, c := range cases {
		err := ValidateProtocolDescriptor(c)
		if err == nil {
			t.Fatalf("expected error for %v", c.Detail)
		}
		if !errors.Is(err, ErrInvalidProtocolDescriptor) {
			t.Fatalf("want ErrInvalidProtocolDescriptor, got %v", err)
		}
	}
}

func TestValidateProtocolDescriptor_AcceptsFiniteFloats(t *testing.T) {
	t.Parallel()

	desc := ProtocolDescriptor{
		Kind: ProtocolKindCLI,
		Detail: map[string]any{
			"x": 1.5,
			"y": float32(2.25),
			"z": map[string]any{"w": []any{0.0, -3.14}},
		},
	}
	if err := ValidateProtocolDescriptor(desc); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestSpecSetRegister_ErrorClassification(t *testing.T) {
	t.Parallel()

	s := NewSpecSet()
	if err := s.Register(nil); !errors.Is(err, ErrInvalidAgentSpec) {
		t.Fatalf("Register(nil): want ErrInvalidAgentSpec, got %v", err)
	}

	errBadType := s.Register(fakeSpec{
		agentType:   "Bad_Type",
		displayName: "x",
		layout:      fakeLayout{},
		desc:        ProtocolDescriptor{Kind: ProtocolKindCLI},
	})
	if !errors.Is(errBadType, ErrInvalidAgentType) {
		t.Fatalf("Register invalid type: want ErrInvalidAgentType, got %v", errBadType)
	}

	errEmptyName := s.Register(fakeSpec{
		agentType:   "claude-code",
		displayName: "",
		layout:      fakeLayout{},
		desc:        ProtocolDescriptor{Kind: ProtocolKindCLI},
	})
	if !errors.Is(errEmptyName, ErrInvalidAgentSpec) {
		t.Fatalf("empty display name: want ErrInvalidAgentSpec, got %v", errEmptyName)
	}
}

func TestSpecSetLookup_ReturnsUnknown(t *testing.T) {
	t.Parallel()

	s := NewSpecSet()
	_, err := s.Lookup("missing-type")
	if !errors.Is(err, ErrAgentTypeUnknown) {
		t.Fatalf("want ErrAgentTypeUnknown, got %v", err)
	}
}
