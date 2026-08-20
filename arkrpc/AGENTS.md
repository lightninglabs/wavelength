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
  authoritative lineage metadata including the structured `TreePath`, and
  the optional Taproot Asset triple `taproot_asset_root` /
  `taproot_asset_ref` / `taproot_asset_amount`.
- `LeaseOORCarrierRequest` / `LeaseOORCarrierResponse` — the operator's
  carrier-float lease (see below).

## Carrier Lease Surface

New asset leaves in an out-of-round transfer ride Bitcoin carriers the
**operator** funds, so the client never has to hold satoshis in Ark to
send an asset. Two pieces of `ArkService` make that possible:

- `GetInfoResponse.oor_carrier_pubkey` (field 24) — the x-only public key
  owning the operator's carrier float, empty when carrier funding is
  disabled. The client parses it into `types.OperatorTerms`
  (`OORCarrierPubKey`) and uses it to authenticate the leased policy: the
  float's policy owner must be this exact key.
- `rpc LeaseOORCarrier(LeaseOORCarrierRequest) returns
  (LeaseOORCarrierResponse)` — the client asks for `required_sat` (the
  operator's minimum VTXO amount times the number of new asset leaves)
  and gets back the float VTXO to spend: `outpoint`, `value_sat`,
  `vtxo_policy_template`, `pk_script`, `expires_at_unix`. The client
  spends the float whole and returns the residual to `pk_script` as the
  operator's change.

There is no release RPC by design: an unused lease expires on its own
once `expires_at_unix` passes with no submit consuming the outpoint, so a
client that crashes between leasing and submitting costs the operator
nothing permanent and needs no compensating call.

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
- A `LeaseOORCarrierResponse` is never trusted on its face. The client
  re-derives the policy from `vtxo_policy_template`, requires it to match
  `pk_script`, validates it against the operator's long-term key, and
  requires its owner to be the `oor_carrier_pubkey` from `GetInfo` before
  the float becomes a transfer input. A lease worth less than the
  requested amount is refused.
- An empty `oor_carrier_pubkey` means the operator does not fund
  carriers, which makes asset OOR sends unavailable rather than
  silently sender-funded.

## Deep Docs

- [docs/taproot_assets_architecture.md](../docs/taproot_assets_architecture.md)
  — How the carrier lease is consumed, and the reclaim arithmetic on the
  operator's change.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
