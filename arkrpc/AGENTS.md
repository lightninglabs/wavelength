# arkrpc

## Purpose

Server-side gRPC service definitions (ArkService, IndexerService) with generated
Go stubs, plus hand-written conversion utilities for domain types.
Proto source: `arkrpc/ark.proto`, `arkrpc/indexer.proto`.

## Key Types

- `TreePath` / `TreePathNode` / `TxOut` — Structured proto messages for the
  VTXO commitment tree path. Replaces opaque TLV bytes on the wire.
- `TreePathFromTree` / `TreePathToTree` — Lossless conversion between
  `tree.Tree` and `arkrpc.TreePath`. Uses deterministic pre-order flattening
  with sorted child indices. Re-exported under the narrower
  `arkrpc/treeconv` sub-package for callers that do not need the full gRPC
  surface.
- `AncestryPath` — Proto message carrying one commitment-tree fragment for
  multi-tree VTXO ancestry. Fields: `TreePath`, `CommitmentTxid []byte`,
  `InputIndices []uint32`, `TreeDepth uint32`. An indexer `VTXO` response
  carries `[]AncestryPath` for OOR VTXOs with cross-commitment ancestry.
- `AncestryPathFromTree(t, commitmentTxID, inputIndices)` — Constructs an
  `AncestryPath` from a `tree.Tree` by calling `TreePathFromTree` and computing
  `treeMaxDepth`. Depth walk is bounded by `MaxAncestryTreeWalkDepth = 32`.
- `AncestryPathToTree(p)` — Inverse of `AncestryPathFromTree`; converts proto
  `AncestryPath` back to `*tree.Tree` via `TreePathToTree`.
- `AncestryCommitmentTxID(p)` — Decodes `p.CommitmentTxid` bytes into a
  `chainhash.Hash`; returns an error if the length is not `chainhash.HashSize`.
- `MaxAncestryTreeWalkDepth = 32` — Max tree depth walked by `treeMaxDepth`;
  prevents runaway recursion on untrusted blobs (same bound as
  `db.MaxTreeDeserializeDepth`).
- `IncomingOOREvent` — Lightweight notification (wake-up hint). Carries only
  session_id, pk_script, event_id. Triggers the three-phase receive flow.
- `OORRecipientEvent` — Phase 1 query response from
  `ListOORRecipientEventsByScript`. Carries the full Ark PSBT and checkpoint
  PSBTs that `IncomingOOREvent` intentionally omits.
- `VTXO` — Phase 2 query response from `ListVTXOsByScripts`. Carries
  authoritative lineage metadata including `[]AncestryPath` (replaces the
  singular `TreePath` field).

## Relationships

- **Depends on**: `lib/tree` (for conversion utilities in `tree_path_convert.go`
  and `ancestry_path_convert.go`).
- **Depended on by**: `indexer`, `darepod`, `serverconn`, `oor` (uses generated
  clients and conversion helpers); `darepod` uses `AncestryPathToTree` /
  `AncestryCommitmentTxID` in `incoming_metadata.go` to convert indexer
  `VTXO.ancestry_paths` into `[]vtxo.Ancestry` for domain materialization.

## Invariants

- **Never edit generated code** — regenerate via `make rpc`.
- Conversion round-trip: `TreePathFromTree(t)` → `TreePathToTree(pb)` must
  reproduce the original tree (excluding derived `FinalKey` fields).
- Child iteration during flattening is sorted by output index for
  deterministic serialization.
- `AncestryPathFromTree` copies `inputIndices` to avoid aliasing the caller's
  slice; the proto bytes for `CommitmentTxid` are also cloned.
- `treeMaxDepth` is bounded by `MaxAncestryTreeWalkDepth = 32` to prevent
  unbounded recursion on operator-sourced tree blobs.
