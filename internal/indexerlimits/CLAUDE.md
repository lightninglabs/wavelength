# internal/indexerlimits

## Purpose

Client-side safety bounds for indexer pagination to prevent resource
exhaustion from untrusted remote servers. Validates opaque cursor blobs
before they are used in `ListVTXOsByScripts` queries.

## Key Types

- `MaxVTXOsByScriptsCursorBytes = 256` — Maximum byte length accepted for
  an opaque `ListVTXOsByScripts` pagination cursor. The current server
  uses a 36-byte outpoint keyset cursor; the cap leaves room for format
  evolution while bounding untrusted remote bytes.
- `ValidateVTXOsByScriptsCursor(cursor []byte) error` — Returns an error
  if `len(cursor)` exceeds `MaxVTXOsByScriptsCursorBytes`. Called in the
  OOR phase-2 metadata resolution path before the cursor is forwarded
  into the next page request.

## Relationships

- **Depends on**: (no internal repo imports).
- **Depended on by**: `indexer` (cursor length validation before passing
  server-supplied cursors to subsequent `ListVTXOsByScripts` requests).

## Invariants

- Validation is a pure byte-length check; no attempt is made to decode
  or interpret the cursor format, which is server-defined and opaque to
  the client.

## Deep Docs

- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
