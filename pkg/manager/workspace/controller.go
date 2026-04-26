package workspace

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AiRanthem/ANA/pkg/logs"
	"github.com/AiRanthem/ANA/pkg/manager/agent"
	"github.com/AiRanthem/ANA/pkg/manager/infraops"
	"github.com/AiRanthem/ANA/pkg/manager/plugin"
)

const (
	defaultInstallTimeout      = 10 * time.Minute
	defaultInstallWorkers      = 4
	defaultMaxPluginSize       = int64(256 << 20)
	defaultProbeConfirmTimeout = 5 * time.Second
)

var repoRetryBackoff = []time.Duration{250 * time.Millisecond, 750 * time.Millisecond}

// ControllerConfig configures a workspace controller.
type ControllerConfig struct {
	Repo          Repository
	PluginStorage plugin.Storage
	AgentSpecs    *agent.SpecSet
	Factories     *infraops.FactorySet
	Clock         func() time.Time

	InstallTimeout time.Duration
	InstallWorkers int
	MaxPluginSize  int64
	ProbeTimeout   time.Duration
}

// Controller drives workspace install and delete flows.
type Controller struct {
	repo          Repository
	pluginStorage plugin.Storage
	agentSpecs    *agent.SpecSet
	factories     *infraops.FactorySet
	clock         func() time.Time

	installTimeout time.Duration
	installWorkers int
	maxPluginSize  int64
	probeTimeout   time.Duration

	mu         sync.Mutex
	queueCond  *sync.Cond
	queue      []installJob
	started    bool
	stopped    bool
	installCtx context.Context
	cancel     context.CancelFunc

	workersDone      chan struct{}
	workersRemaining atomic.Int64
	inflight         atomic.Int64

	installCancel  map[WorkspaceID]context.CancelFunc
	installRunning map[WorkspaceID]int
}

type installJob struct {
	workspace Workspace
	params    agent.InstallParams
}

// NewController constructs a controller with defaults applied.
func NewController(cfg ControllerConfig) (*Controller, error) {
	if cfg.Repo == nil {
		return nil, errors.New("workspace controller: nil repo")
	}
	if cfg.PluginStorage == nil {
		return nil, errors.New("workspace controller: nil plugin storage")
	}
	if cfg.AgentSpecs == nil {
		return nil, errors.New("workspace controller: nil agent specs")
	}
	if cfg.Factories == nil {
		return nil, errors.New("workspace controller: nil factories")
	}
	if cfg.Clock == nil {
		cfg.Clock = func() time.Time { return time.Now().UTC() }
	}
	if cfg.InstallTimeout <= 0 {
		cfg.InstallTimeout = defaultInstallTimeout
	}
	if cfg.InstallWorkers <= 0 {
		cfg.InstallWorkers = defaultInstallWorkers
	}
	if cfg.MaxPluginSize <= 0 {
		cfg.MaxPluginSize = defaultMaxPluginSize
	}
	if cfg.ProbeTimeout <= 0 {
		cfg.ProbeTimeout = defaultProbeConfirmTimeout
	}

	c := &Controller{
		repo:           cfg.Repo,
		pluginStorage:  cfg.PluginStorage,
		agentSpecs:     cfg.AgentSpecs,
		factories:      cfg.Factories,
		clock:          cfg.Clock,
		installTimeout: cfg.InstallTimeout,
		installWorkers: cfg.InstallWorkers,
		maxPluginSize:  cfg.MaxPluginSize,
		probeTimeout:   cfg.ProbeTimeout,
		installCancel:  make(map[WorkspaceID]context.CancelFunc),
		installRunning: make(map[WorkspaceID]int),
	}
	c.queueCond = sync.NewCond(&c.mu)
	return c, nil
}

// Start launches the install worker pool.
//
// Logging uses [logs.FromContext] on contexts derived from ctx. Callers
// should attach a logger with [logs.IntoContext] on ctx before Start
// (the manager wires this at build time). Parent cancellation is not
// inherited: the install root uses [context.WithoutCancel] so only
// [Controller.Stop] cancels workers.
func (c *Controller) Start(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.stopped {
		return ErrControllerShutdown
	}
	if c.started {
		return nil
	}

	parent := ctx
	if parent == nil {
		parent = context.Background()
	}
	root := context.WithoutCancel(parent)
	c.installCtx, c.cancel = context.WithCancel(root)
	c.workersDone = make(chan struct{})
	c.workersRemaining.Store(int64(c.installWorkers))
	c.started = true

	for i := 0; i < c.installWorkers; i++ {
		go c.worker()
	}
	return nil
}

// Stop cancels in-flight installs and waits for workers to exit.
func (c *Controller) Stop(ctx context.Context) error {
	c.mu.Lock()
	if c.stopped {
		started := c.started
		workersDone := c.workersDone
		c.mu.Unlock()
		if !started {
			return nil
		}
		if workersDone == nil {
			return nil
		}
		select {
		case <-workersDone:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	c.stopped = true
	started := c.started
	cancel := c.cancel
	workersDone := c.workersDone
	c.queueCond.Broadcast()
	c.mu.Unlock()

	if !started {
		return nil
	}

	if cancel != nil {
		cancel()
	}

	select {
	case <-workersDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Submit enqueues a workspace install job and returns immediately.
func (c *Controller) Submit(ctx context.Context, w Workspace, params agent.InstallParams) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.started || c.stopped {
		return ErrControllerShutdown
	}

	c.queue = append(c.queue, installJob{
		workspace: cloneWorkspace(w),
		params:    cloneInstallParams(params),
	})
	c.queueCond.Signal()
	return nil
}

// Delete tears a workspace down synchronously and always attempts repository deletion.
// It drops any queued install jobs for the workspace, cancels an in-flight install for
// that id, then waits until no worker is still running that install before opening infra.
func (c *Controller) Delete(ctx context.Context, id WorkspaceID) error {
	c.mu.Lock()
	stopped := c.stopped
	c.mu.Unlock()
	if stopped {
		return ErrControllerShutdown
	}

	cancelFn := c.takeAndCancelQueuedInstalls(id)
	if cancelFn != nil {
		cancelFn()
	}
	if err := c.waitInstallIdle(ctx, id); err != nil {
		return fmt.Errorf("workspace delete wait install %q: %w", id, err)
	}

	row, err := c.repo.Get(ctx, id)
	if err != nil {
		return err
	}

	factory, ok := c.factories.Get(infraops.InfraType(row.InfraType))
	if !ok {
		return fmt.Errorf("%w: %q", infraops.ErrInfraTypeUnknown, row.InfraType)
	}
	ops, err := factory(ctx, row.InfraOptions)
	if err != nil {
		return fmt.Errorf("workspace delete build infra %q: %w", row.ID, err)
	}

	spec, ok := c.agentSpecs.Get(agent.AgentType(row.AgentType))
	if !ok {
		return fmt.Errorf("%w: %q", agent.ErrAgentTypeUnknown, row.AgentType)
	}

	if err := ops.Open(ctx); err != nil {
		logs.FromContext(ctx).Warn("workspace open before uninstall failed",
			"component", "workspace_controller",
			"workspace_id", row.ID,
			"workspace_alias", row.Alias,
			"namespace", row.Namespace,
			"agent_type", row.AgentType,
			"infra_type", row.InfraType,
			"phase", "open",
			"err", err,
		)
	} else if err := spec.Uninstall(ctx, ops); err != nil {
		logs.FromContext(ctx).Warn("workspace uninstall failed",
			"component", "workspace_controller",
			"workspace_id", row.ID,
			"workspace_alias", row.Alias,
			"namespace", row.Namespace,
			"agent_type", row.AgentType,
			"infra_type", row.InfraType,
			"phase", "uninstall",
			"err", err,
		)
	}
	if err := ops.Clear(ctx); err != nil {
		logs.FromContext(ctx).Warn("workspace clear failed",
			"component", "workspace_controller",
			"workspace_id", row.ID,
			"workspace_alias", row.Alias,
			"namespace", row.Namespace,
			"agent_type", row.AgentType,
			"infra_type", row.InfraType,
			"phase", "clear",
			"err", err,
		)
	}
	return c.repo.Delete(ctx, id)
}

// CountInflight reports the number of installs currently executing.
func (c *Controller) CountInflight() int {
	return int(c.inflight.Load())
}

func (c *Controller) worker() {
	defer func() {
		if c.workersRemaining.Add(-1) == 0 {
			close(c.workersDone)
		}
	}()

	for {
		job, jobCtx, jobCancel, ok := c.dequeue()
		if !ok {
			return
		}
		c.runOneInstall(job, jobCtx, jobCancel)
	}
}

func (c *Controller) runOneInstall(job installJob, jobCtx context.Context, jobCancel context.CancelFunc) {
	c.inflight.Add(1)
	defer c.inflight.Add(-1)
	defer func() {
		jobCancel()
		c.finishTrackedInstall(job.workspace.ID)
	}()

	c.runInstall(job, jobCtx)
}

// takeAndCancelQueuedInstalls removes queued install jobs for id and returns the
// cancel function for an in-flight install, if any (caller should invoke it
// outside the lock). The returned function may be nil.
func (c *Controller) takeAndCancelQueuedInstalls(id WorkspaceID) context.CancelFunc {
	c.mu.Lock()
	defer c.mu.Unlock()

	newQ := c.queue[:0]
	for _, j := range c.queue {
		if j.workspace.ID != id {
			newQ = append(newQ, j)
		}
	}
	c.queue = newQ
	fn := c.installCancel[id]
	c.queueCond.Broadcast()
	return fn
}

func (c *Controller) waitInstallIdle(ctx context.Context, id WorkspaceID) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for c.installRunning[id] > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		c.queueCond.Wait()
	}
	return nil
}

func (c *Controller) finishTrackedInstall(id WorkspaceID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.installRunning[id]--
	if c.installRunning[id] <= 0 {
		delete(c.installRunning, id)
		delete(c.installCancel, id)
	}
	c.queueCond.Broadcast()
}

func (c *Controller) dequeue() (installJob, context.Context, context.CancelFunc, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for len(c.queue) == 0 && !c.stopped {
		c.queueCond.Wait()
	}

	if len(c.queue) == 0 && c.stopped {
		return installJob{}, nil, nil, false
	}

	job := c.queue[0]
	c.queue[0] = installJob{}
	c.queue = c.queue[1:]

	jobCtx, jobCancel := context.WithCancel(c.installCtx)
	if prev := c.installCancel[job.workspace.ID]; prev != nil {
		prev()
	}
	c.installCancel[job.workspace.ID] = jobCancel
	c.installRunning[job.workspace.ID]++

	return job, jobCtx, jobCancel, true
}

func (c *Controller) runInstall(job installJob, parent context.Context) {
	persistCtx := context.Background()

	if err := parent.Err(); err != nil {
		c.transitionToFailed(persistCtx, parent, job.workspace.ID, failureForShutdown(c.clock()), time.Time{})
		return
	}

	installCtx, cancel := context.WithTimeout(parent, c.installTimeout)
	defer cancel()

	factory, ok := c.factories.Get(infraops.InfraType(job.workspace.InfraType))
	if !ok {
		c.transitionToFailed(persistCtx, installCtx, job.workspace.ID, failureFromError(c.clock(), "install", fmt.Errorf("%w: %q", infraops.ErrInfraTypeUnknown, job.workspace.InfraType)), time.Time{})
		return
	}

	ops, err := factory(installCtx, job.workspace.InfraOptions)
	if err != nil {
		c.transitionToFailed(persistCtx, installCtx, job.workspace.ID, c.classifyFailure(installCtx, "install", err), time.Time{})
		return
	}

	if err := ops.Init(installCtx); err != nil {
		c.transitionToFailed(persistCtx, installCtx, job.workspace.ID, c.classifyFailure(installCtx, "install", err), time.Time{})
		return
	}

	spec, ok := c.agentSpecs.Get(agent.AgentType(job.workspace.AgentType))
	if !ok {
		c.transitionToFailed(persistCtx, installCtx, job.workspace.ID, failureFromError(c.clock(), "install", fmt.Errorf("%w: %q", agent.ErrAgentTypeUnknown, job.workspace.AgentType)), time.Time{})
		return
	}

	attachedPlugins, err := c.attachPlugins(installCtx, ops, spec, job.workspace.Plugins)
	if err != nil {
		c.transitionToFailed(persistCtx, installCtx, job.workspace.ID, c.classifyFailure(installCtx, "install", err), time.Time{})
		return
	}

	if err := spec.Install(installCtx, ops, job.params); err != nil {
		c.transitionToFailed(persistCtx, installCtx, job.workspace.ID, c.classifyFailure(installCtx, "install", err), time.Time{})
		return
	}

	probeCtx, probeCancel := context.WithTimeout(installCtx, c.probeTimeout)
	result, err := spec.Probe(probeCtx, ops)
	probeCancel()
	probedAt := c.clock()
	if err != nil {
		c.transitionToFailed(persistCtx, probeCtx, job.workspace.ID, c.classifyFailure(probeCtx, "probe", err), probedAt)
		return
	}
	if !result.Healthy {
		c.transitionToFailed(persistCtx, probeCtx, job.workspace.ID, failureFromProbeResult(c.clock(), result), probedAt)
		return
	}

	updated := cloneWorkspace(job.workspace)
	updated.Plugins = attachedPlugins
	updated.UpdatedAt = c.clock()
	if err := c.retryRepoWrite(persistCtx, installCtx, "persist_attached_plugins", func(ctx context.Context) error {
		return c.repo.Update(ctx, updated)
	}); err != nil {
		c.transitionToFailed(persistCtx, installCtx, job.workspace.ID, failureFromError(c.clock(), "install", fmt.Errorf("persist attached plugins: %w", err)), time.Time{})
		return
	}

	if err := c.retryRepoWrite(persistCtx, installCtx, "update_status_healthy", func(ctx context.Context) error {
		return c.repo.UpdateStatus(ctx, updated.ID, StatusHealthy, nil, probedAt)
	}); err != nil {
		logs.FromContext(installCtx).Error("workspace healthy transition failed",
			"component", "workspace_controller",
			"workspace_id", updated.ID,
			"workspace_alias", updated.Alias,
			"namespace", updated.Namespace,
			"agent_type", updated.AgentType,
			"infra_type", updated.InfraType,
			"phase", "status",
			"err", err,
		)
	}
}

func (c *Controller) attachPlugins(ctx context.Context, ops infraops.InfraOps, spec agent.AgentSpec, plugins []AttachedPlugin) ([]AttachedPlugin, error) {
	if len(plugins) == 0 {
		return nil, nil
	}

	type openedPlugin struct {
		ref    AttachedPlugin
		reader plugin.Reader
	}

	opened := make([]openedPlugin, 0, len(plugins))
	closeReaders := func() {
		for _, o := range opened {
			if o.reader != nil {
				_ = o.reader.Close()
			}
		}
	}

	for _, pluginRef := range plugins {
		body, obj, err := c.pluginStorage.Get(ctx, plugin.PluginID(pluginRef.PluginID))
		if err != nil {
			closeReaders()
			if errors.Is(err, plugin.ErrStorageNotFound) {
				return nil, fmt.Errorf("%w: %q", plugin.ErrPluginNotFound, pluginRef.PluginID)
			}
			return nil, err
		}
		if obj.ContentHash != pluginRef.ContentHash {
			_ = body.Close()
			closeReaders()
			return nil, fmt.Errorf("%w for plugin %q: snapshot %q storage %q", ErrPluginContentHashMismatch, pluginRef.PluginID, pluginRef.ContentHash, obj.ContentHash)
		}

		reader, openErr := plugin.OpenZipReaderFromStream(ctx, body, obj.Size, c.maxPluginSize)
		closeErr := body.Close()
		if openErr != nil {
			closeReaders()
			if closeErr != nil {
				return nil, errors.Join(openErr, closeErr)
			}
			return nil, openErr
		}
		if closeErr != nil {
			_ = reader.Close()
			closeReaders()
			return nil, closeErr
		}

		opened = append(opened, openedPlugin{ref: pluginRef, reader: reader})
	}

	layout := spec.PluginLayout()
	if lk, ok := layout.(agent.PluginLayoutDirectoryKey); ok {
		seen := make(map[string]struct{}, len(opened))
		for _, o := range opened {
			dir, err := lk.PluginDirectoryKey(o.reader.Manifest())
			if err != nil {
				closeReaders()
				return nil, err
			}
			if dir == "" {
				continue
			}
			if _, dup := seen[dir]; dup {
				closeReaders()
				return nil, fmt.Errorf("%w: duplicate plugin layout directory %q", agent.ErrInvalidPluginLayout, dir)
			}
			seen[dir] = struct{}{}
		}
	}

	attached := make([]AttachedPlugin, 0, len(opened))
	for i := range opened {
		o := &opened[i]
		placedPaths, applyErr := layout.Apply(ctx, ops, o.reader.Manifest(), o.reader.FS())
		readerCloseErr := o.reader.Close()
		o.reader = nil
		if applyErr != nil {
			closeReaders()
			if readerCloseErr != nil {
				return nil, errors.Join(applyErr, readerCloseErr)
			}
			return nil, applyErr
		}
		if readerCloseErr != nil {
			closeReaders()
			return nil, readerCloseErr
		}

		attachedPlugin := o.ref
		attachedPlugin.PlacedPaths = slices.Clone(placedPaths)
		attached = append(attached, attachedPlugin)
	}
	return attached, nil
}

func (c *Controller) transitionToFailed(repoCtx, logCtx context.Context, id WorkspaceID, statusErr *Error, lastProbeAt time.Time) {
	if logCtx == nil {
		logCtx = repoCtx
	}
	if err := c.retryRepoWrite(repoCtx, logCtx, "update_status_failed", func(ctx context.Context) error {
		return c.repo.UpdateStatus(ctx, id, StatusFailed, statusErr, lastProbeAt)
	}); err != nil {
		logs.FromContext(logCtx).Error("workspace failed transition failed",
			"component", "workspace_controller",
			"workspace_id", id,
			"phase", "status",
			"err", err,
		)
	}
}

func (c *Controller) retryRepoWrite(repoCtx, logCtx context.Context, op string, fn func(context.Context) error) error {
	if logCtx == nil {
		logCtx = repoCtx
	}
	err := fn(repoCtx)
	if err == nil {
		return nil
	}
	for _, delay := range repoRetryBackoff {
		logs.FromContext(logCtx).Warn("workspace repo write retry",
			"component", "workspace_controller",
			"op", op,
			"delay_ms", delay.Milliseconds(),
			"err", err,
		)
		select {
		case <-time.After(delay):
		case <-repoCtx.Done():
			return repoCtx.Err()
		}
		err = fn(repoCtx)
		if err == nil {
			return nil
		}
	}
	return err
}

func (c *Controller) classifyFailure(ctx context.Context, phase string, err error) *Error {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded), errors.Is(err, context.DeadlineExceeded):
		return failureFromError(c.clock(), phase, fmt.Errorf("%w: %v", ErrInstallTimeout, err))
	case errors.Is(ctx.Err(), context.Canceled), errors.Is(err, context.Canceled):
		return failureForShutdown(c.clock())
	default:
		return failureFromError(c.clock(), phase, err)
	}
}

func failureForShutdown(now time.Time) *Error {
	return &Error{
		Code:       "shutdown",
		Message:    "controller stopped",
		Phase:      "install",
		RecordedAt: now,
	}
}

func failureFromError(now time.Time, phase string, err error) *Error {
	if err == nil {
		return nil
	}
	return &Error{
		Code:       failureCode(phase, err),
		Message:    err.Error(),
		Phase:      phase,
		RecordedAt: now,
	}
}

func failureFromProbeResult(now time.Time, result agent.ProbeResult) *Error {
	if result.Error != nil {
		err := cloneError(result.Error)
		if err.Phase == "" {
			err.Phase = "probe"
		}
		if err.RecordedAt.IsZero() {
			err.RecordedAt = now
		}
		if err.Code == "" {
			err.Code = "probe.unhealthy"
		}
		if err.Message == "" {
			err.Message = "probe reported unhealthy"
		}
		return err
	}
	return &Error{
		Code:       "probe.unhealthy",
		Message:    "probe reported unhealthy",
		Phase:      "probe",
		RecordedAt: now,
	}
}

func failureCode(phase string, err error) string {
	switch {
	case errors.Is(err, ErrInstallTimeout):
		return "install.timeout"
	case errors.Is(err, plugin.ErrPluginNotFound), errors.Is(err, plugin.ErrStorageNotFound):
		return phase + ".plugin_not_found"
	case errors.Is(err, ErrPluginContentHashMismatch):
		return phase + ".plugin_hash_mismatch"
	case errors.Is(err, infraops.ErrInfraTypeUnknown):
		return phase + ".infra_type_unknown"
	case errors.Is(err, agent.ErrAgentTypeUnknown):
		return phase + ".agent_type_unknown"
	default:
		return phase + ".error"
	}
}

func cloneInstallParams(params agent.InstallParams) agent.InstallParams {
	params.UserParams = cloneMapAny(params.UserParams)
	params.Workspace.Plugins = slices.Clone(params.Workspace.Plugins)
	return params
}
