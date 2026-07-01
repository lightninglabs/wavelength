# daemonrpc

## Purpose

Daemon gRPC API definitions for wallet operations, VTXO lifecycle
(expiry/refresh/forfeit-signing), OOR, boarding, round queries, and VHTLC
recovery. Proto source: `daemonrpc/daemon.proto`.

See [`ARCHITECTURE.md`](../ARCHITECTURE.md) for how this package fits into the
overall system.

## Key Types

- `DaemonService` — the full daemon RPC surface implemented by `darepod` and
  consumed by `cmd/darepocli` and other daemon clients.
- `ServerInfo` — operator terms cached from the Ark server's `GetInfo`, now
  including `max_vtxo_amount` (renamed from `max_boarding_amount`; applies to
  boarding, round, and OOR recipient outputs alike), `min_vtxo_amount_sat`,
  and `max_user_balance`. Field numbers 4–6 (`forfeit_script`, `sweep_key`,
  `sweep_delay`) are retired: those values now arrive per-round via
  `roundpb.ClientBatchInfo`.
- `GetVTXOExpiryInfo` / `VTXOExpiryInfo` / `VTXOExpiryStatus` — classifies a
  VTXO's batch-expiry posture (`SAFE`, `NEEDS_REFRESH`, `CRITICAL`,
  `EXPIRED`) by outpoint (local wallet store) or pkScript (authoritative
  indexer), driving cooperative-refresh vs. unilateral-exit decisions.
- `SignVTXOForfeit` — low-level primitive that signs the VTXO input of an
  exact, round-assigned forfeit transaction with the daemon identity key;
  callers must enforce swap-specific authorization before invoking it.
- `RefreshCustomVTXOs` / `CustomRefreshVTXOInput` / `CustomRefreshVTXOOutput`
  — queues caller-supplied custom-policy VTXOs (not necessarily
  wallet-managed) for refresh, with explicit policy/auth/forfeit spend
  paths. Because the round has not yet assigned a connector when this is
  called, it cannot collect non-daemon participant signatures inline.
- `ForfeitSigningContext` / `ForfeitSigningRoute` — attached to a custom
  refresh input, tells the daemon whether the later connector-bound forfeit
  signature should be answered by a `LOCAL_SIGNER` or published as a
  `PENDING_REQUEST` for an external participant.
- `ListPendingForfeitParticipantSignatureRequests` /
  `PendingForfeitParticipantSignatureRequest` — exposes exact
  connector-bound forfeit transcripts once the round assigns connectors, so
  an external participant can validate, sign, and submit.
- `SubmitForfeitParticipantSignatures` / `ForfeitParticipantSignature` —
  supplies external participant signatures (or an empty set, if the spend
  path needs none) for a pending request, waking the blocked VTXO actor.
- `SendOORRequest`/`SendOORResponse` — now carry `recipients`/
  `recipient_outpoints` (plural, request-recipient order) instead of a
  single `recipient`/`recipient_outpoint`, supporting multi-recipient OOR
  transfers in one session.
- `InitWalletRequest`/`InitWalletResponse` — gained `recover_state` and
  `recovery_window` (request) and `recovery_ran` plus
  `recovered_boarding_addresses`/`recovered_boarding_utxos`/
  `recovered_vtxos`/`recovered_oor_receive_scripts`/`recovered_oor_events`
  counters (response), for rebuilding Ark wallet state from chain/indexer
  data after seed restore.
- `ListVTXOsRequest.exclude_checkpoint_psbts` — skips attaching finalized
  OOR checkpoint PSBTs per VTXO for listing-only consumers (balance views,
  coin selection) that never inspect them, avoiding one artifact-store read
  per VTXO.
- VHTLC recovery RPCs (`ArmVHTLCRecovery`, `EscalateVHTLCRecovery`,
  `CancelVHTLCRecovery`, `GetVHTLCRecoveryStatus`, `ListVHTLCRecoveries`) and
  unroll RPCs (`Unroll`, `GetUnrollStatus`) for unilateral-exit paths.

## Relationships

- **Depends on**: nothing (proto definitions); documents concepts owned by
  `roundpb` (`ClientBatchInfo`) and `wallet`/`vtxo` packages.
- **Depended on by**: `darepod` (implements services), `cmd/darepocli` (uses generated clients).

## Invariants

- **Never edit generated code** — regenerate via `make rpc`.
- `SignVTXOForfeit` is a bare signing primitive: it does not itself enforce
  swap/participant authorization — callers are responsible for that.
- `ForfeitSigningRoute.PENDING_REQUEST` transcripts must be validated in
  full by external submitters before signing; `SubmitForfeitParticipantSignatures`
  matches signatures to the request by `request_id`.
