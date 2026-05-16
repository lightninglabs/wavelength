# internal/indexerlimits

## Purpose

Shared client-side resource caps for indexer query parameters. Centralises
limit constants so that both the gRPC handler layer and the durable unary
query layer enforce identical bounds without duplicating magic numbers.

## Key Types

- `MaxVTXOsByScriptsCursorBytes` — Constant (256) capping the opaque cursor
  accepted for `ListVTXOsByScripts` pagination requests.
- `ValidateVTXOsByScriptsCursor(cursor []byte) error` — Rejects cursors
  exceeding `MaxVTXOsByScriptsCursorBytes`; returns a descriptive error
  carrying the actual and allowed lengths.

## Relationships

- **Depends on**: nothing beyond standard `fmt`.
- **Depended on by**: `serverconn` (validates cursors in durable unary query
  messages before persisting them), `darepod` (RPC handler pre-validation).

## Invariants

- The cursor cap is intentionally generous (256 bytes vs the current 36-byte
  server cursor) to leave room for format evolution while bounding untrusted
  remote bytes.

## Deep Docs

- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
