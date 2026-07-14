# internal/indexerlimits

## Purpose

Client-side resource limits for indexer pagination cursors. Guards against
untrusted remote pagination state by bounding opaque cursor bytes before
passing them to server queries.

## Key Types

- `MaxVTXOsByScriptsCursorBytes = 256` — Maximum byte length for a
  `ListVTXOsByScripts` pagination cursor accepted from a remote indexer.
- `ValidateVTXOsByScriptsCursor(cursor []byte) error` — Rejects cursors
  exceeding the limit. Returns an error with the allowed and actual sizes.

## Relationships

- **Depends on**: nothing (stdlib only).
- **Depended on by**: `serverconn`, `waved`, `indexer`, `vtxo` (validate
  inbound `ListVTXOsByScripts` cursors before query execution).

## Invariants

- The current server cursor format is a 36-byte outpoint keyset cursor;
  the 256-byte limit provides headroom for format evolution while bounding
  the memory impact of a malformed or misbehaving indexer.

## Deep Docs

- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
