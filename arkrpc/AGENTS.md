# arkrpc

## Purpose

Server-side gRPC service definitions (ArkService, IndexerService) with generated
Go stubs, plus hand-written conversion utilities for domain types.
Proto source: `arkrpc/ark.proto`, `arkrpc/indexer.proto`.

## Key Types

- `TreePath` / `TreePathNode` / `TxOut` — Structured proto messages for the
  VTXO commitment tree path.
- `TreePathFromTree` / `TreePathToTree` — Lossless conversion between
  `tree.Tree` and `arkrpc.TreePath`. Uses deterministic pre-order flattening
  with sorted child indices. Re-exported under the narrower
  `arkrpc/treeconv` sub-package for callers that do not need the full gRPC
  surface.
- `IncomingOOREvent` — Lightweight notification (wake-up hint). Carries only
  session_id, pk_script, event_id. Triggers the three-phase receive flow.
- `OORRecipientEvent` — Phase 1 query response from
  `ListOORRecipientEventsByScript`. Carries the full Ark PSBT and checkpoint
  PSBTs that `IncomingOOREvent` intentionally omits.
- `VTXO` — Phase 2 query response from `ListVTXOsByScripts`. Carries
  authoritative lineage metadata including the structured `TreePath`.

## Relationships

- **Depends on**: `lib/tree` (for conversion utilities in `tree_path_convert.go`).
- **Depended on by**: `indexer`, `waved`, `serverconn`, `oor` (uses generated
  clients and conversion helpers).

## Invariants

- **Never edit generated code** — regenerate via `make rpc`.
- Conversion round-trip: `TreePathFromTree(t)` → `TreePathToTree(pb)` must
  reproduce the original tree (excluding derived `FinalKey` fields).
- `TreePathToTree` sits on the untrusted indexer receive path
  (`vtxo.AncestryFromRPC` → `AncestryPathToTree`), so it validates the
  decoded shape rather than trusting it: pre-order child indices (no
  cycles), single parent (no node named as a child twice), full
  reachability from index 0 (one connected tree, not a forest), index
  bounds, and a node cap (`DefaultMaxTreePathNodes`, overridable via
  `WithMaxTreePathNodes`). Single-parent is the load-bearing one:
  `Children` is a map, so one node can point every output at the same
  next node, and `nodeMaxDepth` — which the receive path runs on this
  tree to validate the claimed ancestry depth — has no memoization, so a
  shared child multiplies its work by the number of paths reaching it.
  These mirror `roundpb.TreeFromProto`; keep the two decoders in step.
- Child iteration during flattening is sorted by output index for
  deterministic serialization.
