# protocol/PLAN.md

## Purpose

Pure parsing and templating utilities for the Epistolary protocol.

The package is the only place that reads a raw text body and decides whether
it carries a Salutation. Every other module accepts already-parsed
`RouteDirective` values produced here.

## Public surface (intent only)

- `ParseDirective(body string) (RouteDirective, error)`
  - Returns `RouteDirective` with `IsExplicit == false` and `TargetAlias ==
    ""` when the body has no Salutation. This is **not** an error and is
    used to detect plain output.
  - Returns `ErrInvalidRouteDirective` when the first non-empty line begins
    in a way that looks like an attempted Salutation but does not satisfy
    the strict regex (see §Parser rules). The orchestrator surfaces this as
    a task-level failure.
- `Format(alias string) string`
  - Renders a Salutation prefix exactly as the spec defines:
    `"{to #" + alias + "}"`. Used only by tests and tools; agents emit
    Salutations themselves.
- `RouteDirective` struct as defined in `DESIGN.md` §6.2.
- Exported sentinel `ErrInvalidRouteDirective`.

## Parser rules

The parser is deliberately strict to avoid silent route-mismatches.

1. The body is split on the first newline run that yields a non-empty line
   (i.e., leading blank lines are skipped). If no non-empty line exists, the
   directive is absent and the body is returned as the payload (no error).
2. The non-empty line is examined verbatim, including whitespace.
3. The line MUST match the regex
   `^\s*\{to\s+#(?P<alias>[A-Za-z0-9_\-]+)\}\s*(?P<inline>.*)$`.
   - `alias` rule mirrors the §Alias rules section below; the registry
     enforces the same character set independently.
   - `inline` may be empty or carry payload text (e.g., the example
     `"{to #Alice} check stock prices"`).
4. If the regex does not match but the line begins with the literal `{to`
   token (case-insensitive, allowing for surrounding whitespace), return
   `ErrInvalidRouteDirective` so the orchestrator can fail fast instead of
   silently treating it as plain text.
5. Otherwise (line does not look like an attempted Salutation), return a
   `RouteDirective` with `IsExplicit == false`, `TargetAlias == ""`, and
   `Payload == body`.
6. When the directive is explicit:
   - `Payload` is the concatenation of the inline group and the rest of the
     body separated by a single newline. Trailing whitespace on the inline
     group is preserved; leading whitespace is trimmed.
   - `RawHeader` is the directive line as observed (without surrounding
     newline characters). This is forwarded to the audit log unchanged.

## Alias rules

- Allowed characters: `[A-Za-z0-9_-]`. No spaces, no Unicode, no `#`.
- Length: 1..64.
- Aliases are case-sensitive; the registry decides whether to fold case at
  registration time. The parser does not change case.

## Notes templates (rendered upstream)

The actual Notes block is composed by `prompt/`, but the directive-format
example inside the Notes preamble must remain consistent with the parser's
expectations. `protocol/` exports a small `ExampleHeader(alias string)`
helper used by `prompt/` to render the example line:

```
{to #<alias>}
```

## Edge cases & decisions

- Multiple `{to ...}` directives on the same line: only the first one
  satisfies the regex; the rest are part of the payload. The example given
  by the spec aligns with this behavior.
- Empty alias inside braces (`{to #}`) → `ErrInvalidRouteDirective`.
- A Salutation that is not on the first non-empty line → ignored. Treated
  as plain output. The agent is expected to put the directive at the very
  start.
- Mixed whitespace before the directive: leading whitespace on the directive
  line is allowed because some agents emit a leading space.
- CRLF line endings are normalized: the parser splits on either `\n` or
  `\r\n`.

## Tests to write (no implementation in this pass)

Acceptance scenarios the implementation must cover:

1. Spec example end-to-end: `"{to #Alice} check stock prices"` →
   `IsExplicit=true`, `TargetAlias="Alice"`, `Payload="check stock prices"`.
2. Multi-line payload preserved verbatim under the directive.
3. Plain output (no directive) round-trip: parse → format from
   directive on output is empty.
4. Invalid alias (`{to #al ice}`) → `ErrInvalidRouteDirective`.
5. Loose-form `{to Alice}` (missing `#`) → `ErrInvalidRouteDirective`.
6. Salutation past the first non-empty line → no directive detected.
7. Leading whitespace and CRLF normalization.

## Out of scope

- Notes preamble assembly (lives in `prompt/`).
- Determining the partner list (lives in `registry/`).
- Stripping the directive from a streamed agent output before flushing it to
  the next session (lives in the engine; this package only inspects the
  final aggregated text).
