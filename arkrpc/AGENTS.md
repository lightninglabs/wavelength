# arkrpc

## Purpose

Server-side gRPC service definitions (ArkService, IndexerService) with generated
Go stubs, plus hand-written conversion utilities for domain types and the Ark
protocol version constant.
Proto source: `arkrpc/ark.proto`, `arkrpc/indexer.proto`.

See [`ARCHITECTURE.md`](../ARCHITECTURE.md) for how this package fits into the
overall system.

## Key Types

- `ArkService` — `GetInfo` (bootstrap/handshake) and `EstimateFee`.
  `GetInfoRequest.supported_ark_versions` lists the Ark protocol versions the
  client can use, most-preferred first; `GetInfoResponse.selected_ark_version`
  is the operator's pick (zero means no common version — client must not
  start its mailbox runtime), and `ark_version_policies` (`ArkVersionPolicy`,
  states `STATE_ACTIVE`/`STATE_DISABLED`) advertises the operator's full
  version lifecycle. `ArkProtocolVersionV1` (`version.go`) is the only
  version production advertises today.
- `GetInfoResponse` — Operator parameters clients need to construct boarding
  scripts and validate VTXOs: `pubkey`, `boarding_exit_delay`,
  `vtxo_exit_delay`, `dust_limit`, `min_boarding_amount`, `max_vtxo_amount`
  (cap applies to every VTXO the operator creates — boarding, round outputs,
  and OOR recipient outputs; renamed from `max_boarding_amount`),
  `min_vtxo_amount_sat`, `max_user_balance` (advisory total-balance cap),
  `fee_rate`, `max_oor_lineage_vbytes`. Field numbers 7–9 (`forfeit_script`,
  `sweep_key`, `sweep_delay`) are retired and unused: those values are now
  delivered per-round via `roundpb.ClientBatchInfo` instead of globally.
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
  PSBTs that `IncomingOOREvent` intentionally omits, plus
  `vtxo_policy_template` — the semantic arkscript policy template for the
  recipient output. Older servers may omit `vtxo_policy_template`; clients
  fall back to standard VTXO materialization when it is absent.
- `VTXO` — Phase 2 query response from `ListVTXOsByScripts`. Carries
  authoritative lineage metadata including the structured `TreePath`.

## Relationships

- **Depends on**: `lib/tree` (for conversion utilities in `tree_path_convert.go`).
- **Depended on by**: `indexer`, `darepod`, `serverconn`, `oor` (uses generated
  clients and conversion helpers). `roundpb.ClientBatchInfo` now carries the
  forfeit/sweep parameters previously exposed here.

## Invariants

- **Never edit generated code** — regenerate via `make rpc`.
- Conversion round-trip: `TreePathFromTree(t)` → `TreePathToTree(pb)` must
  reproduce the original tree (excluding derived `FinalKey` fields).
- Child iteration during flattening is sorted by output index for
  deterministic serialization.
- Ark version negotiation is additive-compatible: an old `GetInfoRequest`
  (no `supported_ark_versions`) decodes with the field empty; an old
  `GetInfoResponse` (pre-versioning fields only) decodes with all new
  version fields at zero. See `version_compat_test.go`.
