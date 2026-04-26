# Manager Repair Plan

## Objective

- User goal: fix the defects found in the heterogeneous review of `pkg/manager`, especially lifecycle correctness for probes, plugin snapshot consistency, duplicate plugin attach behavior, and `localdir` safety.
- Non-goals: do not add plugin versioning UX, multi-node coordination, persistent SQL migrations, new infra backends, or orchestrator integration. Keep the manager decoupled from `pkg/agentio` and `pkg/orchestrator/...`.
- Success criteria:
  - A workspace created with `infraops/localdir` can be probed by `ProbeScheduler` after install without being marked failed due to `ErrNotInitialized`.
  - `CreateWorkspace` rejects duplicate plugin IDs before persisting the workspace.
  - The install worker refuses to attach a plugin blob whose hash differs from the workspace snapshot.
  - Plugin overwrite either updates storage and metadata consistently or rolls storage back to the previous blob.
  - `localdir.Clear` no longer deletes through unanchored recursive path operations.
  - `go test ./pkg/manager/...` and `go vet ./pkg/manager/...` pass.

## Relevant Context

- Repository language: Go. Follow root `AGENTS.md`: format with `gofmt -s`, keep error messages diagnostic, wrap errors with `%w`, avoid new third-party deps.
- Manager package rules: `pkg/manager/AGENTS.md` says `pkg/manager` is the workspace lifecycle plane and must not import `pkg/agentio` or `pkg/orchestrator/...`.
- `pkg/manager/infraops/infraops.go` owns the `InfraOps` interface. Adding a method must update all implementations, tests, and `DESIGN.md`.
- `pkg/manager/infraops/localdir/localdir.go` is the only concrete infra implementation. It currently has `Init`, `Exec`, `PutFile`, `GetFile`, `Request`, and `Clear`.
- `pkg/manager/workspace/scheduler.go` creates a fresh `InfraOps` for every probe and currently calls `spec.Probe` without preparing that fresh instance.
- `pkg/manager/workspace/controller.go` creates a fresh `InfraOps` for install and currently calls `ops.Init`, which must remain the create-time empty-directory guard.
- `pkg/manager/manager.go` resolves plugin refs in `resolveAttachedPlugins`, creates workspace rows in `CreateWorkspace`, and overwrites plugins in `CreatePlugin`.
- Current plugin upload body limit is `defaultPluginUploadLimit = 64 << 20`; rollback can safely buffer an old blob within that same practical scale for the reference storage path.
- Current `plugin.Storage` interface exposes `Get`, `Put`, and `Delete`. Do not change it for this repair.

## Design Decisions

- Add `Open(ctx context.Context) error` to `infraops.InfraOps`.
- `Init` remains create-time only: it prepares an empty backing state and rejects non-empty directories.
- `Open` is attach-time only: it prepares an `InfraOps` instance for an already existing backing state and must not create, clear, or require the backing state to be empty.
- `ProbeScheduler` must call `ops.Open(probeCtx)` before `spec.Probe`.
- `Controller.Delete` must call `ops.Open(ctx)` before `spec.Uninstall`; if `Open` fails because the backing state is missing or invalid, log and skip uninstall, then still call `ops.Clear(ctx)`.
- `localdir.Clear` must use an opened `*os.Root` to delete contents. It must not use `os.RemoveAll(o.dir)` or `os.RemoveAll(filepath.Join(dir, entry.Name()))`.
- `CreateWorkspace` must reject duplicate plugin IDs in `req.Plugins` before repository insert.
- `Controller.attachPlugins` must compare `plugin.Storage.Get` returned `StoredObject.ContentHash` with the persisted `AttachedPlugin.ContentHash`.
- `CreatePlugin` overwrite must preserve the previous blob before writing the replacement and restore it if repository update fails.
- Do not disallow multiple different plugins for Claude Code. Multiple distinct manifest names remain valid. Only duplicate plugin IDs in one workspace request are rejected at manager level.
- Keep `request_loopback_host` configurable, but validate it is loopback-only. This avoids SSRF while preserving IPv4/IPv6 local testing.

## Implementation Plan

1. Update `pkg/manager/infraops/infraops.go`.
   - Add `Open(ctx context.Context) error` to `InfraOps` after `Init`.
   - Document that `Init` is for empty create and `Open` is for existing state.
   - Evidence done: all fake infra implementations fail to compile until updated.

2. Update `pkg/manager/infraops/localdir/localdir.go`.
   - Add `Open(ctx)` on `*ops`.
   - Refactor root state assignment into a helper to avoid duplicating the second lock/check.
   - Update both `Init(ctx)` and `Open(ctx)` to reject a workspace root whose final path component is a symlink. Use `os.Lstat(o.dir)` before `os.OpenRoot(o.dir)`.
   - Change `Clear(ctx)` so it opens or reuses root, clears entries through `root.RemoveAll`, and removes the root path itself with `os.Remove` only after a same-file check. Never call `os.RemoveAll(o.dir)`.
   - Validate `request_loopback_host` by parsing it as an IP and requiring `ip.IsLoopback()`.
   - Evidence done: new localdir tests for `Open`, scheduler-style `Exec`, symlink/root replacement during `Clear`, and non-loopback host rejection pass.

3. Update all fake infra implementations in tests.
   - Files:
     - `pkg/manager/manager_test.go`
     - `pkg/manager/workspace/controller_test.go`
     - `pkg/manager/workspace/scheduler_test.go`
     - `pkg/manager/agent/specset_test.go`
     - `pkg/manager/agent/claudecode/spec_test.go`
   - Add `Open(context.Context) error` methods. Fakes should track `openCalls` when tests need assertions.
   - Evidence done: `go test ./pkg/manager/...` compiles.

4. Update `pkg/manager/workspace/scheduler.go`.
   - In `probeOne`, create `probeCtx` before opening the infra.
   - Call `ops.Open(probeCtx)` before looking up the spec or calling `spec.Probe`.
   - On `Open` failure, record failed probe unless the scheduler context is canceled.
   - Evidence done: a scheduler test with an infra fake that requires `Open` passes.

5. Update `pkg/manager/workspace/controller.go`.
   - In `Delete`, call `ops.Open(ctx)` before `spec.Uninstall`.
   - If `Open` fails, log warning with `phase=open`, skip uninstall, and continue to `ops.Clear(ctx)`.
   - In `attachPlugins`, compare `obj.ContentHash` to `pluginRef.ContentHash`; mismatch returns a deterministic install error and does not call layout.
   - Evidence done: tests show delete still clears if open fails, and hash mismatch fails before writes.

6. Update `pkg/manager/manager.go`.
   - In `resolveAttachedPlugins`, reject duplicate plugin IDs with a new package-level sentinel `ErrDuplicatePluginRef`.
   - In `CreatePlugin` overwrite path, read and buffer the old blob before writing the new blob; if repository update fails, restore the old blob.
   - Evidence done: tests for duplicate refs and overwrite rollback pass.

7. Update documentation.
   - `pkg/manager/DESIGN.md`: add `Open` to the InfraOps sketch and explain create/open lifecycle split.
   - `pkg/manager/infraops/PLAN.md`: update interface contract and lifecycle notes.
   - `pkg/manager/infraops/localdir/PLAN.md`: update `Clear`, `Open`, and loopback validation behavior.
   - Evidence done: docs no longer imply scheduler can probe a fresh `InfraOps` without opening it.

## Required Code Snippets

```go
// pkg/manager/infraops/infraops.go
type InfraOps interface {
	Type() InfraType
	Dir() string

	// Init prepares a newly-created backing state. Implementations may require
	// the target to be missing or empty and should reject existing non-empty
	// state to protect create idempotency.
	Init(ctx context.Context) error

	// Open attaches this instance to an existing backing state. It must not
	// create or clear state and must not require the state to be empty.
	Open(ctx context.Context) error

	Exec(ctx context.Context, cmd ExecCommand) (ExecResult, error)
	PutFile(ctx context.Context, path string, content io.Reader, mode fs.FileMode) error
	GetFile(ctx context.Context, path string) (io.ReadCloser, error)
	Request(ctx context.Context, port int, req *http.Request) (*http.Response, error)
	Clear(ctx context.Context) error
}
```

```go
// pkg/manager/infraops/localdir/localdir.go
func (o *ops) Open(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	o.mu.Lock()
	if o.cleared {
		o.mu.Unlock()
		return infraops.ErrCleared
	}
	if o.initialized {
		o.mu.Unlock()
		return nil
	}
	o.mu.Unlock()

	info, err := os.Lstat(o.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: localdir open missing dir %q", infraops.ErrNotInitialized, o.dir)
		}
		return fmt.Errorf("localdir open stat dir %q: %w", o.dir, err)
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return newInvalidDirError("dir must not be a symlink: %q", o.dir)
	}
	if !info.IsDir() {
		return newInvalidDirError("dir is not a directory: %q", o.dir)
	}

	root, err := os.OpenRoot(o.dir)
	if err != nil {
		return fmt.Errorf("localdir open root %q: %w", o.dir, err)
	}
	return o.setRoot(root)
}

func (o *ops) setRoot(root *os.Root) error {
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.cleared {
		_ = root.Close()
		return infraops.ErrCleared
	}
	if o.initialized {
		_ = root.Close()
		return nil
	}
	o.root = root
	o.initialized = true
	return nil
}
```

```go
// pkg/manager/infraops/localdir/localdir.go
// In Init(ctx), replace the current os.Stat(o.dir) branch with Lstat-based
// validation so a symlink at the workspace root is never accepted.
info, err := os.Lstat(o.dir)
switch {
case err == nil:
	if info.Mode()&fs.ModeSymlink != 0 {
		return newInvalidDirError("dir must not be a symlink: %q", o.dir)
	}
	if !info.IsDir() {
		return newInvalidDirError("dir is not a directory: %q", o.dir)
	}
	empty, err := dirIsEmpty(o.dir)
	if err != nil {
		return fmt.Errorf("localdir init check empty %q: %w", o.dir, err)
	}
	if !empty {
		return fmt.Errorf("%w: %w: %s", infraops.ErrAlreadyInitialized, ErrDirNotEmpty, o.dir)
	}
case errors.Is(err, os.ErrNotExist):
	if err := os.MkdirAll(o.dir, 0o755); err != nil {
		return fmt.Errorf("localdir init mkdir %q: %w", o.dir, err)
	}
default:
	return fmt.Errorf("localdir init stat dir %q: %w", o.dir, err)
}
```

```go
// pkg/manager/infraops/localdir/localdir.go
func parseLoopbackHost(raw string) (string, error) {
	host := strings.TrimSpace(raw)
	if host == "" {
		return "", fmt.Errorf("%w: localdir option %q must not be empty", infraops.ErrInvalidOption, "request_loopback_host")
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return "", fmt.Errorf("%w: localdir option %q must be a loopback IP", infraops.ErrInvalidOption, "request_loopback_host")
	}
	return host, nil
}

// Replace the existing request_loopback_host parsing branch with:
if rawHost, ok := opts["request_loopback_host"]; ok {
	host, ok := rawHost.(string)
	if !ok {
		return options{}, fmt.Errorf("%w: localdir option %q must be string", infraops.ErrInvalidOption, "request_loopback_host")
	}
	loopback, err := parseLoopbackHost(host)
	if err != nil {
		return options{}, err
	}
	out.requestLoopbackHost = loopback
}
```

```go
// pkg/manager/infraops/localdir/localdir.go
func (o *ops) Clear(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	o.mu.Lock()
	if o.cleared {
		o.mu.Unlock()
		return nil
	}
	o.cleared = true
	root := o.root
	o.root = nil
	o.initialized = false
	o.mu.Unlock()

	if root == nil {
		pathInfo, err := os.Lstat(o.dir)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("localdir clear lstat dir %q: %w", o.dir, err)
		}
		if pathInfo.Mode()&fs.ModeSymlink != 0 {
			if o.keepDir {
				return nil
			}
			if err := os.Remove(o.dir); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("localdir clear remove symlink %q: %w", o.dir, err)
			}
			return nil
		}
		if !pathInfo.IsDir() {
			if o.keepDir {
				return newInvalidDirError("dir is not a directory: %q", o.dir)
			}
			if err := os.Remove(o.dir); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("localdir clear remove non-dir %q: %w", o.dir, err)
			}
			return nil
		}
		opened, err := os.OpenRoot(o.dir)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return fmt.Errorf("localdir clear open root %q: %w", o.dir, err)
		}
		root = opened
	}
	defer root.Close()

	rootInfo, statErr := root.Stat(".")
	if statErr != nil {
		return fmt.Errorf("localdir clear stat root %q: %w", o.dir, statErr)
	}
	if err := clearRootContents(ctx, root); err != nil {
		return fmt.Errorf("localdir clear contents %q: %w", o.dir, err)
	}
	if o.keepDir {
		return nil
	}

	pathInfo, err := os.Lstat(o.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("localdir clear lstat dir %q: %w", o.dir, err)
	}
	if !os.SameFile(rootInfo, pathInfo) {
		// The path was replaced after the root was opened. Contents were cleared
		// through the safe root handle; do not remove the replacement path.
		return nil
	}
	if err := os.Remove(o.dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("localdir clear remove dir %q: %w", o.dir, err)
	}
	return nil
}

func clearRootContents(ctx context.Context, root *os.Root) error {
	dir, err := root.Open(".")
	if err != nil {
		if isPathEscapeError(err) {
			return infraops.ErrPathOutsideDir
		}
		return err
	}
	entries, readErr := dir.ReadDir(-1)
	closeErr := dir.Close()
	if readErr != nil {
		return readErr
	}
	if closeErr != nil {
		return closeErr
	}

	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := root.RemoveAll(entry.Name()); err != nil {
			if isPathEscapeError(err) {
				return infraops.ErrPathOutsideDir
			}
			return err
		}
	}
	return nil
}
```

```go
// pkg/manager/workspace/scheduler.go
func (s *ProbeScheduler) probeOne(ctx context.Context, row Workspace) {
	s.probesAttempted.Add(1)

	factory, ok := s.factories.Get(infraops.InfraType(row.InfraType))
	if !ok {
		s.recordProbeOutcome(row, StatusFailed, failureFromError(s.clock(), "probe", fmt.Errorf("%w: %q", infraops.ErrInfraTypeUnknown, row.InfraType)), s.clock())
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
		s.recordProbeOutcome(row, StatusFailed, failureFromError(s.clock(), "probe", err), s.clock())
		s.probesFailed.Add(1)
		return
	}
	if err := ops.Open(probeCtx); err != nil {
		if probeCtx.Err() != nil && ctx.Err() != nil {
			return
		}
		s.recordProbeOutcome(row, StatusFailed, failureFromError(s.clock(), "probe", err), s.clock())
		s.probesFailed.Add(1)
		return
	}

	spec, ok := s.agentSpecs.Get(agent.AgentType(row.AgentType))
	if !ok {
		s.recordProbeOutcome(row, StatusFailed, failureFromError(s.clock(), "probe", fmt.Errorf("%w: %q", agent.ErrAgentTypeUnknown, row.AgentType)), s.clock())
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
		s.recordProbeOutcome(row, StatusHealthy, nil, probedAt)
		s.probesHealthy.Add(1)
		return
	}

	var statusErr *Error
	if err != nil {
		statusErr = failureFromError(s.clock(), "probe", err)
	} else {
		statusErr = failureFromProbeResult(s.clock(), result)
	}
	s.recordProbeOutcome(row, StatusFailed, statusErr, probedAt)
	s.probesFailed.Add(1)
}
```

```go
// pkg/manager/workspace/controller.go
func (c *Controller) Delete(ctx context.Context, id WorkspaceID) error {
	// Keep existing stopped check, repo.Get, factory lookup, and factory call.
	// After ops is constructed and spec is looked up:
	if err := ops.Open(ctx); err != nil {
		c.logger.Warn("workspace open before uninstall failed",
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
		c.logger.Warn("workspace uninstall failed",
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
	// Always keep the existing ops.Clear(ctx), warning, and repo.Delete(ctx, id).
}
```

```go
// pkg/manager/workspace/controller.go
func (c *Controller) attachPlugins(ctx context.Context, ops infraops.InfraOps, spec agent.AgentSpec, plugins []AttachedPlugin) ([]AttachedPlugin, error) {
	if len(plugins) == 0 {
		return nil, nil
	}

	attached := make([]AttachedPlugin, 0, len(plugins))
	for _, pluginRef := range plugins {
		body, obj, err := c.pluginStorage.Get(ctx, plugin.PluginID(pluginRef.PluginID))
		if err != nil {
			if errors.Is(err, plugin.ErrStorageNotFound) {
				return nil, fmt.Errorf("%w: %q", plugin.ErrPluginNotFound, pluginRef.PluginID)
			}
			return nil, err
		}
		if obj.ContentHash != pluginRef.ContentHash {
			_ = body.Close()
			return nil, fmt.Errorf("plugin %q content hash changed: snapshot %q storage %q", pluginRef.PluginID, pluginRef.ContentHash, obj.ContentHash)
		}

		reader, openErr := plugin.OpenZipReaderFromStream(ctx, body, obj.Size, c.maxPluginSize)
		closeErr := body.Close()
		if openErr != nil {
			if closeErr != nil {
				return nil, errors.Join(openErr, closeErr)
			}
			return nil, openErr
		}
		if closeErr != nil {
			_ = reader.Close()
			return nil, closeErr
		}

		placedPaths, applyErr := spec.PluginLayout().Apply(ctx, ops, reader.Manifest(), reader.FS())
		readerCloseErr := reader.Close()
		if applyErr != nil {
			if readerCloseErr != nil {
				return nil, errors.Join(applyErr, readerCloseErr)
			}
			return nil, applyErr
		}
		if readerCloseErr != nil {
			return nil, readerCloseErr
		}

		attachedPlugin := pluginRef
		attachedPlugin.PlacedPaths = slices.Clone(placedPaths)
		attached = append(attached, attachedPlugin)
	}
	return attached, nil
}
```

```go
// pkg/manager/types.go
var (
	ErrPluginNotFound         = plugin.ErrPluginNotFound
	ErrPluginNameConflict     = plugin.ErrPluginNameConflict
	ErrDuplicatePluginRef     = errors.New("manager: duplicate plugin ref")
	ErrWorkspaceNotFound      = workspace.ErrWorkspaceNotFound
	ErrAliasConflict          = workspace.ErrAliasConflict
	ErrAgentTypeUnknown       = agent.ErrAgentTypeUnknown
	ErrInfraTypeUnknown       = infraops.ErrInfraTypeUnknown
	ErrInstallTimeout         = workspace.ErrInstallTimeout
	ErrShutdown               = errors.New("manager: shutdown")
	ErrUnsupportedDownloadURL = errors.New("manager: unsupported download URL")
)
```

```go
// pkg/manager/manager.go
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
```

```go
// pkg/manager/manager.go
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

// In CreatePlugin overwrite path, before writing the new body:
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

// In the overwrite repository update error branch:
if overwrite {
	if err := m.pluginRepository.Update(ctx, row); err != nil {
		rollbackErr := m.restorePluginBlob(context.Background(), row.ID, oldBody)
		if rollbackErr != nil {
			m.logger.Error("plugin overwrite rollback failed",
				"op", opCreatePlugin,
				"component", "manager.plugin",
				"plugin_id", row.ID,
				"err", rollbackErr,
			)
			return Plugin{}, fmt.Errorf("%s: %w", opCreatePlugin, errors.Join(err, rollbackErr))
		}
		return Plugin{}, fmt.Errorf("%s: %w", opCreatePlugin, err)
	}
}
```

```go
// Test fake infra method shape to add wherever fakeInfra implements infraops.InfraOps.
func (i *fakeInfra) Open(context.Context) error {
	if i.state != nil {
		i.state.openCalls.Add(1)
	}
	return nil
}
```

## Test Plan

- Required commands:
  - `gofmt -s -w pkg/manager`
  - `go test ./pkg/manager/...`
  - `go vet ./pkg/manager/...`
  - `go test -race ./pkg/manager/workspace ./pkg/manager/infraops/localdir ./pkg/manager/plugin`

- Unit test cases to add:
  - `pkg/manager/infraops/localdir/localdir_test.go`
    - `TestOpenExistingNonEmptyDirAllowsExec`: create dir with file, `New`, `Open`, then `Exec("pwd")` or `GetFile` succeeds without `Init`.
    - `TestOpenMissingDirReturnsNotInitialized`: `Open` on missing dir returns `infraops.ErrNotInitialized`.
    - `TestOpenRejectsSymlinkRoot`: `dir` points at a symlink to a real directory; `Open` returns an error wrapping `ErrInvalidDir` and `infraops.ErrInvalidOption`.
    - `TestClearKeepDirDoesNotFollowReplacedRoot`: initialize workspace, create a victim dir with file, rename workspace dir aside, replace workspace dir path with symlink to victim or another dir, call `Clear`, assert victim file remains.
    - `TestClearWithoutKeepDirDoesNotRemoveReplacementContents`: same replacement setup with `keep_dir=false`, assert replacement contents remain and no recursive delete occurs.
    - `TestNewRejectsNonLoopbackRequestHost`: `request_loopback_host` values `169.254.169.254`, `192.168.1.10`, and `localhost` are rejected; `127.0.0.1` and `::1` are accepted.
  - `pkg/manager/workspace/scheduler_test.go`
    - `TestProbeSchedulerOpensInfraBeforeProbe`: fake infra requires `Open` before `Exec` or `Probe`; scheduler should mark row healthy and record one open call.
    - `TestProbeSchedulerOpenFailureMarksFailed`: fake infra `Open` returns error; scheduler marks workspace failed with phase `probe`.
  - `pkg/manager/workspace/controller_test.go`
    - `TestControllerDeleteOpensBeforeUninstall`: fake infra records open and uninstall order; assert open happens before uninstall and clear.
    - `TestControllerDeleteContinuesToClearWhenOpenFails`: fake infra `Open` fails; assert uninstall not called, clear called, row deleted.
    - `TestControllerSubmitPluginHashMismatchFailsBeforeApply`: storage blob hash differs from attached snapshot; assert status failed and fake layout apply count is zero.
  - `pkg/manager/manager_test.go`
    - `TestManagerCreateWorkspaceRejectsDuplicatePluginRefs`: pass the same plugin ID twice, expect `errors.Is(err, ErrDuplicatePluginRef)`, no workspace row persisted.
    - `TestManagerCreatePluginOverwriteRollbackOnRepositoryUpdateFailure`: fake plugin repo fails `Update`; assert returned error is non-nil and storage still contains old content hash/body.
  - `pkg/manager/agent/claudecode/spec_test.go`
    - `TestClaudeCodeLayoutAppliesMultipleDistinctPlugins`: apply two manifests with different plugin names to same infra, assert both plugin directories exist.
    - `TestClaudeCodeLayoutDuplicatePluginCollision`: apply same manifest twice, assert second returns `agent.ErrInvalidPluginLayout`.

```go
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

	_, err = managerInstance.CreateWorkspace(context.Background(), CreateWorkspaceRequest{
		Alias:        Alias("Alice"),
		AgentType:    AgentType("claude-code"),
		InfraType:    InfraType("localdir"),
		InfraOptions: infraops.Options{"dir": "/tmp/wsp_duplicate_plugin"},
		Plugins:      []PluginRef{{ID: plug.ID}, {ID: plug.ID}},
	})
	if !errors.Is(err, ErrDuplicatePluginRef) {
		t.Fatalf("CreateWorkspace() error = %v, want ErrDuplicatePluginRef", err)
	}
}
```

```go
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
```

```go
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
	if h.spec.layout.applyCalls.Load() != 0 {
		t.Fatalf("layout apply calls = %d, want 0", h.spec.layout.applyCalls.Load())
	}
}
```

```go
func TestClearKeepDirDoesNotFollowReplacedRoot(t *testing.T) {
	t.Parallel()

	parent := t.TempDir()
	dir := filepath.Join(parent, "workspace")
	victim := filepath.Join(parent, "victim")
	if err := os.MkdirAll(victim, 0o755); err != nil {
		t.Fatalf("mkdir victim: %v", err)
	}
	victimFile := filepath.Join(victim, "keep.txt")
	if err := os.WriteFile(victimFile, []byte("keep"), 0o644); err != nil {
		t.Fatalf("write victim: %v", err)
	}

	ops := mustNewOps(t, infraops.Options{"dir": dir, "keep_dir": true})
	if err := ops.Init(context.Background()); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := ops.PutFile(context.Background(), "owned.txt", strings.NewReader("owned"), 0o644); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := os.Rename(dir, filepath.Join(parent, "workspace-moved")); err != nil {
		t.Fatalf("rename workspace: %v", err)
	}
	if err := os.Symlink(victim, dir); err != nil {
		t.Fatalf("symlink replacement: %v", err)
	}

	if err := ops.Clear(context.Background()); err != nil {
		t.Fatalf("clear: %v", err)
	}
	got, err := os.ReadFile(victimFile)
	if err != nil {
		t.Fatalf("victim file removed or unreadable: %v", err)
	}
	if string(got) != "keep" {
		t.Fatalf("victim content = %q, want keep", got)
	}
}
```

## Self-Check Loop

1. Re-read this plan and confirm these symbols were updated: `infraops.InfraOps`, `localdir.(*ops).Open`, `localdir.(*ops).Clear`, `ProbeScheduler.probeOne`, `Controller.Delete`, `Controller.attachPlugins`, `managerFacade.resolveAttachedPlugins`, `managerFacade.CreatePlugin`.
2. Run `gofmt -s -w pkg/manager`.
3. Run targeted tests while iterating: `go test ./pkg/manager/infraops/localdir ./pkg/manager/workspace ./pkg/manager`.
4. Run broader checks: `go test ./pkg/manager/...`, `go vet ./pkg/manager/...`, and `go test -race ./pkg/manager/workspace ./pkg/manager/infraops/localdir ./pkg/manager/plugin`.
5. Inspect `git diff -- pkg/manager` manually and verify there are no imports of `pkg/agentio` or `pkg/orchestrator/...`.
6. Verify all new failure branches preserve sentinel classification with `errors.Is`: `ErrDuplicatePluginRef`, `plugin.ErrPluginNotFound`, `infraops.ErrNotInitialized`, `infraops.ErrInvalidOption`, `workspace.ErrControllerShutdown`.
7. Verify no logs include `InstallParams`, `InfraOptions` values other than structural fields, plugin file contents, stdout, stderr, or secrets.
8. Verify `localdir.Open` and `localdir.Init` both reject a symlink as the workspace root path, and `localdir.Clear` never uses recursive deletion on `o.dir` directly.
9. If any test, vet, race, or diff check fails, edit the implementation and repeat from step 2. Do not claim completion while any required command fails.
