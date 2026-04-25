# pkg/orchestrator AGENTS.md

## Scope

`pkg/orchestrator` is the control plane that turns a normalized user message
into a chain of agent invocations and returns the final result. It owns:

- Task lifecycle (`Submit`, `Wait`, `Cancel`, `Snapshot`).
- Session lifecycle, including the call-stack model that resumes paused
  sessions when a callee replies without a Salutation.
- Prompt construction (Notes block injection in role-aware fashion).
- Per-Request dispatching to `agentio.Agent` implementations supplied by the
  caller, plus per-Request lifecycle reporting.
- Two observability surfaces: a fail-fast audit sink and a best-effort
  runtime event bus.

This package depends on `pkg/agentio` (canonical contracts) and is consumed
by ingress adapters (IM bridges, web SSE, schedulers). It MUST NOT import
`pkg/bridge/...`; transport adapters reach the orchestrator only by being
registered as `agentio.Agent` factories on `Dependencies`.

## Authoritative documents

- `DESIGN.md` (this directory) — the comprehensive design and the source of
  truth for cross-module decisions. Any change to the call-stack model,
  event taxonomy, audit guarantees, or upstream contract requirements MUST
  update `DESIGN.md` first.
- Per-module `PLAN.md` files — module-level details (interfaces, data
  shapes, edge cases). They MUST stay consistent with `DESIGN.md`.

If a module needs to deviate from `DESIGN.md`, update `DESIGN.md` in the
same change.

## Module conventions

- Every subpackage is small and exports a focused interface plus one
  default implementation.
- Subpackages do not import each other except as listed in
  `DESIGN.md` §13.1 (the dependency graph). Adding a new edge requires
  updating that section.
- Tests for cross-module behavior live in this directory (root package),
  not in subpackages, to avoid circular references and to keep integration
  tests close to the engine.

## Naming

- Identifier types: `TaskID`, `SessionID`, `RequestID` are aliases over
  `string` declared in `types.go` to keep call sites readable.
- Engine errors wrap module sentinels with `fmt.Errorf("<engine op>: %w",
  err)`. Sentinels are exported from the originating subpackage.
- Logger keys follow the repository convention: `task_id`, `session_id`,
  `request_id`, `workspace_alias`, `workspace_id`, `op`, `component`,
  `latency_ms`, `err`.

## Things to avoid

- No silent fall-through when routing fails. Either an alias resolves or the
  task fails with a clear error; never treat a missing route as plain text.
- No goroutine leaks. Engine goroutines must terminate when the task ctx is
  cancelled or the task reaches a terminal state.
- No reaching into bridge implementations. If the orchestrator needs new
  capabilities from `pkg/bridge`, raise it as a contract change in
  `DESIGN.md` §7 first.
- No business logic inside `audit/` or `events/` beyond delivery. They are
  pipes, not policy.
