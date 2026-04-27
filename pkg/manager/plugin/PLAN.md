# plugin/PLAN.md

## Purpose

Own everything plugin-related at the data layer:

- The on-disk **plugin package format** (zip layout + `manifest.toml`).
- The `Plugin` value type and the `Manifest` schema.
- The `Repository` interface (metadata persistence).
- The `Storage` interface (blob persistence) and a `MemoryStorage`
  reference implementation for tests.
- A `MemoryRepository` reference implementation for tests.
- Helpers for parsing and validating manifests, and for streaming a
  zip body into a virtual file system that callers (specifically
  `agent.PluginLayout` implementations) consume to lay files out
  inside an `InfraOps`.

This package is **agnostic** to:

- the agent kind (no Claude Code awareness),
- the transport (no HTTP, no S3 — `Storage` is the abstraction),
- the workspace (no `WorkspaceID` references).

## Public surface (intent only)

- Types:
  - `Plugin` per `DESIGN.md` §6.2.
  - `Manifest` per §3 below.
  - `Section` enum for the canonical sections.
- `Repository` interface (§4).
- `Storage` interface (§5).
- `Reader` interface — fs.FS-shaped view over an unpacked plugin
  package, used by `agent.PluginLayout.Apply`. Covered in §6.
- Helpers:
  - `ParseManifest(data []byte) (Manifest, error)`
  - `ValidateManifest(m Manifest) error`
  - `OpenZipReader(r io.ReaderAt, size int64) (Reader, error)` —
    when the caller already has a `ReaderAt` (e.g., file-backed
    storage) and the exact size.
  - `OpenZipReaderFromStream(ctx context.Context, r io.Reader, sizeHint int64, maxSize int64) (Reader, error)` —
    convenience wrapper that buffers the stream into memory (or an
    operator-supplied tempfile via a future option) up to `maxSize`
    bytes and then calls `OpenZipReader`. `sizeHint` lets the
    implementation pre-size the buffer when known. Used by the
    workspace install worker, which receives an `io.ReadCloser`
    from `Storage.Get`.
  - `Hash(content io.Reader) (sha string, n int64, err error)`
- Sentinels:
  - `ErrPluginNotFound`
  - `ErrPluginNameConflict`
  - `ErrInvalidManifest`
  - `ErrCorruptArchive`
  - `ErrStorageClosed`
  - `ErrStorageNotFound`
  - `ErrUnsupported` — returned by `Storage` methods that a backend
    cannot implement (e.g., `MemoryStorage.PresignURL`).
  - `ErrUnsupportedDownloadURL` — manager-level wrapper of
    `ErrUnsupported` returned from `Manager.GetPluginDownloadURL`
    when the configured backend does not mint URLs.

## Plugin package format

A plugin is a zip file that has the following layout. Section names
are reserved; everything else at the root is preserved verbatim and
made available to per-agent layouts that want it.

```
plugin.zip
├── manifest.toml          # REQUIRED. See §3.
├── README.md              # OPTIONAL. Author-facing overview.
├── AGENTS.md              # OPTIONAL. Agent-facing context, copied per layout rules.
├── skills/                # OPTIONAL. One subdirectory per skill (e.g., skills/<id>/SKILL.md + extras).
├── rules/                 # OPTIONAL. Files (e.g., *.mdc, *.md) describing rules.
├── hooks/                 # OPTIONAL. Hook descriptors (e.g., hooks.json + scripts).
├── subagents/             # OPTIONAL. Subagent prompt + config files.
├── mcps/                  # OPTIONAL. MCP server descriptors.
└── assets/                # OPTIONAL. Free-form referenced files.
```

Reserved names at zip root: `manifest.toml`, `README.md`, `AGENTS.md`,
`skills/`, `rules/`, `hooks/`, `subagents/`, `mcps/`, `assets/`. All
other names at the root are passed through but emit a structured
warning via `pkg/logs` (same `logs.FromContext` conventions as the rest
of the repo) during ingest.

### File-level rules

- All paths inside the zip use forward slashes and are relative to
  the zip root. Absolute paths or paths containing `..` are rejected
  as `ErrCorruptArchive`.
- File modes are stored verbatim; on extraction the layout decides
  whether to honor or normalize them (default: `0644` for files,
  `0755` for directories, executable bit kept if the source set it).
- Default maximum **compressed** body size: 64 MiB. Enforced by
  `Manager.CreatePlugin` before calling `Storage.Put`; the storage
  and repository layers trust the input. Operators that need larger
  plugins raise the limit at the manager root.
- Default maximum **decompressed** size during extraction: 256 MiB.
  Enforced by `OpenZipReaderFromStream` when it expands entries.
  Plugins beyond this are rejected as `ErrCorruptArchive` (a hostile
  zip-bomb defense, not a feature limit).
- Symlinks are rejected. Plugins MUST be portable across infras.
- **Explicit directory entries** in the zip are allowed. A directory
  header must use **exactly one** trailing `/` in the stored name; that
  slash is stripped, then the same `isSafeArchivePath` / `path.Clean`
  checks apply as for files. After normalization, a duplicate canonical
  path is still `ErrCorruptArchive`. Directory entries are recorded in
  the virtual `fs.FS` but **do not** appear in the manifest path
  `fileSet` (only regular files do); directory bodies are not read.

## Manifest

The manifest is `manifest.toml` at the zip root.

```toml
schema_version = 1

[plugin]
name        = "trading-research"
description = "Stock-research skills + hooks for Alice."

[plugin.metadata]
author        = "ANA contributors"
license       = "MIT"
homepage      = "https://example.invalid/trading-research"
tags          = ["finance", "research"]

# All four sections below are optional. Each section is a table whose
# entries describe one resource. The keys MAY restate the on-disk path
# (`path = "skills/<id>"`) but they MUST match what the zip actually
# contains; mismatches cause ErrInvalidManifest.

[skills.stock_lookup]
display_name = "Stock lookup"
path         = "skills/stock_lookup"

[skills.macro_recap]
display_name = "Macro recap"
path         = "skills/macro_recap"

[rules.style]
description = "Tone and citation rules"
path        = "rules/style.mdc"

[hooks.on_request]
description = "Pre-request normalizer"
path        = "hooks/on_request.json"

[subagents.researcher]
description = "Researcher subagent"
path        = "subagents/researcher.md"

[mcps.web]
description = "Web search MCP"
path        = "mcps/web.json"
```

### Validation rules

- `schema_version` MUST equal `1`. Future schemas bump the number; the
  package documents migrations.
- `plugin.name` matches `[a-z0-9-]{1,64}`. It is the canonical
  identity used in directory placement.
- `plugin.description` ≤ 1024 characters; markdown-friendly free text.
- `plugin.metadata` is a free table; the package validates only that
  it is a TOML table (no nested arrays beyond simple scalar arrays
  like `tags`).
- Each section entry MUST point to a `path` that exists in the zip.
  Validation walks the archive once and produces all mismatches in a
  single error.
- Duplicated entry keys across sections are allowed (e.g., `skills.x`
  and `rules.x`). Within one section, keys are unique.

`ParseManifest` returns the structured value; `ValidateManifest`
runs the rules above plus cross-validates against the archive when
called via `OpenZipReader` (which threads the validation internally).

### Manifest Go shape

```go
type Manifest struct {
    SchemaVersion int                            `toml:"schema_version"`
    Plugin        ManifestPlugin                 `toml:"plugin"`
    Skills        map[string]ManifestEntry       `toml:"skills,omitempty"`
    Rules         map[string]ManifestEntry       `toml:"rules,omitempty"`
    Hooks         map[string]ManifestEntry       `toml:"hooks,omitempty"`
    Subagents     map[string]ManifestEntry       `toml:"subagents,omitempty"`
    MCPs          map[string]ManifestEntry       `toml:"mcps,omitempty"`
}

type ManifestPlugin struct {
    Name        string                 `toml:"name"`
    Description string                 `toml:"description,omitempty"`
    Metadata    map[string]any         `toml:"metadata,omitempty"`
}

type ManifestEntry struct {
    Description string `toml:"description,omitempty"`
    DisplayName string `toml:"display_name,omitempty"`
    Path        string `toml:"path"`
}
```

The `Plugin.Manifest` field on the manager `Plugin` value (per
`DESIGN.md` §6.2) is `Manifest` — copied by value at upload time.

## Repository

```go
type Repository interface {
    // Insert atomically inserts a Plugin row. Returns
    // ErrPluginNameConflict when (Namespace, Name) is taken.
    Insert(ctx context.Context, p Plugin) error

    // Update replaces an existing Plugin (used for "overwrite on
    // re-upload"). Implementations MUST keep the original ID and
    // CreatedAt; the input's UpdatedAt is taken verbatim.
    Update(ctx context.Context, p Plugin) error

    Get(ctx context.Context, id PluginID) (Plugin, error)
    GetByName(ctx context.Context, namespace Namespace, name string) (Plugin, error)
    List(ctx context.Context, opts ListOptions) ([]Plugin, string, error) // returns (rows, nextCursor, err)
    Delete(ctx context.Context, id PluginID) error
    Close(ctx context.Context) error
}

type ListOptions struct {
    Namespace Namespace
    NameLike  string
    Limit     int
    Cursor    string
}
```

Implementations:

- `MemoryRepository` — reference, mutex-protected, suitable for
  tests.
- Production deployments supply a SQL-backed implementation and
  inject it through `Builder.PluginRepository`. v1 ships only the
  in-memory reference.

### Concurrency contract

- All methods are safe for concurrent use.
- `Insert` and `Update` MUST be atomic with respect to themselves and
  each other. The combined "upsert" used by `Manager.CreatePlugin`
  is implemented as `Insert` then, on `ErrPluginNameConflict`,
  `Update`. Implementations MAY expose a private `Upsert` for
  efficiency but it is not part of the v1 surface.

## Storage

```go
type Storage interface {
    // Put stores the zip body under `id`. Implementations MUST detect
    // that the same `id` is being written concurrently and either
    // serialize or fail with ErrStorageBusy (an internal sentinel
    // that callers retry; not exported in v1).
    Put(ctx context.Context, id PluginID, body io.Reader) (StoredObject, error)

    // Get returns a streaming reader for the stored body.
    Get(ctx context.Context, id PluginID) (io.ReadCloser, StoredObject, error)

    // Delete is idempotent; deleting a missing id returns nil.
    Delete(ctx context.Context, id PluginID) error

    // PresignURL returns a time-limited URL the caller can use to
    // download the body without going through the manager. The
    // implementation may return ErrUnsupported when the backend
    // cannot mint URLs (e.g., MemoryStorage); the manager surfaces
    // that as ErrUnsupportedDownloadURL and the caller falls back
    // to streaming Get.
    PresignURL(ctx context.Context, id PluginID, opts PresignOptions) (string, error)

    // Optional housekeeping; implementations may return ErrUnsupported.
    List(ctx context.Context) ([]PluginID, error)

    Close(ctx context.Context) error
}

type StoredObject struct {
    Size        int64
    ContentHash string // sha256:<hex>; computed by manager and verified on retrieval
}

type PresignOptions struct {
    TTL     time.Duration
    Method  string // "GET" by default; future: "PUT" for uploads (out of v1)
}
```

Implementations:

- `MemoryStorage` — keeps zip bodies in a `map[PluginID][]byte` under
  a mutex. Returns `ErrUnsupported` from `PresignURL` and
  `ErrUnsupported` from `List` is allowed but the reference
  implementation supports `List` for tests.
- Production deployments supply an S3- or GCS-backed implementation
  and inject it through `Builder.PluginStorage`.

### Concurrency contract

- All methods are safe for concurrent use.
- `Put` MUST overwrite a previous body for the same id atomically: a
  concurrent `Get` either sees the old body or the new body, never a
  mix. `MemoryStorage` achieves this with a write-lock around the
  swap; backends that lack this guarantee MUST keep both versions
  briefly and switch references atomically.

## Reader (unpacked plugin)

```go
type Reader interface {
    // Manifest returns the parsed manifest for the plugin.
    Manifest() Manifest
    // FS returns an fs.FS rooted at the zip root, with directories
    // mirroring the on-disk layout. Implementations MUST NOT decode
    // entries lazily in a way that races with Close.
    FS() fs.FS
    // Close releases any underlying resources (e.g., the zip reader).
    Close() error
}
```

`OpenZipReader(r io.ReaderAt, size int64) (Reader, error)` validates
the manifest against the archive contents and returns the reader.

`agent.PluginLayout.Apply` consumes a `Reader.FS()` to walk the tree
and stream files into `InfraOps.PutFile`. The `Reader` is short-lived
— closed after layout finishes.

## Edge cases & decisions

- **Zip without manifest.** Rejected as `ErrInvalidManifest` at
  `OpenZipReader`.
- **Manifest references a missing path.** Rejected with a single
  `ErrInvalidManifest` carrying every miss as `errors.Join`.
- **Re-upload (same `(namespace, name)`).** `Manager.CreatePlugin`
  treats this as overwrite. The flow is:
  1. `Repository.GetByName(ns, name)` resolves the existing
     `PluginID` (if any).
  2. `Storage.Put(existingID, body)` overwrites the previous zip
     under the **same** id (per the Storage atomicity contract above).
  3. `Repository.Update(plugin)` rewrites the metadata row, keeping
     `ID` and `CreatedAt` unchanged and bumping `UpdatedAt`,
     `ContentHash`, `Size`, and `Manifest`.
  Workspaces that attached the plugin previously are not affected;
  they own their extracted files. There is no "freshly-keyed" blob —
  the blob lives under the original id for the lifetime of the
  Plugin row.
- **Storage out of disk.** `Put` returns the underlying error wrapped
  as `fmt.Errorf("plugin storage put: %w", err)`. The manager fails
  the upload and does not retry.
- **Storage and Repository drift (orphan blob).** Tolerated; surfaced
  via the optional `Storage.List` minus `Repository.List`. v1 does
  not ship a sweep loop.
- **Aggregate size limits.** Enforced by manager-level options before
  calling `Storage.Put`; the storage layer itself trusts the input.

## Tests to write (no implementation in this pass)

1. `MemoryRepository` round-trip (`Insert`, `Get`, `GetByName`,
   `List`, `Delete`).
2. `MemoryStorage` Put/Get/Delete with hash verification.
3. `MemoryStorage.Put` overwrite atomicity under `-race`.
4. `ParseManifest` accepts the example in §3, rejects empty `name`,
   wrong `schema_version`, missing path, etc.
5. `OpenZipReader` rejects archives with absolute paths, `..`, or
   symlinks.
6. `Reader.FS()` round-trip over a synthetic zip.
7. `OpenZipReaderFromStream` honors `maxSize` and rejects oversized
   archives with `ErrCorruptArchive`.
8. `Repository.Insert` rejects duplicates with
   `ErrPluginNameConflict`.

## Out of scope

- Concrete production storage backends (S3, GCS, local-fs).
  Operators implement them and inject through the Builder.
- A plugin-authoring CLI (lint/build/sign). It will live in a
  separate package once the format stabilizes.
- Cross-plugin dependencies (one plugin requiring another). v2.
- Signing / verification. v2.
