# infraops/localdir/PLAN.md

## Purpose

Reference `infraops.InfraOps` implementation that targets a **local
directory on the host running the manager**. It is the simplest infra
backend and the basis for unit tests, single-machine deployments, and
the early phases of multi-tenant rollouts before container backends
land.

## Public surface (intent only)

- `New(opts infraops.Options) (infraops.InfraOps, error)` — registered
  with the manager via
  `Builder.RegisterInfraType("localdir", localdir.New)`.
- `Type() == "localdir"` for any constructed instance.
- Sentinels:
  - `ErrInvalidDir` (wrapping `infraops.ErrInvalidOption`)
  - `ErrDirNotEmpty`
- No long-lived resources beyond the absolute path; instances are
  cheap value-types backed by an `*os.Root` (Go 1.24+) for safe path
  resolution.

## Options

```go
infraops.Options{
    "dir":      "/var/lib/ana/workspaces/<alias>",  // REQUIRED, absolute
    "keep_dir": true,                               // OPTIONAL, default false
    "umask":    0o022,                              // OPTIONAL, default 0o022
    "base_env": []string{"LANG=C.UTF-8"},           // OPTIONAL; appended to inherited env
    "request_loopback_host": "127.0.0.1",           // OPTIONAL; default 127.0.0.1
}
```

Validation:

- `dir` MUST be a non-empty absolute path. The factory returns
  `ErrInvalidDir` otherwise.
- `dir` MUST resolve outside reserved system locations (`/`, `/etc`,
  `/usr`, `/bin`, `/sbin`, `/var/log`, `$HOME`-relative parents). The
  v1 reference checks against a hard-coded deny list with
  `logs.FromContext(ctx).Warn` on overlap; operators that need other paths replace the validator
  via a future `Options["allow_path"]` knob (out of v1).
- `keep_dir`, when `true`, instructs `Clear` to remove the directory's
  contents but leave the directory itself in place (useful when an
  enclosing volume is mounted by the operator). Default `false`:
  remove the whole directory.
- `umask` is applied around `PutFile` writes via `os.OpenFile` mode
  bits. Default `0o022`.
- `base_env` is the literal slice prepended to the manager process'
  env (which is otherwise inherited verbatim by `Exec`).
- `request_loopback_host` overrides the IP used by `Request`
  (default `127.0.0.1`). Values MUST parse as a loopback IP (`127.0.0.1`,
  `::1`, …); hostnames such as `localhost` are rejected. IPv6 callers set `"::1"`.

Unknown keys produce `ErrInvalidOption` so typos do not silently
drift; the v1 reference uses an allow-list of the keys above.

## Lifecycle implementation notes

### `Init(ctx)`

1. Stat the parent of `dir`; if missing, return
   `ErrInvalidDir`.
2. `Lstat` `dir`. The final path component MUST NOT be a symlink; reject
   with `ErrInvalidDir` wrapping `ErrInvalidOption`.
3. If `dir` exists and is non-empty, return `ErrAlreadyInitialized` (per
   the interface contract).
4. If it exists and is empty, accept it. Otherwise, `os.MkdirAll(dir,
   0o755)`.
5. Open an `*os.Root` rooted at `dir` and stash it on the instance.

### `Open(ctx)`

1. `Lstat` `dir`; if missing, return `ErrNotInitialized`.
2. Reject symlinks and non-directories like `Init`.
3. Open `*os.Root` at `dir` and stash it (same initialized state as after
   `Init`, without requiring an empty directory).

### `Clear(ctx)`

Clears contents through the opened `*os.Root` (`RemoveAll` per entry),
never `os.RemoveAll` on an unanchored path join. When `keep_dir` is false,
removes `dir` only after verifying the on-disk path is still the same
device/inode as the opened root (so a replaced path is not followed).

### `Exec(ctx, cmd)`

1. Validate `cmd.Program` (non-empty), `cmd.WorkDir` (relative,
   resolves inside `Dir()`).
2. Build `*exec.CommandContext` with derived ctx (for the optional
   `Timeout`).
3. Inherit the manager process env, append `base_env`, append
   `cmd.Env`.
4. Wire `Stdin` if provided; wire `Stdout`/`Stderr` to either the
   caller's writer or a `bytes.Buffer` capped at 8 MiB per stream
   (oversized output emits `logs.FromContext(ctx).Warn` and truncates).
5. `Run` and translate `*exec.ExitError` → `ExitCode` field. Other
   errors (program not found, IO failure) bubble up.
6. Record `Duration`.

**Cwd / `WorkDir`:** After `*os.Root` validates the relative work
directory, Linux sets `Cmd.Dir` to `/proc/self/fd/<fd>` for an opened
directory handle so the child process is not launched with a stale
joined string path. Non-Linux builds return `infraops.ErrPathOutsideDir`
instead of falling back to `filepath.Join(Dir(), workDir)`. The
implementation also rejects when the configured `dir` path is a symlink
or no longer the same inode as the opened root (path replacement).

### `PutFile(ctx, path, content, mode)`

1. Resolve `path` through the `*os.Root`; reject escapes.
2. `MkdirAll` the parent (via `Root.MkdirAll`) with `0o755 & ^umask`.
3. Write to a tempfile sibling (`.<basename>.tmp.<rand>`) with the
   requested `mode` masked by `umask`.
4. `fsync` the file, `rename` over the destination.
5. `fsync` the parent directory.
6. Honor ctx cancellation: cancel the ongoing copy by switching to a
   ctx-aware reader wrapper.

### `GetFile(ctx, path)`

1. Resolve through `*os.Root`.
2. `os.Open`; the caller closes.
3. Return a thin wrapper that aborts further reads on ctx cancel
   (since `os.File` does not honor ctx natively, the wrapper checks
   `ctx.Err()` per chunk; chunk size 64 KiB).

### `Request(ctx, port, req)`

1. Override `req.URL.Scheme = "http"`, `req.URL.Host =
   net.JoinHostPort(loopback, strconv.Itoa(port))`.
2. Use a `*http.Client` per instance with a 30s default transport
   timeout (caller's ctx still wins).
3. Pass-through; close the response body is the caller's job.

The HTTP client is reused across calls; per-instance not per-call to
avoid socket churn.

### `Clear(ctx)`

1. If `keep_dir == true`: walk `Dir()` and delete entries while
   leaving `Dir()` itself.
2. Otherwise: `os.RemoveAll(Dir())`.
3. Mark the instance as cleared; subsequent ops return
   `ErrCleared`.

`Clear` is idempotent; deleting a missing directory returns nil.

## Concurrency

- Multiple goroutines may call any method on the same instance
  concurrently. The instance protects internal state (e.g., the
  cleared flag, the HTTP client) with a mutex; the underlying file
  system handles per-call atomicity.
- Two `localdir` instances pointing at the same `dir` would race;
  the manager refuses to register two workspaces with the same
  `localdir.dir`. (This check happens at workspace-creation time,
  not in this package.)

## Safety

- `*os.Root` (Go 1.24+) prevents traversal escapes natively. Earlier
  Go fallback uses a `filepath.Clean` + prefix check; the package
  builds with `go1.24` or newer.
- Symlinks **inside** `Dir()` are followed for reads (consistent
  with normal POSIX behavior); they are **not** created by `PutFile`.
- The factory rejects `dir` paths whose parent does not already
  exist, to avoid surprise `MkdirAll` of `/tmp/some/deep/path`.

## Tests to write

1. `New` rejects relative `dir`, missing `dir`, deny-listed roots,
   unknown options.
2. `Init` happy path: empty dir → no-op; missing dir → created.
3. `Init` on a non-empty dir returns `ErrAlreadyInitialized` and
   does not modify contents.
4. `Exec` honors `WorkDir` relative to `Dir()`; rejects escapes.
5. `Exec` non-zero exit produces nil error + correct `ExitCode`.
6. `Exec` ctx cancel kills the child process within ~250 ms.
7. `PutFile` is atomic — concurrent reader sees old or new content,
   never partial.
8. `PutFile` rejects path escapes via `*os.Root`.
9. `GetFile` aborts on ctx cancel.
10. `Request` default host is `127.0.0.1`; override works.
11. `Clear` with `keep_dir` true vs false.
12. Cleared instance returns `ErrCleared` for ops; another `New(opts)`
    yields a fresh instance.
13. `-race` over a mixed read/write workload.

## Out of scope

- Containers, sandboxes, remote SSH. Those are sibling packages.
- Resource limits (CPU / memory / disk). The host's facilities
  (cgroups, ulimit) apply outside the manager's purview.
- Snapshotting. `Clear` followed by `New + Init` reproduces the
  empty-dir state; backup/restore is operator territory.
