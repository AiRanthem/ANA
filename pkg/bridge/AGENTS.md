# AGENTS.md

## Scope
- `pkg/bridge` is transport-only.
- Keep transport implementations inside these subpackages:
  - `pkg/bridge/rest`
  - `pkg/bridge/cli`
  - `pkg/bridge/socket`
- Canonical request, event, stream, and session contracts live in `pkg/agentio`.

## Responsibilities
- Translate transport payloads to and from `pkg/agentio`.
- Preserve transport semantics when emitting `agentio.Event`.
- Honor `context.Context` cancellation and clean shutdown.

## Non-Responsibilities
- No canonical contract definitions in `pkg/bridge`.
- No routing, orchestration, transcript persistence, or call-chain assembly.
- No new root-level helper files under `pkg/bridge`; keep helpers local to the transport subpackage that uses them.

## Parallel Boundary
- One worker may own `pkg/bridge/rest`.
- One worker may own `pkg/bridge/cli`.
- One worker may own `pkg/bridge/socket`.
