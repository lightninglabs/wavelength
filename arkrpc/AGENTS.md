# arkrpc

## Purpose

Server-side gRPC service definitions (ArkService, IndexerService) with generated
Go stubs, plus hand-written conversion utilities for domain types.
Proto source: `arkrpc/ark.proto`, `arkrpc/indexer.proto`.

## Key Types

- `TreePath` / `TreePathNode` / `TxOut` — Structured proto messages for the
  VTXO commitment tree path. `TreePath.batch_asset_root` carries the optional
  32-byte Taproot Asset commitment sibling hash composed beside
  `sweep_tapscript_root` in `batch_output` for asset trees.
- `TreePathFromTree` / `TreePathToTree` — Lossless conversion between
  `tree.Tree` and `arkrpc.TreePath`. Uses deterministic pre-order flattening
  with sorted child indices. Re-exported under the narrower
  `arkrpc/treeconv` sub-package for callers that do not need the full gRPC
  surface.
- `IncomingOOREvent` — Lightweight notification (wake-up hint). Carries only
  session_id, pk_script, event_id. Triggers the three-phase receive flow.
- `OORRecipientEvent` — Phase 1 query response from
  `ListOORRecipientEventsByScript`. Carries the full Ark PSBT, checkpoint
  PSBTs, and optional Taproot Asset fields (`taproot_asset_root`,
  `taproot_asset_transfer`, `taproot_asset_ref`, `taproot_asset_amount`) that
  `IncomingOOREvent` intentionally omits. `OORSessionPackage` and
  `GetOORSessionByTxidResponse` also carry `taproot_asset_transfer` for
  ancestor and refetch lookups.
- `VTXO` / `IncomingVTXOEvent` — Authoritative inventory and push-event
  metadata, including the structured `TreePath` and optional Taproot Asset
  metadata (`taproot_asset_root`, `taproot_asset_ref`, `taproot_asset_amount`).
- `AncestryPath.commitment_sweep_key` / `commitment_sweep_delay` — Additive
  evidence that lets clients reconstruct the tree sweep leaf and authenticate
  an absolute expiry against a locally confirmed commitment transaction.
- `GetInfoResponse.oor_carrier_pubkey` / `LeaseOORCarrier` — Advertises the
  33-byte compressed public key owning the operator's OOR carrier float (empty
  when disabled) and leases a float VTXO (value `V >= required_sat` with
  `V == required_sat` or `V - required_sat >= min_vtxo_amount_sat`) to fund
  new asset-leaf carriers. Because the operator controls both keys of the
  leased float VTXO, operator-funded asset OOR outputs remain operator-trusted
  until refreshed in a round.

## Relationships

- **Depends on**: `lib/tree` (for conversion utilities in `tree_path_convert.go`) and
  `rpc/roundpb` (shared configured tree decode budgets).
- **Depended on by**: `indexer`, `waved`, `serverconn`, `oor` (uses generated
  clients and conversion helpers).

## Invariants

- `BatchSchedule` (version 1) publishes an opaque schedule ID, the operator's
  clock, and up to 32 concrete registration windows. `ParseBatchSchedule`
  validates the list on its own terms, keeps `server_time_unix`, and fails on
  any malformed or unknown-version schedule rather than falling back to
  event-driven registration. See `docs/scheduled_batches.md`.

- **Never edit generated code** — regenerate via `make rpc`.
- Conversion round-trip: `TreePathFromTree(t)` → `TreePathToTree(pb)` must
  reproduce the original tree (excluding derived `FinalKey` fields).
- `TreePathToTree` sits on the untrusted indexer receive path
  (`vtxo.AncestryFromRPC` → `AncestryPathToTree`), so it validates the
  decoded shape rather than trusting it: pre-order child indices (no
  cycles), single parent (no node named as a child twice), full
  reachability from index 0 (one connected tree, not a forest), index
  bounds, a node cap (`DefaultMaxTreePathNodes`, overridable via
  `WithMaxTreePathNodes`), and aggregate output and co-signer budgets that scale with the configured
  node limit. Explicit zero keeps default aggregate work budgets.
  Single-parent is the load-bearing shape check: `Children` is a map, so one
  node can point every output at the same next node, and `nodeMaxDepth` —
  which the receive path runs on this tree to validate the claimed ancestry
  depth — has no memoization, so a shared child multiplies its work by the
  number of paths reaching it.
  These mirror `roundpb.TreeFromProto`; keep the two decoders in step.
- Child iteration during flattening is sorted by output index for
  deterministic serialization.
