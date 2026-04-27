package workspace

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"path"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AiRanthem/ANA/pkg/manager/agent"
	"github.com/AiRanthem/ANA/pkg/manager/agent/claudecode"
	"github.com/AiRanthem/ANA/pkg/manager/infraops"
	"github.com/AiRanthem/ANA/pkg/manager/plugin"
)

func TestControllerSubmitReturnsImmediately(t *testing.T) {
	t.Parallel()

	started := make(chan struct{}, 1)
	release := make(chan struct{})
	h := newControllerHarness(t, controllerHarnessOptions{
		installFn: func(ctx context.Context, _ infraops.InfraOps, _ agent.InstallParams) error {
			select {
			case started <- struct{}{}:
			default:
			}
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	})
	defer h.stop(t)

	row := h.insertWorkspace(t, "wsp_submit", "submit-immediate", StatusInit)

	submitDone := make(chan error, 1)
	go func() {
		submitDone <- h.controller.Submit(context.Background(), row, installParamsFor(row))
	}()

	select {
	case err := <-submitDone:
		if err != nil {
			t.Fatalf("Submit() error = %v", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatalf("Submit() blocked, want immediate return")
	}

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatalf("install did not start")
	}

	if got := h.controller.CountInflight(); got != 1 {
		t.Fatalf("CountInflight() = %d, want 1 while install is running", got)
	}

	close(release)
	waitForWorkspaceStatus(t, h.repo, row.ID, StatusHealthy, time.Second)
}

func TestControllerSubmitInstallSuccess(t *testing.T) {
	t.Parallel()

	h := newControllerHarness(t, controllerHarnessOptions{})
	defer h.stop(t)

	row := h.insertWorkspace(t, "wsp_success", "success", StatusInit)

	if err := h.controller.Submit(context.Background(), row, installParamsFor(row)); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}

	got := waitForWorkspaceStatus(t, h.repo, row.ID, StatusHealthy, time.Second)
	if got.StatusError != nil {
		t.Fatalf("StatusError = %#v, want nil", got.StatusError)
	}
	if len(got.Plugins) != 1 {
		t.Fatalf("Plugins length = %d, want 1", len(got.Plugins))
	}
	if len(got.Plugins[0].PlacedPaths) == 0 {
		t.Fatalf("PlacedPaths = %v, want non-empty", got.Plugins[0].PlacedPaths)
	}
	if got.Plugins[0].PlacedPaths[0] != "plugins/manifest.toml" {
		t.Fatalf("PlacedPaths[0] = %q, want %q", got.Plugins[0].PlacedPaths[0], "plugins/manifest.toml")
	}
	if h.spec.installCalls.Load() != 1 {
		t.Fatalf("Install() calls = %d, want 1", h.spec.installCalls.Load())
	}
	if h.spec.probeCalls.Load() != 1 {
		t.Fatalf("Probe() calls = %d, want 1", h.spec.probeCalls.Load())
	}
}

func TestControllerSubmitInstallFailureTransitionsFailed(t *testing.T) {
	t.Parallel()

	h := newControllerHarness(t, controllerHarnessOptions{
		installFn: func(context.Context, infraops.InfraOps, agent.InstallParams) error {
			return errors.New("install boom")
		},
	})
	defer h.stop(t)

	row := h.insertWorkspace(t, "wsp_fail", "install-fail", StatusInit)

	if err := h.controller.Submit(context.Background(), row, installParamsFor(row)); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}

	got := waitForWorkspaceStatus(t, h.repo, row.ID, StatusFailed, time.Second)
	if got.StatusError == nil {
		t.Fatalf("StatusError = nil, want non-nil")
	}
	if got.StatusError.Phase != "install" {
		t.Fatalf("StatusError.Phase = %q, want %q", got.StatusError.Phase, "install")
	}
	if got.StatusError.Code != "install.error" {
		t.Fatalf("StatusError.Code = %q, want %q", got.StatusError.Code, "install.error")
	}
}

func TestControllerSubmitTimeoutTransitionsFailed(t *testing.T) {
	t.Parallel()

	h := newControllerHarness(t, controllerHarnessOptions{
		installTimeout: 40 * time.Millisecond,
		installFn: func(ctx context.Context, _ infraops.InfraOps, _ agent.InstallParams) error {
			<-ctx.Done()
			return ctx.Err()
		},
	})
	defer h.stop(t)

	row := h.insertWorkspace(t, "wsp_timeout", "timeout", StatusInit)

	if err := h.controller.Submit(context.Background(), row, installParamsFor(row)); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}

	got := waitForWorkspaceStatus(t, h.repo, row.ID, StatusFailed, time.Second)
	if got.StatusError == nil {
		t.Fatalf("StatusError = nil, want non-nil")
	}
	if got.StatusError.Code != "install.timeout" {
		t.Fatalf("StatusError.Code = %q, want %q", got.StatusError.Code, "install.timeout")
	}
	if !strings.Contains(got.StatusError.Message, ErrInstallTimeout.Error()) {
		t.Fatalf("StatusError.Message = %q, want to contain %q", got.StatusError.Message, ErrInstallTimeout.Error())
	}
}

func TestControllerDeleteContinuesPastUninstallAndClearErrors(t *testing.T) {
	t.Parallel()

	h := newControllerHarness(t, controllerHarnessOptions{
		uninstallFn: func(context.Context, infraops.InfraOps) error {
			return errors.New("uninstall boom")
		},
	})
	defer h.stop(t)

	row := h.insertWorkspace(t, "wsp_delete", "delete", StatusHealthy)
	state := h.registry.stateFor(string(row.ID))
	state.clearErr = errors.New("clear boom")

	if err := h.controller.Delete(context.Background(), row.ID); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}

	if _, err := h.repo.Get(context.Background(), row.ID); !errors.Is(err, ErrWorkspaceNotFound) {
		t.Fatalf("Get() after Delete error = %v, want ErrWorkspaceNotFound", err)
	}
	if h.spec.uninstallCalls.Load() != 1 {
		t.Fatalf("Uninstall() calls = %d, want 1", h.spec.uninstallCalls.Load())
	}
	if state.clearCalls.Load() != 1 {
		t.Fatalf("Clear() calls = %d, want 1", state.clearCalls.Load())
	}
}

func TestControllerDeleteWaitsForInFlightInstall(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	started := make(chan struct{}, 1)
	h := newControllerHarness(t, controllerHarnessOptions{
		installFn: func(ctx context.Context, _ infraops.InfraOps, _ agent.InstallParams) error {
			select {
			case started <- struct{}{}:
			default:
			}
			<-release
			return nil
		},
	})
	defer h.stop(t)

	row := h.insertWorkspace(t, "wsp_del_wait", "del-wait", StatusInit)

	if err := h.controller.Submit(context.Background(), row, installParamsFor(row)); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatalf("install did not start")
	}

	delDone := make(chan error, 1)
	go func() {
		delDone <- h.controller.Delete(context.Background(), row.ID)
	}()

	select {
	case err := <-delDone:
		t.Fatalf("Delete() completed early with err=%v", err)
	case <-time.After(200 * time.Millisecond):
	}

	close(release)

	select {
	case err := <-delDone:
		if err != nil {
			t.Fatalf("Delete() error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("Delete() did not complete after install released")
	}

	if _, err := h.repo.Get(context.Background(), row.ID); !errors.Is(err, ErrWorkspaceNotFound) {
		t.Fatalf("Get() after Delete error = %v, want ErrWorkspaceNotFound", err)
	}
}

func TestControllerDelete_ReturnsWhenWaitDeadlineExceeded(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	started := make(chan struct{}, 1)
	h := newControllerHarness(t, controllerHarnessOptions{
		installFn: func(_ context.Context, _ infraops.InfraOps, _ agent.InstallParams) error {
			select {
			case started <- struct{}{}:
			default:
			}
			<-release
			return nil
		},
	})
	defer h.stop(t)

	row := h.insertWorkspace(t, "wsp_del_deadline", "del-deadline", StatusInit)
	if err := h.controller.Submit(context.Background(), row, installParamsFor(row)); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatalf("install did not start")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	err := h.controller.Delete(ctx, row.ID)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Delete() error = %v, want context.DeadlineExceeded", err)
	}

	close(release)
}

func TestControllerStopCancelsInflightAndRejectsNewSubmit(t *testing.T) {
	t.Parallel()

	started := make(chan struct{}, 1)
	h := newControllerHarness(t, controllerHarnessOptions{
		installFn: func(ctx context.Context, _ infraops.InfraOps, _ agent.InstallParams) error {
			select {
			case started <- struct{}{}:
			default:
			}
			<-ctx.Done()
			return ctx.Err()
		},
	})

	row := h.insertWorkspace(t, "wsp_shutdown", "shutdown", StatusInit)
	if err := h.controller.Submit(context.Background(), row, installParamsFor(row)); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatalf("install did not start")
	}

	if err := h.controller.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}

	got := waitForWorkspaceStatus(t, h.repo, row.ID, StatusFailed, time.Second)
	if got.StatusError == nil {
		t.Fatalf("StatusError = nil, want non-nil")
	}
	if got.StatusError.Code != "shutdown" {
		t.Fatalf("StatusError.Code = %q, want %q", got.StatusError.Code, "shutdown")
	}
	if err := h.controller.Submit(context.Background(), row, installParamsFor(row)); !errors.Is(err, ErrControllerShutdown) {
		t.Fatalf("Submit() after Stop error = %v, want ErrControllerShutdown", err)
	}
}

func TestControllerSubmit_PostInstallProbeHonorsProbeTimeout(t *testing.T) {
	t.Parallel()

	h := newControllerHarness(t, controllerHarnessOptions{
		installTimeout: time.Hour,
		probeTimeout:   25 * time.Millisecond,
		probeFn: func(ctx context.Context, _ infraops.InfraOps) (agent.ProbeResult, error) {
			<-ctx.Done()
			return agent.ProbeResult{}, ctx.Err()
		},
	})
	defer h.stop(t)

	row := h.insertWorkspace(t, "wsp_probe_timeout", "probe-timeout", StatusInit)

	if err := h.controller.Submit(context.Background(), row, installParamsFor(row)); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}

	got := waitForWorkspaceStatus(t, h.repo, row.ID, StatusFailed, time.Second)
	if got.StatusError == nil {
		t.Fatalf("StatusError = nil, want non-nil")
	}
	if got.StatusError.Phase != "probe" {
		t.Fatalf("StatusError.Phase = %q, want %q", got.StatusError.Phase, "probe")
	}
}

func TestControllerSubmit_RetriesTransientRepoWrites(t *testing.T) {
	t.Parallel()

	h := newControllerHarness(t, controllerHarnessOptions{
		flakyRepo: true,
	})
	defer h.stop(t)

	row := h.insertWorkspace(t, "wsp_flaky", "flaky", StatusInit)

	if err := h.controller.Submit(context.Background(), row, installParamsFor(row)); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}

	waitForWorkspaceStatus(t, h.repo, row.ID, StatusHealthy, 2*time.Second)

	if h.flaky == nil {
		t.Fatalf("expected flaky repo")
	}
	if h.flaky.statusCalls < 3 {
		t.Fatalf("UpdateStatusCAS calls = %d, want >= 3 (initial + 2 retries)", h.flaky.statusCalls)
	}
}

// failHealthyCASRepo rejects UpdateStatusCAS(init→healthy); other transitions delegate to MemoryRepository.
type failHealthyCASRepo struct {
	*MemoryRepository
}

func (r *failHealthyCASRepo) UpdateStatusCAS(ctx context.Context, id WorkspaceID, writer StatusWriter, expect Status, next Status, statusErr *Error, lastProbeAt time.Time) error {
	if writer == StatusWriterController && expect == StatusInit && next == StatusHealthy {
		return errors.New("forced healthy CAS failure")
	}
	return r.MemoryRepository.UpdateStatusCAS(ctx, id, writer, expect, next, statusErr, lastProbeAt)
}

func TestControllerSubmit_HealthyCASFailureTransitionsFailed(t *testing.T) {
	t.Parallel()

	repo := &failHealthyCASRepo{MemoryRepository: NewMemoryRepository()}
	h := newControllerHarness(t, controllerHarnessOptions{repo: repo})
	defer h.stop(t)

	row := h.insertWorkspace(t, "wsp_healthy_cas_fail", "healthy-cas-fail", StatusInit)

	if err := h.controller.Submit(context.Background(), row, installParamsFor(row)); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}

	waitForWorkspaceStatus(t, h.repo, row.ID, StatusFailed, 2*time.Second)

	got, err := h.repo.Get(context.Background(), row.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.StatusError == nil || got.StatusError.Phase != "status" {
		t.Fatalf("StatusError = %+v, want Phase status", got.StatusError)
	}
	if got.StatusError.Code != "status.error" {
		t.Fatalf("StatusError.Code = %q, want status.error", got.StatusError.Code)
	}
}

func TestControllerDelete_MissingRowReturnsNotFound(t *testing.T) {
	t.Parallel()

	h := newControllerHarness(t, controllerHarnessOptions{})
	defer h.stop(t)

	if err := h.controller.Delete(context.Background(), WorkspaceID("wsp_missing")); !errors.Is(err, ErrWorkspaceNotFound) {
		t.Fatalf("Delete() error = %v, want ErrWorkspaceNotFound", err)
	}
	if h.spec.uninstallCalls.Load() != 0 {
		t.Fatalf("Uninstall calls = %d, want 0", h.spec.uninstallCalls.Load())
	}
}

func TestControllerDeleteOpensBeforeUninstall(t *testing.T) {
	t.Parallel()

	h := newControllerHarness(t, controllerHarnessOptions{
		uninstallFn: func(ctx context.Context, ops infraops.InfraOps) error {
			if fi, ok := ops.(*fakeInfra); ok {
				fi.state.appendEvent("uninstall")
			}
			return nil
		},
	})
	defer h.stop(t)

	row := h.insertWorkspace(t, "wsp_del_open", "del-open", StatusHealthy)
	st := h.registry.stateFor(string(row.ID))

	if err := h.controller.Delete(context.Background(), row.ID); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if st.openCalls.Load() != 1 {
		t.Fatalf("Open() calls = %d, want 1", st.openCalls.Load())
	}
	if h.spec.uninstallCalls.Load() != 1 {
		t.Fatalf("Uninstall() calls = %d, want 1", h.spec.uninstallCalls.Load())
	}
	if st.clearCalls.Load() != 1 {
		t.Fatalf("Clear() calls = %d, want 1", st.clearCalls.Load())
	}
	st.eventsMu.Lock()
	got := slices.Clone(st.events)
	st.eventsMu.Unlock()
	want := []string{"open", "uninstall", "clear"}
	if !slices.Equal(got, want) {
		t.Fatalf("infra event order = %v, want %v", got, want)
	}
}

func TestControllerDeleteContinuesToClearWhenOpenFails(t *testing.T) {
	t.Parallel()

	h := newControllerHarness(t, controllerHarnessOptions{})
	defer h.stop(t)

	row := h.insertWorkspace(t, "wsp_open_fail", "open-fail", StatusHealthy)
	st := h.registry.stateFor(string(row.ID))
	st.openErr = errors.New("open failed")

	if err := h.controller.Delete(context.Background(), row.ID); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if h.spec.uninstallCalls.Load() != 0 {
		t.Fatalf("Uninstall() calls = %d, want 0", h.spec.uninstallCalls.Load())
	}
	if st.clearCalls.Load() != 1 {
		t.Fatalf("Clear() calls = %d, want 1", st.clearCalls.Load())
	}
	if _, err := h.repo.Get(context.Background(), row.ID); !errors.Is(err, ErrWorkspaceNotFound) {
		t.Fatalf("Get() after Delete error = %v, want ErrWorkspaceNotFound", err)
	}
}

func TestControllerStop_DoesNotHangWhenFailedStatusPersistBlocks(t *testing.T) {
	t.Parallel()

	mem := NewMemoryRepository()
	block := &blockingControllerUpdateStatusCASRepo{
		MemoryRepository: mem,
		signal:           make(chan struct{}, 1),
	}
	h := newControllerHarness(t, controllerHarnessOptions{repo: block})
	defer h.stop(t)

	now := time.Now().UTC()
	row := h.insertWorkspaceRaw(t, Workspace{
		ID:           "wsp_block_failed",
		Namespace:    "default",
		Alias:        "block-failed",
		AgentType:    AgentType("claude-code"),
		InfraType:    InfraType("missing-infra"),
		InfraOptions: infraops.Options{"dir": "wsp_block_failed"},
		Plugins: []AttachedPlugin{
			{PluginID: PluginID("plg_1"), Name: "demo-plugin", ContentHash: h.pluginObj.ContentHash},
		},
		Status:    StatusInit,
		CreatedAt: now,
		UpdatedAt: now,
	})
	block.blockID = row.ID

	if err := h.controller.Submit(context.Background(), row, installParamsFor(row)); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}

	select {
	case <-block.signal:
	case <-time.After(3 * time.Second):
		t.Fatal("UpdateStatusCAS did not block")
	}

	stopCtx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	stopErr := h.controller.Stop(stopCtx)
	cancel()
	if errors.Is(stopErr, context.DeadlineExceeded) {
		t.Fatalf("Stop() error = %v", stopErr)
	}
	if stopErr != nil {
		t.Fatalf("Stop() error = %v", stopErr)
	}
}

func TestControllerDelete_MissingFactoryStillDeletesRow(t *testing.T) {
	t.Parallel()

	h := newControllerHarness(t, controllerHarnessOptions{})
	defer h.stop(t)

	now := time.Now().UTC()
	row := h.insertWorkspaceRaw(t, Workspace{
		ID:           "wsp_miss_fac",
		Namespace:    "default",
		Alias:        "miss-fac",
		AgentType:    AgentType("claude-code"),
		InfraType:    InfraType("missing-infra"),
		InfraOptions: infraops.Options{"dir": "wsp_miss_fac"},
		Plugins: []AttachedPlugin{
			{PluginID: PluginID("plg_1"), Name: "demo-plugin", ContentHash: h.pluginObj.ContentHash},
		},
		Status:    StatusHealthy,
		CreatedAt: now,
		UpdatedAt: now,
	})

	if err := h.controller.Delete(context.Background(), row.ID); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, err := h.repo.Get(context.Background(), row.ID); !errors.Is(err, ErrWorkspaceNotFound) {
		t.Fatalf("Get() after Delete error = %v, want ErrWorkspaceNotFound", err)
	}
}

func TestControllerDelete_FactoryBuildErrorStillDeletesRow(t *testing.T) {
	t.Parallel()

	h := newControllerHarness(t, controllerHarnessOptions{})
	defer h.stop(t)

	now := time.Now().UTC()
	row := h.insertWorkspaceRaw(t, Workspace{
		ID:           "wsp_bad_opts",
		Namespace:    "default",
		Alias:        "bad-opts",
		AgentType:    AgentType("claude-code"),
		InfraType:    InfraType("localdir"),
		InfraOptions: infraops.Options{},
		Plugins: []AttachedPlugin{
			{PluginID: PluginID("plg_1"), Name: "demo-plugin", ContentHash: h.pluginObj.ContentHash},
		},
		Status:    StatusHealthy,
		CreatedAt: now,
		UpdatedAt: now,
	})

	if err := h.controller.Delete(context.Background(), row.ID); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, err := h.repo.Get(context.Background(), row.ID); !errors.Is(err, ErrWorkspaceNotFound) {
		t.Fatalf("Get() after Delete error = %v, want ErrWorkspaceNotFound", err)
	}
}

func TestControllerDelete_MissingAgentSpecStillDeletesRow(t *testing.T) {
	t.Parallel()

	h := newControllerHarness(t, controllerHarnessOptions{})
	defer h.stop(t)

	now := time.Now().UTC()
	row := h.insertWorkspaceRaw(t, Workspace{
		ID:           "wsp_miss_spec",
		Namespace:    "default",
		Alias:        "miss-spec",
		AgentType:    AgentType("missing-agent-type"),
		InfraType:    InfraType("localdir"),
		InfraOptions: infraops.Options{"dir": "wsp_miss_spec"},
		Plugins: []AttachedPlugin{
			{PluginID: PluginID("plg_1"), Name: "demo-plugin", ContentHash: h.pluginObj.ContentHash},
		},
		Status:    StatusHealthy,
		CreatedAt: now,
		UpdatedAt: now,
	})
	st := h.registry.stateFor(string(row.ID))

	if err := h.controller.Delete(context.Background(), row.ID); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if st.clearCalls.Load() != 1 {
		t.Fatalf("Clear() calls = %d, want 1", st.clearCalls.Load())
	}
	if _, err := h.repo.Get(context.Background(), row.ID); !errors.Is(err, ErrWorkspaceNotFound) {
		t.Fatalf("Get() after Delete error = %v, want ErrWorkspaceNotFound", err)
	}
}

// blockingControllerUpdateStatusCASRepo blocks on failed-status CAS matching FIXV2 harness rules.
type blockingControllerUpdateStatusCASRepo struct {
	*MemoryRepository
	signal  chan struct{}
	blockID WorkspaceID
}

func (r *blockingControllerUpdateStatusCASRepo) UpdateStatusCAS(ctx context.Context, id WorkspaceID, writer StatusWriter, expect Status, next Status, statusErr *Error, lastProbeAt time.Time) error {
	if writer == StatusWriterController && expect == StatusInit && next == StatusFailed && id == r.blockID {
		select {
		case r.signal <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return ctx.Err()
	}
	return r.MemoryRepository.UpdateStatusCAS(ctx, id, writer, expect, next, statusErr, lastProbeAt)
}

func TestControllerSubmitPluginHashMismatchFailsBeforeApply(t *testing.T) {
	t.Parallel()

	h := newControllerHarness(t, controllerHarnessOptions{})
	defer h.stop(t)

	row := h.insertWorkspace(t, "wsp_hash_mismatch", "hash-mismatch", StatusInit)
	row.Plugins[0].ContentHash = "sha256:stale"
	if err := h.repo.Update(context.Background(), row); err != nil {
		t.Fatalf("Update stale hash row: %v", err)
	}

	if err := h.controller.Submit(context.Background(), row, installParamsFor(row)); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}

	got := waitForWorkspaceStatus(t, h.repo, row.ID, StatusFailed, time.Second)
	if got.StatusError == nil || got.StatusError.Phase != "install" {
		t.Fatalf("StatusError = %#v, want install failure", got.StatusError)
	}
	if got.StatusError.Code != "install.plugin_hash_mismatch" {
		t.Fatalf("StatusError.Code = %q, want install.plugin_hash_mismatch", got.StatusError.Code)
	}
	if h.spec.layout.applyCalls.Load() != 0 {
		t.Fatalf("layout apply calls = %d, want 0", h.spec.layout.applyCalls.Load())
	}
}

type controllerHarnessOptions struct {
	installTimeout time.Duration
	probeTimeout   time.Duration
	installFn      func(context.Context, infraops.InfraOps, agent.InstallParams) error
	uninstallFn    func(context.Context, infraops.InfraOps) error
	probeFn        func(context.Context, infraops.InfraOps) (agent.ProbeResult, error)
	flakyRepo      bool
	// sharedRepo, when set, is used as the backing store (e.g. with a scheduler sharing the same repo).
	sharedRepo *MemoryRepository
	// repo, when non-nil, is used as the controller repository (e.g. blocking wrappers). Mutually exclusive with flakyRepo and sharedRepo.
	repo Repository
}

type controllerHarness struct {
	controller *Controller
	repo       Repository
	flaky      *flakyRepo
	storage    *plugin.MemoryStorage
	spec       *fakeAgentSpec
	registry   *fakeInfraRegistry
	pluginObj  plugin.StoredObject
}

// flakyRepo fails the first two UpdateStatusCAS calls then delegates (exercises retryRepoWrite).
type flakyRepo struct {
	*MemoryRepository
	mu          sync.Mutex
	statusCalls int
}

func (r *flakyRepo) UpdateStatus(ctx context.Context, id WorkspaceID, status Status, statusErr *Error, lastProbeAt time.Time) error {
	return r.MemoryRepository.UpdateStatus(ctx, id, status, statusErr, lastProbeAt)
}

func (r *flakyRepo) UpdateStatusCAS(ctx context.Context, id WorkspaceID, writer StatusWriter, expect Status, next Status, statusErr *Error, lastProbeAt time.Time) error {
	r.mu.Lock()
	r.statusCalls++
	n := r.statusCalls
	r.mu.Unlock()
	if n <= 2 {
		return errors.New("transient")
	}
	return r.MemoryRepository.UpdateStatusCAS(ctx, id, writer, expect, next, statusErr, lastProbeAt)
}

func newControllerHarness(t *testing.T, opts controllerHarnessOptions) *controllerHarness {
	t.Helper()

	specSet := agent.NewSpecSet()
	layout := &fakePluginLayout{}
	spec := &fakeAgentSpec{
		layout:      layout,
		installFn:   opts.installFn,
		uninstallFn: opts.uninstallFn,
		probeFn:     opts.probeFn,
	}
	if err := specSet.Register(spec); err != nil {
		t.Fatalf("Register(spec) error = %v", err)
	}

	registry := newFakeInfraRegistry()
	factories := infraops.NewFactorySet()
	if err := factories.Register(infraops.InfraType("localdir"), registry.factory()); err != nil {
		t.Fatalf("Register(factory) error = %v", err)
	}

	storage := plugin.NewMemoryStorage()
	obj := seedPluginZip(t, storage, plugin.PluginID("plg_1"), "demo-plugin")

	if opts.repo != nil && opts.flakyRepo {
		t.Fatal("controllerHarnessOptions: repo and flakyRepo are mutually exclusive")
	}
	if opts.repo != nil && opts.sharedRepo != nil {
		t.Fatal("controllerHarnessOptions: repo and sharedRepo are mutually exclusive")
	}

	var baseRepo *MemoryRepository
	if opts.sharedRepo != nil {
		baseRepo = opts.sharedRepo
	} else if opts.repo == nil {
		baseRepo = NewMemoryRepository()
	}

	var repo Repository
	var flaky *flakyRepo
	switch {
	case opts.repo != nil:
		repo = opts.repo
	case opts.flakyRepo:
		flaky = &flakyRepo{MemoryRepository: baseRepo}
		repo = flaky
	default:
		repo = baseRepo
	}

	controller, err := NewController(ControllerConfig{
		Repo:           repo,
		PluginStorage:  storage,
		AgentSpecs:     specSet,
		Factories:      factories,
		InstallTimeout: opts.installTimeout,
		ProbeTimeout:   opts.probeTimeout,
		InstallWorkers: 1,
		MaxPluginSize:  1 << 20,
	})
	if err != nil {
		t.Fatalf("NewController() error = %v", err)
	}
	if err := controller.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	return &controllerHarness{
		controller: controller,
		repo:       repo,
		flaky:      flaky,
		storage:    storage,
		spec:       spec,
		registry:   registry,
		pluginObj:  obj,
	}
}

func (h *controllerHarness) stop(t *testing.T) {
	t.Helper()
	if err := h.controller.Stop(context.Background()); err != nil && !errors.Is(err, ErrControllerShutdown) {
		t.Fatalf("Stop() error = %v", err)
	}
}

func (h *controllerHarness) insertWorkspace(t *testing.T, id WorkspaceID, alias Alias, status Status) Workspace {
	t.Helper()

	row := Workspace{
		ID:           id,
		Namespace:    Namespace("default"),
		Alias:        alias,
		AgentType:    AgentType("claude-code"),
		InfraType:    InfraType("localdir"),
		InfraOptions: infraops.Options{"dir": string(id)},
		Plugins: []AttachedPlugin{
			{
				PluginID:    PluginID("plg_1"),
				Name:        "demo-plugin",
				ContentHash: h.pluginObj.ContentHash,
			},
		},
		Status:    status,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	if err := h.repo.Insert(context.Background(), row); err != nil {
		t.Fatalf("Insert() error = %v", err)
	}
	return row
}

// insertWorkspaceRaw inserts an arbitrary workspace row (for cases where Update cannot change immutable identity fields).
func (h *controllerHarness) insertWorkspaceRaw(t *testing.T, row Workspace) Workspace {
	t.Helper()
	if err := h.repo.Insert(context.Background(), row); err != nil {
		t.Fatalf("Insert() error = %v", err)
	}
	return row
}

func installParamsFor(w Workspace) agent.InstallParams {
	plugins := make([]agent.AttachedPluginRef, 0, len(w.Plugins))
	for _, attached := range w.Plugins {
		plugins = append(plugins, agent.AttachedPluginRef{
			PluginID:    agent.PluginID(attached.PluginID),
			Name:        attached.Name,
			ContentHash: attached.ContentHash,
		})
	}
	return agent.InstallParams{
		Workspace: agent.WorkspaceSummary{
			ID:        agent.WorkspaceID(w.ID),
			Namespace: agent.Namespace(w.Namespace),
			Alias:     agent.Alias(w.Alias),
			AgentType: agent.AgentType(w.AgentType),
			InfraType: agent.InfraType(w.InfraType),
			Plugins:   plugins,
		},
		UserParams: map[string]any{"token": "secret"},
	}
}

func waitForWorkspaceStatus(t *testing.T, repo Repository, id WorkspaceID, want Status, timeout time.Duration) Workspace {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		row, err := repo.Get(context.Background(), id)
		if err == nil && row.Status == want {
			return row
		}
		time.Sleep(10 * time.Millisecond)
	}

	row, err := repo.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get() after waiting error = %v", err)
	}
	t.Fatalf("Workspace status = %q, want %q", row.Status, want)
	return Workspace{}
}

type fakeAgentSpec struct {
	layout *fakePluginLayout

	installFn   func(context.Context, infraops.InfraOps, agent.InstallParams) error
	uninstallFn func(context.Context, infraops.InfraOps) error
	probeFn     func(context.Context, infraops.InfraOps) (agent.ProbeResult, error)

	installCalls   atomic.Int64
	uninstallCalls atomic.Int64
	probeCalls     atomic.Int64
}

func (s *fakeAgentSpec) Type() agent.AgentType { return agent.AgentType("claude-code") }
func (s *fakeAgentSpec) DisplayName() string   { return "Claude Code" }
func (s *fakeAgentSpec) Description() string   { return "fake" }
func (s *fakeAgentSpec) PluginLayout() agent.PluginLayout {
	return s.layout
}
func (s *fakeAgentSpec) Install(ctx context.Context, ops infraops.InfraOps, params agent.InstallParams) error {
	s.installCalls.Add(1)
	if s.installFn != nil {
		return s.installFn(ctx, ops, params)
	}
	return nil
}
func (s *fakeAgentSpec) Uninstall(ctx context.Context, ops infraops.InfraOps) error {
	s.uninstallCalls.Add(1)
	if s.uninstallFn != nil {
		return s.uninstallFn(ctx, ops)
	}
	return nil
}
func (s *fakeAgentSpec) Probe(ctx context.Context, ops infraops.InfraOps) (agent.ProbeResult, error) {
	s.probeCalls.Add(1)
	if s.probeFn != nil {
		return s.probeFn(ctx, ops)
	}
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

type fakePluginLayout struct {
	applyCalls atomic.Int64
}

func (l *fakePluginLayout) Apply(ctx context.Context, ops infraops.InfraOps, _ plugin.Manifest, pluginRoot fs.FS) ([]string, error) {
	l.applyCalls.Add(1)
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

	mu       sync.Mutex
	files    map[string][]byte
	clearErr error

	initCalls  atomic.Int64
	openCalls  atomic.Int64
	clearCalls atomic.Int64
	openErr    error
	eventsMu   sync.Mutex
	events     []string
}

func (s *fakeInfraState) appendEvent(name string) {
	s.eventsMu.Lock()
	s.events = append(s.events, name)
	s.eventsMu.Unlock()
}

type fakeInfra struct {
	state *fakeInfraState
}

func (i *fakeInfra) Type() infraops.InfraType { return infraops.InfraType("localdir") }
func (i *fakeInfra) Dir() string              { return i.state.dir }
func (i *fakeInfra) Init(context.Context) error {
	i.state.initCalls.Add(1)
	return nil
}
func (i *fakeInfra) Open(context.Context) error {
	i.state.openCalls.Add(1)
	i.state.appendEvent("open")
	return i.state.openErr
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
	i.state.appendEvent("clear")
	i.state.clearCalls.Add(1)
	return i.state.clearErr
}

func TestAttachPlugins_RejectsBatchLayoutDirectoryCollision(t *testing.T) {
	t.Parallel()

	storage := plugin.NewMemoryStorage()
	objA := seedPluginZip(t, storage, plugin.PluginID("plg_a"), "my--plugin")
	objB := seedPluginZip(t, storage, plugin.PluginID("plg_b"), "my-plugin")

	ccSpec, err := claudecode.New(claudecode.Options{})
	if err != nil {
		t.Fatalf("claudecode.New() error = %v", err)
	}

	c := &Controller{
		pluginStorage: storage,
		maxPluginSize: 1 << 20,
	}

	plugins := []AttachedPlugin{
		{PluginID: PluginID("plg_a"), Name: "my--plugin", ContentHash: objA.ContentHash},
		{PluginID: PluginID("plg_b"), Name: "my-plugin", ContentHash: objB.ContentHash},
	}

	state := &fakeInfraState{dir: "ws", files: make(map[string][]byte)}
	ops := &fakeInfra{state: state}

	_, err = c.attachPlugins(context.Background(), ops, ccSpec, plugins)
	if !errors.Is(err, agent.ErrInvalidPluginLayout) {
		t.Fatalf("attachPlugins() error = %v, want ErrInvalidPluginLayout", err)
	}
	state.mu.Lock()
	nFiles := len(state.files)
	state.mu.Unlock()
	if nFiles != 0 {
		t.Fatalf("infra files written = %d, want 0 (preflight should fail before Apply)", nFiles)
	}
}

func TestController_DoesNotPromoteFailedToHealthyAfterInitTimeoutRace(t *testing.T) {
	t.Parallel()

	repo := NewMemoryRepository()
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	now := time.Date(2026, time.April, 27, 12, 0, 0, 0, time.UTC)

	h := newControllerHarness(t, controllerHarnessOptions{
		sharedRepo:     repo,
		installTimeout: time.Hour,
		installFn: func(ctx context.Context, _ infraops.InfraOps, _ agent.InstallParams) error {
			select {
			case started <- struct{}{}:
			default:
			}
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	})
	defer h.stop(t)

	specSet := agent.NewSpecSet()
	schedSpec := &fakeAgentSpec{layout: &fakePluginLayout{}}
	if err := specSet.Register(schedSpec); err != nil {
		t.Fatalf("Register(spec) error = %v", err)
	}
	schedRegistry := newFakeInfraRegistry()
	schedFactories := infraops.NewFactorySet()
	if err := schedFactories.Register(infraops.InfraType("localdir"), schedRegistry.factory()); err != nil {
		t.Fatalf("Register(factory) error = %v", err)
	}
	scheduler, err := NewProbeScheduler(ProbeSchedulerConfig{
		Repo:           repo,
		AgentSpecs:     specSet,
		Factories:      schedFactories,
		Clock:          func() time.Time { return now },
		InstallTimeout: 30 * time.Second,
		Timeout:        5 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewProbeScheduler() error = %v", err)
	}

	row := Workspace{
		ID:           "wsp_init_race",
		Namespace:    "default",
		Alias:        "init-race",
		AgentType:    AgentType("claude-code"),
		InfraType:    InfraType("localdir"),
		InfraOptions: infraops.Options{"dir": "wsp_init_race"},
		Plugins: []AttachedPlugin{
			{PluginID: PluginID("plg_1"), Name: "demo-plugin", ContentHash: h.pluginObj.ContentHash},
		},
		Status:    StatusInit,
		CreatedAt: now.Add(-2 * time.Minute),
		UpdatedAt: now.Add(-2 * time.Minute),
	}
	if err := repo.Insert(context.Background(), row); err != nil {
		t.Fatalf("Insert() error = %v", err)
	}

	if err := h.controller.Submit(context.Background(), row, installParamsFor(row)); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatalf("install did not start")
	}

	if err := scheduler.runTick(context.Background()); err != nil {
		t.Fatalf("runTick() error = %v", err)
	}

	gotFailed, err := repo.Get(context.Background(), row.ID)
	if err != nil {
		t.Fatalf("Get() after watchdog error = %v", err)
	}
	if gotFailed.Status != StatusFailed {
		t.Fatalf("status after watchdog = %q, want failed", gotFailed.Status)
	}

	close(release)
	time.Sleep(400 * time.Millisecond)

	final, err := repo.Get(context.Background(), row.ID)
	if err != nil {
		t.Fatalf("Get() final error = %v", err)
	}
	if final.Status == StatusHealthy {
		t.Fatalf("controller promoted failed -> healthy after watchdog; want failed")
	}
	if final.Status != StatusFailed {
		t.Fatalf("final status = %q, want failed", final.Status)
	}
}

func TestController_PersistAttachedPluginsPreservesConcurrentMetadataUpdate(t *testing.T) {
	t.Parallel()

	installStarted := make(chan struct{})
	metaWritten := make(chan struct{})
	h := newControllerHarness(t, controllerHarnessOptions{
		installFn: func(ctx context.Context, _ infraops.InfraOps, _ agent.InstallParams) error {
			select {
			case installStarted <- struct{}{}:
			default:
			}
			select {
			case <-metaWritten:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	})
	defer h.stop(t)

	row := h.insertWorkspace(t, "wsp_meta", "meta-race", StatusInit)
	go func() {
		<-installStarted
		cur, err := h.repo.Get(context.Background(), row.ID)
		if err != nil {
			t.Errorf("concurrent Get: %v", err)
			close(metaWritten)
			return
		}
		cur.Description = "concurrent-desc"
		cur.Labels = map[string]string{"tier": "gold"}
		if err := h.repo.Update(context.Background(), cur); err != nil {
			t.Errorf("concurrent Update: %v", err)
		}
		close(metaWritten)
	}()

	if err := h.controller.Submit(context.Background(), row, installParamsFor(row)); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}

	got := waitForWorkspaceStatus(t, h.repo, row.ID, StatusHealthy, 2*time.Second)
	if got.Description != "concurrent-desc" {
		t.Fatalf("Description = %q, want concurrent-desc", got.Description)
	}
	if got.Labels == nil || got.Labels["tier"] != "gold" {
		t.Fatalf("Labels = %#v, want tier=gold", got.Labels)
	}
	if len(got.Plugins) != 1 || len(got.Plugins[0].PlacedPaths) == 0 {
		t.Fatalf("Plugins = %#v, want placed paths from install", got.Plugins)
	}
}

func TestController_TransitionToFailed_UsesCASFromInitOnly(t *testing.T) {
	t.Parallel()

	h := newControllerHarness(t, controllerHarnessOptions{
		installFn: func(context.Context, infraops.InfraOps, agent.InstallParams) error {
			return errors.New("install boom")
		},
	})
	defer h.stop(t)

	row := h.insertWorkspace(t, "wsp_cas_failed", "cas-failed", StatusInit)
	if err := h.controller.Submit(context.Background(), row, installParamsFor(row)); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	waitForWorkspaceStatus(t, h.repo, row.ID, StatusFailed, time.Second)

	err := h.repo.UpdateStatusCAS(context.Background(), row.ID, StatusWriterController, StatusInit, StatusFailed,
		&Error{Code: "again", Message: "m", Phase: "install", RecordedAt: time.Now().UTC()}, time.Time{})
	if !errors.Is(err, ErrStatusPreconditionFailed) {
		t.Fatalf("second controller CAS from init error = %v, want ErrStatusPreconditionFailed", err)
	}
}

func seedPluginZip(t *testing.T, storage *plugin.MemoryStorage, id plugin.PluginID, name string) plugin.StoredObject {
	t.Helper()

	body := buildPluginZip(t, name)
	obj, err := storage.Put(context.Background(), id, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("storage.Put() error = %v", err)
	}
	return obj
}

func buildPluginZip(t *testing.T, name string) []byte {
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
	addZipFile(t, zw, "skills/echo/SKILL.md", []byte("# Echo\n"))

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
