package manager

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/AiRanthem/ANA/pkg/logs"
	"github.com/AiRanthem/ANA/pkg/manager/agent"
	"github.com/AiRanthem/ANA/pkg/manager/infraops"
	"github.com/AiRanthem/ANA/pkg/manager/plugin"
	"github.com/AiRanthem/ANA/pkg/manager/workspace"
)

const (
	opBuild                  = "manager build"
	opCreatePlugin           = "manager create_plugin"
	opGetPlugin              = "manager get_plugin"
	opGetPluginByName        = "manager get_plugin_by_name"
	opListPlugins            = "manager list_plugins"
	opDeletePlugin           = "manager delete_plugin"
	opGetPluginDownloadURL   = "manager get_plugin_download_url"
	opNewInfraOps            = "manager new_infra_ops"
	opCreateWorkspace        = "manager create_workspace"
	opGetWorkspace           = "manager get_workspace"
	opGetWorkspaceByAlias    = "manager get_workspace_by_alias"
	opListWorkspaces         = "manager list_workspaces"
	opDeleteWorkspace        = "manager delete_workspace"
	opStart                  = "manager start"
	opStop                   = "manager stop"
	defaultPluginUploadLimit = int64(64 << 20)
)

var errBuilderConsumed = errors.New("manager: builder already used")

// Manager is the public facade for workspace lifecycle operations.
type Manager interface {
	CreatePlugin(ctx context.Context, req CreatePluginRequest) (Plugin, error)
	GetPlugin(ctx context.Context, id PluginID) (Plugin, error)
	GetPluginByName(ctx context.Context, namespace Namespace, name string) (Plugin, error)
	ListPlugins(ctx context.Context, opts ListPluginsOptions) (rows []Plugin, nextCursor string, err error)
	DeletePlugin(ctx context.Context, id PluginID) error
	GetPluginDownloadURL(ctx context.Context, id PluginID, opts DownloadURLOptions) (string, error)

	NewInfraOps(ctx context.Context, infraType InfraType, options infraops.Options) (infraops.InfraOps, error)
	InfraTypes() []InfraType

	CreateWorkspace(ctx context.Context, req CreateWorkspaceRequest) (Workspace, error)
	GetWorkspace(ctx context.Context, id WorkspaceID) (Workspace, error)
	GetWorkspaceByAlias(ctx context.Context, namespace Namespace, alias Alias) (Workspace, error)
	ListWorkspaces(ctx context.Context, opts ListWorkspacesOptions) (rows []Workspace, nextCursor string, err error)
	DeleteWorkspace(ctx context.Context, id WorkspaceID) error
	AgentTypes() []AgentType

	Start(ctx context.Context) error
	Stop(ctx context.Context) error
}

// Builder wires the manager's backends and registered extension points.
type Builder struct {
	PluginRepository    plugin.Repository
	PluginStorage       plugin.Storage
	WorkspaceRepository workspace.Repository

	Clock       func() time.Time
	IDGenerator IDGenerator
	Logger      logs.Logger

	InstallTimeout time.Duration
	InstallWorkers int

	ProbeInterval time.Duration
	ProbeWorkers  int
	ProbeTimeout  time.Duration

	agentSpecs     []agent.AgentSpec
	infraFactories []registeredInfraFactory
	built          bool
}

type registeredInfraFactory struct {
	infraType InfraType
	factory   infraops.Factory
}

// NewBuilder constructs an empty builder.
func NewBuilder() *Builder {
	return &Builder{}
}

// RegisterAgentSpec queues an agent spec for build-time registration.
func (b *Builder) RegisterAgentSpec(spec agent.AgentSpec) *Builder {
	if b == nil {
		return nil
	}
	b.agentSpecs = append(b.agentSpecs, spec)
	return b
}

// RegisterInfraType queues an infra factory for build-time registration.
func (b *Builder) RegisterInfraType(infraType InfraType, factory infraops.Factory) *Builder {
	if b == nil {
		return nil
	}
	b.infraFactories = append(b.infraFactories, registeredInfraFactory{
		infraType: infraType,
		factory:   factory,
	})
	return b
}

// Build validates the wiring and constructs a manager instance.
func (b *Builder) Build(ctx context.Context) (Manager, error) {
	if b == nil {
		return nil, fmt.Errorf("%s: nil builder", opBuild)
	}
	if b.built {
		return nil, fmt.Errorf("%s: %w", opBuild, errBuilderConsumed)
	}
	b.built = true

	if b.PluginRepository == nil {
		return nil, fmt.Errorf("%s: nil plugin repository", opBuild)
	}
	if b.PluginStorage == nil {
		return nil, fmt.Errorf("%s: nil plugin storage", opBuild)
	}
	if b.WorkspaceRepository == nil {
		return nil, fmt.Errorf("%s: nil workspace repository", opBuild)
	}

	if ctx == nil {
		ctx = context.Background()
	}

	clock := b.Clock
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	idGenerator := b.IDGenerator
	if idGenerator == nil {
		idGenerator = randomIDGenerator{}
	}
	logger := b.Logger
	if logger == nil {
		logger = logs.NewSlog(slog.New(slog.NewTextHandler(io.Discard, nil)))
	}
	ctx = logs.IntoContext(ctx, logger)

	specs := agent.NewSpecSet()
	for _, spec := range b.agentSpecs {
		if err := specs.Register(spec); err != nil {
			return nil, fmt.Errorf("%s: %w", opBuild, err)
		}
	}

	factories := infraops.NewFactorySet()
	for _, registration := range b.infraFactories {
		if registration.factory == nil {
			return nil, fmt.Errorf("%s: nil infra factory for %q", opBuild, registration.infraType)
		}
		if err := factories.Register(infraops.InfraType(registration.infraType), registration.factory); err != nil {
			return nil, fmt.Errorf("%s: %w", opBuild, err)
		}
	}

	if specs.Len() == 0 {
		logs.FromContext(ctx).Warn("manager built with no registered agent specs",
			"op", opBuild,
			"component", "manager",
		)
	}
	if len(factories.Types()) == 0 {
		logs.FromContext(ctx).Warn("manager built with no registered infra factories",
			"op", opBuild,
			"component", "manager",
		)
	}

	controller, err := workspace.NewController(workspace.ControllerConfig{
		Repo:           b.WorkspaceRepository,
		PluginStorage:  b.PluginStorage,
		AgentSpecs:     specs,
		Factories:      factories,
		Clock:          clock,
		InstallTimeout: b.InstallTimeout,
		InstallWorkers: b.InstallWorkers,
		ProbeTimeout:   b.ProbeTimeout,
	})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", opBuild, err)
	}
	if err := controller.Start(ctx); err != nil {
		return nil, fmt.Errorf("%s: %w", opBuild, err)
	}

	scheduler, err := workspace.NewProbeScheduler(workspace.ProbeSchedulerConfig{
		Repo:           b.WorkspaceRepository,
		AgentSpecs:     specs,
		Factories:      factories,
		Clock:          clock,
		Interval:       b.ProbeInterval,
		Workers:        b.ProbeWorkers,
		Timeout:        b.ProbeTimeout,
		InstallTimeout: b.InstallTimeout,
	})
	if err != nil {
		_ = controller.Stop(ctx)
		return nil, fmt.Errorf("%s: %w", opBuild, err)
	}

	return &managerFacade{
		pluginRepository:    b.PluginRepository,
		pluginStorage:       b.PluginStorage,
		workspaceRepository: b.WorkspaceRepository,
		clock:               clock,
		idGenerator:         idGenerator,
		agentSpecs:          specs,
		infraFactories:      factories,
		controller:          controller,
		scheduler:           scheduler,
	}, nil
}

type managerFacade struct {
	pluginRepository    plugin.Repository
	pluginStorage       plugin.Storage
	workspaceRepository workspace.Repository
	clock               func() time.Time
	idGenerator         IDGenerator
	agentSpecs          *agent.SpecSet
	infraFactories      *infraops.FactorySet
	controller          *workspace.Controller
	scheduler           *workspace.ProbeScheduler

	mu             sync.RWMutex
	stopInProgress bool
	shutDown       bool
}

var _ Manager = (*managerFacade)(nil)

func (m *managerFacade) CreatePlugin(ctx context.Context, req CreatePluginRequest) (Plugin, error) {
	if err := m.ensureAvailable(opCreatePlugin); err != nil {
		return Plugin{}, err
	}
	if err := ctx.Err(); err != nil {
		return Plugin{}, fmt.Errorf("%s: %w", opCreatePlugin, err)
	}

	namespace := normalizeNamespace(req.Namespace)
	if err := validateNamespace(namespace); err != nil {
		return Plugin{}, fmt.Errorf("%s: %w", opCreatePlugin, err)
	}
	if req.Content == nil {
		return Plugin{}, fmt.Errorf("%s: nil content", opCreatePlugin)
	}

	body, err := readPluginBody(ctx, req.Content)
	if err != nil {
		return Plugin{}, fmt.Errorf("%s: %w", opCreatePlugin, err)
	}

	reader, err := plugin.OpenZipReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return Plugin{}, fmt.Errorf("%s: %w", opCreatePlugin, err)
	}
	manifest := reader.Manifest()
	if closeErr := reader.Close(); closeErr != nil {
		return Plugin{}, fmt.Errorf("%s: %w", opCreatePlugin, closeErr)
	}

	name := req.Name
	if name == "" {
		name = manifest.Plugin.Name
	}
	if name != manifest.Plugin.Name {
		return Plugin{}, fmt.Errorf("%s: plugin name %q does not match manifest %q", opCreatePlugin, name, manifest.Plugin.Name)
	}

	description := req.Description
	if description == "" {
		description = manifest.Plugin.Description
	}
	if err := validateDescription(description); err != nil {
		return Plugin{}, fmt.Errorf("%s: %w", opCreatePlugin, err)
	}

	existing, err := m.pluginRepository.GetByName(ctx, plugin.Namespace(namespace), name)
	overwrite := false
	switch {
	case err == nil:
		overwrite = true
	case errors.Is(err, plugin.ErrPluginNotFound):
		err = nil
	default:
		return Plugin{}, fmt.Errorf("%s: %w", opCreatePlugin, err)
	}

	id := m.idGenerator.PluginID()
	createdAt := m.clock()
	if overwrite {
		id = PluginID(existing.ID)
		createdAt = existing.CreatedAt
	}

	var oldBody []byte
	if overwrite {
		oldReader, _, err := m.pluginStorage.Get(ctx, plugin.PluginID(id))
		if err != nil {
			return Plugin{}, fmt.Errorf("%s: read previous plugin blob %q: %w", opCreatePlugin, id, err)
		}
		oldBody, err = readAllAndClose(oldReader)
		if err != nil {
			return Plugin{}, fmt.Errorf("%s: buffer previous plugin blob %q: %w", opCreatePlugin, id, err)
		}
	}

	stored, err := m.pluginStorage.Put(ctx, plugin.PluginID(id), bytes.NewReader(body))
	if err != nil {
		return Plugin{}, fmt.Errorf("%s: %w", opCreatePlugin, err)
	}

	row := plugin.Plugin{
		ID:          plugin.PluginID(id),
		Namespace:   plugin.Namespace(namespace),
		Name:        name,
		Description: description,
		Manifest:    cloneManifest(manifest),
		ContentHash: stored.ContentHash,
		Size:        stored.Size,
		CreatedAt:   createdAt,
		UpdatedAt:   m.clock(),
	}

	if overwrite {
		if err := m.pluginRepository.Update(ctx, row); err != nil {
			rollbackErr := m.restorePluginBlob(context.Background(), row.ID, oldBody)
			if rollbackErr != nil {
				logs.FromContext(ctx).Error("plugin overwrite rollback failed",
					"op", opCreatePlugin,
					"component", "manager.plugin",
					"plugin_id", row.ID,
					"err", rollbackErr,
				)
				return Plugin{}, fmt.Errorf("%s: %w", opCreatePlugin, errors.Join(err, rollbackErr))
			}
			return Plugin{}, fmt.Errorf("%s: %w", opCreatePlugin, err)
		}
	} else {
		if err := m.pluginRepository.Insert(ctx, row); err != nil {
			if deleteErr := m.pluginStorage.Delete(context.Background(), row.ID); deleteErr != nil {
				logs.FromContext(ctx).Warn("plugin upload cleanup failed",
					"op", opCreatePlugin,
					"component", "manager.plugin",
					"plugin_id", row.ID,
					"err", deleteErr,
				)
			}
			return Plugin{}, fmt.Errorf("%s: %w", opCreatePlugin, err)
		}
	}

	return pluginFromRow(row), nil
}

func (m *managerFacade) GetPlugin(ctx context.Context, id PluginID) (Plugin, error) {
	if err := m.ensureAvailable(opGetPlugin); err != nil {
		return Plugin{}, err
	}

	row, err := m.pluginRepository.Get(ctx, plugin.PluginID(id))
	if err != nil {
		return Plugin{}, fmt.Errorf("%s: %w", opGetPlugin, err)
	}
	return pluginFromRow(row), nil
}

func (m *managerFacade) GetPluginByName(ctx context.Context, namespace Namespace, name string) (Plugin, error) {
	if err := m.ensureAvailable(opGetPluginByName); err != nil {
		return Plugin{}, err
	}

	namespace = normalizeNamespace(namespace)
	if err := validateNamespace(namespace); err != nil {
		return Plugin{}, fmt.Errorf("%s: %w", opGetPluginByName, err)
	}

	row, err := m.pluginRepository.GetByName(ctx, plugin.Namespace(namespace), name)
	if err != nil {
		return Plugin{}, fmt.Errorf("%s: %w", opGetPluginByName, err)
	}
	return pluginFromRow(row), nil
}

func (m *managerFacade) ListPlugins(ctx context.Context, opts ListPluginsOptions) ([]Plugin, string, error) {
	if err := m.ensureAvailable(opListPlugins); err != nil {
		return nil, "", err
	}

	namespace := opts.Namespace
	if namespace != "" {
		namespace = normalizeNamespace(namespace)
		if err := validateNamespace(namespace); err != nil {
			return nil, "", fmt.Errorf("%s: %w", opListPlugins, err)
		}
	}

	rows, next, err := m.pluginRepository.List(ctx, plugin.ListOptions{
		Namespace: plugin.Namespace(namespace),
		NameLike:  opts.NameLike,
		Limit:     opts.Limit,
		Cursor:    opts.Cursor,
	})
	if err != nil {
		return nil, "", fmt.Errorf("%s: %w", opListPlugins, err)
	}

	out := make([]Plugin, 0, len(rows))
	for _, row := range rows {
		out = append(out, pluginFromRow(row))
	}
	return out, next, nil
}

func (m *managerFacade) DeletePlugin(ctx context.Context, id PluginID) error {
	if err := m.ensureAvailable(opDeletePlugin); err != nil {
		return err
	}

	row, err := m.pluginRepository.Get(ctx, plugin.PluginID(id))
	if err != nil {
		return fmt.Errorf("%s: %w", opDeletePlugin, err)
	}
	if err := m.pluginStorage.Delete(ctx, row.ID); err != nil {
		return fmt.Errorf("%s: %w", opDeletePlugin, err)
	}
	if err := m.pluginRepository.Delete(ctx, row.ID); err != nil {
		return fmt.Errorf("%s: %w", opDeletePlugin, err)
	}
	return nil
}

func (m *managerFacade) GetPluginDownloadURL(ctx context.Context, id PluginID, opts DownloadURLOptions) (string, error) {
	if err := m.ensureAvailable(opGetPluginDownloadURL); err != nil {
		return "", err
	}

	if _, err := m.pluginRepository.Get(ctx, plugin.PluginID(id)); err != nil {
		return "", fmt.Errorf("%s: %w", opGetPluginDownloadURL, err)
	}

	url, err := m.pluginStorage.PresignURL(ctx, plugin.PluginID(id), plugin.PresignOptions{
		TTL:    opts.TTL,
		Method: httpMethodGet,
	})
	if err != nil {
		if errors.Is(err, plugin.ErrUnsupported) {
			return "", fmt.Errorf("%s: %w", opGetPluginDownloadURL, ErrUnsupportedDownloadURL)
		}
		return "", fmt.Errorf("%s: %w", opGetPluginDownloadURL, err)
	}
	return url, nil
}

func (m *managerFacade) NewInfraOps(ctx context.Context, infraType InfraType, options infraops.Options) (infraops.InfraOps, error) {
	if err := m.ensureAvailable(opNewInfraOps); err != nil {
		return nil, err
	}

	factory, ok := m.infraFactories.Get(infraops.InfraType(infraType))
	if !ok {
		return nil, fmt.Errorf("%s: %w: %q", opNewInfraOps, ErrInfraTypeUnknown, infraType)
	}

	rawDir, _ := options["dir"]
	dir, _ := rawDir.(string)
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("%s: infra dir option must be non-empty: %w", opNewInfraOps, infraops.ErrInvalidOption)
	}

	ops, err := factory(ctx, cloneMapAny(options))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", opNewInfraOps, err)
	}
	return ops, nil
}

func (m *managerFacade) InfraTypes() []InfraType {
	types := m.infraFactories.Types()
	out := make([]InfraType, 0, len(types))
	for _, infraType := range types {
		out = append(out, InfraType(infraType))
	}
	return out
}

func (m *managerFacade) CreateWorkspace(ctx context.Context, req CreateWorkspaceRequest) (Workspace, error) {
	if err := m.ensureAvailable(opCreateWorkspace); err != nil {
		return Workspace{}, err
	}
	if err := ctx.Err(); err != nil {
		return Workspace{}, fmt.Errorf("%s: %w", opCreateWorkspace, err)
	}

	namespace := normalizeNamespace(req.Namespace)
	if err := validateNamespace(namespace); err != nil {
		return Workspace{}, fmt.Errorf("%s: %w", opCreateWorkspace, err)
	}
	if err := validateAlias(req.Alias); err != nil {
		return Workspace{}, fmt.Errorf("%s: %w", opCreateWorkspace, err)
	}
	if err := validateDescription(req.Description); err != nil {
		return Workspace{}, fmt.Errorf("%s: %w", opCreateWorkspace, err)
	}

	if _, ok := m.agentSpecs.Get(agent.AgentType(req.AgentType)); !ok {
		return Workspace{}, fmt.Errorf("%s: %w: %q", opCreateWorkspace, ErrAgentTypeUnknown, req.AgentType)
	}
	if _, ok := m.infraFactories.Get(infraops.InfraType(req.InfraType)); !ok {
		return Workspace{}, fmt.Errorf("%s: %w: %q", opCreateWorkspace, ErrInfraTypeUnknown, req.InfraType)
	}

	attached, refs, err := m.resolveAttachedPlugins(ctx, req.Plugins)
	if err != nil {
		return Workspace{}, fmt.Errorf("%s: %w", opCreateWorkspace, err)
	}

	now := m.clock()
	row := workspace.Workspace{
		ID:            workspace.WorkspaceID(m.idGenerator.WorkspaceID()),
		Namespace:     workspace.Namespace(namespace),
		Alias:         workspace.Alias(req.Alias),
		AgentType:     workspace.AgentType(req.AgentType),
		InfraType:     workspace.InfraType(req.InfraType),
		InfraOptions:  cloneMapAny(req.InfraOptions),
		InstallParams: cloneMapAny(req.InstallParams),
		Plugins:       attached,
		Status:        workspace.StatusInit,
		Description:   req.Description,
		Labels:        cloneLabels(req.Labels),
		CreatedAt:     now,
		UpdatedAt:     now,
	}

	if err := m.workspaceRepository.Insert(ctx, row); err != nil {
		return Workspace{}, fmt.Errorf("%s: %w", opCreateWorkspace, err)
	}

	params := agent.InstallParams{
		Workspace: agent.WorkspaceSummary{
			ID:        agent.WorkspaceID(row.ID),
			Namespace: agent.Namespace(row.Namespace),
			Alias:     agent.Alias(row.Alias),
			AgentType: agent.AgentType(row.AgentType),
			InfraType: agent.InfraType(row.InfraType),
			Plugins:   refs,
		},
		UserParams: cloneMapAny(req.InstallParams),
	}

	if err := m.controller.Submit(context.Background(), row, params); err != nil {
		statusErr := newWorkspaceError(m.clock(), "install.error", "install", err.Error())
		if errors.Is(err, workspace.ErrControllerShutdown) {
			statusErr = newWorkspaceError(m.clock(), "shutdown", "install", err.Error())
		}
		if updateErr := m.workspaceRepository.UpdateStatus(context.Background(), row.ID, workspace.StatusFailed, workspaceErrorToRow(statusErr), time.Time{}); updateErr != nil {
			logs.FromContext(ctx).Error("workspace submission failure transition failed",
				"op", opCreateWorkspace,
				"component", "manager.workspace",
				"workspace_id", row.ID,
				"workspace_alias", row.Alias,
				"namespace", row.Namespace,
				"agent_type", row.AgentType,
				"infra_type", row.InfraType,
				"err", updateErr,
			)
		}
		return Workspace{}, fmt.Errorf("%s: %w", opCreateWorkspace, err)
	}

	return workspaceFromRow(row), nil
}

func (m *managerFacade) GetWorkspace(ctx context.Context, id WorkspaceID) (Workspace, error) {
	if err := m.ensureAvailable(opGetWorkspace); err != nil {
		return Workspace{}, err
	}

	row, err := m.workspaceRepository.Get(ctx, workspace.WorkspaceID(id))
	if err != nil {
		return Workspace{}, fmt.Errorf("%s: %w", opGetWorkspace, err)
	}
	return workspaceFromRow(row), nil
}

func (m *managerFacade) GetWorkspaceByAlias(ctx context.Context, namespace Namespace, alias Alias) (Workspace, error) {
	if err := m.ensureAvailable(opGetWorkspaceByAlias); err != nil {
		return Workspace{}, err
	}

	namespace = normalizeNamespace(namespace)
	if err := validateNamespace(namespace); err != nil {
		return Workspace{}, fmt.Errorf("%s: %w", opGetWorkspaceByAlias, err)
	}
	if err := validateAlias(alias); err != nil {
		return Workspace{}, fmt.Errorf("%s: %w", opGetWorkspaceByAlias, err)
	}

	row, err := m.workspaceRepository.GetByAlias(ctx, workspace.Namespace(namespace), workspace.Alias(alias))
	if err != nil {
		return Workspace{}, fmt.Errorf("%s: %w", opGetWorkspaceByAlias, err)
	}
	return workspaceFromRow(row), nil
}

func (m *managerFacade) ListWorkspaces(ctx context.Context, opts ListWorkspacesOptions) ([]Workspace, string, error) {
	if err := m.ensureAvailable(opListWorkspaces); err != nil {
		return nil, "", err
	}

	namespace := opts.Namespace
	if namespace != "" {
		namespace = normalizeNamespace(namespace)
		if err := validateNamespace(namespace); err != nil {
			return nil, "", fmt.Errorf("%s: %w", opListWorkspaces, err)
		}
	}

	rows, next, err := m.workspaceRepository.List(ctx, workspace.ListOptions{
		Namespace: workspace.Namespace(namespace),
		AgentType: workspace.AgentType(opts.AgentType),
		InfraType: workspace.InfraType(opts.InfraType),
		Status:    workspace.Status(opts.Status),
		Labels:    cloneLabels(opts.Labels),
		Limit:     opts.Limit,
		Cursor:    opts.Cursor,
	})
	if err != nil {
		return nil, "", fmt.Errorf("%s: %w", opListWorkspaces, err)
	}

	out := make([]Workspace, 0, len(rows))
	for _, row := range rows {
		out = append(out, workspaceFromRow(row))
	}
	return out, next, nil
}

func (m *managerFacade) DeleteWorkspace(ctx context.Context, id WorkspaceID) error {
	if err := m.ensureAvailable(opDeleteWorkspace); err != nil {
		return err
	}
	if err := m.controller.Delete(ctx, workspace.WorkspaceID(id)); err != nil {
		return fmt.Errorf("%s: %w", opDeleteWorkspace, err)
	}
	return nil
}

func (m *managerFacade) AgentTypes() []AgentType {
	types := m.agentSpecs.Types()
	out := make([]AgentType, 0, len(types))
	for _, agentType := range types {
		out = append(out, AgentType(agentType))
	}
	return out
}

func (m *managerFacade) Start(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%s: %w", opStart, err)
	}
	if err := m.ensureAvailable(opStart); err != nil {
		return err
	}
	if err := m.controller.Start(ctx); err != nil {
		return fmt.Errorf("%s: %w", opStart, err)
	}
	if err := m.scheduler.Start(ctx); err != nil {
		return fmt.Errorf("%s: %w", opStart, err)
	}
	return nil
}

func (m *managerFacade) Stop(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%s: %w", opStop, err)
	}

	m.mu.Lock()
	if m.shutDown {
		m.mu.Unlock()
		return nil
	}
	if m.stopInProgress {
		m.mu.Unlock()
		return fmt.Errorf("%s: shutdown already in progress", opStop)
	}
	m.stopInProgress = true
	m.mu.Unlock()

	if err := m.scheduler.Stop(ctx); err != nil {
		m.mu.Lock()
		m.stopInProgress = false
		m.mu.Unlock()
		return fmt.Errorf("%s: %w", opStop, err)
	}
	if err := m.controller.Stop(ctx); err != nil {
		m.mu.Lock()
		m.stopInProgress = false
		m.mu.Unlock()
		return fmt.Errorf("%s: %w", opStop, err)
	}

	m.closeQuietly(ctx, opStop, m.workspaceRepository.Close)
	m.closeQuietly(ctx, opStop, m.pluginRepository.Close)
	m.closeQuietly(ctx, opStop, m.pluginStorage.Close)

	m.mu.Lock()
	m.stopInProgress = false
	m.shutDown = true
	m.mu.Unlock()
	return nil
}

func (m *managerFacade) resolveAttachedPlugins(ctx context.Context, refs []PluginRef) ([]workspace.AttachedPlugin, []agent.AttachedPluginRef, error) {
	if len(refs) == 0 {
		return nil, nil, nil
	}

	seen := make(map[PluginID]struct{}, len(refs))
	attached := make([]workspace.AttachedPlugin, 0, len(refs))
	installRefs := make([]agent.AttachedPluginRef, 0, len(refs))
	for _, ref := range refs {
		if ref.ID == "" {
			return nil, nil, fmt.Errorf("%w: empty plugin id", ErrPluginNotFound)
		}
		if _, ok := seen[ref.ID]; ok {
			return nil, nil, fmt.Errorf("%w: %q", ErrDuplicatePluginRef, ref.ID)
		}
		seen[ref.ID] = struct{}{}

		row, err := m.pluginRepository.Get(ctx, plugin.PluginID(ref.ID))
		if err != nil {
			return nil, nil, err
		}
		attached = append(attached, workspace.AttachedPlugin{
			PluginID:    workspace.PluginID(ref.ID),
			Name:        row.Name,
			ContentHash: row.ContentHash,
		})
		installRefs = append(installRefs, agent.AttachedPluginRef{
			PluginID:    agent.PluginID(ref.ID),
			Name:        row.Name,
			ContentHash: row.ContentHash,
		})
	}
	return attached, installRefs, nil
}

func (m *managerFacade) ensureAvailable(op string) error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.shutDown || m.stopInProgress {
		return fmt.Errorf("%s: %w", op, ErrShutdown)
	}
	return nil
}

func (m *managerFacade) closeQuietly(ctx context.Context, op string, closeFn func(context.Context) error) {
	if closeFn == nil {
		return
	}
	if err := closeFn(context.Background()); err != nil {
		logs.FromContext(ctx).Warn("manager close failed",
			"op", op,
			"component", "manager",
			"err", err,
		)
	}
}

func readAllAndClose(rc io.ReadCloser) ([]byte, error) {
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		return nil, err
	}
	return body, nil
}

func (m *managerFacade) restorePluginBlob(ctx context.Context, id plugin.PluginID, body []byte) error {
	_, err := m.pluginStorage.Put(ctx, id, bytes.NewReader(body))
	return err
}

func readPluginBody(ctx context.Context, content io.Reader) ([]byte, error) {
	limited := &io.LimitedReader{R: content, N: defaultPluginUploadLimit + 1}
	var buf bytes.Buffer
	if _, err := copyWithContext(ctx, &buf, limited); err != nil {
		return nil, err
	}
	if int64(buf.Len()) > defaultPluginUploadLimit {
		return nil, fmt.Errorf("%w: compressed payload exceeds %d bytes", plugin.ErrCorruptArchive, defaultPluginUploadLimit)
	}
	return buf.Bytes(), nil
}

func copyWithContext(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	buf := make([]byte, 32*1024)
	var written int64
	for {
		if ctx != nil {
			select {
			case <-ctx.Done():
				return written, ctx.Err()
			default:
			}
		}
		nr, er := src.Read(buf)
		if nr > 0 {
			nw, ew := dst.Write(buf[:nr])
			written += int64(nw)
			if ew != nil {
				return written, ew
			}
			if nw != nr {
				return written, io.ErrShortWrite
			}
		}
		if er != nil {
			if er == io.EOF {
				return written, nil
			}
			return written, er
		}
	}
}

func pluginFromRow(row plugin.Plugin) Plugin {
	return clonePluginValue(Plugin{
		ID:          PluginID(row.ID),
		Namespace:   Namespace(row.Namespace),
		Name:        row.Name,
		Description: row.Description,
		Manifest:    cloneManifest(row.Manifest),
		ContentHash: row.ContentHash,
		Size:        row.Size,
		CreatedAt:   row.CreatedAt,
		UpdatedAt:   row.UpdatedAt,
	})
}

func workspaceFromRow(row workspace.Workspace) Workspace {
	attached := make([]AttachedPlugin, 0, len(row.Plugins))
	for _, item := range row.Plugins {
		attached = append(attached, AttachedPlugin{
			PluginID:    PluginID(item.PluginID),
			Name:        item.Name,
			ContentHash: item.ContentHash,
			PlacedPaths: append([]string(nil), item.PlacedPaths...),
		})
	}
	return cloneWorkspaceValue(Workspace{
		ID:            WorkspaceID(row.ID),
		Namespace:     Namespace(row.Namespace),
		Alias:         Alias(row.Alias),
		AgentType:     AgentType(row.AgentType),
		InfraType:     InfraType(row.InfraType),
		InfraOptions:  cloneMapAny(row.InfraOptions),
		InstallParams: cloneMapAny(row.InstallParams),
		Plugins:       attached,
		Status:        WorkspaceStatus(row.Status),
		StatusError:   workspaceErrorFromRow(row.StatusError),
		Description:   row.Description,
		Labels:        cloneLabels(row.Labels),
		LastProbeAt:   row.LastProbeAt,
		CreatedAt:     row.CreatedAt,
		UpdatedAt:     row.UpdatedAt,
	})
}

func workspaceErrorFromRow(err *workspace.Error) *WorkspaceError {
	if err == nil {
		return nil
	}
	return &WorkspaceError{
		Code:       err.Code,
		Message:    err.Message,
		Phase:      err.Phase,
		RecordedAt: err.RecordedAt,
	}
}

func workspaceErrorToRow(err *WorkspaceError) *workspace.Error {
	if err == nil {
		return nil
	}
	return &workspace.Error{
		Code:       err.Code,
		Message:    err.Message,
		Phase:      err.Phase,
		RecordedAt: err.RecordedAt,
	}
}

func newWorkspaceError(now time.Time, code, phase, message string) *WorkspaceError {
	return &WorkspaceError{
		Code:       code,
		Message:    message,
		Phase:      phase,
		RecordedAt: now,
	}
}

const httpMethodGet = "GET"
