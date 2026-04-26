# 修复 pkg/manager/agent 审查问题的跨模型实施计划

  ## Objective

  - 用户目标：仅在 pkg/manager/agent 目录内修复已识别问题，使实现与目录约束一致，避免运行时序
    列化失败和插件布局误判。
  - 非目标：
      - 不修改 pkg/manager/agent 以外目录。
      - 不调整跨模块架构或引入新依赖。
      - 不改动 AgentSpec 接口形状。
  - 成功标准：
      - ProtocolDescriptor 校验拒绝 NaN/Inf。
      - Claude 插件布局在“目录已被占用但无 manifest”场景下，PutFile 前即失败。
      - 错误语义区分“unknown type”和“invalid type/spec input”。
      - 新增/更新测试覆盖以上行为并通过。

  ## Relevant Context

  - 目标文件：
      - pkg/manager/agent/types.go
      - pkg/manager/agent/specset.go
      - pkg/manager/agent/specset_test.go
      - pkg/manager/agent/claudecode/layout.go
      - pkg/manager/agent/claudecode/spec_test.go
  - 关键符号：
      - ValidateAgentType, ValidateProtocolDescriptor, validateJSONCompatibleValue
      - ErrAgentTypeUnknown, ErrInvalidProtocolDescriptor, ErrInvalidPluginLayout
      - SpecSet.Register, SpecSet.Lookup
      - ensureNoPluginNameCollision, layout.Apply
      - fakeInfraOps.GetFile
  - 必须遵循的既有模式：
      - 错误包装形式：fmt.Errorf("<op>: %w", err)。
      - 测试风格：table-driven + t.Run + t.Parallel()。
      - 插件布局失败前不应写入文件（putCalls 断言）。

  ## Design Decisions

  - 方案选择：
      - 在 JSON 值校验阶段直接拒绝非有限浮点，避免“校验通过、持久化失败”。
      - 插件冲突检测从仅检查 manifest.toml 扩展为检查“插件基目录路径 + manifest 路径”占用状
        态。
      - 引入更精确的错误哨兵，保留 ErrAgentTypeUnknown 专用于 lookup miss。
  - 拒绝方案：
      - 不用“先 json.Marshal 再反序列化”做校验，避免不必要开销和行为不透明。
      - 不依赖 infra 外部目录遍历 API（当前接口无此约定），仅通过 GetFile +
        ErrNotARegularFile 语义实现。
  - 不变量与边界：
      - Lookup miss 必须仍返回 ErrAgentTypeUnknown。
      - 目录冲突场景下 layout.Apply 必须在第一次 PutFile 前失败。
      - float32/float64 仅允许有限值；容器内嵌套同样生效。

  ## Implementation Plan

  1. 在 types.go 引入更准确错误语义与浮点校验。
      - 修改文件：pkg/manager/agent/types.go
      - 变更符号：var (...) 错误集合、ValidateAgentType、validateJSONCompatibleValue
      - 行为目标：
          - 新增 ErrInvalidAgentType（和可选 ErrInvalidAgentSpec，见代码片段）；
          - ValidateAgentType 返回 ErrInvalidAgentType；
          - validateJSONCompatibleValue 拒绝 NaN/Inf（含 float32、float64）。
      - 完成证据：新增测试可用 errors.Is(err, ErrInvalidAgentType)；NaN/Inf 用例失败为预期。
  2. 在 specset.go 收敛错误语义。
      - 修改文件：pkg/manager/agent/specset.go
      - 变更符号：SpecSet.Register, SpecSet.Lookup
      - 行为目标：
          - Register(nil) 与“空 display name”返回 invalid-spec 类错误（不要再复用 unknown）；
          - Lookup miss 继续返回 ErrAgentTypeUnknown（不变）。
      - 完成证据：新增测试精确断言 errors.Is 分类。
  3. 在 claudecode/layout.go 提前拦截目录占用冲突。
      - 修改文件：pkg/manager/agent/claudecode/layout.go
      - 变更符号：ensureNoPluginNameCollision（可新增私有 helper）
      - 行为目标：
          - 先检查 pluginBase 是否已占用（文件或目录）；
          - 再检查 pluginBase/manifest.toml；
          - 任一占用都返回 ErrInvalidPluginLayout；
          - 未占用才允许继续。
      - 完成证据：新增测试模拟目录已存在但无 manifest，putCalls == 0 且返回冲突错误。
  4. 更新测试覆盖。
      - 修改文件：pkg/manager/agent/specset_test.go, pkg/manager/agent/claudecode/
        spec_test.go
      - 行为目标：
          - 新增 NaN/Inf 校验用例（含嵌套 map/slice）；
          - 新增错误分类用例（invalid vs unknown）；
          - 新增“目录占用冲突提前失败”用例。
      - 完成证据：目标测试均通过，且 race 通过。

  ## Required Code Snippets

  // pkg/manager/agent/types.go

  var (
      ErrAgentTypeConflict         = errors.New("agent: agent type conflict")
      ErrAgentTypeUnknown          = errors.New("agent: agent type unknown")
      ErrInvalidAgentType          = errors.New("agent: invalid agent type")
      ErrInvalidAgentSpec          = errors.New("agent: invalid agent spec")
      ErrInvalidProtocolDescriptor = errors.New("agent: invalid protocol descriptor")
      ErrInvalidPluginLayout       = errors.New("agent: invalid plugin layout")
  )

  func ValidateAgentType(agentType AgentType) error {
      if !agentTypePattern.MatchString(string(agentType)) {
          return fmt.Errorf("%w: %q", ErrInvalidAgentType, agentType)
      }
      return nil
  }

  func validateJSONCompatibleValue(v any) error {
      if v == nil {
          return nil
      }

      switch n := v.(type) {
      case bool, string, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
          return nil
      case float32:
          f := float64(n)
          if math.IsNaN(f) || math.IsInf(f, 0) {
              return fmt.Errorf("non-finite float32 %v", n)
          }
          return nil
      case float64:
          if math.IsNaN(n) || math.IsInf(n, 0) {
              return fmt.Errorf("non-finite float64 %v", n)
          }
          return nil
      }

      // 保留原有 slice/array/map 递归逻辑
      ...
  }

  // pkg/manager/agent/specset.go

  func (s *SpecSet) Register(spec AgentSpec) error {
      if spec == nil {
          return fmt.Errorf("%w: nil spec", ErrInvalidAgentSpec)
      }

      agentType := spec.Type()
      if err := ValidateAgentType(agentType); err != nil {
          return err
      }

      if spec.DisplayName() == "" {
          return fmt.Errorf("%w: empty display name for %q", ErrInvalidAgentSpec, agentType)
      }

      if spec.PluginLayout() == nil {
          return fmt.Errorf("%w: %q", ErrInvalidPluginLayout, agentType)
      }

      if err := ValidateProtocolDescriptor(spec.ProtocolDescriptor()); err != nil {
          return err
      }

      s.mu.Lock()
      defer s.mu.Unlock()

      if _, ok := s.m[agentType]; ok {
          return fmt.Errorf("%w: %q", ErrAgentTypeConflict, agentType)
      }

      s.m[agentType] = spec
      return nil
  }

  func (s *SpecSet) Lookup(t AgentType) (AgentSpec, error) {
      spec, ok := s.Get(t)
      if !ok {
          return nil, fmt.Errorf("%w: %q", ErrAgentTypeUnknown, t)
      }
      return spec, nil
  }

  // pkg/manager/agent/claudecode/layout.go

  func ensureNoPluginNameCollision(ctx context.Context, ops infraops.InfraOps, pluginBase
  string) error {
      occupied, err := pathOccupied(ctx, ops, pluginBase)
      if err != nil {
          return fmt.Errorf("claudecode layout check plugin base %q: %w", pluginBase, err)
      }
      if occupied {
          return fmt.Errorf("%w: plugin directory collision %q", ErrInvalidPluginLayout,
  pluginBase)
      }

      manifestPath := path.Join(pluginBase, manifestFile)
      occupied, err = pathOccupied(ctx, ops, manifestPath)
      if err != nil {
          return fmt.Errorf("claudecode layout check existing plugin %q: %w", pluginBase,
  err)
      }
      if occupied {
          return fmt.Errorf("%w: plugin directory collision %q", ErrInvalidPluginLayout,
  pluginBase)
      }

      return nil
  }

  func pathOccupied(ctx context.Context, ops infraops.InfraOps, p string) (bool, error) {
      rc, err := ops.GetFile(ctx, p)
      if err == nil {
          _ = rc.Close()
          return true, nil
      }
      if errors.Is(err, fs.ErrNotExist) {
          return false, nil
      }
      if errors.Is(err, infraops.ErrNotARegularFile) {
          return true, nil // 目录或非常规节点占用
      }
      return false, err
  }

  // pkg/manager/agent/specset_test.go (核心新增断言)

  func TestValidateProtocolDescriptor_RejectsNonFiniteFloats(t *testing.T) {
      t.Parallel()

      cases := []agent.ProtocolDescriptor{
          {Kind: agent.ProtocolKindCLI, Detail: map[string]any{"x": math.NaN()}},
          {Kind: agent.ProtocolKindCLI, Detail: map[string]any{"x": math.Inf(1)}},
          {Kind: agent.ProtocolKindCLI, Detail: map[string]any{"x": []any{1, math.Inf(-1)}}},
          {Kind: agent.ProtocolKindCLI, Detail: map[string]any{"x": map[string]any{"y":
  float32(math.NaN())}}},
      }

      for _, c := range cases {
          if err := agent.ValidateProtocolDescriptor(c); err == nil {
              t.Fatalf("expected error for %v", c.Detail)
          }
      }
  }

  func TestSpecSetRegister_ErrorClassification(t *testing.T) {
      t.Parallel()

      s := agent.NewSpecSet()
      if err := s.Register(nil); !errors.Is(err, agent.ErrInvalidAgentSpec) {
          t.Fatalf("want ErrInvalidAgentSpec, got %v", err)
      }
  }

  // pkg/manager/agent/claudecode/spec_test.go (fakeInfraOps 扩展 + 新用例)

  // fakeInfraOps 增加:
  dirs map[string]struct{}

  func newFakeInfraOps() *fakeInfraOps {
      return &fakeInfraOps{
          dir:   "/tmp/fake",
          files: make(map[string][]byte),
          dirs:  make(map[string]struct{}),
      }
  }

  func (f *fakeInfraOps) markDir(p string) { f.dirs[p] = struct{}{} }

  func (f *fakeInfraOps) GetFile(ctx context.Context, path string) (io.ReadCloser, error) {
      _ = ctx
      if _, ok := f.dirs[path]; ok {
          return nil, infraops.ErrNotARegularFile
      }
      if data, ok := f.files[path]; ok {
          return io.NopCloser(bytes.NewReader(slices.Clone(data))), nil
      }
      return nil, fs.ErrNotExist
  }

  func TestPluginLayoutApply_RejectsOccupiedDirectoryWithoutManifest(t *testing.T) {
      t.Parallel()

      spec, _ := New(Options{})
      layout := spec.PluginLayout()
      ops := newFakeInfraOps()
      ops.markDir(".claude/plugins/market-research") // 仅目录占用，无 manifest

      root := pluginFS("market--research", map[string]string{
          "skills/s1/SKILL.md": "skill",
      })
      manifest := mustManifestFromFS(t, root)

      _, err := layout.Apply(context.Background(), ops, manifest, root)
      if !errors.Is(err, ErrInvalidPluginLayout) {
          t.Fatalf("want ErrInvalidPluginLayout, got %v", err)
      }
      if ops.putCalls != 0 {
          t.Fatalf("expected no writes, got %d", ops.putCalls)
      }
  }

  ## Test Plan

  - 运行命令：
      - gofmt -s -w pkg/manager/agent/types.go pkg/manager/agent/specset.go pkg/manager/
        agent/specset_test.go pkg/manager/agent/claudecode/layout.go pkg/manager/agent/
        claudecode/spec_test.go
      - go test ./pkg/manager/agent/...
      - go test -race ./pkg/manager/agent/...
      - go vet ./pkg/manager/agent/...
  - 单元测试用例清单（必须全部落地）：
      1. ValidateProtocolDescriptor 拒绝 float64 NaN。
      2. ValidateProtocolDescriptor 拒绝 float64 +Inf/-Inf。
      3. ValidateProtocolDescriptor 拒绝嵌套 []any 中的非有限浮点。
      4. ValidateProtocolDescriptor 拒绝嵌套 map[string]any 中的 float32 NaN。
      5. ValidateProtocolDescriptor 接受普通有限浮点（回归保护）。
      6. SpecSet.Register(nil) 返回 ErrInvalidAgentSpec。
      7. ValidateAgentType("bad_type") 返回 ErrInvalidAgentType。
      8. SpecSet.Lookup("missing") 仍返回 ErrAgentTypeUnknown。
      9. layout.Apply 在 pluginBase 被目录占用且无 manifest 时返回 ErrInvalidPluginLayout。
     10. 上述冲突用例中 putCalls == 0（确保写入前失败）。
  - 回归风险覆盖点：
      - 不影响已存在“manifest 冲突”行为。
      - 不影响正常插件安装/布局路径。
      - 不改变 Lookup 对外语义。

  ## Self-Check Loop

  1. 逐项对照本计划确认文件和符号都已更新，且未触碰 pkg/manager/agent 之外路径。
  2. 运行 gofmt、go vet、go test、go test -race，任一失败即回到对应代码继续修复。
  3. 手工检查 diff：
      - 错误哨兵命名是否一致。
      - Lookup 是否仍只用 ErrAgentTypeUnknown。
      - 冲突检测是否在任何 PutFile 前执行。
  4. 复查测试断言是否使用 errors.Is（避免字符串匹配）。
  5. 若实现与计划有偏差（尤其错误分类、冲突时序、非有限浮点处理），继续迭代直到代码、测试、
     diff 全部对齐。