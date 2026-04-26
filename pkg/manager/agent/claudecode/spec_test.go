package claudecode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"reflect"
	"slices"
	"testing"
	"testing/fstest"

	"github.com/AiRanthem/ANA/pkg/manager/agent"
	"github.com/AiRanthem/ANA/pkg/manager/infraops"
	"github.com/AiRanthem/ANA/pkg/manager/plugin"
)

func TestSpecType_IsClaudeCode(t *testing.T) {
	t.Parallel()

	spec, err := New(Options{})
	if err != nil {
		t.Fatalf("new spec: %v", err)
	}

	if got := spec.Type(); got != "claude-code" {
		t.Fatalf("unexpected type: got=%q", got)
	}
}

func TestInstall_FailsWhenBinaryUnavailable(t *testing.T) {
	t.Parallel()

	spec, err := New(Options{})
	if err != nil {
		t.Fatalf("new spec: %v", err)
	}

	ops := newFakeInfraOps()
	ops.execFn = func(context.Context, infraops.ExecCommand) (infraops.ExecResult, error) {
		return infraops.ExecResult{
			ExitCode: 127,
			Stderr:   []byte("claude: command not found"),
		}, nil
	}

	err = spec.Install(context.Background(), ops, agent.InstallParams{
		Workspace: agent.WorkspaceSummary{
			Alias:     "alpha",
			Namespace: "default",
		},
	})
	if !errors.Is(err, ErrBinaryUnavailable) {
		t.Fatalf("expected ErrBinaryUnavailable, got %v", err)
	}
	if ops.putCalls != 0 {
		t.Fatalf("expected no writes on install failure, got %d", ops.putCalls)
	}
	if len(ops.execCommands) != 1 {
		t.Fatalf("expected one exec call, got %d", len(ops.execCommands))
	}
	cmd := ops.execCommands[0]
	if cmd.Program != "claude" {
		t.Fatalf("unexpected program: %q", cmd.Program)
	}
	if !reflect.DeepEqual(cmd.Args, []string{"--version"}) {
		t.Fatalf("unexpected args: %v", cmd.Args)
	}
}

func TestInstallAndLayout_WriteExpectedFiles(t *testing.T) {
	t.Parallel()

	spec, err := New(Options{})
	if err != nil {
		t.Fatalf("new spec: %v", err)
	}

	ops := newFakeInfraOps()
	ops.execFn = func(context.Context, infraops.ExecCommand) (infraops.ExecResult, error) {
		return infraops.ExecResult{
			ExitCode: 0,
			Stdout:   []byte("claude-code 1.2.3\n"),
		}, nil
	}

	layout := spec.PluginLayout()

	firstPlugin := pluginFS("market--research", map[string]string{
		"AGENTS.md":                 "# plugin agents\n",
		"skills/s1/SKILL.md":        "skill one\n",
		"assets/prompt/example.txt": "asset one\n",
	})
	secondPlugin := pluginFS("ops-tools", map[string]string{
		"README.md":    "# readme\n",
		"rules/r1.mdc": "rule one\n",
	})

	firstManifest := mustManifestFromFS(t, firstPlugin)
	secondManifest := mustManifestFromFS(t, secondPlugin)

	if _, err := layout.Apply(context.Background(), ops, firstManifest, firstPlugin); err != nil {
		t.Fatalf("apply first plugin: %v", err)
	}
	if _, err := layout.Apply(context.Background(), ops, secondManifest, secondPlugin); err != nil {
		t.Fatalf("apply second plugin: %v", err)
	}

	err = spec.Install(context.Background(), ops, agent.InstallParams{
		Workspace: agent.WorkspaceSummary{
			Alias:     "workspace-alpha",
			Namespace: "team-one",
			Plugins: []agent.AttachedPluginRef{
				{Name: "zeta"},
				{Name: "alpha"},
			},
		},
	})
	if err != nil {
		t.Fatalf("install: %v", err)
	}

	wantFiles := map[string]string{
		".claude/plugins/market-research/manifest.toml":             manifestTOML("market--research"),
		".claude/plugins/market-research/AGENTS.md":                 "# plugin agents\n",
		".claude/plugins/market-research/skills/s1/SKILL.md":        "skill one\n",
		".claude/plugins/market-research/assets/prompt/example.txt": "asset one\n",
		".claude/plugins/ops-tools/manifest.toml":                   manifestTOML("ops-tools"),
		".claude/plugins/ops-tools/README.md":                       "# readme\n",
		".claude/plugins/ops-tools/rules/r1.mdc":                    "rule one\n",
	}
	for path, want := range wantFiles {
		got, ok := ops.read(path)
		if !ok {
			t.Fatalf("missing file %q", path)
		}
		if string(got) != want {
			t.Fatalf("content mismatch for %q: got=%q want=%q", path, string(got), want)
		}
	}

	settingsRaw, ok := ops.read(".claude/settings.json")
	if !ok {
		t.Fatalf("missing settings file")
	}
	var settings struct {
		SchemaVersion int `json:"schema_version"`
		Workspace     struct {
			Alias     string `json:"alias"`
			Namespace string `json:"namespace"`
		} `json:"workspace"`
	}
	if err := json.Unmarshal(settingsRaw, &settings); err != nil {
		t.Fatalf("decode settings json: %v", err)
	}
	if settings.SchemaVersion != 1 {
		t.Fatalf("unexpected schema_version: %d", settings.SchemaVersion)
	}
	if settings.Workspace.Alias != "workspace-alpha" || settings.Workspace.Namespace != "team-one" {
		t.Fatalf("unexpected workspace settings: %+v", settings.Workspace)
	}

	agentsDoc, ok := ops.read("AGENTS.md")
	if !ok {
		t.Fatalf("missing top-level AGENTS.md")
	}
	wantAgents := "# Workspace AGENTS\n\n" +
		"alias: workspace-alpha\n" +
		"namespace: team-one\n" +
		"agent_type: claude-code\n\n" +
		"attached_plugins:\n" +
		"- alpha\n" +
		"- zeta\n"
	if string(agentsDoc) != wantAgents {
		t.Fatalf("unexpected AGENTS.md content:\n%s", string(agentsDoc))
	}
}

func TestPluginLayoutApply_RejectsSanitizedNameCollision(t *testing.T) {
	t.Parallel()

	spec, err := New(Options{})
	if err != nil {
		t.Fatalf("new spec: %v", err)
	}

	layout := spec.PluginLayout()
	ops := newFakeInfraOps()

	first := pluginFS("my--plugin", map[string]string{
		"skills/a/SKILL.md": "a",
	})
	second := pluginFS("my-plugin", map[string]string{
		"rules/rule.mdc": "same sanitized name",
	})

	firstManifest := mustManifestFromFS(t, first)
	secondManifest := mustManifestFromFS(t, second)

	if _, err := layout.Apply(context.Background(), ops, firstManifest, first); err != nil {
		t.Fatalf("apply first plugin: %v", err)
	}
	putCountAfterFirst := ops.putCalls

	_, err = layout.Apply(context.Background(), ops, secondManifest, second)
	if !errors.Is(err, ErrInvalidPluginLayout) {
		t.Fatalf("expected ErrInvalidPluginLayout, got %v", err)
	}
	if ops.putCalls != putCountAfterFirst {
		t.Fatalf("expected no writes on collision, got before=%d after=%d", putCountAfterFirst, ops.putCalls)
	}
}

func TestPluginLayoutApply_AcceptsManifest(t *testing.T) {
	t.Parallel()

	spec, err := New(Options{})
	if err != nil {
		t.Fatalf("new spec: %v", err)
	}

	layout := spec.PluginLayout()
	ops := newFakeInfraOps()
	root := pluginFS("layout-test", map[string]string{
		"skills/x/SKILL.md": "x\n",
	})
	manifest := mustManifestFromFS(t, root)

	paths, err := layout.Apply(context.Background(), ops, manifest, root)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(paths) == 0 {
		t.Fatalf("want non-empty placed paths, got %v", paths)
	}
}

// TestClaudeCodeLayoutAppliesMultipleDistinctPlugins mirrors REPAIR_PLAN.md:
// two different manifest names on the same infra both land under .claude/plugins/.
func TestClaudeCodeLayoutAppliesMultipleDistinctPlugins(t *testing.T) {
	t.Parallel()

	spec, err := New(Options{})
	if err != nil {
		t.Fatalf("new spec: %v", err)
	}
	layout := spec.PluginLayout()
	ops := newFakeInfraOps()

	firstRoot := pluginFS("alpha-tools", map[string]string{
		"skills/a/SKILL.md": "alpha skill\n",
	})
	secondRoot := pluginFS("beta-tools", map[string]string{
		"rules/b.mdc": "beta rule\n",
	})
	firstManifest := mustManifestFromFS(t, firstRoot)
	secondManifest := mustManifestFromFS(t, secondRoot)

	if _, err := layout.Apply(context.Background(), ops, firstManifest, firstRoot); err != nil {
		t.Fatalf("apply first plugin: %v", err)
	}
	if _, err := layout.Apply(context.Background(), ops, secondManifest, secondRoot); err != nil {
		t.Fatalf("apply second plugin: %v", err)
	}

	cases := []struct {
		path string
		want string
	}{
		{".claude/plugins/alpha-tools/manifest.toml", manifestTOML("alpha-tools")},
		{".claude/plugins/alpha-tools/skills/a/SKILL.md", "alpha skill\n"},
		{".claude/plugins/beta-tools/manifest.toml", manifestTOML("beta-tools")},
		{".claude/plugins/beta-tools/rules/b.mdc", "beta rule\n"},
	}
	for _, tc := range cases {
		got, ok := ops.read(tc.path)
		if !ok {
			t.Fatalf("missing path %q", tc.path)
		}
		if string(got) != tc.want {
			t.Fatalf("content mismatch %q: got=%q want=%q", tc.path, string(got), tc.want)
		}
	}
}

// TestClaudeCodeLayoutDuplicatePluginCollision mirrors REPAIR_PLAN.md:
// applying the same plugin layout twice to the same infra must fail before overwriting.
func TestClaudeCodeLayoutDuplicatePluginCollision(t *testing.T) {
	t.Parallel()

	spec, err := New(Options{})
	if err != nil {
		t.Fatalf("new spec: %v", err)
	}
	layout := spec.PluginLayout()
	ops := newFakeInfraOps()

	root := pluginFS("duplicate-me", map[string]string{
		"skills/x/SKILL.md": "first body\n",
	})
	manifest := mustManifestFromFS(t, root)

	if _, err := layout.Apply(context.Background(), ops, manifest, root); err != nil {
		t.Fatalf("apply first: %v", err)
	}
	putAfterFirst := ops.putCalls

	_, err = layout.Apply(context.Background(), ops, manifest, root)
	if !errors.Is(err, agent.ErrInvalidPluginLayout) {
		t.Fatalf("second apply: got %v, want agent.ErrInvalidPluginLayout", err)
	}
	if ops.putCalls != putAfterFirst {
		t.Fatalf("putCalls after collision = %d, want %d (no writes)", ops.putCalls, putAfterFirst)
	}
}

func TestProtocolDescriptor_StableAcrossCalls(t *testing.T) {
	t.Parallel()

	spec, err := New(Options{})
	if err != nil {
		t.Fatalf("new spec: %v", err)
	}

	first := spec.ProtocolDescriptor()
	second := spec.ProtocolDescriptor()
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("descriptor mismatch between calls: first=%v second=%v", first, second)
	}

	first.Detail["resume_flag"] = "--changed"
	command, ok := first.Detail["command"].([]any)
	if !ok {
		t.Fatalf("command field type mismatch: %T", first.Detail["command"])
	}
	command[0] = "changed"
	first.Detail["command"] = command

	third := spec.ProtocolDescriptor()
	if !reflect.DeepEqual(third, second) {
		t.Fatalf("descriptor should be stable despite caller mutation: third=%v second=%v", third, second)
	}

	want := agent.ProtocolDescriptor{
		Kind: agent.ProtocolKindCLI,
		Detail: map[string]any{
			"command":             []any{"claude", "code"},
			"resume_flag":         "--resume",
			"cwd_relative_to_dir": "",
			"stdin_input":         true,
		},
	}
	if !reflect.DeepEqual(third, want) {
		t.Fatalf("descriptor mismatch: got=%v want=%v", third, want)
	}
}

func TestProbe_ExtractsVersion(t *testing.T) {
	t.Parallel()

	spec, err := New(Options{})
	if err != nil {
		t.Fatalf("new spec: %v", err)
	}

	ops := newFakeInfraOps()
	ops.execFn = func(context.Context, infraops.ExecCommand) (infraops.ExecResult, error) {
		return infraops.ExecResult{
			ExitCode: 0,
			Stdout:   []byte("claude-code 9.9.9\n"),
		}, nil
	}

	result, err := spec.Probe(context.Background(), ops)
	if err != nil {
		t.Fatalf("probe returned error: %v", err)
	}
	if !result.Healthy {
		t.Fatalf("expected healthy probe result, got %+v", result)
	}
	if got := result.Detail["version"]; got != "claude-code 9.9.9" {
		t.Fatalf("unexpected version detail: %q", got)
	}
	if result.Error != nil {
		t.Fatalf("expected nil probe error payload, got %+v", result.Error)
	}
	if len(ops.execCommands) != 1 {
		t.Fatalf("expected one exec call, got %d", len(ops.execCommands))
	}
	if !reflect.DeepEqual(ops.execCommands[0].Args, []string{"--version"}) {
		t.Fatalf("unexpected probe args: %v", ops.execCommands[0].Args)
	}
}

func TestLayoutPaths_Deterministic(t *testing.T) {
	t.Parallel()

	manifest := plugin.Manifest{
		Plugin: plugin.ManifestPlugin{
			Name: "agent--tools",
		},
		Skills: map[string]plugin.ManifestEntry{
			"s2": {Path: "skills/s2"},
			"s1": {Path: "skills/s1"},
		},
		Rules: map[string]plugin.ManifestEntry{
			"style": {Path: "rules/style.mdc"},
		},
	}

	got := LayoutPaths(manifest)
	want := []string{
		".claude/plugins/agent-tools/manifest.toml",
		".claude/plugins/agent-tools/rules/style.mdc",
		".claude/plugins/agent-tools/skills/s1",
		".claude/plugins/agent-tools/skills/s2",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("layout paths mismatch:\n got: %v\nwant: %v", got, want)
	}
}

type fakeInfraOps struct {
	dir string

	execFn       func(ctx context.Context, cmd infraops.ExecCommand) (infraops.ExecResult, error)
	execCommands []infraops.ExecCommand

	files    map[string][]byte
	putCalls int
}

func newFakeInfraOps() *fakeInfraOps {
	return &fakeInfraOps{
		dir:   "/tmp/fake",
		files: make(map[string][]byte),
	}
}

func (f *fakeInfraOps) Type() infraops.InfraType { return "fake" }
func (f *fakeInfraOps) Dir() string              { return f.dir }
func (f *fakeInfraOps) Init(context.Context) error {
	return nil
}

func (f *fakeInfraOps) Open(context.Context) error {
	return nil
}

func (f *fakeInfraOps) Exec(ctx context.Context, cmd infraops.ExecCommand) (infraops.ExecResult, error) {
	f.execCommands = append(f.execCommands, cloneExecCommand(cmd))
	if f.execFn == nil {
		return infraops.ExecResult{}, nil
	}
	return f.execFn(ctx, cmd)
}

func (f *fakeInfraOps) PutFile(ctx context.Context, path string, content io.Reader, mode fs.FileMode) error {
	_ = ctx
	_ = mode

	data, err := io.ReadAll(content)
	if err != nil {
		return err
	}
	f.files[path] = slices.Clone(data)
	f.putCalls++
	return nil
}

func (f *fakeInfraOps) GetFile(ctx context.Context, path string) (io.ReadCloser, error) {
	_ = ctx

	data, ok := f.files[path]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(slices.Clone(data))), nil
}

func (f *fakeInfraOps) Request(context.Context, int, *http.Request) (*http.Response, error) {
	return nil, infraops.ErrUnsupportedRequest
}

func (f *fakeInfraOps) Clear(context.Context) error { return nil }

func (f *fakeInfraOps) read(path string) ([]byte, bool) {
	data, ok := f.files[path]
	return slices.Clone(data), ok
}

func cloneExecCommand(cmd infraops.ExecCommand) infraops.ExecCommand {
	cloned := cmd
	cloned.Args = slices.Clone(cmd.Args)
	cloned.Env = slices.Clone(cmd.Env)
	return cloned
}

func mustManifestFromFS(t *testing.T, root fs.FS) plugin.Manifest {
	t.Helper()
	data, err := fs.ReadFile(root, manifestFile)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	m, err := plugin.ParseManifest(data)
	if err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	return m
}

func pluginFS(name string, files map[string]string) fstest.MapFS {
	root := fstest.MapFS{
		manifestFile: {
			Data: []byte(manifestTOML(name)),
			Mode: 0o644,
		},
	}
	for p, content := range files {
		root[p] = &fstest.MapFile{
			Data: []byte(content),
			Mode: 0o644,
		}
	}
	return root
}

func manifestTOML(name string) string {
	return fmt.Sprintf("schema_version = 1\n\n[plugin]\nname = %q\n", name)
}
