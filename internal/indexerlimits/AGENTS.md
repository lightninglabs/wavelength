# internal/indexerlimits

## Purpose

Provides shared client-side validation bounds for opaque pagination cursors
returned by the indexer's `ListVTXOsByScripts` RPC. Centralizes the limit so
all consumers enforce the same cap rather than each duplicating a magic number.

## Key Types

- `MaxVTXOsByScriptsCursorBytes` — `const = 256`. Maximum byte length for a
  `ListVTXOsByScripts` cursor. Sized to accommodate the current 36-byte
  outpoint keyset cursor format with room for protocol evolution.
- `ValidateVTXOsByScriptsCursor(cursor []byte) error` — Rejects cursors that
  exceed `MaxVTXOsByScriptsCursorBytes`. Returns a descriptive error carrying
  the actual cursor length.

## Relationships

- **Depends on**: nothing (standard library only).
- **Depended on by**:
  - `darepod` (`incoming_metadata.go` — validates cursors in the
    `ListVTXOsByScripts` metadata handler).
  - `serverconn` (`durable_unary_queries.go` — validates cursors in the
    paginated and streaming `SendListVTXOsByScriptsRequest` paths).
  - `indexer` (`client.go` — validates cursors in the
    `ListVTXOsByScripts` paginator client).

## Invariants

- All indexer-cursor callers MUST import from this package. Duplicating the
  bound in each call site would allow them to diverge silently on protocol
  changes.

## Deep Docs

- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
