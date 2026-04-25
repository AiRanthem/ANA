# pkg/orchestrator/session AGENTS.md

## Scope

Session and Request data model, lifecycle state machine, per-task call
stack, and the `Store` interface that persists sessions and requests.

## Rules

- Enforce session and request state transitions strictly. Return
  `ErrInvalidTransition` on illegal moves; never silently coerce state.
- Keep the in-memory `Store` race-free; verify with `go test -race`.
- The Stack is in-memory only; engines that need restart survival rely on
  the audit sink for replay.
- Use the cross-module names from `DESIGN.md` §6.4 verbatim
  (`SessionID`, `RequestID`, `Status`, etc.); do not invent local
  synonyms.

## Don'ts

- Do not embed routing logic. Salutation parsing and prompt building live
  in `protocol/` and `prompt/` respectively.
- Do not call `agentio.Agent`. The invoker handles transport.
- Do not GC sessions automatically; rely on operators to truncate the
  audit-tier store as policy dictates.
- Do not import `task/` to avoid a circular dependency. Tasks own
  sessions, not the other way around; use IDs only.
