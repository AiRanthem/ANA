# AGENTS.md

## Scope
- This repository is written in Go. `AGENTS.md` is the first context every agent should read before making changes.
- This document defines only foundational Go development best practices for this repository.
- Keep rules minimal, practical, and easy to enforce in daily work.
- If a more specific rule (package-level doc, directory-level `AGENTS.md`, or explicit user instruction) conflicts with this document, the more specific rule wins.

## Git branches and commits

A short always-on summary lives in `.cursor/rules/git-conventions.mdc`; full wording and examples are in `.cursor/rules/git-conventions.detail.mdc`.

## Style And Formatting
- Always format code with `gofmt -s` (or `go fmt ./...`) and make sure `go vet ./...` is clean before committing.
- Use clear, intention-revealing names; avoid abbreviations unless they are standard in Go.
- Follow Go export conventions: exported identifiers use `UpperCamelCase`, unexported identifiers use `lowerCamelCase`. Acronyms keep consistent case (`ID`, `URL`, `HTTP`).
- Receiver names are short, lowercase, and consistent for each type (e.g. `s *Server`, not mixed `server`/`srv`).
- Keep packages focused and cohesive; avoid large "utility" packages with mixed responsibilities.

## Error Handling
- Return errors explicitly instead of hiding failures.
- Make error messages highly readable and diagnostic: one error should expose as much locating context as possible (operation, target, key inputs, and failure reason).
- Prefer actionable error text that helps identify where and why failure happened without extra reproduction steps.
- Add context by wrapping with `fmt.Errorf("<op>: %w", err)`; compare and extract with `errors.Is` / `errors.As` instead of string matching.
- Define sentinel errors (`var ErrXxx = errors.New(...)`) or error types only when callers need to branch on them; otherwise a wrapped error is enough.
- Handle errors close to where they occur when meaningful recovery is possible.
- Do not use `panic` for normal control flow; reserve it for unrecoverable programmer errors.

## Logging
- Default to the standard library `log/slog` for structured logging; only introduce another logger if there is a concrete reason.
- Use stable keys across the codebase (for example: `op`, `component`, `request_id`, `resource`, `latency_ms`, `err`).
- Keep log messages concise and machine-queryable; put variable data in fields, not free-form strings.
- Use log levels consistently (`debug`, `info`, `warn`, `error`) and avoid noisy logs in hot paths.
- Never log secrets or sensitive payloads.

## Concurrency Basics
- Pass `context.Context` as the first parameter for operations that can be canceled or timed out.
- Ensure every started goroutine has a clear shutdown path; prefer `errgroup.Group` or `sync.WaitGroup` over ad-hoc tracking.
- Avoid sharing mutable state across goroutines without synchronization.
- Prefer simple channel communication patterns; close channels only from the sender side.
- Validate concurrent code with `go test -race` before shipping.

## High-Performance Background IO Services
- Treat resource lifetime as first-class: ensure clean startup, graceful shutdown, and bounded retries for long-running workers.
- Use connection pooling and reuse clients/buffers to reduce allocation and syscall overhead.
- Apply backpressure with bounded queues/channels; avoid unbounded buffering in producer-consumer pipelines.
- Set explicit timeouts/deadlines for all external IO to prevent goroutine and fd leaks.
- Measure before tuning: use metrics/profiling to validate latency, throughput, and tail behavior changes.

## Testing Basics
- Write table-driven tests for logic with multiple input/output combinations; use `t.Run` for named subtests.
- Keep tests deterministic and isolated; avoid hidden dependence on time, network, or global state.
- Use `t.Helper`, `t.Cleanup`, and `t.TempDir` instead of hand-rolled setup/teardown or global fixtures.
- Cover critical paths and boundary conditions, not only happy paths.
- Use clear test names that describe expected behavior (`TestThing_Does_X_When_Y`).
- For packages with concurrency, run `go test -race ./...` as part of the standard loop.

## Engineering Hygiene
- Prefer small functions with single responsibilities.
- Reduce coupling through explicit interfaces at boundaries, not everywhere by default.
- Delay abstraction until duplication or variation is real and repeated.
- Keep changes incremental and easy to review.
- Do not introduce new third-party dependencies without a clear reason; keep `go.mod` / `go.sum` tidy (`go mod tidy`).

## Verify Before Submitting
Before declaring a change done, run the relevant subset of:

- `gofmt -s -w .` (or `go fmt ./...`)
- `go vet ./...`
- `go build ./...`
- `go test ./...` (add `-race` for packages with concurrency)
- `go mod tidy` (if imports or dependencies changed)

Only claim completion after these commands pass on the touched code.
