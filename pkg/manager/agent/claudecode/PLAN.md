# agent/claudecode/PLAN.md

## Purpose

Provide the v1 reference `agent.AgentSpec` implementation for the
**Claude Code** CLI. It pins the conventions for how Claude Code
workspaces are bootstrapped inside an `infraops.InfraOps`-shaped
environment, how canonical plugins are projected onto Claude Code's
expected directory layout, and how operators reach the resulting
workspace through the orchestrator's CLI bridge.

This is a *reference* implementation: it describes one reasonable way
to install the binary, manage configuration, and probe liveness for
Claude Code. Operators who want a different binary path, a different
config layout, or a custom probe replace the spec.

## Public surface (intent only)

- `Spec` — concrete `agent.AgentSpec` value. Constructed via
  `New(opts Options) (Spec, error)`.
- `Options` struct (see §3 below).
- Sentinel errors local to this package:
  - `ErrBinaryUnavailable` — `claude --version` returned non-zero or
    the binary was not found.
  - `ErrInvalidPluginLayout` — re-exported from `agent` for the
    layout collision case.
  All other failures bubble up as wrapped errors (e.g.,
  `fmt.Errorf("claudecode install put settings: %w", err)`); the
  workspace controller maps them to `WorkspaceError` records with
  `Phase: "install"` / `"probe"`.
- Helper: `LayoutPaths(manifest plugin.Manifest) []string` — exposed
  for tests; returns the relative paths where the layout would place
  the plugin's contents.

The `Spec` value is safe to register multiple times across managers
but only once per manager (per `agent.SpecSet` rules).

## AgentSpec method behavior

### `Type()` and `DisplayName()`

- `Type() == "claude-code"`.
- `DisplayName() == "Claude Code"`.
- `Description()` is a 1–3 sentence blurb describing the binary, e.g.
  "Anthropic's Claude Code CLI as a long-lived workspace bootstrapped
  in an InfraOps environment."

### `PluginLayout()`

Returns a `claudecode.layout` struct that maps the canonical plugin
tree onto the per-workspace layout below:

```
<InfraOps.Dir()>/
└── .claude/
    └── plugins/
        └── <plugin-name>/
            ├── manifest.toml          # canonical manifest verbatim
            ├── AGENTS.md              # if present in the plugin
            ├── skills/                # canonical skills/
            ├── rules/                 # canonical rules/
            ├── hooks/                 # canonical hooks/
            ├── subagents/             # canonical subagents/
            ├── mcps/                  # canonical mcps/
            └── assets/                # canonical assets/
```

Rules:

- The plugin name segment is sanitized: lowercase, `[a-z0-9-]`, max
  64 chars; conflicts (two plugins with the same sanitized name)
  return `ErrInvalidPluginLayout` and the workspace install fails.
- Sections that the plugin does not include are simply not written.
- Files outside the canonical sections (manifest.toml, AGENTS.md,
  README.md) are still copied verbatim — they are common author
  conventions and harmless.
- The layout MUST NOT modify file contents (no template substitution,
  no rewriting). The plugin author is responsible for plugin-internal
  references.
- If two plugins both want `.claude/plugins/<same-name>/...`, the
  layout returns `ErrInvalidPluginLayout` before any write — the
  install worker MUST detect this **before** calling `PutFile`, so
  the working directory remains coherent.

### `Install(ctx, ops, params)`

Steps in order:

1. **Verify binary.** Run
   ```go
   ops.Exec(ctx, infraops.ExecCommand{
       Program: opts.Binary,            // defaults to "claude"
       Args:    []string{"--version"},
   })
   ```
   Fail with `ErrBinaryUnavailable` if the exit code is non-zero or
   the program is not found. The version string is captured for
   `Probe` to expose.
2. **Write `.claude/settings.json`.** Minimal seed config:
   ```json
   {
     "schema_version": 1,
     "workspace": {
       "alias": "<from params.Workspace.Alias>",
       "namespace": "<from params.Workspace.Namespace>"
     }
   }
   ```
   Operators that need richer settings supply them through
   `Options.SettingsTemplate` (a `text/template`) or post-install
   tooling.
3. **Write top-level `AGENTS.md`.** A short, deterministic file that
   names the workspace and lists attached plugin names. This file is
   NOT a plugin's AGENTS.md; it is a per-workspace one. Plugins keep
   their own AGENTS.md inside their plugin directory.
4. **No daemon.** Claude Code is invoked per-request through the CLI
   bridge; there is no long-running process to start. `Install`
   returns success.

`Install` MUST be idempotent within a single call: if step 1 already
ran successfully and step 2 fails halfway, retrying step 1 is a
no-op. Cross-call idempotence is not required (the manager calls
`Install` exactly once per workspace).

### `Uninstall(ctx, ops)`

Returns `nil` immediately. There is no daemon to stop and the
workspace controller calls `ops.Clear` next, which removes everything
on disk.

### `Probe(ctx, ops)`

- Runs the same `infraops.ExecCommand` shape as Install step 1
  (`Program: opts.Binary`, `Args: ["--version"]` by default — or
  `Options.ProbeArgs` when set).
- On success: `ProbeResult{Healthy: true, Detail: {"version": ...}}`.
- On failure: `ProbeResult{Healthy: false, Error: &WorkspaceError{
  Code: "probe.binary_unavailable", Message: ..., Phase: "probe"}}`.
- Latency is measured around the `Exec` call and recorded.

Probe duration is dominated by process startup; on a healthy host the
budget is well under the manager's default `ProbeTimeout` (5s).

### `ProtocolDescriptor()`

```go
ProtocolDescriptor{
    Kind: ProtocolKindCLI,
    Detail: map[string]any{
        "command":              []any{"claude", "code"},
        "resume_flag":          "--resume",
        "cwd_relative_to_dir":  "",
        "stdin_input":          true,
    },
}
```

The `Detail` keys above are the conventional ones in
`agent/PLAN.md` §ProtocolDescriptor plus one Claude-Code-specific
addition (`stdin_input`) signalling that the prompt is delivered
through stdin (the orchestrator's CLI bridge reads this).

## 3. `Options`

```go
type Options struct {
    // Binary, if non-empty, is the absolute path of the Claude Code
    // executable used by Install / Probe. Defaults to "claude" (relying
    // on PATH inside the infra).
    Binary string

    // SettingsTemplate, if non-nil, overrides the default seed
    // .claude/settings.json. The template receives {Workspace}
    // (WorkspaceSummary).
    SettingsTemplate *template.Template

    // ProbeArgs, if non-empty, replaces the default probe command
    // arguments (`--version`).
    ProbeArgs []string
}
```

`New(opts)` returns `Spec` and validates:

- `Binary`, when set, is non-empty and not a shell metachar string
  (`;`, `|`, `&`, `>`, `<`).
- `ProbeArgs` length ≤ 8, no element empty.

## Plugin layout details

The canonical plugin layout (defined in `plugin/PLAN.md`) is mapped
1:1 with one renaming convention:

| Canonical path           | Workspace path inside `Dir()`                         |
|--------------------------|-------------------------------------------------------|
| `manifest.toml`          | `.claude/plugins/<name>/manifest.toml`                |
| `AGENTS.md`              | `.claude/plugins/<name>/AGENTS.md`                    |
| `README.md`              | `.claude/plugins/<name>/README.md`                    |
| `skills/<id>/...`        | `.claude/plugins/<name>/skills/<id>/...`              |
| `rules/<id>...`          | `.claude/plugins/<name>/rules/<id>...`                |
| `hooks/<id>...`          | `.claude/plugins/<name>/hooks/<id>...`                |
| `subagents/<id>...`      | `.claude/plugins/<name>/subagents/<id>...`            |
| `mcps/<id>...`           | `.claude/plugins/<name>/mcps/<id>...`                 |
| `assets/<rel>`           | `.claude/plugins/<name>/assets/<rel>`                 |

`<name>` is the **manifest's `name` field** sanitized to
`[a-z0-9-]{1,64}`. Two plugins with the same sanitized name rejected
as `ErrInvalidPluginLayout`.

Files at the plugin zip root that are not in the table above are
copied verbatim to `.claude/plugins/<name>/<basename>` so authors can
ship freeform extras.

## Edge cases & decisions

- **Operator wants a different plugin root.** Configure the plugin
  layout via a future `Options.PluginRoot string` knob. v1 hardcodes
  `.claude/plugins/`; users with strong needs replace the spec
  entirely.
- **Plugins claim the same name after sanitization.** Hard error at
  install; the operator renames one of the plugins. The manager does
  not auto-disambiguate (collisions usually mean a packaging mistake).
- **No `claude` binary in the infra.** `Install` fails fast with
  `ErrBinaryUnavailable`; the workspace ends up in `failed`. Operators
  fix the infra image and recreate.
- **Concurrent `Install` for two workspaces sharing a host (localdir
  with overlapping dirs).** Disallowed by the workspace controller —
  it reserves the working dir via `InfraOps.Init`. If two specs
  somehow end up inside the same dir (operator misconfiguration), the
  later one races and the controller surfaces the resulting error.

## Tests to write (no implementation in this pass)

1. `Type()` is `"claude-code"`.
2. `Install` happy path against a fake `infraops.InfraOps` writes the
   expected `.claude/settings.json`, top-level `AGENTS.md`, and the
   plugin layout for two attached plugins.
3. `Install` fails with `ErrBinaryUnavailable` when the fake `Exec`
   returns non-zero.
4. `PluginLayout.Apply` rejects two plugins that sanitize to the
   same name.
5. `Probe` round-trip extracts the version string.
6. `ProtocolDescriptor()` returns the documented shape and is stable
   across calls.

## Out of scope

- Bootstrapping the `claude` binary itself (downloading, npm install,
  permission setting). Operators bake it into the infra image, supply
  `Options.Binary`, or swap in a custom spec.
- Custom session-state persistence. Claude Code's own session store
  lives under the user's home directory; v1 does not relocate it.
- Multi-process gateways. Claude Code is invoked per-request via the
  CLI bridge.
