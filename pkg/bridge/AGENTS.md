# AGENTS.md

## Scope
- `pkg/bridge` contains transport adapters only.
- Subpackages are:
  - `pkg/bridge/rest`
  - `pkg/bridge/cli`
  - `pkg/bridge/socket`

## Responsibilities
- Translate transport-specific payloads into `pkg/agentio` contracts.
- Preserve semantics in `agentio.Event`.
- Honor `context.Context` cancellation.

## Non-Responsibilities
- No canonical contract definitions.
- No routing, orchestration, transcript persistence, or call-chain assembly.

## Parallel Implementation Boundary
- One worker may own `pkg/bridge/rest`.
- One worker may own `pkg/bridge/cli`.
- One worker may own `pkg/bridge/socket`.
- Any decode helpers stay inside `pkg/bridge/rest`, `pkg/bridge/cli`, or `pkg/bridge/socket`; do not add root-level helper files under `pkg/bridge`.
