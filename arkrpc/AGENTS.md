# arkrpc

## Purpose

Server-side gRPC service definitions (ArkService, IndexerService) with generated
Go stubs, plus hand-written conversion utilities for domain types.
Proto source: `arkrpc/ark.proto`, `arkrpc/indexer.proto`.

## Key Types

- `TreePath` / `TreePathNode` / `TxOut` — Structured proto messages for a
  single flattened commitment-tree fragment (batch root to a served leaf).
- `TreePathFromTree` / `TreePathToTree` — Lossless conversion between
  `tree.Tree` and `arkrpc.TreePath`. Uses deterministic pre-order flattening
  with sorted child indices. Re-exported under the narrower
  `arkrpc/treeconv` sub-package for callers that do not need the full gRPC
  surface.
- `AncestryPath` — Wraps a `TreePath` with the anchoring `commitment_txid`,
  the `input_indices` (Ark tx input indices) it serves, and a claimed
  `tree_depth` scalar. A VTXO carries one `AncestryPath` per distinct
  contributing commitment tx; cross-round multi-input OOR VTXOs have more
  than one.
- `AncestryPathFromTree` / `AncestryPathToTree` (`ancestry_path_convert.go`)
  — Build/reconstruct an `AncestryPath`, delegating the tree body to
  `TreePathFromTree`/`TreePathToTree`. `ValidateAncestryPathDepth` is the
  trust boundary for indexer-supplied `tree_depth`: rejects zero, rejects
  values above `MaxAncestryTreeWalkDepth` (32, matching
  `db.MaxTreeDeserializeDepth`), and requires an exact match against the
  reconstructed tree's actual depth. `treeMaxDepth`/`nodeMaxDepth` compute
  that actual depth with recursion explicitly bounded by
  `MaxAncestryTreeWalkDepth` so a hostile deep chain in indexer-supplied
  bytes cannot overflow the goroutine stack.
- `IncomingOOREvent` — Lightweight notification (wake-up hint). Carries only
  session_id, pk_script, event_id. Triggers the three-phase receive flow.
- `OORRecipientEvent` — Phase 1 query response from
  `ListOORRecipientEventsByScript`. Carries the full Ark PSBT and checkpoint
  PSBTs that `IncomingOOREvent` intentionally omits.
- `VTXO` — Phase 2 query response from `ListVTXOsByScripts`. Carries
  authoritative lineage metadata via `ancestry_paths` (repeated
  `AncestryPath`), one entry per distinct contributing commitment tx. The
  old singular `tree_path`/scalar `tree_depth` fields on `VTXO` were
  retired in favor of this repeated field to support cross-round
  multi-input OOR VTXOs.

## Relationships

- **Depends on**: `lib/tree` (for conversion utilities in
  `tree_path_convert.go` and `ancestry_path_convert.go`).
- **Depended on by**: `indexer`, `darepod`, `serverconn`, `oor` (uses generated
  clients and conversion helpers).

## Invariants

- **Never edit generated code** — regenerate via `make rpc`.
- Conversion round-trip: `TreePathFromTree(t)` → `TreePathToTree(pb)` must
  reproduce the original tree (excluding derived `FinalKey` fields).
- Child iteration during flattening is sorted by output index for
  deterministic serialization.
- Ancestry depth is a trust boundary, not a convenience field: indexer
  responses are untrusted, so `ValidateAncestryPathDepth` must fail closed
  on a zero, oversized, or tree-inconsistent `tree_depth` claim rather than
  silently accepting it.
