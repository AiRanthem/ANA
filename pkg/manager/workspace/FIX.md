# Workspace 状态一致性修复计划（cross-model handoff）

## Objective
- 用户目标：只针对 `pkg/manager/workspace` 做代码审查并给出可直接执行的修复方案，消除状态机越权写入与并发覆盖问题。
- Non-goals：不改动其他目录；不在本计划中引入 v2 重试/分布式调度能力；不调整 `agent`/`infraops` 领域模型。
- Success criteria：
  - `Controller` 只能写 `init -> healthy|failed`，不能写 `failed -> healthy`。
  - `ProbeScheduler` 只能写 `healthy <-> failed` 与 `init -> failed`（超时 watchdog）。
  - 安装成功后写入 `PlacedPaths` 时，不覆盖并发更新的 `Description/Labels/InstallParams` 等可变字段。
  - 新增测试覆盖并发竞争与状态写入权限，`go test ./pkg/manager/workspace` 通过。

## Relevant Context
- 关键文件：
  - `pkg/manager/workspace/types.go`
  - `pkg/manager/workspace/memory_repository.go`
  - `pkg/manager/workspace/controller.go`
  - `pkg/manager/workspace/scheduler.go`
  - `pkg/manager/workspace/controller_test.go`
  - `pkg/manager/workspace/scheduler_test.go`
  - `pkg/manager/workspace/memory_repository_test.go`
- 关键现状问题（本次审查结论）：
  - `Controller` 在 [controller.go](/home/airan/workspace/ANA/pkg/manager/workspace/controller.go#L484) 直接 `UpdateStatus(..., StatusHealthy, ...)`，仓储层仅按“当前状态是否合法迁移”校验，未校验“谁在写”，导致在竞争场景下可能出现控制器执行 `failed -> healthy`（越过目录 AGENTS 约束）。
  - `Controller` 在 [controller.go](/home/airan/workspace/ANA/pkg/manager/workspace/controller.go#L474) 用提交时快照 `job.workspace` 构造 `updated` 并 `repo.Update`，可能覆盖安装期间其他调用对行的并发可变字段更新。
  - 仓储 `UpdateStatus` 在 [memory_repository.go](/home/airan/workspace/ANA/pkg/manager/workspace/memory_repository.go#L176) 只校验迁移合法，不区分 writer（controller/scheduler），无法表达目录级状态机写入所有权。
- 约束：
  - 包内 AGENTS 明确：状态机写入权限分离，且 `Update` 不得做状态迁移。
  - 保持并发安全与可测试性；不引入外部依赖。

## Design Decisions
- 方案选择：在 `Repository` 增加“带 writer 与 expected-from 的 CAS 状态写入”接口，禁止无条件状态迁移。
  - 选择理由：单纯 `Get + UpdateStatus` 不能原子保证竞争场景正确性；必须把“期望前置状态”与“写入角色”放到同一次仓储写入中。
- 方案选择：安装成功后先读取最新行，再仅覆盖 `Plugins/UpdatedAt`，避免回写旧快照。
  - 选择理由：避免丢失并发 metadata 更新，且不改变状态迁移路径。
- 拒绝方案：
  - 仅在 controller 增加前置 `Get` 校验：仍有 TOCTOU 竞争窗口。
  - 让 scheduler 不再写 `failed -> healthy`：违反当前状态机需求（探活恢复）。
- 关键不变量：
  - `Controller`：仅允许 `init -> healthy|failed`。
  - `ProbeScheduler`：仅允许 `healthy -> failed`、`failed -> healthy`、`init -> failed`（仅 watchdog 路径）。
  - 状态写入失败若为“期望状态不匹配”，视为竞争冲突，按已过期快照处理，不应导致 panic。

## Implementation Plan
1. 在 `types.go` 扩展状态写入契约与错误语义。
2. 在 `memory_repository.go` 实现 CAS + writer 权限校验，并保留并发安全。
3. 在 `controller.go` 改为：
   - 失败/成功状态更新都走 `UpdateStatusCAS`，`expect=StatusInit`，`writer=StatusWriterController`。
   - 成功路径先 `repo.Get` 最新行，再只修改 `Plugins` 与 `UpdatedAt` 后 `repo.Update`。
4. 在 `scheduler.go` 改为：
   - `init` 超时分支：`expect=StatusInit`，`writer=StatusWriterScheduler`。
   - probe 翻转分支：分别使用 `expect=StatusHealthy` 或 `expect=StatusFailed` 的 CAS。
5. 在测试文件新增/调整用例，覆盖 writer 权限、CAS 冲突、并发覆盖防护。
6. 执行格式化与测试命令，确保仅本目录改动且行为与计划一致。

## Required Code Snippets
```go
// pkg/manager/workspace/types.go
type StatusWriter string

const (
	StatusWriterController StatusWriter = "controller"
	StatusWriterScheduler  StatusWriter = "scheduler"
)

var (
	ErrStatusPreconditionFailed = errors.New("workspace: status precondition failed")
)

type Repository interface {
	Insert(ctx context.Context, w Workspace) error
	Get(ctx context.Context, id WorkspaceID) (Workspace, error)
	GetByAlias(ctx context.Context, namespace Namespace, alias Alias) (Workspace, error)
	List(ctx context.Context, opts ListOptions) ([]Workspace, string, error)
	Update(ctx context.Context, w Workspace) error
	UpdateStatusCAS(
		ctx context.Context,
		id WorkspaceID,
		writer StatusWriter,
		expect Status,
		next Status,
		statusError *Error,
		lastProbeAt time.Time,
	) error
	Delete(ctx context.Context, id WorkspaceID) error
	Close(ctx context.Context) error
}
```

```go
// pkg/manager/workspace/memory_repository.go
func (r *MemoryRepository) UpdateStatusCAS(
	_ context.Context,
	id WorkspaceID,
	writer StatusWriter,
	expect Status,
	next Status,
	statusError *Error,
	lastProbeAt time.Time,
) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return errRepositoryClosed
	}
	row, ok := r.byID[id]
	if !ok {
		return ErrWorkspaceNotFound
	}
	if row.Status != expect {
		return fmt.Errorf("%w: workspace %q current=%q expect=%q",
			ErrStatusPreconditionFailed, id, row.Status, expect)
	}
	if err := validateTransitionByWriter(writer, expect, next); err != nil {
		return err
	}

	row.Status = next
	row.LastProbeAt = lastProbeAt
	row.UpdatedAt = time.Now().UTC()
	if next == StatusHealthy {
		row.StatusError = nil
	} else {
		row.StatusError = cloneError(statusError)
	}
	r.byID[id] = row
	return nil
}

func validateTransitionByWriter(writer StatusWriter, from, to Status) error {
	switch writer {
	case StatusWriterController:
		if from == StatusInit && (to == StatusHealthy || to == StatusFailed) {
			return nil
		}
	case StatusWriterScheduler:
		if from == StatusInit && to == StatusFailed {
			return nil
		}
		if from == StatusHealthy && to == StatusFailed {
			return nil
		}
		if from == StatusFailed && to == StatusHealthy {
			return nil
		}
	}
	return fmt.Errorf("%w: writer=%q %q->%q", ErrInvalidStatusTransition, writer, from, to)
}
```

```go
// pkg/manager/workspace/controller.go (核心收敛逻辑)
latest, err := c.repo.Get(persistCtx, job.workspace.ID)
if err != nil {
	c.transitionToFailed(persistCtx, installCtx, job.workspace.ID,
		failureFromError(c.clock(), "install", fmt.Errorf("reload before finalize: %w", err)),
		time.Time{},
	)
	return
}
if latest.Status != StatusInit {
	// 说明快照已过期（如 watchdog 已写 failed），不再覆盖状态。
	return
}

latest.Plugins = attachedPlugins
latest.UpdatedAt = c.clock()
if err := c.retryRepoWrite(persistCtx, installCtx, "persist_attached_plugins", func(ctx context.Context) error {
	return c.repo.Update(ctx, latest)
}); err != nil {
	// ...
}

if err := c.retryRepoWrite(persistCtx, installCtx, "update_status_healthy", func(ctx context.Context) error {
	return c.repo.UpdateStatusCAS(ctx, latest.ID, StatusWriterController, StatusInit, StatusHealthy, nil, probedAt)
}); err != nil {
	// ...
}
```

```go
// pkg/manager/workspace/scheduler.go (状态翻转与 watchdog)
if s.installTimedOut(now, row) {
	_ = s.repo.UpdateStatusCAS(
		context.Background(),
		row.ID,
		StatusWriterScheduler,
		StatusInit,
		StatusFailed,
		failureFromError(now, "install", ErrInstallTimeout),
		time.Time{},
	)
}

func (s *ProbeScheduler) transitionProbeStatus(row Workspace, next Status, statusErr *Error, probedAt time.Time) error {
	return s.repo.UpdateStatusCAS(
		context.Background(),
		row.ID,
		StatusWriterScheduler,
		row.Status,
		next,
		statusErr,
		probedAt,
	)
}
```

```go
// pkg/manager/workspace/controller_test.go (核心测试骨架)
func TestController_DoesNotPromoteFailedToHealthyAfterInitTimeoutRace(t *testing.T) {
	// Arrange: install 阻塞到 watchdog 将 init->failed。
	// Act: 放开 install + probe 成功。
	// Assert: 最终状态仍为 failed；controller 不得执行 failed->healthy。
}

func TestController_PersistAttachedPluginsPreservesConcurrentMetadataUpdate(t *testing.T) {
	// Arrange: Submit 后、install 完成前，并发 Update Description/Labels。
	// Act: install 成功，controller 写 placed paths。
	// Assert: Description/Labels 保留最新值，Plugins 带 placed paths。
}
```

## Test Plan
- 命令：
  - `gofmt -s -w pkg/manager/workspace/*.go`
  - `go test ./pkg/manager/workspace`
  - `go test -race ./pkg/manager/workspace`
- 必须新增/调整的单测清单：
  - `MemoryRepository`
    - `TestMemoryRepository_UpdateStatusCAS_RejectsWrongExpectedStatus`
    - `TestMemoryRepository_UpdateStatusCAS_RejectsWriterForbiddenTransition`
    - `TestMemoryRepository_UpdateStatusCAS_AllowsControllerInitToHealthy`
    - `TestMemoryRepository_UpdateStatusCAS_AllowsSchedulerFailedToHealthy`
  - `Controller`
    - `TestController_DoesNotPromoteFailedToHealthyAfterInitTimeoutRace`
    - `TestController_PersistAttachedPluginsPreservesConcurrentMetadataUpdate`
    - `TestController_TransitionToFailed_UsesCASFromInitOnly`
  - `ProbeScheduler`
    - `TestProbeScheduler_WatchdogUsesInitExpectation`
    - `TestProbeScheduler_ProbeTransitionCASConflictIsHandled`
- 核心断言要求：
  - CAS 冲突必须返回 `ErrStatusPreconditionFailed`（`errors.Is` 可识别）。
  - controller 的成功路径在冲突场景不应把 `failed` 覆盖为 `healthy`。
  - scheduler 对 `failed->healthy` 与 `healthy->failed` 仍可正常翻转。
  - 并发 metadata 更新不会被插件持久化步骤回滚。

## Self-Check Loop
1. 对照本计划逐项确认：`types.go`、`memory_repository.go`、`controller.go`、`scheduler.go`、对应测试文件均已按符号级要求修改。
2. 运行 `gofmt -s -w pkg/manager/workspace/*.go`。
3. 运行 `go test ./pkg/manager/workspace`；失败则先修正语义错误再重跑。
4. 运行 `go test -race ./pkg/manager/workspace`；若发现数据竞争，优先修复仓储与 controller/scheduler 写路径。
5. 人工检查 diff：确认无其他目录改动；确认未引入绕过 writer 权限的状态写入入口。
6. 最终复核：
  - controller 是否仍只有 `init -> healthy|failed`
  - scheduler 是否仍只有 `init->failed` watchdog 与 `healthy<->failed`
  - 插件持久化是否只更新必要字段
  - 测试名称与断言是否覆盖计划列出的风险点。
