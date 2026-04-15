# arkrpc

## Purpose

Server-side gRPC service definitions (ArkService, IndexerService) with generated
Go stubs. Tree-path conversion utilities now live in the `arkrpc/treeconv`
subpackage to keep the generated client package lighter.
Proto source: `arkrpc/ark.proto`, `arkrpc/indexer.proto`.

## Key Types

- `TreePath` / `TreePathNode` / `TxOut` — Structured proto messages for the
  VTXO commitment tree path. Replaces opaque TLV bytes on the wire.
- `IncomingOOREvent` — Lightweight notification (wake-up hint). Carries only
  session_id, pk_script, event_id. Triggers the three-phase receive flow.
- `OORRecipientEvent` — Phase 1 query response from
  `ListOORRecipientEventsByScript`. Carries the full Ark PSBT and checkpoint
  PSBTs that `IncomingOOREvent` intentionally omits.
- `VTXO` — Phase 2 query response from `ListVTXOsByScripts`. Carries
  authoritative lineage metadata including the structured `TreePath`.

## Relationships

- **Depends on**: generated gRPC types only.
- **Depended on by**: `indexer`, `darepod`, `serverconn`, `oor` (uses generated
  clients). Tree conversion helpers are consumed via `arkrpc/treeconv`.

## Invariants

- **Never edit generated code** — regenerate via `make rpc`.
- Child iteration during flattening is sorted by output index for
  deterministic serialization.
