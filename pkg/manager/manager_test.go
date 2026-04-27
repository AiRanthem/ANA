package manager

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
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

	if _, err := builder.Build(context.Background()); !errors.Is(err, agent.ErrAgentTypeConflict) {
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

	managerInstance, err := builder.Build(context.Background())
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	t.Cleanup(func() {
		if err := managerInstance.Stop(context.Background()); err != nil && !errors.Is(err, ErrShutdown) {
			t.Fatalf("Stop() error = %v", err)
		}
	})

	if _, err := builder.Build(context.Background()); err == nil {
		t.Fatalf("second Build() error = nil, want non-nil")
	}
}

func TestManagerStop_SecondCallSucceedsIdempotently(t *testing.T) {
	t.Parallel()

	builder := NewBuilder()
	builder.PluginRepository = plugin.NewMemoryRepository()
	builder.PluginStorage = plugin.NewMemoryStorage()
	builder.WorkspaceRepository = workspace.NewMemoryRepository()
	builder.IDGenerator = fixedIDGenerator{
		nextPluginID:    PluginID("plg_test"),
		nextWorkspaceID: WorkspaceID("wsp_test"),
	}
	builder.InstallWorkers = 1
	builder.ProbeInterval = time.Hour
	builder.ProbeWorkers = 1
	builder.ProbeTimeout = time.Second
	builder.RegisterAgentSpec(&fakeAgentSpec{})
	builder.RegisterInfraType(InfraType("localdir"), newFakeInfraRegistry().factory())

	managerInstance, err := builder.Build(context.Background())
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	if err := managerInstance.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if err := managerInstance.Stop(context.Background()); err != nil {
		t.Fatalf("second Stop() error = %v, want nil", err)
	}
}

func TestManagerBuild_DoesNotStartWorkersBeforeStart(t *testing.T) {
	t.Parallel()

	spec := &fakeAgentSpec{}
	_ = newTestManager(t, testManagerOptions{
		spec:          spec,
		probeInterval: 10 * time.Millisecond,
	})

	time.Sleep(150 * time.Millisecond)
	if got := spec.installCalls.Load(); got != 0 {
		t.Fatalf("Install() calls after Build without Start = %d, want 0 (no background workers)", got)
	}
}

func TestManagerStart_StartsWorkersAndScheduler(t *testing.T) {
	t.Parallel()

	spec := &fakeAgentSpec{}
	managerInstance := newTestManager(t, testManagerOptions{
		spec:          spec,
		probeInterval: 10 * time.Millisecond,
	})

	pluginBody := buildPluginZip(t, "demo-plugin", "ws")
	plug, err := managerInstance.CreatePlugin(context.Background(), CreatePluginRequest{
		Name:    "demo-plugin",
		Content: bytes.NewReader(pluginBody),
	})
	if err != nil {
		t.Fatalf("CreatePlugin() error = %v", err)
	}

	if err := managerInstance.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	row, err := managerInstance.CreateWorkspace(context.Background(), CreateWorkspaceRequest{
		Alias:        Alias("StartProbe"),
		AgentType:    AgentType("claude-code"),
		InfraType:    InfraType("localdir"),
		InfraOptions: infraops.Options{"dir": t.TempDir()},
		Plugins:      []PluginRef{{ID: plug.ID}},
	})
	if err != nil {
		t.Fatalf("CreateWorkspace() error = %v", err)
	}
	if row.Status != StatusInit {
		t.Fatalf("initial Status = %q, want %q", row.Status, StatusInit)
	}

	_ = waitForWorkspaceStatus(t, managerInstance, row.ID, StatusHealthy, time.Second)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if spec.probeCalls.Load() >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if spec.probeCalls.Load() < 1 {
		t.Fatalf("Probe() calls = %d, want scheduler running after Start()", spec.probeCalls.Load())
	}
}

func TestManagerCreateWorkspace_SubmitFailureDeletesRowWithoutStatusWrite(t *testing.T) {
	t.Parallel()

	managerInstance := newTestManager(t, testManagerOptions{})
	pluginBody := buildPluginZip(t, "demo-plugin", "body")
	plug, err := managerInstance.CreatePlugin(context.Background(), CreatePluginRequest{
		Name:    "demo-plugin",
		Content: bytes.NewReader(pluginBody),
	})
	if err != nil {
		t.Fatalf("CreatePlugin() error = %v", err)
	}

	// Controller is not running until Start; Submit fails and the row must be removed.
	_, err = managerInstance.CreateWorkspace(context.Background(), CreateWorkspaceRequest{
		Alias:        Alias("NoStart"),
		AgentType:    AgentType("claude-code"),
		InfraType:    InfraType("localdir"),
		InfraOptions: infraops.Options{"dir": t.TempDir()},
		Plugins:      []PluginRef{{ID: plug.ID}},
	})
	if err == nil {
		t.Fatal("CreateWorkspace() error = nil, want non-nil")
	}
	if !errors.Is(err, workspace.ErrControllerShutdown) {
		t.Fatalf("CreateWorkspace() error = %v, want errors.Is(..., workspace.ErrControllerShutdown)", err)
	}

	_, err = managerInstance.GetWorkspace(context.Background(), WorkspaceID("wsp_fixed"))
	if !errors.Is(err, ErrWorkspaceNotFound) {
		t.Fatalf("GetWorkspace() error = %v, want ErrWorkspaceNotFound", err)
	}
}

// errForcedWorkspaceDeleteCompensation is returned by failWorkspaceDeleteRepo.Delete for tests.
var errForcedWorkspaceDeleteCompensation = errors.New("forced workspace delete failure")

type failWorkspaceDeleteRepo struct {
	*workspace.MemoryRepository
}

func (*failWorkspaceDeleteRepo) Delete(context.Context, workspace.WorkspaceID) error {
	return errForcedWorkspaceDeleteCompensation
}

func TestManagerCreateWorkspace_SubmitFailure_CompensatingDeleteFails_JoinsErrors(t *testing.T) {
	t.Parallel()

	managerInstance := newTestManager(t, testManagerOptions{
		workspaceRepo: &failWorkspaceDeleteRepo{MemoryRepository: workspace.NewMemoryRepository()},
	})
	pluginBody := buildPluginZip(t, "demo-plugin", "body")
	plug, err := managerInstance.CreatePlugin(context.Background(), CreatePluginRequest{
		Name:    "demo-plugin",
		Content: bytes.NewReader(pluginBody),
	})
	if err != nil {
		t.Fatalf("CreatePlugin() error = %v", err)
	}

	_, err = managerInstance.CreateWorkspace(context.Background(), CreateWorkspaceRequest{
		Alias:        Alias("JoinErr"),
		AgentType:    AgentType("claude-code"),
		InfraType:    InfraType("localdir"),
		InfraOptions: infraops.Options{"dir": t.TempDir()},
		Plugins:      []PluginRef{{ID: plug.ID}},
	})
	if err == nil {
		t.Fatal("CreateWorkspace() error = nil, want non-nil")
	}
	if !errors.Is(err, workspace.ErrControllerShutdown) {
		t.Fatalf("CreateWorkspace() error = %v, want ErrControllerShutdown", err)
	}
	if !errors.Is(err, errForcedWorkspaceDeleteCompensation) {
		t.Fatalf("CreateWorkspace() error should wrap compensating delete failure; got %v", err)
	}

	row, err := managerInstance.GetWorkspace(context.Background(), WorkspaceID("wsp_fixed"))
	if err != nil {
		t.Fatalf("GetWorkspace() error = %v, want row retained when compensation delete fails", err)
	}
	if row.Alias != Alias("JoinErr") {
		t.Fatalf("GetWorkspace() Alias = %q, want JoinErr", row.Alias)
	}
}

func TestManagerDeletePlugin_StorageDeleteFailureStillDeletesRepository(t *testing.T) {
	t.Parallel()

	inner := plugin.NewMemoryStorage()
	managerInstance := newTestManager(t, testManagerOptions{
		pluginStorage: errOnPluginStorageDelete{MemoryStorage: inner},
	})

	body := buildPluginZip(t, "hold-repo", "blob")
	created, err := managerInstance.CreatePlugin(context.Background(), CreatePluginRequest{
		Name:    "hold-repo",
		Content: bytes.NewReader(body),
	})
	if err != nil {
		t.Fatalf("CreatePlugin() error = %v", err)
	}

	if err := managerInstance.DeletePlugin(context.Background(), created.ID); err != nil {
		t.Fatalf("DeletePlugin() error = %v", err)
	}

	if _, err := managerInstance.GetPlugin(context.Background(), created.ID); !errors.Is(err, ErrPluginNotFound) {
		t.Fatalf("GetPlugin() after DeletePlugin error = %v, want ErrPluginNotFound", err)
	}
}

func TestManagerDeletePlugin_RepositoryDeleteFailureDoesNotDeleteBlobOrMetadata(t *testing.T) {
	t.Parallel()

	stor := plugin.NewMemoryStorage()
	repoWrap := &errOnPluginRepoDelete{MemoryRepository: plugin.NewMemoryRepository()}
	managerInstance := newTestManager(t, testManagerOptions{
		pluginRepo:    repoWrap,
		pluginStorage: stor,
	})

	body := buildPluginZip(t, "repo-fail", "blob")
	created, err := managerInstance.CreatePlugin(context.Background(), CreatePluginRequest{
		Name:    "repo-fail",
		Content: bytes.NewReader(body),
	})
	if err != nil {
		t.Fatalf("CreatePlugin() error = %v", err)
	}

	if err := managerInstance.DeletePlugin(context.Background(), created.ID); err == nil {
		t.Fatal("DeletePlugin() error = nil, want non-nil")
	}

	if _, err := managerInstance.GetPlugin(context.Background(), created.ID); err != nil {
		t.Fatalf("GetPlugin() error = %v, want metadata still present", err)
	}
	rc, _, err := stor.Get(context.Background(), plugin.PluginID(created.ID))
	if err != nil {
		t.Fatalf("storage Get() error = %v, want blob still present", err)
	}
	_ = rc.Close()
}

func TestManagerDeletePlugin_StorageNotFoundStillDeletesRepository(t *testing.T) {
	t.Parallel()

	managerInstance := newTestManager(t, testManagerOptions{
		pluginStorage: notFoundPluginStorageDelete{MemoryStorage: plugin.NewMemoryStorage()},
	})

	body := buildPluginZip(t, "orphan-meta", "blob")
	created, err := managerInstance.CreatePlugin(context.Background(), CreatePluginRequest{
		Name:    "orphan-meta",
		Content: bytes.NewReader(body),
	})
	if err != nil {
		t.Fatalf("CreatePlugin() error = %v", err)
	}

	if err := managerInstance.DeletePlugin(context.Background(), created.ID); err != nil {
		t.Fatalf("DeletePlugin() error = %v", err)
	}

	if _, err := managerInstance.GetPlugin(context.Background(), created.ID); !errors.Is(err, ErrPluginNotFound) {
		t.Fatalf("GetPlugin() error = %v, want ErrPluginNotFound", err)
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

func TestManagerCreateWorkspace_DeletesRowWhenSubmitFailsWithShutdown(t *testing.T) {
	t.Parallel()

	// P1 regression: Insert succeeds, then Stop races so Submit returns
	// ErrControllerShutdown. CreateWorkspace must delete the row. Manager.Stop
	// closes the workspace repository; this wrapper's Close is a no-op so the
	// compensating Delete can still run on the underlying MemoryRepository.

	pluginRepo := plugin.NewMemoryRepository()
	pluginStorage := plugin.NewMemoryStorage()
	innerWS := workspace.NewMemoryRepository()
	pauseRepo := &pauseInsertWorkspaceRepo{
		MemoryRepository: innerWS,
		insertDone:       make(chan struct{}),
		allowProceed:     make(chan struct{}),
	}

	spec := &fakeAgentSpec{}
	builder := NewBuilder()
	builder.PluginRepository = pluginRepo
	builder.PluginStorage = pluginStorage
	builder.WorkspaceRepository = pauseRepo
	builder.IDGenerator = fixedIDGenerator{
		nextPluginID:    PluginID("plg_fixed"),
		nextWorkspaceID: WorkspaceID("wsp_shutdown_race"),
	}
	builder.InstallWorkers = 1
	builder.ProbeInterval = time.Hour
	builder.ProbeWorkers = 1
	builder.ProbeTimeout = time.Minute
	builder.RegisterAgentSpec(spec)
	builder.RegisterInfraType(InfraType("localdir"), newFakeInfraRegistry().factory())

	managerInstance, err := builder.Build(context.Background())
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if err := managerInstance.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { _ = managerInstance.Stop(context.Background()) })

	pluginBody := buildPluginZip(t, "demo-plugin", "test-body")
	plug, err := managerInstance.CreatePlugin(context.Background(), CreatePluginRequest{
		Name:    "demo-plugin",
		Content: bytes.NewReader(pluginBody),
	})
	if err != nil {
		t.Fatalf("CreatePlugin() error = %v", err)
	}

	alias := Alias("ShutdownRace")
	dir := t.TempDir()
	wantID := workspace.WorkspaceID("wsp_shutdown_race")

	var createErr error
	createDone := make(chan struct{})
	go func() {
		defer close(createDone)
		_, createErr = managerInstance.CreateWorkspace(context.Background(), CreateWorkspaceRequest{
			Alias:        alias,
			AgentType:    AgentType("claude-code"),
			InfraType:    InfraType("localdir"),
			InfraOptions: infraops.Options{"dir": dir},
			Plugins:      []PluginRef{{ID: plug.ID}},
		})
	}()

	<-pauseRepo.insertDone

	if err := managerInstance.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}

	close(pauseRepo.allowProceed)
	<-createDone

	if createErr == nil {
		t.Fatal("CreateWorkspace() error = nil, want non-nil")
	}
	if !errors.Is(createErr, workspace.ErrControllerShutdown) {
		t.Fatalf("CreateWorkspace() error = %v, want errors.Is(..., workspace.ErrControllerShutdown)", createErr)
	}

	if _, err := innerWS.Get(context.Background(), wantID); !errors.Is(err, workspace.ErrWorkspaceNotFound) {
		t.Fatalf("Get(%q) after failed create error = %v, want ErrWorkspaceNotFound", wantID, err)
	}
}

// pauseInsertWorkspaceRepo blocks after a successful Insert until allowProceed
// is closed, so the test can call Manager.Stop before Submit runs.
// Close is a no-op so Manager.Stop does not mark the inner MemoryRepository
// closed before CreateWorkspace's compensating Delete runs.
type pauseInsertWorkspaceRepo struct {
	*workspace.MemoryRepository
	insertOnce   sync.Once
	insertDone   chan struct{}
	allowProceed chan struct{}
}

func (r *pauseInsertWorkspaceRepo) Insert(ctx context.Context, w workspace.Workspace) error {
	if err := r.MemoryRepository.Insert(ctx, w); err != nil {
		return err
	}
	r.insertOnce.Do(func() { close(r.insertDone) })
	<-r.allowProceed
	return nil
}

func (r *pauseInsertWorkspaceRepo) Close(context.Context) error {
	return nil
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
	pluginStorage plugin.Storage
	workspaceRepo workspace.Repository
}

// errOnPluginStorageDelete wraps MemoryStorage and forces Delete to fail.
type errOnPluginStorageDelete struct {
	*plugin.MemoryStorage
}

func (errOnPluginStorageDelete) Delete(context.Context, plugin.PluginID) error {
	return errors.New("forced storage delete failure")
}

// errOnPluginRepoDelete wraps MemoryRepository and forces Delete to fail.
type errOnPluginRepoDelete struct {
	*plugin.MemoryRepository
}

func (r *errOnPluginRepoDelete) Delete(ctx context.Context, id plugin.PluginID) error {
	return errors.New("forced repository delete failure")
}

// notFoundPluginStorageDelete wraps MemoryStorage and makes Delete return ErrStorageNotFound.
type notFoundPluginStorageDelete struct {
	*plugin.MemoryStorage
}

func (notFoundPluginStorageDelete) Delete(context.Context, plugin.PluginID) error {
	return plugin.ErrStorageNotFound
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
	if opts.pluginStorage != nil {
		builder.PluginStorage = opts.pluginStorage
	} else {
		builder.PluginStorage = plugin.NewMemoryStorage()
	}
	if opts.workspaceRepo != nil {
		builder.WorkspaceRepository = opts.workspaceRepo
	} else {
		builder.WorkspaceRepository = workspace.NewMemoryRepository()
	}
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

	managerInstance, err := builder.Build(context.Background())
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
