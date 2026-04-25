# pkg/manager/plugin AGENTS.md

## Scope

Plugin data layer: the on-disk plugin package format (zip layout +
`manifest.toml`), the `Plugin` value type, the `Manifest` schema, the
`Repository` interface (metadata persistence) and the `Storage`
interface (blob persistence), plus reference in-memory implementations
(`MemoryRepository`, `MemoryStorage`) used in tests.

This package is agent-agnostic and transport-agnostic. Per-agent
layout policies live in `pkg/manager/agent/<type>/`; production
storage backends (S3, GCS, …) live in operator code.

## Rules

- The plugin package format is the contract with plugin authors. Any
  change to the manifest schema or zip layout requires a
  `schema_version` bump and an entry in `DESIGN.md`.
- All exported helpers (`ParseManifest`, `ValidateManifest`,
  `OpenZipReader`, `OpenZipReaderFromStream`, `Hash`) are pure
  functions — no IO that the caller cannot observe through the
  supplied reader. `OpenZipReaderFromStream` may allocate a buffer
  bounded by `maxSize`; that is its only side effect.
- `Repository.Insert` and `Repository.Update` are atomic with respect
  to themselves and each other. Verify with `go test -race`.
- `Storage.Put` overwrite is atomic from a `Get`-er's perspective.
  Verify with `go test -race`.
- `MemoryStorage.PresignURL` returns `ErrUnsupported`; durable
  backends implement it.

## Don'ts

- Do not import `pkg/manager/agent`, `pkg/manager/workspace`, or the
  manager root. Plugin is a leaf module in the dependency graph.
- Do not couple to a specific transport (HTTP handlers, gRPC, S3 SDK).
  Backends plug in through the `Storage` interface.
- Do not perform plugin **content** transformations here (renaming
  files, rewriting manifests). Layout decisions belong to
  `agent.PluginLayout` implementations.
- Do not silently downgrade a `schema_version` mismatch. Reject with
  `ErrInvalidManifest`; the operator updates the plugin or pins an
  older manager.
- Do not log plugin metadata values that may carry author secrets
  (license keys, credentials). The `metadata` table is free-form;
  treat it as opaque.
