# pkg/orchestrator/prompt AGENTS.md

## Scope

Build the role-aware `agentio.InvokeRequest` for one Request, including the
Notes block injection strategy keyed on `RuntimeKind`.

## Rules

- Output is byte-deterministic for a given `BuildInput`. The Notes block
  uses sorted partner aliases; punctuation and headers are fixed literals.
  Tests must pin this.
- On `Kind == session.RequestKindResume`, Notes are omitted; bridges
  retain prior turns via their session-state stores.
- Partner list excludes the target workspace.
- Per-part role tags are required for `chat_api` workspaces; flat user-role
  text is required for `resumable_cli` and `socket_session`.

## Don'ts

- Do not embed workspace-specific system prompts here. Per-workspace
  templates are a v2 feature; raise a design change before adding hooks.
- Do not parse Salutations here; consume the `RouteDirective` produced by
  `protocol/`.
- Do not call into `agentio.Agent`; this package only constructs the
  request.
- Do not silently truncate the partner list; emit a Diagnostic instead.
