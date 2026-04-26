# infraops/PLAN.md

## Purpose

Define the **`InfraOps` interface** — the abstraction over a workspace's
runtime environment — plus the supporting types (`Options`,
`ExecCommand`, `ExecResult`, `Factory`, `FactorySet`) and the sentinel
errors. Concrete implementations (local directory, Docker, E2B, …) live
in subpackages.

Every interaction with a workspace's filesystem or processes — install
worker writing files, probe scheduler exec'ing a version check, the
operator reading a log — flows through `InfraOps`. Picking the right
abstraction shape is what lets future infras (Docker, E2B, serverless)
plug in without churn.

## Public surface (intent only)

- Types:
  - `InfraType` — string alias re-exported from manager root.
  - `Options` — `map[string]any` value-type alias; JSON-serializable.
  - `ExecCommand`, `ExecResult` (§3 below).
  - `InfraOps` interface (`DESIGN.md` §7.1, repeated below).
  - `Factory` function type — constructs an `InfraOps` from
    `Options`.
- `FactorySet` helper:
  - `NewFactorySet() *FactorySet`
  - `(s *FactorySet) Register(infraType InfraType, f Factory) error`
    — duplicate registrations return `ErrInfraTypeConflict`.
  - `(s *FactorySet) Get(infraType InfraType) (Factory, bool)`
  - `(s *FactorySet) Types() []InfraType`
- Sentinels:
  - `ErrInfraTypeUnknown`
  - `ErrInfraTypeConflict`
  - `ErrAlreadyInitialized`
  - `ErrNotInitialized`
  - `ErrInvalidOption`
  - `ErrPathOutsideDir`
  - `ErrNotARegularFile` (returned by `GetFile` when the path is a
    directory or special file)
  - `ErrUnsupportedRequest`
  - `ErrCleared` (returned by ops on a torn-down infra)

## Interface (verbatim)

```go
type InfraOps interface {
    Type() InfraType
    Dir() string

    Init(ctx context.Context) error
    Open(ctx context.Context) error

    Exec(ctx context.Context, cmd ExecCommand) (ExecResult, error)

    PutFile(ctx context.Context, path string, content io.Reader, mode fs.FileMode) error
    GetFile(ctx context.Context, path string) (io.ReadCloser, error)

    Request(ctx context.Context, port int, req *http.Request) (*http.Response, error)

    Clear(ctx context.Context) error
}
```

`Type()` returns the same id used to register the factory; the manager
relies on this for crash-recovery checks ("the persisted type matches
the implementation we just constructed").

`Dir()` returns the absolute working directory **as it appears inside
the infra**. For `localdir` this is the host path; for Docker it is
the container path; for E2B it is the sandbox path. The manager passes
this to `agent.PluginLayout.Apply` for diagnostics only — file
operations always go through `PutFile`/`GetFile`.

## ExecCommand & ExecResult

```go
type ExecCommand struct {
    Program string            // required; not parsed as a shell line
    Args    []string          // verbatim; quoting is the implementation's job
    Env     []string          // KEY=VALUE pairs appended to base env
    Stdin   io.Reader         // optional
    WorkDir string            // optional; relative to Dir(); empty = Dir()
    Stdout  io.Writer         // optional; if set, the implementation streams to it and ExecResult.Stdout is empty
    Stderr  io.Writer         // optional; same behavior as Stdout
    Timeout time.Duration     // optional; 0 = use ctx deadline
}

type ExecResult struct {
    ExitCode int
    Stdout   []byte // empty when caller supplied a Stdout writer
    Stderr   []byte // empty when caller supplied a Stderr writer
    Duration time.Duration
}
```

Behavior rules:

- `Program` MUST be non-empty. The implementation does NOT parse it
  as a shell command line; that is the caller's responsibility (use
  `Program: "sh", Args: []string{"-c", "..."}` for shell features).
- `Env` is appended to the base environment (which is
  implementation-defined; localdir inherits the manager process env;
  containerized infras define their own base env).
- `WorkDir`, when set, is resolved relative to `Dir()`. Absolute paths
  or paths escaping `Dir()` (resolves outside via `..`) return
  `ErrPathOutsideDir` before exec.
- `Timeout`, if positive, overlays a child context with this
  deadline. The earlier of `ctx.Deadline()` and `Timeout` wins.
- A non-zero `ExitCode` is **not** an error — `Exec` returns
  `(ExecResult{ExitCode: N, …}, nil)`. Errors in `Exec` are reserved
  for operational failures (program not found, IO failure, ctx
  cancelled). Callers branch on `ExitCode` for application logic.
- Streaming `Stdout`/`Stderr` and the buffered `[]byte` fields are
  mutually exclusive per stream: if the caller supplies a writer for
  one stream, the corresponding result slice is empty (and vice
  versa).

## File operations

```go
PutFile(ctx, path, content, mode) error
GetFile(ctx, path) (io.ReadCloser, error)
```

Behavior rules:

- `path` is interpreted as relative to `Dir()`. Absolute paths or
  paths escaping `Dir()` return `ErrPathOutsideDir`.
- Intermediate directories are created with mode `0755` for
  `PutFile`. `mode` applies to the resulting file.
- Existing files at `path` are overwritten atomically (write to a
  temp sibling then rename). Implementations document deviations
  per-backend in their PLAN.
- Symlinks at `path` are followed (the file at the symlink target is
  replaced). Symlinks **inside** the prefix are otherwise unsupported
  (most infras forbid them anyway).
- `GetFile` returns a stream the caller MUST close. The stream may
  outlive the `InfraOps` for `localdir`; for container backends, the
  stream MUST keep the connection alive until close.

`PutFile` is the workhorse for plugin layout: an `agent.PluginLayout`
walks the plugin's `fs.FS` and calls `PutFile` per entry. There is no
batch upload in v1; backends that benefit from batching wrap the
interface in their own helper.

## Network operations

```go
Request(ctx, port, req) (*http.Response, error)
```

Behavior rules:

- `port` is an inside-the-infra port number. Implementations resolve
  it to a host:port reachable from the manager process — for
  `localdir` it is `127.0.0.1:<port>`; for Docker it is the published
  port (or the bridge IP); for E2B it is the tunneled URL.
- `req.URL.Host` is overwritten by the resolved address; any value
  the caller passed is ignored for routing. `req.URL.Scheme` defaults
  to `http`; HTTPS requires the implementation to expose a separate
  `Dial` extension (out of v1).
- The caller supplies a fully-formed `*http.Request` (method, path,
  body, headers). The implementation passes it through; no header
  rewriting beyond `Host`.
- Returns the `*http.Response` from the underlying client. The caller
  MUST close `resp.Body`.
- Implementations that cannot expose HTTP at all return
  `ErrUnsupportedRequest`. The manager treats this as a probe failure.

WebSocket / arbitrary TCP / gRPC are deferred. They will be added as
extension methods (e.g., `Dial(ctx, port) (net.Conn, error)`) detected
via type assertion when needed. Adding a method to `InfraOps` itself
breaks every implementation, so we keep the surface tight.

## Lifecycle

```
        Factory(opts)
           │
           ▼
   ┌────────────────┐
   │  constructed   │   Type(), Dir() OK
   └──────┬─────────┘
          │ Init()  (create-time)  or  Open()  (attach to existing state)
   ┌──────▼─────────┐
   │ initialized    │   all methods OK
   └──────┬─────────┘
          │ Clear()
   ┌──────▼─────────┐
   │   cleared      │   only Type(), Dir() OK; other ops return ErrCleared
   └────────────────┘
```

Rules:

- `Init()` or `Open()` is required before `Exec`/`PutFile`/`GetFile`/`Request`.
  Calling those before initialization returns `ErrNotInitialized`.
- `Open()` attaches to existing backing state and must not create or clear
  it. The probe scheduler calls `Open` before `Probe` on a fresh factory
  instance; installs use `Init` on a new directory then continue without
  calling `Open` again on the same instance.
- `Init()` is idempotent in the *constructive* sense only: if the
  backing state already exists from a previous run, `Init` returns
  `ErrAlreadyInitialized`. The workspace controller catches this on
  a deliberate-resume path; routine flows treat it as a hard error.
- `Clear()` is idempotent and safe to call from any post-`Init`
  state. After `Clear`, the same `InfraOps` instance is dead;
  callers obtain a fresh instance from the factory if they want to
  re-init.
- The same `Options` MAY be passed to `Factory` again after `Clear`;
  the new instance starts in the constructed state.

### Empty-directory invariant

After `Init` returns nil, `Dir()` exists and contains no files. If
`Init` finds `Dir()` non-empty, it returns `ErrAlreadyInitialized`
without touching the contents. The workspace controller relies on
this for crash safety: if it sees `ErrAlreadyInitialized`, the
previous attempt already touched the directory and the workspace
must transition to `failed`.

## Factory

```go
type Factory func(ctx context.Context, opts Options) (InfraOps, error)
```

Required option for every factory:

| Key   | Type     | Notes                                |
|-------|----------|--------------------------------------|
| `dir` | `string` | Absolute working directory path.     |

Per-implementation options are documented in that implementation's
PLAN. Examples: `"image"` for Docker, `"template_id"` for E2B,
`"keep_dir"` for `localdir`.

Factories MUST be cheap: do real work in `Init`/`Open`, not in the factory.
The manager constructs `InfraOps` per probe tick and calls `Open` before probing.

### FactorySet

```go
type FactorySet struct {
    // unexported map[InfraType]Factory under sync.RWMutex
}

func NewFactorySet() *FactorySet
func (s *FactorySet) Register(infraType InfraType, f Factory) error
func (s *FactorySet) Get(infraType InfraType) (Factory, bool)
func (s *FactorySet) Types() []InfraType
```

The Manager `Builder` uses `FactorySet` internally; consumers may
construct one directly for tests.

## Edge cases & decisions

- **Caller passes shell metachars in `Program`.** Allowed; the
  implementation does no parsing. If the program name itself contains
  spaces, that is the operator's choice and the implementation honors
  it.
- **`PutFile` against a non-existent intermediate directory.**
  Created with `0755`. To use a different directory mode, write the
  directory's `.keep` file first with the desired mode (then `Exec`
  `chmod` if the implementation does not honor the directory mode).
- **`GetFile` on a directory.** Returns `ErrNotARegularFile` (an
  exported sentinel in this package). No tar streaming in v1 — call
  sites that need bulk transfer construct an `ExecCommand` for
  `tar -cf` and read the resulting archive via a follow-up
  `GetFile`.
- **`Request` while the daemon is not yet listening.** Returns the
  underlying connection error wrapped in
  `fmt.Errorf("infraops request: %w", err)`. Probes use this to
  detect liveness.
- **Two concurrent `PutFile` to the same path.** Last writer wins;
  atomicity is per-call (temp+rename), so readers always see one
  consistent file.
- **Init followed by Clear, then Init again on the same instance.**
  Disallowed; the cleared instance is dead. Callers create a new
  one. This keeps state machines simple.

## Tests to write (no implementation in this pass)

1. `FactorySet.Register` rejects duplicates with
   `ErrInfraTypeConflict`.
2. A fake `InfraOps` reaches `ErrNotInitialized` for ops invoked
   before `Init`.
3. `ExecCommand` validation: empty `Program`, escaping `WorkDir`
   produce `ErrPathOutsideDir`.
4. `ExecResult` non-zero `ExitCode` is not an error; an unknown
   program **is**.
5. `PutFile` overwrite is atomic — concurrent reader sees either old
   or new content, never partial.
6. `Clear` is idempotent.
7. `Factory` is cheap: a stub factory called 1000× completes within
   a small budget (regression test against accidental I/O at
   construct time).

## Out of scope

- Concrete implementations beyond `localdir`. Docker / E2B / serverless
  are separate subpackages added later.
- Bulk file transfer (`PutTree` / `GetTree`). Operators use `Exec`
  with `tar` for now; a future extension may add tree primitives.
- WebSocket / TCP `Dial`. Extension-interface territory; documented
  but not implemented in v1.
- Capability negotiation (e.g., asking the infra "do you support
  GPU?"). Future via type-assertion-checked extension interfaces.
