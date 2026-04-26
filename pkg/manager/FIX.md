# pkg/manager 主模块审查修复计划（cross-model handoff）

## Objective
- 用户目标：仅审查 `pkg/manager` 当前目录（不含子目录）实现，并给出可直接执行的修复计划。
- Non-goals：
  - 不修改 `pkg/manager/*` 子目录实现细节（`agent/`、`workspace/`、`plugin/`、`infraops/`）。
  - 不引入新三方依赖。
- Success criteria：
  - `CreateWorkspace` 不再在主模块直接做状态机失败迁移写入。
  - `DeletePlugin` 在 repo/storage 跨存储失败时保持可重试与错误语义一致，避免“已删元数据但返回失败且留孤儿 blob”。
  - `Build` 不提前启动后台 worker；`Start` 才是后台循环唯一启动点。
  - 增加针对以上行为的单测，`go test ./pkg/manager` 通过。

## Relevant Context
- 仅涉及文件：
  - `pkg/manager/manager.go`
  - `pkg/manager/manager_test.go`
  - （可选）`pkg/manager/types.go`（仅当需要新增错误哨兵）
- 发现的问题（审查结论）：
  - 问题 1：`Build` 阶段提前启动 controller，导致未调用 `Start` 时也会执行安装 worker，生命周期语义与 `Start/Stop` 接口不一致。位置：[manager.go](/home/airan/workspace/ANA/pkg/manager/manager.go#L198)。
  - 问题 2：`CreateWorkspace` 在 `Submit` 非 shutdown 错误分支直接 `UpdateStatus(...failed...)`，主模块越过了控制器状态写入边界，且把“入队失败”误标为“安装失败”。位置：[manager.go](/home/airan/workspace/ANA/pkg/manager/manager.go#L579)。
  - 问题 3：`DeletePlugin` 先删 repo 再删 storage，storage 删除失败时函数返回错误，但 repo 已不可恢复，调用方重试会拿到 not found，且 blob 可能泄漏。位置：[manager.go](/home/airan/workspace/ANA/pkg/manager/manager.go#L442)。

## Design Decisions
- 决策 A：将 controller 的 `Start` 从 `Build` 移到 `Manager.Start`，保证所有后台循环由 `Start/Stop` 管理。
  - 结果：`Build` 只做依赖装配，不产生后台副作用。
- 决策 B：`CreateWorkspace` 中只要 `Submit` 失败（含非 shutdown），统一走“补偿删除已插入行并返回错误”。
  - 结果：避免主模块直接改 workspace 状态，也避免把“未开始安装”误写成 `install.error`。
- 决策 C：`DeletePlugin` 调整为“先删 storage，后删 repo”，并把 `storage not found` 视为幂等成功。
  - 结果：杜绝“repo 已删、blob 留存”的不可重试状态；若 repo 删除失败，至少不会留下引用损坏（metadata 指向缺失 blob）。

## Implementation Plan
1. 更新 `manager.go` 的 `Build`，移除 `controller.Start(ctx)`。
2. 更新 `manager.go` 的 `Start`：
   - 先 `controller.Start(m.buildCtx)`，再 `scheduler.Start(m.buildCtx)`。
   - 若 `scheduler.Start` 失败，立即 `controller.Stop(context.Background())` 回滚，并返回组合错误。
3. 更新 `manager.go` 的 `CreateWorkspace` 提交流程：
   - 删除“非 shutdown 分支写 failed”逻辑。
   - 提交失败统一执行 `workspaceRepository.Delete(context.Background(), row.ID)` 补偿，记录日志；返回原始 `Submit` 错误。
4. 更新 `manager.go` 的 `DeletePlugin`：
   - 执行顺序改为 `pluginStorage.Delete` -> `pluginRepository.Delete`。
   - 对 `plugin.ErrStorageNotFound` 进行幂等放行。
5. 更新 `manager_test.go`：新增针对 3 个问题的回归用例。
6. 运行格式化与测试，确认行为与计划一致。

## Required Code Snippets
```go
// pkg/manager/manager.go (Build)
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
// 删除 controller.Start(ctx)
```

```go
// pkg/manager/manager.go (Start)
func (m *managerFacade) Start(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%s: %w", opStart, err)
	}
	if err := m.ensureAvailable(opStart); err != nil {
		return err
	}
	if err := m.controller.Start(m.buildCtx); err != nil {
		return fmt.Errorf("%s: %w", opStart, err)
	}
	if err := m.scheduler.Start(m.buildCtx); err != nil {
		stopErr := m.controller.Stop(context.Background())
		if stopErr != nil {
			return fmt.Errorf("%s: %w", opStart, errors.Join(err, stopErr))
		}
		return fmt.Errorf("%s: %w", opStart, err)
	}
	return nil
}
```

```go
// pkg/manager/manager.go (CreateWorkspace submit failure path)
if err := m.controller.Submit(context.Background(), row, params); err != nil {
	if deleteErr := m.workspaceRepository.Delete(context.Background(), row.ID); deleteErr != nil {
		logs.FromContext(ctx).Error("failed to delete workspace row after submit failure",
			"op", opCreateWorkspace,
			"component", "manager.workspace",
			"workspace_id", row.ID,
			"workspace_alias", row.Alias,
			"namespace", row.Namespace,
			"err", deleteErr,
		)
	}
	return Workspace{}, fmt.Errorf("%s: %w", opCreateWorkspace, err)
}
```

```go
// pkg/manager/manager.go (DeletePlugin)
func (m *managerFacade) DeletePlugin(ctx context.Context, id PluginID) error {
	if err := m.ensureAvailable(opDeletePlugin); err != nil {
		return err
	}
	row, err := m.pluginRepository.Get(ctx, plugin.PluginID(id))
	if err != nil {
		return fmt.Errorf("%s: %w", opDeletePlugin, err)
	}
	if err := m.pluginStorage.Delete(ctx, row.ID); err != nil && !errors.Is(err, plugin.ErrStorageNotFound) {
		return fmt.Errorf("%s: %w", opDeletePlugin, err)
	}
	if err := m.pluginRepository.Delete(ctx, row.ID); err != nil {
		return fmt.Errorf("%s: %w", opDeletePlugin, err)
	}
	return nil
}
```

```go
// pkg/manager/manager_test.go (核心新增测试骨架)
func TestManagerBuild_DoesNotStartWorkersBeforeStart(t *testing.T) {
	// Build 后不调用 Start，CreateWorkspace 应只写 init，不应自动转 healthy。
}

func TestManagerCreateWorkspace_SubmitFailureDeletesRowWithoutStatusWrite(t *testing.T) {
	// 构造 Submit 失败（例如先 Stop 再触发 CreateWorkspace 竞态），断言行被删除且不残留 failed。
}

func TestManagerDeletePlugin_StorageDeleteFailureDoesNotDeleteRepository(t *testing.T) {
	// storage Delete 返回错误时，repo 仍可 Get 到该插件。
}

func TestManagerDeletePlugin_StorageNotFoundStillDeletesRepository(t *testing.T) {
	// storage not found 视为幂等成功，最终 repo 中条目被删除。
}
```

## Test Plan
- 命令：
  - `gofmt -s -w pkg/manager/*.go`
  - `go test ./pkg/manager`
- 单测用例清单（逐条实现）：
  - `TestManagerBuild_DoesNotStartWorkersBeforeStart`
    - 构建 manager（不调用 `Start`），创建 workspace，等待短时间后状态仍为 `init`。
  - `TestManagerStart_StartsWorkersAndScheduler`
    - 调用 `Start` 后创建 workspace，状态能从 `init` 迁移到 `healthy`。
  - `TestManagerCreateWorkspace_SubmitFailureDeletesRowWithoutStatusWrite`
    - 人为制造 `Submit` 失败，断言 workspace 记录不存在（`ErrWorkspaceNotFound`）。
  - `TestManagerDeletePlugin_StorageDeleteFailureDoesNotDeleteRepository`
    - 用包装 storage 强制 `Delete` 返回错误，断言 `DeletePlugin` 返回错误且 `GetPlugin` 仍存在。
  - `TestManagerDeletePlugin_StorageNotFoundStillDeletesRepository`
    - storage 返回 `ErrStorageNotFound`，断言 `DeletePlugin` 成功且 `GetPlugin` 返回 `ErrPluginNotFound`。

## Self-Check Loop
1. 对照本计划逐项核验：`Build`/`Start`/`CreateWorkspace`/`DeletePlugin` 四处逻辑均按计划修改。
2. 执行 `gofmt -s -w pkg/manager/*.go`。
3. 执行 `go test ./pkg/manager`，失败则先修语义后再重跑。
4. 手工检查 diff：
   - 仅变更 `pkg/manager` 目录当前层级文件；
   - 不新增对子目录实现的直接耦合；
   - 错误包装仍保留 `op` 前缀。
5. 复核行为：
   - Build 无后台副作用；
   - Start/Stop 对后台生命周期可控；
   - Submit 失败不落地错误状态；
   - DeletePlugin 不出现“返回失败但 repo 已删”半失败状态。
