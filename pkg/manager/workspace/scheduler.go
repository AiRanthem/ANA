package workspace

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AiRanthem/ANA/pkg/logs"
	"github.com/AiRanthem/ANA/pkg/manager/agent"
	"github.com/AiRanthem/ANA/pkg/manager/infraops"
)

const (
	defaultProbeInterval = 30 * time.Second
	defaultProbeWorkers  = 4
	defaultProbeTimeout  = 5 * time.Second
	listPageSize         = 100
)

// ProbeSchedulerConfig configures a probe scheduler.
type ProbeSchedulerConfig struct {
	Repo       Repository
	AgentSpecs *agent.SpecSet
	Factories  *infraops.FactorySet
	Clock      func() time.Time

	Interval       time.Duration
	Workers        int
	Timeout        time.Duration
	InstallTimeout time.Duration
}

// ProbeStats exposes scheduler counters for tests and metrics.
type ProbeStats struct {
	TicksRun        uint64
	ProbesAttempted uint64
	ProbesHealthy   uint64
	ProbesFailed    uint64
	Overruns        uint64
}

// ProbeScheduler runs periodic health probes for workspaces.
type ProbeScheduler struct {
	repo       Repository
	agentSpecs *agent.SpecSet
	factories  *infraops.FactorySet
	clock      func() time.Time

	interval       time.Duration
	workers        int
	timeout        time.Duration
	installTimeout time.Duration

	mu       sync.Mutex
	started  bool
	stopped  bool
	runCtx   context.Context
	cancel   context.CancelFunc
	loopDone chan struct{}

	ticksRun        atomic.Uint64
	probesAttempted atomic.Uint64
	probesHealthy   atomic.Uint64
	probesFailed    atomic.Uint64
	overruns        atomic.Uint64
}

// NewProbeScheduler constructs a probe scheduler with defaults applied.
func NewProbeScheduler(cfg ProbeSchedulerConfig) (*ProbeScheduler, error) {
	if cfg.Repo == nil {
		return nil, errors.New("workspace scheduler: nil repo")
	}
	if cfg.AgentSpecs == nil {
		return nil, errors.New("workspace scheduler: nil agent specs")
	}
	if cfg.Factories == nil {
		return nil, errors.New("workspace scheduler: nil factories")
	}
	if cfg.Clock == nil {
		cfg.Clock = func() time.Time { return time.Now().UTC() }
	}
	if cfg.Interval <= 0 {
		cfg.Interval = defaultProbeInterval
	}
	if cfg.Workers <= 0 {
		cfg.Workers = defaultProbeWorkers
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultProbeTimeout
	}
	if cfg.InstallTimeout <= 0 {
		cfg.InstallTimeout = defaultInstallTimeout
	}

	return &ProbeScheduler{
		repo:           cfg.Repo,
		agentSpecs:     cfg.AgentSpecs,
		factories:      cfg.Factories,
		clock:          cfg.Clock,
		interval:       cfg.Interval,
		workers:        cfg.Workers,
		timeout:        cfg.Timeout,
		installTimeout: cfg.InstallTimeout,
	}, nil
}

// Start launches the periodic scheduler loop.
//
// Logging uses [logs.FromContext] on contexts derived from ctx. Callers
// should attach a logger with [logs.IntoContext] on ctx before Start.
// Parent cancellation is not inherited ([context.WithoutCancel]); only
// [ProbeScheduler.Stop] cancels the loop.
func (s *ProbeScheduler) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.stopped {
		return ErrSchedulerShutdown
	}
	if s.started {
		return nil
	}

	parent := ctx
	if parent == nil {
		parent = context.Background()
	}
	root := context.WithoutCancel(parent)
	s.runCtx, s.cancel = context.WithCancel(root)
	s.loopDone = make(chan struct{})
	s.started = true
	go s.loop()
	return nil
}

// Stop cancels the scheduler loop and waits for it to exit.
func (s *ProbeScheduler) Stop(ctx context.Context) error {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return ErrSchedulerShutdown
	}
	s.stopped = true
	started := s.started
	cancel := s.cancel
	loopDone := s.loopDone
	s.mu.Unlock()

	if !started {
		return nil
	}
	if cancel != nil {
		cancel()
	}

	select {
	case <-loopDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Stats returns a snapshot of scheduler counters.
func (s *ProbeScheduler) Stats() ProbeStats {
	return ProbeStats{
		TicksRun:        s.ticksRun.Load(),
		ProbesAttempted: s.probesAttempted.Load(),
		ProbesHealthy:   s.probesHealthy.Load(),
		ProbesFailed:    s.probesFailed.Load(),
		Overruns:        s.overruns.Load(),
	}
}

func (s *ProbeScheduler) loop() {
	defer close(s.loopDone)

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		select {
		case <-s.runCtx.Done():
			return
		case <-ticker.C:
			if err := s.runTick(s.runCtx); err != nil && !errors.Is(err, context.Canceled) {
				logs.FromContext(s.runCtx).Error("workspace probe tick failed",
					"component", "workspace_scheduler",
					"err", err,
				)
			}

			select {
			case <-ticker.C:
				s.overruns.Add(1)
				logs.FromContext(s.runCtx).Warn("probe_overrun",
					"component", "workspace_scheduler",
					"interval", s.interval.String(),
				)
			default:
			}
		}
	}
}

func (s *ProbeScheduler) runTick(ctx context.Context) error {
	s.ticksRun.Add(1)

	rows, err := s.listAll(ctx)
	if err != nil {
		return err
	}

	now := s.clock()
	seen := make(map[WorkspaceID]struct{}, len(rows))
	probeRows := make([]Workspace, 0, len(rows))

	for _, row := range rows {
		if _, ok := seen[row.ID]; ok {
			continue
		}
		seen[row.ID] = struct{}{}

		switch row.Status {
		case StatusInit:
			if s.installTimedOut(now, row) {
				if err := s.repo.UpdateStatus(context.Background(), row.ID, StatusFailed, failureFromError(now, "install", ErrInstallTimeout), time.Time{}); err != nil {
					logs.FromContext(ctx).Error("workspace init timeout transition failed",
						"component", "workspace_scheduler",
						"workspace_id", row.ID,
						"phase", "status",
						"err", err,
					)
				}
			}
		case StatusHealthy, StatusFailed:
			probeRows = append(probeRows, row)
		}
	}

	if len(probeRows) == 0 {
		return nil
	}

	jobs := make(chan Workspace)
	var wg sync.WaitGroup

	workerCount := s.workers
	if workerCount > len(probeRows) {
		workerCount = len(probeRows)
	}
	if workerCount == 0 {
		return nil
	}

	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for row := range jobs {
				s.probeOne(ctx, row)
			}
		}()
	}

	for _, row := range probeRows {
		select {
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return ctx.Err()
		case jobs <- row:
		}
	}

	close(jobs)
	wg.Wait()
	return nil
}

func (s *ProbeScheduler) listAll(ctx context.Context) ([]Workspace, error) {
	var (
		cursor string
		rows   []Workspace
	)

	for {
		page, next, err := s.repo.List(ctx, ListOptions{
			Limit:  listPageSize,
			Cursor: cursor,
		})
		if err != nil {
			return nil, err
		}
		rows = append(rows, page...)
		if next == "" {
			return rows, nil
		}
		cursor = next
	}
}

func (s *ProbeScheduler) installTimedOut(now time.Time, row Workspace) bool {
	if row.Status != StatusInit || s.installTimeout <= 0 {
		return false
	}
	return now.Sub(row.CreatedAt) > s.installTimeout
}

func (s *ProbeScheduler) probeOne(ctx context.Context, row Workspace) {
	s.probesAttempted.Add(1)

	factory, ok := s.factories.Get(infraops.InfraType(row.InfraType))
	if !ok {
		s.recordProbeOutcome(ctx, row, StatusFailed, failureFromError(s.clock(), "probe", fmt.Errorf("%w: %q", infraops.ErrInfraTypeUnknown, row.InfraType)), s.clock())
		s.probesFailed.Add(1)
		return
	}

	probeCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	ops, err := factory(probeCtx, row.InfraOptions)
	if err != nil {
		if probeCtx.Err() != nil && ctx.Err() != nil {
			return
		}
		s.recordProbeOutcome(ctx, row, StatusFailed, failureFromError(s.clock(), "probe", err), s.clock())
		s.probesFailed.Add(1)
		return
	}
	if err := ops.Open(probeCtx); err != nil {
		if probeCtx.Err() != nil && ctx.Err() != nil {
			return
		}
		s.recordProbeOutcome(ctx, row, StatusFailed, failureFromError(s.clock(), "probe", err), s.clock())
		s.probesFailed.Add(1)
		return
	}

	spec, ok := s.agentSpecs.Get(agent.AgentType(row.AgentType))
	if !ok {
		s.recordProbeOutcome(ctx, row, StatusFailed, failureFromError(s.clock(), "probe", fmt.Errorf("%w: %q", agent.ErrAgentTypeUnknown, row.AgentType)), s.clock())
		s.probesFailed.Add(1)
		return
	}

	probedAt := s.clock()
	result, err := spec.Probe(probeCtx, ops)
	if probeCtx.Err() != nil && errors.Is(probeCtx.Err(), context.Canceled) {
		return
	}
	if errors.Is(err, context.Canceled) && ctx.Err() != nil {
		return
	}

	if err == nil && result.Healthy {
		s.recordProbeOutcome(ctx, row, StatusHealthy, nil, probedAt)
		s.probesHealthy.Add(1)
		return
	}

	var statusErr *Error
	if err != nil {
		statusErr = failureFromError(s.clock(), "probe", err)
	} else {
		statusErr = failureFromProbeResult(s.clock(), result)
	}
	s.recordProbeOutcome(ctx, row, StatusFailed, statusErr, probedAt)
	s.probesFailed.Add(1)
}

func (s *ProbeScheduler) recordProbeOutcome(logCtx context.Context, row Workspace, newStatus Status, statusErr *Error, probedAt time.Time) {
	if row.Status != newStatus {
		if err := s.repo.UpdateStatus(context.Background(), row.ID, newStatus, statusErr, probedAt); err != nil {
			logs.FromContext(logCtx).Error("workspace probe transition failed",
				"component", "workspace_scheduler",
				"workspace_id", row.ID,
				"phase", "status",
				"err", err,
			)
		}
		return
	}

	updated := cloneWorkspace(row)
	updated.LastProbeAt = probedAt
	updated.UpdatedAt = s.clock()
	if newStatus == StatusHealthy {
		updated.StatusError = nil
	} else {
		updated.StatusError = cloneError(statusErr)
	}
	if err := s.repo.Update(context.Background(), updated); err != nil {
		logs.FromContext(logCtx).Error("workspace probe metadata update failed",
			"component", "workspace_scheduler",
			"workspace_id", row.ID,
			"phase", "update",
			"err", err,
		)
	}
}
