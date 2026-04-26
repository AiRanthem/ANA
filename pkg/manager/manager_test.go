package manager

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AiRanthem/ANA/pkg/manager/agent"
	"github.com/AiRanthem/ANA/pkg/manager/infraops"
	"github.com/AiRanthem/ANA/pkg/manager/plugin"
	"github.com/AiRanthem/ANA/pkg/manager/workspace"
)

func TestBuilderBuildRejectsDuplicateRegistrationsAndReuse(t *testing.T) {
	t.Parallel()

	builder := NewBuilder()
	builder.PluginRepository = plugin.NewMemoryRepository()
	builder.PluginStorage = plugin.NewMemoryStorage()
	builder.WorkspaceRepository = workspace.NewMemoryRepository()
	builder.IDGenerator = fixedIDGenerator{
		nextPluginID:    PluginID("plg_test"),
		nextWorkspaceID: WorkspaceID("wsp_test"),
	}
	builder.RegisterAgentSpec(&fakeAgentSpec{})
	builder.RegisterAgentSpec(&fakeAgentSpec{})

	if _, err := builder.Build(); !errors.Is(err, agent.ErrAgentTypeConflict) {
		t.Fatalf("Build() duplicate agent error = %v, want ErrAgentTypeConflict", err)
	}

	builder = NewBuilder()
	builder.PluginRepository = plugin.NewMemoryRepository()
	builder.PluginStorage = plugin.NewMemoryStorage()
	builder.WorkspaceRepository = workspace.NewMemoryRepository()
	builder.IDGenerator = fixedIDGenerator{
		nextPluginID:    PluginID("plg_test"),
		nextWorkspaceID: WorkspaceID("wsp_test"),
	}
	builder.RegisterAgentSpec(&fakeAgentSpec{})
	builder.RegisterInfraType(InfraType("localdir"), newFakeInfraRegistry().factory())

	managerInstance, err := builder.Build()
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	t.Cleanup(func() {
		if err := managerInstance.Stop(context.Background()); err != nil && !errors.Is(err, ErrShutdown) {
			t.Fatalf("Stop() error = %v", err)
		}
	})

	if _, err := builder.Build(); err == nil {
		t.Fatalf("second Build() error = nil, want non-nil")
	}
}

func TestManagerCreatePluginUpsertsByNamespaceAndName(t *testing.T) {
	t.Parallel()

	managerInstance := newTestManager(t, testManagerOptions{})

	firstBody := buildPluginZip(t, "demo-plugin", "first")
	first, err := managerInstance.CreatePlugin(context.Background(), CreatePluginRequest{
		Name:        "demo-plugin",
		Description: "first description",
		Content:     bytes.NewReader(firstBody),
	})
	if err != nil {
		t.Fatalf("CreatePlugin(first) error = %v", err)
	}

	if first.Namespace != Namespace("default") {
		t.Fatalf("Namespace = %q, want %q", first.Namespace, Namespace("default"))
	}
	if first.ID == "" {
		t.Fatalf("ID = empty, want non-empty")
	}
	if first.Description != "first description" {
		t.Fatalf("Description = %q, want %q", first.Description, "first description")
	}

	secondBody := buildPluginZip(t, "demo-plugin", "second")
	second, err := managerInstance.CreatePlugin(context.Background(), CreatePluginRequest{
		Name:        "demo-plugin",
		Description: "second description",
		Content:     bytes.NewReader(secondBody),
	})
	if err != nil {
		t.Fatalf("CreatePlugin(second) error = %v", err)
	}

	if second.ID != first.ID {
		t.Fatalf("ID after overwrite = %q, want %q", second.ID, first.ID)
	}
	if second.ContentHash == first.ContentHash {
		t.Fatalf("ContentHash = %q, want changed hash after overwrite", second.ContentHash)
	}
	if second.Description != "second description" {
		t.Fatalf("Description = %q, want %q", second.Description, "second description")
	}

	rows, next, err := managerInstance.ListPlugins(context.Background(), ListPluginsOptions{
		Namespace: Namespace("default"),
	})
	if err != nil {
		t.Fatalf("ListPlugins() error = %v", err)
	}
	if next != "" {
		t.Fatalf("next cursor = %q, want empty", next)
	}
	if len(rows) != 1 {
		t.Fatalf("ListPlugins() rows = %d, want 1", len(rows))
	}
	if rows[0].ID != first.ID {
		t.Fatalf("listed plugin ID = %q, want %q", rows[0].ID, first.ID)
	}
}

type alwaysFailPluginUpdateRepo struct {
	*plugin.MemoryRepository
}

func (alwaysFailPluginUpdateRepo) Update(context.Context, plugin.Plugin) error {
	return errors.New("forced plugin repository update failure")
}

func TestManagerCreatePluginOverwriteRollbackOnRepositoryUpdateFailure(t *testing.T) {
	t.Parallel()

	managerInstance := newTestManager(t, testManagerOptions{
		pluginRepo: &alwaysFailPluginUpdateRepo{MemoryRepository: plugin.NewMemoryRepository()},
	})

	firstBody := buildPluginZip(t, "demo-plugin", "first-body")
	first, err := managerInstance.CreatePlugin(context.Background(), CreatePluginRequest{
		Name:    "demo-plugin",
		Content: bytes.NewReader(firstBody),
	})
	if err != nil {
		t.Fatalf("CreatePlugin(first) error = %v", err)
	}

	secondBody := buildPluginZip(t, "demo-plugin", "second-body")
	_, err = managerInstance.CreatePlugin(context.Background(), CreatePluginRequest{
		Name:    "demo-plugin",
		Content: bytes.NewReader(secondBody),
	})
	if err == nil {
		t.Fatal("CreatePlugin(second) error = nil, want non-nil")
	}

	got, err := managerInstance.GetPlugin(context.Background(), first.ID)
	if err != nil {
		t.Fatalf("GetPlugin() error = %v", err)
	}
	if got.ContentHash != first.ContentHash {
		t.Fatalf("ContentHash after failed overwrite = %q, want %q (rollback)", got.ContentHash, first.ContentHash)
	}
}

func TestManagerCreateWorkspaceRejectsDuplicatePluginRefs(t *testing.T) {
	t.Parallel()

	managerInstance := newTestManager(t, testManagerOptions{})
	plug, err := managerInstance.CreatePlugin(context.Background(), CreatePluginRequest{
		Name:    "demo-plugin",
		Content: bytes.NewReader(buildPluginZip(t, "demo-plugin", "body")),
	})
	if err != nil {
		t.Fatalf("CreatePlugin() error = %v", err)
	}

	dir := filepath.Join(t.TempDir(), "wsp_duplicate")
	_, err = managerInstance.CreateWorkspace(context.Background(), CreateWorkspaceRequest{
		Alias:        Alias("Alice"),
		AgentType:    AgentType("claude-code"),
		InfraType:    InfraType("localdir"),
		InfraOptions: infraops.Options{"dir": dir},
		Plugins:      []PluginRef{{ID: plug.ID}, {ID: plug.ID}},
	})
	if !errors.Is(err, ErrDuplicatePluginRef) {
		t.Fatalf("CreateWorkspace() error = %v, want ErrDuplicatePluginRef", err)
	}
}

func TestNewInfraOps_RejectsEmptyDir(t *testing.T) {
	t.Parallel()

	managerInstance := newTestManager(t, testManagerOptions{})
	_, err := managerInstance.NewInfraOps(context.Background(), InfraType("localdir"), infraops.Options{})
	if err == nil {
		t.Fatal("NewInfraOps() error = nil, want non-nil")
	}
	if !errors.Is(err, infraops.ErrInvalidOption) {
		t.Fatalf("NewInfraOps() error = %v, want errors.Is(..., ErrInvalidOption)", err)
	}
}

func TestManagerCreateWorkspaceWiresRepositoriesAndLifecycle(t *testing.T) {
	t.Parallel()

	spec := &fakeAgentSpec{}
	managerInstance := newTestManager(t, testManagerOptions{
		spec:          spec,
		probeInterval: 10 * time.Millisecond,
	})

	pluginBody := buildPluginZip(t, "demo-plugin", "workspace")
	plug, err := managerInstance.CreatePlugin(context.Background(), CreatePluginRequest{
		Name:        "demo-plugin",
		Description: "workspace plugin",
		Content:     bytes.NewReader(pluginBody),
	})
	if err != nil {
		t.Fatalf("CreatePlugin() error = %v", err)
	}

	if err := managerInstance.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	row, err := managerInstance.CreateWorkspace(context.Background(), CreateWorkspaceRequest{
		Alias:        Alias("Alice"),
		AgentType:    AgentType("claude-code"),
		InfraType:    InfraType("localdir"),
		InfraOptions: infraops.Options{"dir": "/tmp/wsp_alice"},
		Plugins: []PluginRef{
			{ID: plug.ID},
		},
		InstallParams: map[string]any{
			"token": "secret",
		},
		Description: "Alice workspace",
		Labels: map[string]string{
			"env": "test",
		},
	})
	if err != nil {
		t.Fatalf("CreateWorkspace() error = %v", err)
	}

	if row.Status != StatusInit {
		t.Fatalf("initial Status = %q, want %q", row.Status, StatusInit)
	}

	healthy := waitForWorkspaceStatus(t, managerInstance, row.ID, StatusHealthy, time.Second)
	if len(healthy.Plugins) != 1 {
		t.Fatalf("Plugins length = %d, want 1", len(healthy.Plugins))
	}
	if len(healthy.Plugins[0].PlacedPaths) == 0 {
		t.Fatalf("PlacedPaths = %v, want non-empty", healthy.Plugins[0].PlacedPaths)
	}
	if healthy.Plugins[0].PluginID != plug.ID {
		t.Fatalf("PluginID = %q, want %q", healthy.Plugins[0].PluginID, plug.ID)
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if spec.probeCalls.Load() >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := spec.probeCalls.Load(); got < 2 {
		t.Fatalf("Probe() calls = %d, want scheduler to run after Start()", got)
	}

	if err := managerInstance.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}

	if _, err := managerInstance.GetWorkspace(context.Background(), row.ID); !errors.Is(err, ErrShutdown) {
		t.Fatalf("GetWorkspace() after Stop error = %v, want ErrShutdown", err)
	}
}

type testManagerOptions struct {
	spec          *fakeAgentSpec
	probeInterval time.Duration
	pluginRepo    plugin.Repository
}

func newTestManager(t *testing.T, opts testManagerOptions) Manager {
	t.Helper()

	if opts.spec == nil {
		opts.spec = &fakeAgentSpec{}
	}
	if opts.probeInterval <= 0 {
		opts.probeInterval = time.Hour
	}

	builder := NewBuilder()
	if opts.pluginRepo != nil {
		builder.PluginRepository = opts.pluginRepo
	} else {
		builder.PluginRepository = plugin.NewMemoryRepository()
	}
	builder.PluginStorage = plugin.NewMemoryStorage()
	builder.WorkspaceRepository = workspace.NewMemoryRepository()
	builder.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	builder.IDGenerator = fixedIDGenerator{
		nextPluginID:    PluginID("plg_fixed"),
		nextWorkspaceID: WorkspaceID("wsp_fixed"),
	}
	builder.InstallWorkers = 1
	builder.ProbeInterval = opts.probeInterval
	builder.ProbeWorkers = 1
	builder.ProbeTimeout = time.Second
	builder.RegisterAgentSpec(opts.spec)
	builder.RegisterInfraType(InfraType("localdir"), newFakeInfraRegistry().factory())

	managerInstance, err := builder.Build()
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	t.Cleanup(func() {
		if err := managerInstance.Stop(context.Background()); err != nil && !errors.Is(err, ErrShutdown) {
			t.Fatalf("Stop() error = %v", err)
		}
	})
	return managerInstance
}

type fixedIDGenerator struct {
	nextPluginID    PluginID
	nextWorkspaceID WorkspaceID
}

func (g fixedIDGenerator) PluginID() PluginID       { return g.nextPluginID }
func (g fixedIDGenerator) WorkspaceID() WorkspaceID { return g.nextWorkspaceID }

func waitForWorkspaceStatus(t *testing.T, managerInstance Manager, id WorkspaceID, want WorkspaceStatus, timeout time.Duration) Workspace {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		row, err := managerInstance.GetWorkspace(context.Background(), id)
		if err == nil && row.Status == want {
			return row
		}
		time.Sleep(10 * time.Millisecond)
	}

	row, err := managerInstance.GetWorkspace(context.Background(), id)
	if err != nil {
		t.Fatalf("GetWorkspace() error = %v", err)
	}
	t.Fatalf("workspace Status = %q, want %q", row.Status, want)
	return Workspace{}
}

type fakeAgentSpec struct {
	installCalls   atomic.Int64
	uninstallCalls atomic.Int64
	probeCalls     atomic.Int64
}

func (s *fakeAgentSpec) Type() agent.AgentType { return agent.AgentType("claude-code") }
func (s *fakeAgentSpec) DisplayName() string   { return "Claude Code" }
func (s *fakeAgentSpec) Description() string   { return "fake" }
func (s *fakeAgentSpec) PluginLayout() agent.PluginLayout {
	return fakePluginLayout{}
}
func (s *fakeAgentSpec) Install(context.Context, infraops.InfraOps, agent.InstallParams) error {
	s.installCalls.Add(1)
	return nil
}
func (s *fakeAgentSpec) Uninstall(context.Context, infraops.InfraOps) error {
	s.uninstallCalls.Add(1)
	return nil
}
func (s *fakeAgentSpec) Probe(context.Context, infraops.InfraOps) (agent.ProbeResult, error) {
	s.probeCalls.Add(1)
	return agent.ProbeResult{Healthy: true}, nil
}
func (s *fakeAgentSpec) ProtocolDescriptor() agent.ProtocolDescriptor {
	return agent.ProtocolDescriptor{
		Kind: agent.ProtocolKindCLI,
		Detail: map[string]any{
			"command": []any{"ana"},
		},
	}
}

type fakePluginLayout struct{}

func (fakePluginLayout) Apply(ctx context.Context, ops infraops.InfraOps, _ plugin.Manifest, pluginRoot fs.FS) ([]string, error) {
	data, err := fs.ReadFile(pluginRoot, "manifest.toml")
	if err != nil {
		return nil, err
	}
	if err := ops.PutFile(ctx, path.Join("plugins", "manifest.toml"), bytes.NewReader(data), 0o644); err != nil {
		return nil, err
	}
	return []string{"plugins/manifest.toml"}, nil
}

type fakeInfraRegistry struct {
	mu     sync.Mutex
	states map[string]*fakeInfraState
}

func newFakeInfraRegistry() *fakeInfraRegistry {
	return &fakeInfraRegistry{
		states: make(map[string]*fakeInfraState),
	}
}

func (r *fakeInfraRegistry) factory() infraops.Factory {
	return func(_ context.Context, opts infraops.Options) (infraops.InfraOps, error) {
		dir, _ := opts["dir"].(string)
		if dir == "" {
			return nil, infraops.ErrInvalidOption
		}
		return &fakeInfra{state: r.stateFor(dir)}, nil
	}
}

func (r *fakeInfraRegistry) stateFor(dir string) *fakeInfraState {
	r.mu.Lock()
	defer r.mu.Unlock()

	if state, ok := r.states[dir]; ok {
		return state
	}
	state := &fakeInfraState{
		dir:   dir,
		files: make(map[string][]byte),
	}
	r.states[dir] = state
	return state
}

type fakeInfraState struct {
	dir string

	mu    sync.Mutex
	files map[string][]byte
}

type fakeInfra struct {
	state *fakeInfraState
}

func (i *fakeInfra) Type() infraops.InfraType { return infraops.InfraType("localdir") }
func (i *fakeInfra) Dir() string              { return i.state.dir }
func (i *fakeInfra) Init(context.Context) error {
	return nil
}
func (i *fakeInfra) Open(context.Context) error {
	return nil
}
func (i *fakeInfra) Exec(context.Context, infraops.ExecCommand) (infraops.ExecResult, error) {
	return infraops.ExecResult{}, nil
}
func (i *fakeInfra) PutFile(_ context.Context, filePath string, content io.Reader, _ fs.FileMode) error {
	body, err := io.ReadAll(content)
	if err != nil {
		return err
	}
	i.state.mu.Lock()
	defer i.state.mu.Unlock()
	i.state.files[filePath] = append([]byte(nil), body...)
	return nil
}
func (i *fakeInfra) GetFile(_ context.Context, filePath string) (io.ReadCloser, error) {
	i.state.mu.Lock()
	defer i.state.mu.Unlock()

	body, ok := i.state.files[filePath]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(append([]byte(nil), body...))), nil
}
func (i *fakeInfra) Request(context.Context, int, *http.Request) (*http.Response, error) {
	return nil, infraops.ErrUnsupportedRequest
}
func (i *fakeInfra) Clear(context.Context) error {
	return nil
}

func buildPluginZip(t *testing.T, name, skillBody string) []byte {
	t.Helper()

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	manifest := strings.TrimSpace(`
schema_version = 1

[plugin]
name = "`+name+`"
description = "demo"

[skills.echo]
display_name = "Echo"
path = "skills/echo"
`) + "\n"

	addZipFile(t, zw, "manifest.toml", []byte(manifest))
	addZipFile(t, zw, "skills/echo/SKILL.md", []byte(skillBody))

	if err := zw.Close(); err != nil {
		t.Fatalf("zip.Close() error = %v", err)
	}
	return buf.Bytes()
}

func addZipFile(t *testing.T, zw *zip.Writer, name string, body []byte) {
	t.Helper()

	w, err := zw.Create(name)
	if err != nil {
		t.Fatalf("zip.Create(%q) error = %v", name, err)
	}
	if _, err := w.Write(body); err != nil {
		t.Fatalf("zip.Write(%q) error = %v", name, err)
	}
}
