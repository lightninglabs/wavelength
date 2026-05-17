# internal/indexerlimits

## Purpose

Shared client-side bounds for indexer pagination cursors. Prevents untrusted
remote bytes from growing unboundedly when passed through client pagination
logic.

## Key Types

- `MaxVTXOsByScriptsCursorBytes` — Constant (256 bytes) capping the opaque
  cursor accepted for `ListVTXOsByScripts` pagination. The current server
  cursor is a 36-byte outpoint keyset; the cap leaves room for format
  evolution while bounding untrusted remote bytes.
- `ValidateVTXOsByScriptsCursor(cursor []byte) error` — Rejects cursors that
  exceed `MaxVTXOsByScriptsCursorBytes`.

## Relationships

- **Depends on**: nothing (stdlib only).
- **Depended on by**: `serverconn` (validates `AfterCursor` on
  `SendListVTXOsByScriptsRequest` before enqueueing the durable query).

## Deep Docs

- [internal/CLAUDE.md](../CLAUDE.md) — Parent package overview.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
