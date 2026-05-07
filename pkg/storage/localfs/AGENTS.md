# pkg/storage/localfs AGENTS.md

## Scope
Local filesystem-backed object storage implementation for Plugin ZIP files. Implements `plugin.Storage`.

## Rules
- `Put` operations MUST be atomic from a reader's perspective (e.g., write to a temporary file, then `os.Rename`).
- Strictly validate all paths to prevent directory traversal escapes.
- Provide a robust cleanup mechanism for orphaned temporary files.

## Don'ts
- Do not store metadata here; this is strictly for binary blobs.
- Do not load entire files into memory; rely on `io.Reader` and `io.ReadCloser` for streaming.
