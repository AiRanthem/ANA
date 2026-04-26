# infraops/localdir 上下文取消与输入校验修复计划（跨模型交接）

  ## Objective

  - 用户目标：仅在 pkg/manager/infraops 目录内修复已识别问题，消除 PutFile(nil) panic，并让
    Init/Open/Clear 在等待锁条件时遵守 context.Context 取消语义。
  - 非目标：不改动其他目录；不引入新依赖；不调整 InfraOps 对外接口语义（除返回更准确错误
    外）。
  - 成功标准：
      - PutFile 传入 nil content 返回 infraops.ErrInvalidOption，不再 panic。
      - Clear(ctx) 在有 in-flight 操作且 ctx 超时/取消时，能及时返回
        context.DeadlineExceeded/context.Canceled。
      - Init(ctx)、Open(ctx) 在 clearing 阶段等待时，能因 ctx 超时/取消及时返回。
      - 现有测试继续通过，并新增覆盖以上回归点的测试。

  ## Relevant Context

  - 目标文件：
    pkg/manager/infraops/localdir/localdir.go
    pkg/manager/infraops/localdir/localdir_test.go
  - 关键符号：
      - func (o *ops) PutFile(...) error
      - func (o *ops) Init(ctx context.Context) error
      - func (o *ops) Open(ctx context.Context) error
      - func (o *ops) Clear(ctx context.Context) error
      - func (o *ops) beginOp() ...（将改为接收 ctx）
      - o.mu / o.cond / o.clearing / o.activeOps
  - 现有约束（目录 AGENTS.md）：
      - Clear 应可幂等且终态后返回 ErrCleared
      - 不吞错误，错误需可诊断
      - 并发路径需有明确关闭/退出机制

  ## Design Decisions

  - 采用最小侵入方案：保留 sync.Cond 模型，不重写为 channel 状态机。
  - 新增一个“带 context 的 cond 等待助手”，用于所有关键等待循环，避免无限阻塞。
  - 将 beginOp 改为 beginOp(ctx)，让 Exec/PutFile/GetFile/Request 在 Clear 正在进行时也可响应
    取消。
  - PutFile 在进入 IO 前显式校验 content != nil，返回 infraops.ErrInvalidOption。
  - 边界与不变量：
      - 只有 clearErr == nil 才设置 o.cleared = true（保持现有语义）。
      - activeOps 计数增减语义不变。
      - 不改变 Exec 非零退出码行为（仍是 result.ExitCode + nil error）。

  ## Implementation Plan

  1. 更新 pkg/manager/infraops/localdir/localdir.go 中 beginOp 签名与调用点。
     将 beginOp() 改为 beginOp(ctx context.Context)，并修改 Exec/PutFile/GetFile/Request 调
     用。
     完成标志：上述 4 个方法均传入自己的 ctx，不再调用无参 beginOp。
  2. 在同文件新增“锁内带 context 等待”助手函数，并替换关键等待循环。
     替换位置：

  - Init 中 for o.clearing { o.cond.Wait() }
  - Open 中同类循环
  - Clear 中等待 o.clearing 和 o.activeOps > 0 的循环
  - beginOp(ctx) 中等待 o.clearing 的循环
    完成标志：所有这些循环都在每次唤醒后检查 ctx.Err()，且 context 取消时能退出并返回错误。

  3. 在 PutFile 增加 nil content 校验。
     位置：beginOp(ctx) 成功后、sanitizeRelativePath 前。
     完成标志：content == nil 时返回 fmt.Errorf("%w: localdir putfile content must not be
     nil", infraops.ErrInvalidOption)，不触发 panic。
  4. 为上述行为补测试到 pkg/manager/infraops/localdir/localdir_test.go。
     新增测试：

  - TestPutFileNilContentReturnsInvalidOption
  - TestClearReturnsContextErrorWhileWaitingForInflightOp
  - TestInitAndOpenReturnContextErrorWhileClearing
  - TestOperationBeginOpReturnsContextErrorWhileClearing（建议用 Exec 或 PutFile 触发
    beginOp(ctx)）
    完成标志：新增测试稳定通过，且无超时挂死。

  5. 回归验证。
     运行该目录测试与 race，确认没有并发回归。
     完成标志：命令全部通过。

  ## Required Code Snippets

  // localdir.go

  // 1) beginOp signature + call sites
  func (o *ops) Exec(ctx context.Context, cmd infraops.ExecCommand) (infraops.ExecResult,
  error) {
        root, release, err := o.beginOp(ctx)
        if err != nil {
                return infraops.ExecResult{}, err
        }
        defer release()
        // existing logic...
  }

  func (o *ops) PutFile(ctx context.Context, path string, content io.Reader, mode
  fs.FileMode) error {
        root, release, err := o.beginOp(ctx)
        if err != nil {
                return err
        }
        defer release()

        if content == nil {
                return fmt.Errorf("%w: localdir putfile content must not be nil",
  infraops.ErrInvalidOption)
        }
        // existing logic...
  }

  // localdir.go

  // 2) context-aware cond wait helper (must be called with o.mu already locked)
  func (o *ops) waitCondWithContextLocked(ctx context.Context, pred func() bool) error {
        if ctx == nil {
                ctx = context.Background()
        }
        done := make(chan struct{})
        go func() {
                select {
                case <-ctx.Done():
                        o.mu.Lock()
                        o.cond.Broadcast()
                        o.mu.Unlock()
                case <-done:
                }
        }()
        defer close(done)

        for pred() {
                if err := ctx.Err(); err != nil {
                        return err
                }
                o.cond.Wait()
        }
        if err := ctx.Err(); err != nil {
                return err
        }
        return nil
  }

  // localdir.go

  func (o *ops) Init(ctx context.Context) error {
        if err := ctx.Err(); err != nil {
                return err
        }
        o.mu.Lock()
        if err := o.waitCondWithContextLocked(ctx, func() bool { return o.clearing }); err != nil {
                o.mu.Unlock()
                return err
        }
        if o.cleared {
                o.mu.Unlock()
                return infraops.ErrCleared
        }
        if o.initialized {
                o.mu.Unlock()
                return infraops.ErrAlreadyInitialized
        }
        o.mu.Unlock()
        // existing logic...
  }

  // localdir.go

  func (o *ops) beginOp(ctx context.Context) (*os.Root, func(), error) {
        o.mu.Lock()
        defer o.mu.Unlock()

        if err := o.waitCondWithContextLocked(ctx, func() bool { return o.clearing }); err != nil {
                return nil, nil, err
        }
        if o.cleared {
                return nil, nil, infraops.ErrCleared
        }
        if !o.initialized || o.root == nil {
                return nil, nil, infraops.ErrNotInitialized
        }
        o.activeOps++
        root := o.root
        release := func() {
                o.mu.Lock()
                o.activeOps--
                if o.activeOps == 0 {
                        o.cond.Broadcast()
                }
                o.mu.Unlock()
        }
        return root, release, nil
  }

  // localdir_test.go

  func TestPutFileNilContentReturnsInvalidOption(t *testing.T) {
        t.Parallel()
        ops := initOpsInTempDir(t, nil)

        err := ops.PutFile(context.Background(), "x.txt", nil, 0o644)
        if err == nil {
                t.Fatal("expected error")
        }
        if !errors.Is(err, infraops.ErrInvalidOption) {
                t.Fatalf("expected ErrInvalidOption, got %v", err)
        }
  }

  // localdir_test.go

  func TestClearReturnsContextErrorWhileWaitingForInflightOp(t *testing.T) {
        t.Parallel()
        ops := initOpsInTempDir(t, nil)

        reader := &gatedReader{
                data:    bytes.Repeat([]byte("z"), 128*1024),
                chunk:   1024,
                started: make(chan struct{}),
                allow:   make(chan struct{}),
        }
        putErrCh := make(chan error, 1)
        go func() {
                putErrCh <- ops.PutFile(context.Background(), "inflight.bin", reader, 0o644)
        }()

        <-reader.started

        ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
        defer cancel()

        clearErrCh := make(chan error, 1)
        go func() { clearErrCh <- ops.Clear(ctx) }()

        err := <-clearErrCh
        if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
                t.Fatalf("expected context error, got %v", err)
        }

        close(reader.allow)
        _ = <-putErrCh
  }

  // localdir_test.go

  func TestInitAndOpenReturnContextErrorWhileClearing(t *testing.T) {
        t.Parallel()

        ops := initOpsInTempDir(t, nil)
        reader := &gatedReader{
                data:    bytes.Repeat([]byte("a"), 64*1024),
                chunk:   1024,
                started: make(chan struct{}),
                allow:   make(chan struct{}),
        }
        putErrCh := make(chan error, 1)
        go func() {
                putErrCh <- ops.PutFile(context.Background(), "hold.bin", reader, 0o644)
        }()
        <-reader.started

        clearErrCh := make(chan error, 1)
        go func() { clearErrCh <- ops.Clear(context.Background()) }()

        ctx1, cancel1 := context.WithTimeout(context.Background(), 100*time.Millisecond)
        defer cancel1()
        if err := ops.Init(ctx1); !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err,
  context.Canceled) {
                t.Fatalf("Init expected context error, got %v", err)
        }

        ctx2, cancel2 := context.WithTimeout(context.Background(), 100*time.Millisecond)
        defer cancel2()
        if err := ops.Open(ctx2); !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err,
  context.Canceled) {
                t.Fatalf("Open expected context error, got %v", err)
        }

        close(reader.allow)
        _ = <-putErrCh
        _ = <-clearErrCh
  }

  ## Test Plan

  - 执行命令：
      - go test ./pkg/manager/infraops/...
      - go test -race ./pkg/manager/infraops/...
  - 必须存在的单测用例：
      - PutFile 传 nil reader 返回 ErrInvalidOption，且不会 panic。
      - Clear(ctx) 在等待 in-flight PutFile 时，ctx 超时后返回 context 错误。
      - Init(ctx) 在 clearing=true 等待期内，ctx 超时后返回 context 错误。
      - Open(ctx) 在 clearing=true 等待期内，ctx 超时后返回 context 错误。
      - 至少一个经 beginOp(ctx) 进入的方法（Exec/PutFile/GetFile/Request 任一）在
        clearing=true 时能返回 context 错误而非阻塞。
  - 关键断言：
      - errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
      - errors.Is(err, infraops.ErrInvalidOption)
      - 测试流程可收敛结束（无 goroutine 卡死、无测试超时）。

  ## Self-Check Loop

  1. 对照本计划逐项核对：localdir.go 的 beginOp、Init/Open/Clear/PutFile 是否都已改到位，
     localdir_test.go 是否新增全部必需用例。
  2. 运行 gofmt -s -w pkg/manager/infraops/localdir/localdir.go pkg/manager/infraops/
     localdir/localdir_test.go。
  3. 运行 go test ./pkg/manager/infraops/...，再运行 go test -race ./pkg/manager/
     infraops/...。
  4. 若任一测试超时或失败，优先检查：
      - waitCondWithContextLocked 是否在 ctx.Done() 时触发 Broadcast。
      - 所有 wait 循环是否通过该 helper 并正确解锁返回。
      - 测试中的阻塞 reader 是否都被释放（close(reader.allow)）。
  5. 人工检查最终 diff：仅应修改 pkg/manager/infraops/localdir/localdir.go 与 pkg/manager/
     infraops/localdir/localdir_test.go。
  6. 若 diff 出现语义外改动（重构、日志噪音、无关行为变化），回退并重做，直到与计划一致。