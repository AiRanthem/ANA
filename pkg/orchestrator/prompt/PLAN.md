# prompt/PLAN.md

## Purpose

Build the role-aware prompt for one Request. Specifically:

1. Strip the Salutation from the upstream payload (already done by
   `protocol/`; this package consumes a `RouteDirective`).
2. Inject the partner Notes block at the position and role best suited to
   the target workspace's `RuntimeKind` for cache friendliness.
3. Produce an `agentio.InvokeRequest` ready for the invoker.

## Public surface (intent only)

- `Builder` interface:
  - `Build(ctx, BuildInput) (BuildOutput, error)`
- `BuildInput`:
  - `Workspace registry.Workspace` — the target.
  - `Partners []registry.Workspace` — all currently enabled workspaces
    other than the target. The order is deterministic (sorted by alias) so
    the resulting prompt is byte-stable across calls and prefix caches hit.
  - `Payload string` — the user-visible payload (post-Salutation strip).
  - `Attachments []agentio.InputPart` — forwarded only on the root Request
    of a Task; nil for in-task delegations.
  - `Kind session.RequestKind` — `root` / `delegation` / `resume`. Used
    to decide whether to include the Notes block. See §Cache plan. The
    type is owned by `session/` (see `DESIGN.md` §6.7).
  - `IDs IDTriple` — `(TaskID, SessionID, RequestID)`.
- `BuildOutput`:
  - `Request agentio.InvokeRequest` — final request.
  - `Diagnostics []string` — optional, for debug logs (e.g., "Notes block
    skipped on resume").

## Strategy: Role-aware injection

The Notes block placement depends on `Workspace.RuntimeKind`:

### `chat_api`

Build the request with two parts:

1. `RoledTextPart{Role: RoleSystem, Text: notes}` — the Notes block.
2. `RoledTextPart{Role: RoleUser, Text: payload}` — the payload body.

Plus any attachments after the user text, with role `RoleUser`.

Rationale: chat APIs cache by message-prefix; keeping the Notes block in a
stable system message maximizes cache hits across delegations to the same
workspace. The payload is the only volatile suffix.

### `resumable_cli`

Concatenate to a single user-role text part (CLI agents flatten roles):

```
<notes>

---

<payload>
```

Notes go first because the CLI agent is launched fresh (or resumed but
re-prompted with the new turn). Even though CLI vendors don't expose prefix
caching today, putting Notes first is consistent with the chat_api strategy
and easier for the agent's `skills`/`rules` to reason about.

### `socket_session`

Same shape as `resumable_cli` (single user-role part with Notes preamble),
because socket agents typically forward text to a model that does its own
prefix-cache handling. The socket adapter is responsible for routing to the
right model session via the `session_id`.

## Notes block content

A single fixed template:

```
You're not alone — feel free to reach out to any of the following partners
for support:

- **<Alias>**: <Description>
- **<Alias>**: <Description>
...

You can start your reply with `{to #name}` to speak directly to a partner.
For example:

{to #<sample-alias>}
Help me please
```

Rules:

- Partners are sorted alphabetically by alias for byte-stability.
- The `<sample-alias>` example is the first partner in the sorted list. If
  there are zero partners, the entire Notes block is omitted.
- The closing example uses a triple-newline before the directive line so it
  reads as a separate paragraph; the trailing `Help me please` is a fixed
  literal so it does not change between calls.
- Header punctuation, capitalization, and spacing are normative; do not
  template per workspace.

The block excludes the target workspace itself (a workspace cannot delegate
to itself in v1).

## Cache plan

`Kind` controls whether the Notes block is regenerated:

| Kind         | Notes block | Rationale |
|--------------|-------------|-----------|
| `root`       | included    | First call to this session |
| `delegation` | included    | New session for the target alias |
| `resume`     | omitted     | Same session continues; system content is stable; do not bust the cache by re-emitting |

For `chat_api` agents, the resume request uses only a `RoleUser` payload;
the chat history (including the original system Notes) stays in the bridge
adapter's `HistoryStore` (see `DESIGN.md` §7.3) so the model still sees
Notes from prior turns.

For `resumable_cli`, the cli adapter runs `--resume <session_id>` and the
agent process is expected to remember the prior Notes; therefore omitting
Notes on resume is correct.

For `socket_session`, the socket adapter holds the connection per session;
the agent has the prior Notes already.

## InvokeRequest field plan

The builder fills `agentio.InvokeRequest` like this:

| Field            | Value |
|------------------|-------|
| `SessionID`      | `IDs.SessionID` (string-cast) |
| `Inputs`         | The role-aware parts above |
| `Options.Stream` | `true` (orchestrator always streams) |
| `Options.RequestID` | `IDs.RequestID` (string-cast) for echo into events |
| `Options.Workspace` | `Workspace.WorkspaceID` |
| `Options.Hints`  | Map containing `runtime_kind`, `kind` (the `RequestKind` value), plus a `task_id` for downstream auditing. Bridges may ignore. |

The orchestrator does not set role at the top level; per-part roles drive
behavior (this is exactly the contract change mandated in `DESIGN.md` §7.1).

## Edge cases & decisions

- Partner list larger than ~32 entries: warn (return a Diagnostic) but keep
  emitting the full list. Truncation is policy that v1 punts on.
- Workspace with empty `Description`: emit the bullet without the colon
  trailer (`- **Bob**:` would look ugly).
- Payload empty: still emit the Notes block; the agent may produce a
  no-op output and the engine pops the session normally.
- Attachments + delegation: in v1, only root requests carry attachments. A
  callee receiving a Salutation handoff gets only text. Diagnostic logged.

## Tests to write (no implementation in this pass)

1. Chat-api role split: system + user, deterministic order under repeated
   calls with same partners.
2. CLI flatten: single user part, Notes first, separator literal.
3. Socket flatten: same as CLI shape.
4. Resume omits Notes for all runtime kinds.
5. Partner alphabetization.
6. Empty partners → no Notes block.
7. Self-exclusion: target workspace never appears in its own Notes.
8. Sample-alias picks the first sorted partner.

## Out of scope

- Workspace-specific system prompts (e.g., a Claude Code `CLAUDE.md`). v2
  feature; would extend `BuildInput` with a workspace-scoped template.
- Tool-use parts (`agentio.ToolResultPart`) are passed through unchanged.
- Message history maintenance (`bridge/rest.HistoryStore`).
