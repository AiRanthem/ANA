# Orchestrator Leaf Packages Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement only the `pkg/orchestrator/idgen`, `pkg/orchestrator/protocol`, and `pkg/orchestrator/registry` leaf packages from the existing phase-1 design.

**Architecture:** `idgen` and `protocol` remain layer-0 pure packages with no sibling orchestrator imports. `registry` remains a layer-0 catalog that imports the canonical `pkg/agentio` contract for `AgentFactory`, but never imports root orchestrator, engine, bridge packages, prompt, invoker, or agent runtime code. Each package gets focused tests before implementation.

**Tech Stack:** Go 1.26.2, standard library only, `sync`, `sync/atomic`, `crypto/rand`, `regexp`, `errors`, and existing `github.com/AiRanthem/ANA/pkg/agentio`.

---

## File Structure

- Create `pkg/orchestrator/idgen/idgen.go`: ID types, generator interface, sequential generator, default generator.
- Create `pkg/orchestrator/idgen/idgen_test.go`: deterministic, uniqueness, category separation, concurrency tests.
- Create `pkg/orchestrator/protocol/protocol.go`: Salutation parser, formatter helpers, sentinel error.
- Create `pkg/orchestrator/protocol/protocol_test.go`: required valid, malformed, first-non-empty-line, and unknown-alias cases.
- Create `pkg/orchestrator/registry/registry.go`: workspace model, runtime-kind enum, registry interface, memory registry.
- Create `pkg/orchestrator/registry/registry_test.go`: lookup, alias collision, disabled/default behavior, failed registration rollback, concurrency tests.
- Do not modify `pkg/orchestrator/engine.go`, `pkg/orchestrator/orchestrator.go`, `pkg/orchestrator/invoker/**`, `pkg/bridge/**`, `pkg/orchestrator/prompt/**`, or `pkg/agentio/**`.
- Do not update `go.mod` or `go.sum`.
- Do not update the three package `PLAN.md` files unless implementation behavior intentionally differs from the current plan; this plan follows the current `PLAN.md` files.

## Task 1: Implement `idgen`

**Files:**
- Create: `pkg/orchestrator/idgen/idgen_test.go`
- Create: `pkg/orchestrator/idgen/idgen.go`

- [ ] **Step 1: Write the failing `idgen` tests**

Create `pkg/orchestrator/idgen/idgen_test.go` with tests named:

```go
func TestSequentialGenerator_DeterministicIDs(t *testing.T)
func TestSequentialGenerator_CategoriesDoNotCollide(t *testing.T)
func TestSequentialGenerator_ConcurrentUseIsUnique(t *testing.T)
func TestDefaultGenerator_ReturnsNonEmptyUniqueIDs(t *testing.T)
```

The tests must assert:

```go
g := NewSequential("T-")

if got, want := g.NewTaskID(), TaskID("T-0000000001"); got != want {
	t.Fatalf("NewTaskID() = %q, want %q", got, want)
}
if got, want := g.NewTaskID(), TaskID("T-0000000002"); got != want {
	t.Fatalf("NewTaskID() = %q, want %q", got, want)
}
if got, want := g.NewSessionID(), SessionID("S-0000000001"); got != want {
	t.Fatalf("NewSessionID() = %q, want %q", got, want)
}
if got, want := g.NewRequestID(), RequestID("R-0000000001"); got != want {
	t.Fatalf("NewRequestID() = %q, want %q", got, want)
}
if got, want := g.NewEventID(), "E-0000000001"; got != want {
	t.Fatalf("NewEventID() = %q, want %q", got, want)
}
```

For category collision coverage, generate one task, session, request, and event ID from one sequential generator and assert the string set has length `4`.

For default uniqueness coverage, use `NewDefault()` and generate at least 100 task IDs plus representative session, request, and event IDs. Assert no generated string is empty and no duplicate appears. Do not assert a concrete default ID format.

- [ ] **Step 2: Run the `idgen` tests and confirm they fail**

Run:

```bash
go test ./pkg/orchestrator/idgen
```

Expected: FAIL because `Generator`, `TaskID`, `SessionID`, `RequestID`, `NewSequential`, and `NewDefault` are not defined yet.

- [ ] **Step 3: Implement `idgen.go`**

Create `pkg/orchestrator/idgen/idgen.go` with this public surface and behavior:

```go
package idgen

type TaskID string
type SessionID string
type RequestID string

type Generator interface {
	NewTaskID() TaskID
	NewSessionID() SessionID
	NewRequestID() RequestID
	NewEventID() string
}
```

Implementation requirements:

- `NewSequential(prefix string) Generator` returns a concurrency-safe generator.
- `NewSequential("T-").NewTaskID()` returns `T-0000000001`, then `T-0000000002`.
- Sequential session, request, and event IDs always use `S-`, `R-`, and `E-`.
- Sequential counters are independent per category and use `atomic.Uint64`.
- `NewDefault() Generator` returns a concurrency-safe generator using only the standard library.
- The default generator must produce non-empty unique opaque strings. Use a mutex-protected monotonic `time.Now().UnixNano()` component plus `crypto/rand` entropy. On entropy failure, panic with an error that includes `idgen default entropy`.

Use this deterministic formatter:

```go
func formatSequential(prefix string, n uint64) string {
	return fmt.Sprintf("%s%010d", prefix, n)
}
```

- [ ] **Step 4: Run the `idgen` package tests**

Run:

```bash
go test ./pkg/orchestrator/idgen
```

Expected: PASS.

## Task 2: Implement `protocol`

**Files:**
- Create: `pkg/orchestrator/protocol/protocol_test.go`
- Create: `pkg/orchestrator/protocol/protocol.go`

- [ ] **Step 1: Write the failing `protocol` tests**

Create `pkg/orchestrator/protocol/protocol_test.go` with tests named:

```go
func TestParseDirective_InlinePayload(t *testing.T)
func TestParseDirective_MultilineBody(t *testing.T)
func TestParseDirective_LeadingBlankLines(t *testing.T)
func TestParseDirective_MalformedDirective(t *testing.T)
func TestParseDirective_SecondNonEmptyLineIsPlainText(t *testing.T)
func TestParseDirective_UnknownTargetReturnedAsAlias(t *testing.T)
func TestFormat(t *testing.T)
```

Required assertions:

- `"{to #Alice} check stock prices"` returns `RouteDirective{TargetAlias:"Alice", Payload:"check stock prices", IsExplicit:true, RawHeader:"{to #Alice} check stock prices"}`.
- `"{to #Alice}\nline one\nline two"` returns target `Alice` and payload `"line one\nline two"`.
- `"\n \n{to #Alice} hello"` treats `{to #Alice} hello` as the first non-empty line.
- `"{to Alice}"`, `"{to #}"`, and `"{to #Alice"` each return an error matching `ErrInvalidRouteDirective`.
- `"hello\n{to #Alice} later"` returns `IsExplicit=false`, empty `TargetAlias`, and payload equal to the original input.
- `"{to #Unknown} ping"` returns target alias `Unknown`; registry lookup is not part of this package.

- [ ] **Step 2: Run the `protocol` tests and confirm they fail**

Run:

```bash
go test ./pkg/orchestrator/protocol
```

Expected: FAIL because `RouteDirective`, `ParseDirective`, `Format`, and `ErrInvalidRouteDirective` are not defined yet.

- [ ] **Step 3: Implement `protocol.go`**

Create `pkg/orchestrator/protocol/protocol.go` with:

```go
type RouteDirective struct {
	TargetAlias string
	Payload     string
	IsExplicit  bool
	RawHeader   string
}

var ErrInvalidRouteDirective = errors.New("invalid route directive")

func ParseDirective(body string) (RouteDirective, error)
func Format(alias string) string
func ExampleHeader(alias string) string
```

Parser rules:

- Normalize CRLF and lone CR to LF for explicit directive parsing.
- Find the first line where `strings.TrimSpace(line) != ""`.
- If no non-empty line exists, return `RouteDirective{Payload: body}` with no error.
- Match only the first non-empty line against `^\s*\{to\s+#([A-Za-z0-9_-]{1,64})\}\s*(.*)$`.
- If it matches, return:
  - `TargetAlias` from the first capture group.
  - `RawHeader` equal to the first non-empty line after newline normalization, without newline bytes.
  - `IsExplicit=true`.
  - `Payload` equal to the inline capture, the remaining body after the directive line, or both joined by one `\n`.
- If it does not match and the first non-empty line begins with `{to` after leading whitespace and case-folding, return `ErrInvalidRouteDirective`.
- If it does not look like an attempted directive, return `RouteDirective{Payload: body}` with no error.
- Do not import `registry` or any sibling orchestrator package.

- [ ] **Step 4: Run the `protocol` package tests**

Run:

```bash
go test ./pkg/orchestrator/protocol
```

Expected: PASS.

## Task 3: Implement `registry`

**Files:**
- Create: `pkg/orchestrator/registry/registry_test.go`
- Create: `pkg/orchestrator/registry/registry.go`

- [ ] **Step 1: Write the failing `registry` tests**

Create `pkg/orchestrator/registry/registry_test.go` with tests named:

```go
func TestMemoryRegistry_RegisterAndLookupByID(t *testing.T)
func TestMemoryRegistry_LookupByAlias(t *testing.T)
func TestMemoryRegistry_AliasCollisionLeavesStateUnchanged(t *testing.T)
func TestMemoryRegistry_DisabledWorkspaceLookup(t *testing.T)
func TestMemoryRegistry_DefaultWorkspace(t *testing.T)
func TestMemoryRegistry_MultipleDefaultRegistrationsDemotePrevious(t *testing.T)
func TestMemoryRegistry_ConcurrentRegisterAndLookup(t *testing.T)
func TestMemoryRegistry_FailedRegistrationLeavesRegistryUnchanged(t *testing.T)
```

Test helper shape:

```go
func testWorkspace(id, alias string) Workspace {
	return Workspace{
		WorkspaceID:   id,
		Alias:         alias,
		RuntimeType:   "cli",
		RuntimeKind:   RuntimeKindResumableCLI,
		Description:   "test workspace",
		Enabled:       true,
		RuntimeConfig: map[string]any{"path": "/tmp/agent"},
	}
}

func testFactory(context.Context, Workspace) (agentio.Agent, error) {
	return stubAgent{name: "test-agent"}, nil
}

type stubAgent struct {
	name string
}

func (a stubAgent) Name() string { return a.name }
func (a stubAgent) Invoke(context.Context, *agentio.InvokeRequest) (agentio.EventStream, error) {
	return nil, errors.New("stub agent is not invoked by registry tests")
}
```

Required assertions:

- Lookup by ID and alias returns a copy of the registered workspace and a non-nil factory.
- Alias collision returns an error matching `ErrAliasConflict`, keeps the original alias mapping, and does not add the failed workspace ID.
- Disabled workspace lookup by ID and alias returns `ErrWorkspaceDisabled`.
- `Default(ctx)` returns the explicit default workspace and factory.
- Registering a second default demotes the previous default deterministically; the latest successful default is returned and the older workspace is still lookup-able with `IsDefaultEntry=false`.
- Concurrent register and lookup uses unique IDs and aliases and passes under `go test -race`.
- A failed registration with duplicate alias and `IsDefaultEntry=true` does not alter the previous default.

- [ ] **Step 2: Run the `registry` tests and confirm they fail**

Run:

```bash
go test ./pkg/orchestrator/registry
```

Expected: FAIL because `Workspace`, `RuntimeKindResumableCLI`, `NewMemory`, `ErrAliasConflict`, and related registry symbols are not defined yet.

- [ ] **Step 3: Implement `registry.go`**

Create `pkg/orchestrator/registry/registry.go` with this public surface:

```go
type RuntimeKind string

const (
	RuntimeKindChatAPI       RuntimeKind = "chat_api"
	RuntimeKindResumableCLI  RuntimeKind = "resumable_cli"
	RuntimeKindSocketSession RuntimeKind = "socket_session"
)

type Workspace struct {
	WorkspaceID    string
	Alias          string
	RuntimeType    string
	RuntimeKind    RuntimeKind
	Description    string
	Enabled        bool
	IsDefaultEntry bool
	RuntimeConfig  map[string]any
}

type AgentFactory func(ctx context.Context, ws Workspace) (agentio.Agent, error)

type Registry interface {
	Register(ctx context.Context, ws Workspace, factory AgentFactory) error
	Update(ctx context.Context, ws Workspace) error
	Disable(ctx context.Context, workspaceID string) error
	Enable(ctx context.Context, workspaceID string) error
	LookupByAlias(ctx context.Context, alias string) (Workspace, AgentFactory, error)
	LookupByID(ctx context.Context, workspaceID string) (Workspace, AgentFactory, error)
	Default(ctx context.Context) (Workspace, AgentFactory, error)
	List(ctx context.Context) ([]Workspace, error)
}
```

Sentinel errors:

```go
var (
	ErrAliasNotFound     = errors.New("alias not found")
	ErrWorkspaceNotFound = errors.New("workspace not found")
	ErrWorkspaceDisabled = errors.New("workspace disabled")
	ErrAliasConflict     = errors.New("alias conflict")
	ErrInvalidWorkspace  = errors.New("invalid workspace")
	ErrNoDefaultWorkspace = errors.New("no default workspace")
)
```

Implementation requirements:

- Provide `func NewMemory() *MemoryRegistry`.
- Store `byID map[string]Workspace`, `byAlias map[string]string`, `factories map[string]AgentFactory`, and `defaultID string` under one `sync.RWMutex`.
- Validate before mutating state:
  - `WorkspaceID` non-empty after trimming for validation.
  - `Alias` trimmed before storage, non-empty, length 1..64, matching `^[A-Za-z0-9_-]+$`.
  - `RuntimeType` non-empty after trimming for validation.
  - `RuntimeKind` one of the three constants.
  - `Description` has at most 256 runes.
  - `factory` is non-nil for `Register`.
- `Register` must be transactional: validation and collision checks complete before any map write.
- Duplicate workspace ID returns `ErrInvalidWorkspace` with context.
- Duplicate alias returns `ErrAliasConflict` and leaves all maps and default state unchanged.
- New default registration demotes the previous default by setting its stored `IsDefaultEntry=false`, then sets `defaultID` to the new workspace ID.
- `LookupByID`, `LookupByAlias`, and `Default` return `ErrWorkspaceDisabled` for disabled workspaces.
- `Disable` and `Enable` are idempotent for existing workspace IDs.
- `List` returns all workspaces, enabled and disabled, sorted by alias for deterministic output.
- Return cloned `Workspace` values with cloned `RuntimeConfig` maps.
- Wrap stored factories so a factory returning `(nil, nil)` returns `ErrInvalidWorkspace` when invoked.
- Do not import `pkg/bridge/...`, root `pkg/orchestrator`, `protocol`, `prompt`, `invoker`, or `agentio` subpackages other than canonical `pkg/agentio`.

- [ ] **Step 4: Run the `registry` package tests**

Run:

```bash
go test ./pkg/orchestrator/registry
```

Expected: PASS.

## Task 4: Package Formatting And Focused Verification

**Files:**
- Verify only: `pkg/orchestrator/idgen/**`
- Verify only: `pkg/orchestrator/protocol/**`
- Verify only: `pkg/orchestrator/registry/**`

- [ ] **Step 1: Format the three leaf packages**

Run:

```bash
gofmt -s -w pkg/orchestrator/idgen pkg/orchestrator/protocol pkg/orchestrator/registry
```

Expected: no output.

- [ ] **Step 2: Run focused package tests**

Run:

```bash
go test ./pkg/orchestrator/idgen ./pkg/orchestrator/protocol ./pkg/orchestrator/registry
```

Expected: PASS for all three packages.

- [ ] **Step 3: Run focused race tests**

Run:

```bash
go test -race ./pkg/orchestrator/idgen ./pkg/orchestrator/protocol ./pkg/orchestrator/registry
```

Expected: PASS for all three packages.

## Task 5: Full Repository Verification

**Files:**
- Verify only: entire repository

- [ ] **Step 1: Run all tests**

Run:

```bash
go test ./...
```

Expected: PASS.

- [ ] **Step 2: Run vet**

Run:

```bash
go vet ./...
```

Expected: PASS.

- [ ] **Step 3: Run build**

Run:

```bash
go build ./...
```

Expected: PASS.

- [ ] **Step 4: Confirm scope**

Run:

```bash
git status --short
```

Expected tracked changes are limited to:

```text
pkg/orchestrator/idgen/idgen.go
pkg/orchestrator/idgen/idgen_test.go
pkg/orchestrator/protocol/protocol.go
pkg/orchestrator/protocol/protocol_test.go
pkg/orchestrator/registry/registry.go
pkg/orchestrator/registry/registry_test.go
```

This planning document may also appear if the implementation session includes the planning artifact. Existing unrelated `.gitignore` changes must remain untouched and must not be reverted.

## Self-Review Checklist

- Spec coverage: the plan covers all required `idgen`, `protocol`, and `registry` acceptance cases from `.pro/pro_context.md`.
- Dependency coverage: `idgen` and `protocol` have no orchestrator sibling imports; `registry` imports only canonical `agentio` from existing code, never bridge.
- Scope coverage: root engine, orchestrator, invoker, prompt, bridge, and agentio files are excluded.
- Test coverage: focused tests, race tests, full tests, vet, and build are explicit.
- Red-flag scan: no implementation step contains unresolved filler text.
- Type consistency: names match `PLAN.md` and `DESIGN.md`: `TaskID`, `SessionID`, `RequestID`, `RouteDirective`, `RuntimeKind`, `Workspace`, `AgentFactory`, `Registry`, and the sentinel errors.
