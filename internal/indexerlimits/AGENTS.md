# internal/indexerlimits

## Purpose

Client-side resource bounds for indexer query pagination cursors. Enforces a
byte-length cap on opaque cursor values returned by the remote indexer for
`ListVTXOsByScripts` queries to prevent memory exhaustion or parse failures
from a misbehaving server.

## Key Types

- `MaxVTXOsByScriptsCursorBytes` — Constant (256). Maximum opaque cursor byte
  length accepted for `ListVTXOsByScripts` pagination. Current server cursor
  is a 36-byte outpoint keyset; 256 leaves room for format evolution.
- `ValidateVTXOsByScriptsCursor(cursor []byte) error` — Returns an error if
  `len(cursor) > MaxVTXOsByScriptsCursorBytes`.

## Relationships

- **Depends on**: nothing (no internal imports).
- **Depended on by**: `indexer` (validates cursors from remote indexer),
  `serverconn` (validates cursors in durable unary query pagination),
  `darepod` (validates cursors in incoming RPC metadata handlers).

## Deep Docs

- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
