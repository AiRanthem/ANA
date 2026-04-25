# pkg/orchestrator/protocol AGENTS.md

## Scope

Pure functions for parsing and emitting the Epistolary protocol's Salutation
directive (`{to #<alias>}`).

## Rules

- No I/O, no logging, no goroutines, no global state.
- Stays in step with `DESIGN.md` §6.2 and `PLAN.md` of this package; both
  must be updated together when grammar or alias rules change.
- Treat parse failures as user-visible errors via
  `ErrInvalidRouteDirective`. Never silently swallow a malformed
  Salutation.
- Aliases are case-sensitive at the parser; case-folding decisions live in
  `registry/`.

## Don'ts

- Do not import any sibling subpackage (this package is at the bottom of
  the dependency graph).
- Do not change the regex without updating `PLAN.md` and the test list in
  it.
- Do not add YAML/JSON/structured directive forms; the spec is text-only.
