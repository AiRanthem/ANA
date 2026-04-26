package workspace

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/AiRanthem/ANA/pkg/manager/agent"
	"github.com/AiRanthem/ANA/pkg/manager/infraops"
)

func TestProbeSchedulerRunTickFlipsHealthyAndFailed(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.April, 26, 11, 0, 0, 0, time.UTC)
	h := newSchedulerHarness(t, schedulerHarnessOptions{
		clock: func() time.Time { return now },
		probeFn: func(_ context.Context, ops infraops.InfraOps) (agent.ProbeResult, error) {
			switch ops.Dir() {
			case "wsp_healthy":
				return agent.ProbeResult{
					Healthy: false,
					Error: &agent.WorkspaceError{
						Code:    "probe.unhealthy",
						Message: "not ready",
						Phase:   "probe",
					},
				}, nil
			case "wsp_failed":
				return agent.ProbeResult{Healthy: true}, nil
			default:
				t.Fatalf("unexpected probe dir %q", ops.Dir())
				return agent.ProbeResult{}, nil
			}
		},
	})

	healthyRow := h.insertWorkspace(t, "wsp_healthy", "healthy", StatusHealthy)
	failedRow := h.insertWorkspace(t, "wsp_failed", "failed", StatusFailed)

	if err := h.scheduler.runTick(context.Background()); err != nil {
		t.Fatalf("runTick() error = %v", err)
	}

	gotHealthy, err := h.repo.Get(context.Background(), healthyRow.ID)
	if err != nil {
		t.Fatalf("Get(healthy) error = %v", err)
	}
	if gotHealthy.Status != StatusFailed {
		t.Fatalf("healthy row status = %q, want %q", gotHealthy.Status, StatusFailed)
	}
	if gotHealthy.StatusError == nil || gotHealthy.StatusError.Code != "probe.unhealthy" {
		t.Fatalf("healthy row StatusError = %#v, want probe.unhealthy", gotHealthy.StatusError)
	}
	if gotHealthy.LastProbeAt.IsZero() {
		t.Fatalf("healthy row LastProbeAt = zero, want non-zero")
	}

	gotFailed, err := h.repo.Get(context.Background(), failedRow.ID)
	if err != nil {
		t.Fatalf("Get(failed) error = %v", err)
	}
	if gotFailed.Status != StatusHealthy {
		t.Fatalf("failed row status = %q, want %q", gotFailed.Status, StatusHealthy)
	}
	if gotFailed.StatusError != nil {
		t.Fatalf("failed row StatusError = %#v, want nil", gotFailed.StatusError)
	}
	if gotFailed.LastProbeAt.IsZero() {
		t.Fatalf("failed row LastProbeAt = zero, want non-zero")
	}

	stats := h.scheduler.Stats()
	if stats.TicksRun != 1 {
		t.Fatalf("TicksRun = %d, want 1", stats.TicksRun)
	}
	if stats.ProbesAttempted != 2 {
		t.Fatalf("ProbesAttempted = %d, want 2", stats.ProbesAttempted)
	}
	if stats.ProbesHealthy != 1 {
		t.Fatalf("ProbesHealthy = %d, want 1", stats.ProbesHealthy)
	}
	if stats.ProbesFailed != 1 {
		t.Fatalf("ProbesFailed = %d, want 1", stats.ProbesFailed)
	}
}

func TestProbeSchedulerRunTickMarksStaleInitRowsFailed(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.April, 26, 11, 0, 0, 0, time.UTC)
	h := newSchedulerHarness(t, schedulerHarnessOptions{
		clock:          func() time.Time { return now },
		installTimeout: 30 * time.Second,
	})

	stale := h.insertWorkspaceAt(t, "wsp_stale", "stale", StatusInit, now.Add(-time.Minute))
	fresh := h.insertWorkspaceAt(t, "wsp_fresh", "fresh", StatusInit, now.Add(-10*time.Second))

	if err := h.scheduler.runTick(context.Background()); err != nil {
		t.Fatalf("runTick() error = %v", err)
	}

	gotStale, err := h.repo.Get(context.Background(), stale.ID)
	if err != nil {
		t.Fatalf("Get(stale) error = %v", err)
	}
	if gotStale.Status != StatusFailed {
		t.Fatalf("stale row status = %q, want %q", gotStale.Status, StatusFailed)
	}
	if gotStale.StatusError == nil || gotStale.StatusError.Code != "install.timeout" {
		t.Fatalf("stale row StatusError = %#v, want install.timeout", gotStale.StatusError)
	}

	gotFresh, err := h.repo.Get(context.Background(), fresh.ID)
	if err != nil {
		t.Fatalf("Get(fresh) error = %v", err)
	}
	if gotFresh.Status != StatusInit {
		t.Fatalf("fresh row status = %q, want %q", gotFresh.Status, StatusInit)
	}

	stats := h.scheduler.Stats()
	if stats.ProbesAttempted != 0 {
		t.Fatalf("ProbesAttempted = %d, want 0", stats.ProbesAttempted)
	}
}

func TestProbeSchedulerStopCancelsActiveProbeAndRejectsRestart(t *testing.T) {
	t.Parallel()

	started := make(chan struct{}, 1)
	h := newSchedulerHarness(t, schedulerHarnessOptions{
		interval: 10 * time.Millisecond,
		probeFn: func(ctx context.Context, _ infraops.InfraOps) (agent.ProbeResult, error) {
			select {
			case started <- struct{}{}:
			default:
			}
			<-ctx.Done()
			return agent.ProbeResult{}, ctx.Err()
		},
	})

	h.insertWorkspace(t, "wsp_probe", "probe", StatusHealthy)

	if err := h.scheduler.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatalf("probe did not start")
	}

	if err := h.scheduler.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if err := h.scheduler.Start(context.Background()); !errors.Is(err, ErrSchedulerShutdown) {
		t.Fatalf("Start() after Stop error = %v, want ErrSchedulerShutdown", err)
	}
}

func TestProbeSchedulerOverrunCounter(t *testing.T) {
	t.Parallel()

	h := newSchedulerHarness(t, schedulerHarnessOptions{
		interval: 5 * time.Millisecond,
		timeout:  time.Second,
		probeFn: func(ctx context.Context, _ infraops.InfraOps) (agent.ProbeResult, error) {
			select {
			case <-time.After(50 * time.Millisecond):
				return agent.ProbeResult{Healthy: true}, nil
			case <-ctx.Done():
				return agent.ProbeResult{}, ctx.Err()
			}
		},
	})

	h.insertWorkspace(t, "wsp_overrun", "overrun", StatusHealthy)

	if err := h.scheduler.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	time.Sleep(120 * time.Millisecond)
	if err := h.scheduler.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}

	if h.scheduler.Stats().Overruns < 1 {
		t.Fatalf("Overruns = %d, want >= 1", h.scheduler.Stats().Overruns)
	}
}

func TestProbeSchedulerStop_SecondCallCompletesAfterFirstDeadline(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	started := make(chan struct{}, 1)
	h := newSchedulerHarness(t, schedulerHarnessOptions{
		interval: 10 * time.Millisecond,
		timeout:  time.Second,
		probeFn: func(context.Context, infraops.InfraOps) (agent.ProbeResult, error) {
			select {
			case started <- struct{}{}:
			default:
			}
			<-release
			return agent.ProbeResult{Healthy: true}, nil
		},
	})

	h.insertWorkspace(t, "wsp_stop_retry", "stop-retry", StatusHealthy)

	if err := h.scheduler.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatalf("probe did not start")
	}

	shortCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	err := h.scheduler.Stop(shortCtx)
	cancel()
	if err == nil {
		t.Fatalf("Stop(short) error = nil, want deadline")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Stop(short) error = %v, want DeadlineExceeded", err)
	}

	close(release)

	if err := h.scheduler.Stop(context.Background()); err != nil {
		t.Fatalf("Stop(bg) error = %v", err)
	}
	if err := h.scheduler.Stop(context.Background()); err != nil {
		t.Fatalf("third Stop error = %v", err)
	}
}

type schedulerHarnessOptions struct {
	clock          func() time.Time
	interval       time.Duration
	timeout        time.Duration
	installTimeout time.Duration
	probeFn        func(context.Context, infraops.InfraOps) (agent.ProbeResult, error)
}

type schedulerHarness struct {
	scheduler *ProbeScheduler
	repo      *MemoryRepository
	spec      *fakeAgentSpec
	registry  *fakeInfraRegistry
}

func newSchedulerHarness(t *testing.T, opts schedulerHarnessOptions) *schedulerHarness {
	t.Helper()

	specSet := agent.NewSpecSet()
	spec := &fakeAgentSpec{
		layout:  &fakePluginLayout{},
		probeFn: opts.probeFn,
	}
	if err := specSet.Register(spec); err != nil {
		t.Fatalf("Register(spec) error = %v", err)
	}

	registry := newFakeInfraRegistry()
	factories := infraops.NewFactorySet()
	if err := factories.Register(infraops.InfraType("localdir"), registry.factory()); err != nil {
		t.Fatalf("Register(factory) error = %v", err)
	}

	repo := NewMemoryRepository()
	scheduler, err := NewProbeScheduler(ProbeSchedulerConfig{
		Repo:           repo,
		AgentSpecs:     specSet,
		Factories:      factories,
		Clock:          opts.clock,
		Interval:       opts.interval,
		Workers:        2,
		Timeout:        opts.timeout,
		InstallTimeout: opts.installTimeout,
	})
	if err != nil {
		t.Fatalf("NewProbeScheduler() error = %v", err)
	}

	return &schedulerHarness{
		scheduler: scheduler,
		repo:      repo,
		spec:      spec,
		registry:  registry,
	}
}

func (h *schedulerHarness) insertWorkspace(t *testing.T, id WorkspaceID, alias Alias, status Status) Workspace {
	t.Helper()
	return h.insertWorkspaceAt(t, id, alias, status, time.Now().UTC())
}

func (h *schedulerHarness) insertWorkspaceAt(t *testing.T, id WorkspaceID, alias Alias, status Status, createdAt time.Time) Workspace {
	t.Helper()

	row := Workspace{
		ID:           id,
		Namespace:    Namespace("default"),
		Alias:        alias,
		AgentType:    AgentType("claude-code"),
		InfraType:    InfraType("localdir"),
		InfraOptions: infraops.Options{"dir": string(id)},
		Status:       status,
		CreatedAt:    createdAt,
		UpdatedAt:    createdAt,
	}
	if err := h.repo.Insert(context.Background(), row); err != nil {
		t.Fatalf("Insert() error = %v", err)
	}
	return row
}

func TestProbeSchedulerOpensInfraBeforeProbe(t *testing.T) {
	t.Parallel()

	var h *schedulerHarness
	h = newSchedulerHarness(t, schedulerHarnessOptions{
		probeFn: func(_ context.Context, ops infraops.InfraOps) (agent.ProbeResult, error) {
			state := h.registry.stateFor(ops.Dir())
			if state.openCalls.Load() == 0 {
				t.Fatalf("Probe called before Open")
			}
			return agent.ProbeResult{Healthy: true}, nil
		},
	})
	row := h.insertWorkspace(t, "wsp_open_probe", "open-probe", StatusFailed)

	if err := h.scheduler.runTick(context.Background()); err != nil {
		t.Fatalf("runTick() error = %v", err)
	}
	got, err := h.repo.Get(context.Background(), row.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Status != StatusHealthy {
		t.Fatalf("Status = %q, want %q", got.Status, StatusHealthy)
	}
}

func TestProbeSchedulerOpenFailureMarksFailed(t *testing.T) {
	t.Parallel()

	h := newSchedulerHarness(t, schedulerHarnessOptions{})
	row := h.insertWorkspace(t, "wsp_open_boom", "open-boom", StatusHealthy)
	st := h.registry.stateFor(string(row.ID))
	st.openErr = errors.New("open failed")

	if err := h.scheduler.runTick(context.Background()); err != nil {
		t.Fatalf("runTick() error = %v", err)
	}
	got, err := h.repo.Get(context.Background(), row.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Status != StatusFailed {
		t.Fatalf("Status = %q, want %q", got.Status, StatusFailed)
	}
	if got.StatusError == nil || got.StatusError.Phase != "probe" {
		t.Fatalf("StatusError = %#v, want probe failure", got.StatusError)
	}
}

func TestProbeScheduler_WatchdogUsesInitExpectation(t *testing.T) {
	t.Parallel()

	repo := NewMemoryRepository()
	now := time.Date(2026, time.April, 27, 12, 0, 0, 0, time.UTC)
	row := testWorkspace("wsp_watchdog_expect", "default", "wdog", StatusHealthy, now)
	if err := repo.Insert(context.Background(), row); err != nil {
		t.Fatalf("Insert() error = %v", err)
	}

	err := repo.UpdateStatusCAS(
		context.Background(),
		row.ID,
		StatusWriterScheduler,
		StatusInit,
		StatusFailed,
		failureFromError(now, "install", ErrInstallTimeout),
		time.Time{},
	)
	if !errors.Is(err, ErrStatusPreconditionFailed) {
		t.Fatalf("UpdateStatusCAS() error = %v, want ErrStatusPreconditionFailed", err)
	}
}

type casConflictRepo struct {
	*MemoryRepository
	mu sync.Mutex
	n  int
}

func (r *casConflictRepo) UpdateStatusCAS(ctx context.Context, id WorkspaceID, writer StatusWriter, expect Status, next Status, statusErr *Error, lastProbeAt time.Time) error {
	r.mu.Lock()
	r.n++
	n := r.n
	r.mu.Unlock()
	if n == 1 {
		return fmt.Errorf("%w: forced conflict", ErrStatusPreconditionFailed)
	}
	return r.MemoryRepository.UpdateStatusCAS(ctx, id, writer, expect, next, statusErr, lastProbeAt)
}

func TestProbeScheduler_ProbeTransitionCASConflictIsHandled(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.April, 27, 13, 0, 0, 0, time.UTC)
	base := NewMemoryRepository()
	repo := &casConflictRepo{MemoryRepository: base}

	specSet := agent.NewSpecSet()
	spec := &fakeAgentSpec{
		layout: &fakePluginLayout{},
		probeFn: func(_ context.Context, ops infraops.InfraOps) (agent.ProbeResult, error) {
			if ops.Dir() == "wsp_cas_conflict" {
				return agent.ProbeResult{
					Healthy: false,
					Error: &agent.WorkspaceError{
						Code:    "probe.unhealthy",
						Message: "not ready",
						Phase:   "probe",
					},
				}, nil
			}
			return agent.ProbeResult{Healthy: true}, nil
		},
	}
	if err := specSet.Register(spec); err != nil {
		t.Fatalf("Register(spec) error = %v", err)
	}

	registry := newFakeInfraRegistry()
	factories := infraops.NewFactorySet()
	if err := factories.Register(infraops.InfraType("localdir"), registry.factory()); err != nil {
		t.Fatalf("Register(factory) error = %v", err)
	}

	scheduler, err := NewProbeScheduler(ProbeSchedulerConfig{
		Repo:           repo,
		AgentSpecs:     specSet,
		Factories:      factories,
		Clock:          func() time.Time { return now },
		Interval:       time.Hour,
		Timeout:        5 * time.Second,
		InstallTimeout: time.Hour,
	})
	if err != nil {
		t.Fatalf("NewProbeScheduler() error = %v", err)
	}

	row := Workspace{
		ID:           "wsp_cas_conflict",
		Namespace:    Namespace("default"),
		Alias:        "cas-conflict",
		AgentType:    AgentType("claude-code"),
		InfraType:    InfraType("localdir"),
		InfraOptions: infraops.Options{"dir": "wsp_cas_conflict"},
		Status:       StatusHealthy,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := repo.Insert(context.Background(), row); err != nil {
		t.Fatalf("Insert() error = %v", err)
	}

	if err := scheduler.runTick(context.Background()); err != nil {
		t.Fatalf("runTick(1) error = %v", err)
	}
	got1, err := repo.Get(context.Background(), row.ID)
	if err != nil {
		t.Fatalf("Get() after first tick error = %v", err)
	}
	if got1.Status != StatusHealthy {
		t.Fatalf("after first tick status = %q, want healthy (CAS conflict ignored)", got1.Status)
	}

	if err := scheduler.runTick(context.Background()); err != nil {
		t.Fatalf("runTick(2) error = %v", err)
	}
	got2, err := repo.Get(context.Background(), row.ID)
	if err != nil {
		t.Fatalf("Get() after second tick error = %v", err)
	}
	if got2.Status != StatusFailed {
		t.Fatalf("after second tick status = %q, want failed", got2.Status)
	}
}
