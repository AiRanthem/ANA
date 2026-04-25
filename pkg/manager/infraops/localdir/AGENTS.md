# pkg/manager/infraops/localdir AGENTS.md

## Scope

Reference `infraops.InfraOps` implementation that targets a local
directory on the host running the manager. Owns the path-sandbox
logic (`*os.Root`-based), the `os/exec` wiring for `Exec`, the
atomic-write `PutFile` implementation, and the loopback HTTP client
for `Request`. CLI is a leaf in the dependency graph.

## Rules

- Use `*os.Root` (Go 1.24+) to enforce sandboxing on every file
  operation. Path validation happens **before** any IO so error
  messages stay precise.
- Atomic writes via temp+rename + `fsync` are mandatory; partial
  files would break plugin layout idempotence.
- Inherit the manager process environment for `Exec` unless the
  operator opts out via a future `inherit_env: false` option (out of
  v1).
- `Clear` always succeeds when the directory is missing. After
  `Clear` the instance is dead; further calls return
  `ErrCleared`.

## Don'ts

- Do not import any other manager subpackage. `localdir` only
  imports `infraops` and the standard library.
- Do not call `os.Chdir`. The manager process may be running other
  workspaces concurrently; `Chdir` would leak state.
- Do not retry `Exec` internally. The manager controls retries (and
  in v1 does not retry).
- Do not log file or command **content**. Log structural fields:
  `op`, `program`, `path`, `bytes`, `latency_ms`, `exit_code`,
  `err`. Never include stdin/stdout values.
- Do not silently follow symlinks inserted by an earlier process. If
  a symlink at `path` resolves outside `Dir()`, treat it as a
  traversal attempt and return `ErrPathOutsideDir`.
