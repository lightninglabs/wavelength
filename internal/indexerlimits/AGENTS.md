# internal/indexerlimits

## Purpose

Client-side resource caps and validation helpers for indexer query parameters.
Keeps limit constants and their validation logic in one place so both the
serverconn durable query layer and the darepod incoming-metadata pipeline
enforce identical bounds on opaque cursors and query sizes.

## Key Types

- `MaxVTXOsByScriptsCursorBytes` — Constant (256): maximum byte length for
  the opaque pagination cursor accepted by `ListVTXOsByScripts` queries.
- `ValidateVTXOsByScriptsCursor` — Rejects cursors that exceed
  `MaxVTXOsByScriptsCursorBytes`; call at untrusted input boundaries.

## Relationships

- **Depends on**: nothing (stdlib only)
- **Depended on by**: `serverconn` (durable unary query construction),
  `darepod` (incoming metadata resolution), `indexer` (client tests)

## Invariants

- The 256-byte cap leaves room for format evolution beyond the current
  36-byte outpoint keyset cursor while still bounding untrusted remote bytes.

## Deep Docs

- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
