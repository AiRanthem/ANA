# Plugin 模块审查修复交接计划

## Objective
- 用户目标
  - 仅修复 `pkg/manager/plugin` 目录内已识别的实现风险，产出可由其他 Agent 直接落地的方案。
- Non-goals
  - 不修改 `pkg/manager/plugin` 目录外代码。
  - 不调整对外接口形状（`Repository`/`Storage`/`Reader` 已有签名保持不变）。
  - 不引入新三方依赖。
- Success criteria
  - zip 读取拒绝重复路径 entry。
  - 路径校验规则一致且可预测（不允许 `.` / `..` 段混入）。
  - `Reader.FS()` 保留 zip entry 的关键 mode 语义（目录位、可执行位）。
  - `MemoryStorage.List` 结果稳定有序。
  - 新增/更新测试覆盖上述行为，`go test -race` 与 `go vet` 通过。

## Relevant Context
- 目录与文件（仅本目录）
  - `pkg/manager/plugin/reader_zip.go`
    - `OpenZipReader`
    - `OpenZipReaderFromStream`
    - `openFromZip`
    - `validateZipFileHeader`
    - `validateManifestArchivePaths`
  - `pkg/manager/plugin/manifest.go`
    - `isSafeArchivePath`
  - `pkg/manager/plugin/memory_storage.go`
    - `List`
  - `pkg/manager/plugin/reader_zip_test.go`
  - `pkg/manager/plugin/manifest_test.go`
  - `pkg/manager/plugin/memory_test.go`
- 需要遵循的现有约束
  - `pkg/manager/plugin/AGENTS.md`：
    - `OpenZipReader*` 为纯函数（除受控缓冲）。
    - 并发语义需可在 `-race` 下验证。
  - `pkg/manager/plugin/PLAN.md`：
    - 归档路径必须相对、无逃逸段。
    - symlink 禁止。
    - 读取后应为布局层提供稳定、可验证的文件系统视图。
- 已确认现状
  - 当前测试可通过，但未覆盖重复 zip entry、mode 保留、`List` 有序等关键语义。

## Design Decisions
- 决策 1：在 zip 扫描阶段按 canonical 路径做“强唯一”检查
  - 选择：在 `openFromZip` 中对每个 entry 名称先 canonicalize，再做重复检测；重复即 `ErrCorruptArchive`。
  - 原因：消除同名覆盖歧义，避免“后写覆盖前写”导致的不确定行为。
  - 放弃方案：继续允许重复并取最后一个。该方案行为不可审计且不安全。
- 决策 2：统一路径合法性规则，禁止 raw path 中 `.` 与 `..`
  - 选择：收紧 `isSafeArchivePath`，显式拒绝任一段为 `"."` 或 `".."`。
  - 原因：避免 raw path 与 clean path 混用导致的语义漂移。
  - 不变量：所有参与比较的路径均为同一 canonical 形式。
- 决策 3：`Reader.FS()` 保留 mode 的核心语义
  - 选择：写入 `fstest.MapFile.Mode` 时基于 `zip.File.Mode()`，保留：
    - `fs.ModeDir`（目录）
    - 文件 owner/group/other 执行位（`0o111`）
    - 读写位按 zip 提供值继承（兜底 regular file 至少 `0o644`，目录至少 `0o755`）。
  - 原因：布局层需要决定是否沿用执行位；全部写死 `0644` 会丢失语义。
- 决策 4：`MemoryStorage.List` 输出排序
  - 选择：对 `[]PluginID` 做字典序排序后返回。
  - 原因：避免 map 迭代随机性影响测试与上层对账流程。
- 边界与错误处理
  - 重复 entry、非法路径、symlink 都归类为 `ErrCorruptArchive`。
  - manifest 与 archive 路径不匹配归类为 `ErrInvalidManifest`。

## Implementation Plan
1. 更新 `pkg/manager/plugin/manifest.go` 的 `isSafeArchivePath`。
   - 修改点：在分段循环中同时拒绝 `"."`、`".."`、空段（保留对首尾斜杠和反斜杠的拒绝）。
   - 完成信号：`isSafeArchivePath("skills/./x") == false`，`isSafeArchivePath("skills/../x") == false`，正常路径仍通过。
2. 更新 `pkg/manager/plugin/reader_zip.go` 的路径 canonical 与重复检测。
   - 修改点：
     - 新增私有函数 `canonicalArchivePath(p string) (string, error)`，内部复用 `isSafeArchivePath` 与 `path.Clean`。
     - `openFromZip` 遍历 entry 时：
       - 将 `f.Name` canonicalize。
       - 用 `seen map[string]struct{}` 做重复检测，重复则返回 `ErrCorruptArchive`。
       - `fileSet` 与 `filesys` 一律使用 canonical 路径作为 key。
   - 完成信号：包含重复 entry 的 zip 在 `OpenZipReader` 返回 `ErrCorruptArchive`。
3. 更新 `pkg/manager/plugin/reader_zip.go` 的 mode 保留策略。
   - 修改点：
     - 新增私有函数 `mapFileModeFromZip(f *zip.File) fs.FileMode`。
     - 写入 `fstest.MapFile{Mode: ...}` 时使用该函数，不再写死 `0o644`。
   - 完成信号：zip 中可执行文件在 `Reader.FS().Open(...).Stat().Mode()` 仍带执行位。
4. 更新 `pkg/manager/plugin/reader_zip.go` 的 manifest 路径比对逻辑。
   - 修改点：
     - `validateManifestArchivePaths` 内对 `entry.Path` 先 canonicalize，再进行“文件命中或目录前缀命中”判断。
     - canonicalize 失败时累积为 `ErrInvalidManifest` 子错误。
   - 完成信号：manifest 使用 `skills/./s1` 会被拒绝（`ErrInvalidManifest`）。
5. 更新 `pkg/manager/plugin/memory_storage.go` 的 `List`。
   - 修改点：`out` 填充后调用 `slices.SortFunc` 或 `sort.Slice` 按字符串升序排序。
   - 完成信号：同一集合多次 `List` 返回顺序稳定。
6. 补全测试（仅本目录测试文件）。
   - `reader_zip_test.go`
     - 新增“重复 entry 拒绝”用例。
     - 新增“dot segment 拒绝”用例（zip entry 与 manifest path 两类）。
     - 新增“执行位保留”用例。
   - `manifest_test.go`
     - 补充 `skills/./x` 非法路径校验用例。
   - `memory_test.go`
     - 新增 `MemoryStorage.List` 有序返回用例。
   - 完成信号：新增用例稳定通过。

## Required Code Snippets
```go
// pkg/manager/plugin/reader_zip.go
func canonicalArchivePath(p string) (string, error) {
	if !isSafeArchivePath(p) {
		return "", fmt.Errorf("%w: invalid archive path %q", ErrCorruptArchive, p)
	}
	cp := path.Clean(p)
	// isSafeArchivePath 已保证 cp 不是 "." "/" ".." 及逃逸形式
	return cp, nil
}
```

```go
// pkg/manager/plugin/reader_zip.go (openFromZip 内核心分支)
seen := make(map[string]struct{}, len(zr.File))
fileSet := make(map[string]struct{}, len(zr.File))
filesys := make(fstest.MapFS, len(zr.File))

for _, f := range zr.File {
	if err := validateZipFileHeader(f); err != nil {
		return nil, err
	}
	canonicalName, err := canonicalArchivePath(f.Name)
	if err != nil {
		return nil, err
	}
	if _, exists := seen[canonicalName]; exists {
		return nil, fmt.Errorf("%w: duplicate zip path %q", ErrCorruptArchive, canonicalName)
	}
	seen[canonicalName] = struct{}{}
	fileSet[canonicalName] = struct{}{}

	rc, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("%w: read %q: %v", ErrCorruptArchive, f.Name, err)
	}
	// 保持现有 budget/ReadAll/close 逻辑
	// ...

	if canonicalName == manifestPathDefault {
		manifestBytes = bytes.Clone(body)
	}
	filesys[canonicalName] = &fstest.MapFile{
		Data: body,
		Mode: mapFileModeFromZip(f),
	}
}
```

```go
// pkg/manager/plugin/reader_zip.go
func mapFileModeFromZip(f *zip.File) fs.FileMode {
	mode := f.Mode()
	if mode&fs.ModeDir != 0 {
		perm := mode.Perm()
		if perm == 0 {
			perm = 0o755
		}
		return fs.ModeDir | perm
	}
	perm := mode.Perm()
	if perm == 0 {
		perm = 0o644
	}
	return perm
}
```

```go
// pkg/manager/plugin/manifest.go (isSafeArchivePath 关键分支)
for _, seg := range strings.Split(p, "/") {
	if seg == "" || seg == "." || seg == ".." {
		return false
	}
}
```

```go
// pkg/manager/plugin/reader_zip.go (validateManifestArchivePaths 中)
check := func(section Section, entries map[string]ManifestEntry) {
	for key, entry := range entries {
		cleanPath, err := canonicalManifestPath(entry.Path)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s.%s.path %q is invalid", section, key, entry.Path))
			continue
		}
		if _, ok := fileSet[cleanPath]; ok {
			continue
		}
		prefix := strings.TrimSuffix(cleanPath, "/") + "/"
		found := false
		for candidate := range fileSet {
			if strings.HasPrefix(candidate, prefix) {
				found = true
				break
			}
		}
		if !found {
			errs = append(errs, fmt.Errorf("%s.%s.path %q does not exist in archive", section, key, entry.Path))
		}
	}
}
```

```go
// pkg/manager/plugin/memory_storage.go
func (s *MemoryStorage) List(_ context.Context) ([]PluginID, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, ErrStorageClosed
	}
	out := make([]PluginID, 0, len(s.blobs))
	for id := range s.blobs {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool {
		return string(out[i]) < string(out[j])
	})
	return out, nil
}
```

```go
// pkg/manager/plugin/reader_zip_test.go
func TestOpenZipReader_RejectsDuplicateEntries(t *testing.T) {
	t.Parallel()
	zipBytes := buildZipWithDuplicate(t,
		zipEntry{name: "manifest.toml", body: []byte(validManifest("demo", "skills/s1"))},
		zipEntry{name: "manifest.toml", body: []byte(validManifest("demo2", "skills/s1"))},
		zipEntry{name: "skills/s1/SKILL.md", body: []byte("# skill")},
	)
	_, err := OpenZipReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if !errors.Is(err, ErrCorruptArchive) {
		t.Fatalf("OpenZipReader() error = %v, want ErrCorruptArchive", err)
	}
}
```

```go
// pkg/manager/plugin/reader_zip_test.go
func TestOpenZipReader_PreservesExecutableBit(t *testing.T) {
	t.Parallel()
	zipBytes := buildZipWithMode(t, []zipEntry{
		{name: "manifest.toml", body: []byte(validManifest("demo", "skills/s1")), mode: 0o644},
		{name: "skills/s1/run.sh", body: []byte("#!/bin/sh\necho hi\n"), mode: 0o755},
	})
	r, err := OpenZipReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		t.Fatalf("OpenZipReader() error = %v", err)
	}
	defer r.Close()

	info, err := fs.Stat(r.FS(), "skills/s1/run.sh")
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("mode = %v, want executable bit", info.Mode())
	}
}
```

```go
// pkg/manager/plugin/memory_test.go
func TestMemoryStorage_List_Sorted(t *testing.T) {
	t.Parallel()
	st := NewMemoryStorage()
	ids := []PluginID{"plg_c", "plg_a", "plg_b"}
	for _, id := range ids {
		if _, err := st.Put(context.Background(), id, bytes.NewReader([]byte(string(id)))); err != nil {
			t.Fatalf("Put(%s) error = %v", id, err)
		}
	}
	got, err := st.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	want := []PluginID{"plg_a", "plg_b", "plg_c"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("List() = %v, want %v", got, want)
	}
}
```

## Test Plan
- 命令
  - `gofmt -s -w pkg/manager/plugin/*.go`
  - `go test ./pkg/manager/plugin`
  - `go test -race ./pkg/manager/plugin`
  - `go vet ./pkg/manager/plugin`
- 必须具备的单测用例清单
  - `TestOpenZipReader_RejectsDuplicateEntries`
    - 构造同路径重复 entry（至少重复 `manifest.toml`）。
    - 断言 `errors.Is(err, ErrCorruptArchive)`。
  - `TestOpenZipReader_RejectsDotSegmentPathInArchive`
    - zip entry 使用 `skills/./s1/SKILL.md`。
    - 断言 `ErrCorruptArchive`。
  - `TestOpenZipReader_RejectsDotSegmentPathInManifest`
    - manifest `path = "skills/./s1"`，archive 中存在 `skills/s1/...`。
    - 断言 `ErrInvalidManifest`（路径本身非法）。
  - `TestOpenZipReader_PreservesExecutableBit`
    - entry mode 为 `0755`。
    - 断言 `fs.Stat(...).Mode().Perm() & 0111 != 0`。
  - `TestValidateManifest_Errors` 增补子用例
    - `path = "skills/./x"` 应命中 `invalid`。
  - `TestMemoryStorage_List_Sorted`
    - 插入乱序 ID。
    - `List` 返回严格升序。
- 核心测试逻辑补充
  - `reader_zip_test.go` 增加 `buildZipWithDuplicate` helper（使用 `zip.NewWriter` + `CreateHeader` 顺序写入，允许重名）。
  - `reader_zip_test.go` 增加 `buildZipWithMode` helper（可设置 mode）。
  - 对错误断言统一使用 `errors.Is`，避免字符串脆弱匹配。
- 回归风险覆盖
  - 防止修复后误伤正常 archive：保留现有 `RoundTripFS` 用例并验证继续通过。
  - 防止 size budget 逻辑退化：保留现有 budget/maxSize 用例。

## Self-Check Loop
1. 对照本计划逐条核对：确认只改动以下文件
   - `pkg/manager/plugin/manifest.go`
   - `pkg/manager/plugin/reader_zip.go`
   - `pkg/manager/plugin/memory_storage.go`
   - `pkg/manager/plugin/manifest_test.go`
   - `pkg/manager/plugin/reader_zip_test.go`
   - `pkg/manager/plugin/memory_test.go`
2. 运行格式化与静态检查
   - `gofmt -s -w pkg/manager/plugin/*.go`
   - `go vet ./pkg/manager/plugin`
3. 运行测试
   - `go test ./pkg/manager/plugin`
   - `go test -race ./pkg/manager/plugin`
4. 若失败则按失败类别迭代
   - 路径相关失败：优先检查 canonicalize 与 `isSafeArchivePath` 是否一致。
   - mode 相关失败：检查 zip mode 到 `MapFile.Mode` 映射是否保留执行位/目录位。
   - 顺序相关失败：检查 `List` 是否稳定排序。
5. 人工 diff 复核
   - 确认无目录外变更。
   - 确认无接口签名漂移、无额外行为扩散。
6. 完成判定
   - 仅当上述命令全部通过，且行为与本计划完全一致，才结束任务。

