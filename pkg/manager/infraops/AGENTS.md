# pkg/manager/infraops AGENTS.md

## Scope

Defines the `InfraOps` interface — the abstraction the manager uses to
file-and-process its way around a workspace's runtime environment —
plus the supporting types (`Options`, `ExecCommand`, `ExecResult`,
`Factory`, `FactorySet`, sentinel errors). Concrete implementations
live in subpackages (`infraops/localdir/`, future `infraops/docker/`,
`infraops/e2b/`, …).

## Rules

- The interface surface is intentionally tight (`Init`, `Exec`,
  `PutFile`, `GetFile`, `Request`, `Clear`, plus `Type` / `Dir`).
  Adding a method touches every implementation; weigh the cost.
  Capability extensions (e.g., `Dial`) ship as separate interfaces
  consumed via type assertion.
- `Exec` is **structured** — `Program` + `Args`, never a shell line.
  Implementations that ride a shell quote inputs themselves.
- `PutFile` / `GetFile` paths are sandboxed inside `Dir()`. Escapes
  return `ErrPathOutsideDir` before any IO.
- A non-zero `ExitCode` is data, not an error. `Exec` returns nil
  error and an `ExecResult` with the code; only operational failures
  (program missing, ctx cancelled, IO error) populate the error.
- `Init` is required before any IO method. `Clear` is idempotent and
  terminal for the instance.
- Factories MUST be cheap; do work in `Init`.

## Don'ts

- Do not import any other manager subpackage. `infraops` is a leaf.
- Do not add a method to `InfraOps` without updating every
  implementation **and** `DESIGN.md` §7.
- Do not interpret `path` arguments as anything other than relative
  to `Dir()`. No tilde expansion, no env var substitution, no shell
  metacharacters.
- Do not log file or command contents at info level — they may carry
  secrets. Log structural fields only (`op`, `program`, `path`,
  `bytes`, `latency_ms`).
- Do not silently swallow non-zero exit codes. The contract is
  explicit: callers branch on `ExitCode`.
