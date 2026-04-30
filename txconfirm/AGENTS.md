# txconfirm

## Purpose

Generic "broadcast this signed tx, tell me when it confirms, and fee-bump it
via CPFP until it does" actor. Subsystem-neutral: no unroll/, vtxo/, oor/, or
round/ semantics leak in. Callers submit a signed v3/TRUC parent via
`EnsureConfirmedReq` and receive a terminal `TxConfirmed` or `TxFailed`
notification. Dedup is by txid: two callers asking to confirm the same txid
share a single confirmation watch, broadcast attempt, and CPFP child, but
each still receives its own terminal notification.

## Key Types

- `TxBroadcasterActor` — Message-driven orchestrator (in `actor.go`). Holds
  a txid-keyed tracked-tx map, runs a protofsm lifecycle per txid, and
  fans chainsource callbacks (confirmations, block epochs) back into
  per-txid state transitions.
- `CPFPBroadcaster` — Actor-free helper (in `broadcaster.go`) that handles
  broadcast mechanics: direct submission for txs without anchors, CPFP
  child construction for anchor parents, fee estimation, script-aware
  child vsize estimation, fee-input selection and reservation, BIP-125
  Rule 3/4 replacement floor enforcement, and optional TestMempoolAccept
  preflight. Usable standalone if a caller only needs the broadcast
  primitives.
- `Wallet` — Wallet interface the broadcaster requires: `ListUnspent`,
  `NewWalletPkScript`, `FinalizePsbt`, plus `wallet.OutputLeaser`
  (`LeaseOutput` / `ReleaseOutput`) for cross-subsystem UTXO lock
  coordination.
- `EnsureConfirmedReq` / `EnsureConfirmedResp` — Public Ask API: register
  interest in a txid with a `TargetConfs`, `ConfirmationPkScript`, and a
  subscriber that receives the terminal notification.
- `CancelInterestReq` / `CancelInterestResp` — Public Ask API: drop a
  subscriber; the last subscriber's cancel also tears down tracking.
- `TxConfirmed` / `TxFailed` — Terminal `Notification` types delivered to
  each subscriber.
- `TxState` (`New`, `Broadcasting`, `AwaitingConfirmation`, `FeeBumping`,
  `Confirmed`, `Failed`) — Public view of the per-txid protofsm state.
- `ErrNonTRUCParent` — Sentinel returned by `Submit` when the parent is
  not v3/TRUC.
- `ErrCPFPFeeInputUnavailable` — Sentinel returned when no confirmed
  wallet UTXO is available for the CPFP fee input.
- `ErrEnsureParamsMismatch` — Sentinel returned when a second caller
  asks to confirm an already-tracked txid with a different `TargetConfs`
  or `ConfirmationPkScript`.

## Relationships

- **Depends on**:
  - `baselib/actor` — actor framework for the orchestrator.
  - `baselib/protofsm` — per-txid state machine engine.
  - `chainsource` — confirmation watches, block epochs, broadcast,
    package submission, fee estimation, preflight.
  - `wallet` — `Utxo`, `OutputLeaser`, `LockID` types for fee-input
    selection and wallet-level lease coordination.
  - `lib/tx/arktx` — canonical `TxVersion` (v3/TRUC) constant and
    `IsAnchorOutput` predicate for CPFP targeting.
- **Depended on by**: (currently no internal callers — new package; future
  wiring will plug `TxBroadcasterActor` into unroll / refresh / oor flows
  that previously rolled their own broadcast loops).
- **Sends**:
  - → `chainsource` (Ask): `BestHeightRequest`, `SubscribeBlocksRequest`,
    `RegisterConfRequest`, `UnregisterConfRequest`, `BroadcastTxRequest`,
    `SubmitPackageRequest`, `TestMempoolAcceptRequest`,
    `FeeEstimateRequest`.
  - → `Wallet` (direct call): `ListUnspent`, `NewWalletPkScript`,
    `FinalizePsbt`, `LeaseOutput`, `ReleaseOutput`.
  - → Caller-supplied subscriber (Tell): `TxConfirmed`, `TxFailed`.
- **Receives**:
  - ← `chainsource` (via mapped Tell refs): `BlockEpoch` (re-wrapped as
    `blockEpochObservedMsg`), `ConfirmationEvent` (re-wrapped as
    `confirmationObservedMsg`).
  - ← API: `EnsureConfirmedReq`, `CancelInterestReq`.

## Invariants

- **Dedup check is strict**: two `EnsureConfirmedReq` for the same txid
  must agree on `TargetConfs` and `ConfirmationPkScript`; mismatches are
  rejected with `ErrEnsureParamsMismatch` rather than silently reusing the
  existing watch.
- **TRUC version gate**: `CPFPBroadcaster.Submit` rejects parents whose
  `Tx.Version != arktx.TxVersion` (v3). Pattern-based anchor detection
  on non-v3 parents is structurally unsafe.
- **Replacement floor**: every fee bump runs through
  `applyReplacementFloor` before selecting a fee input, enforcing
  BIP-125 Rule 4 (strictly higher feerate) and Rule 3 (strictly higher
  absolute fee by at least `IncrementalRelayFeeSatPerVByte *
  packageVSize`) against the last successful submission for the same
  parent txid.
- **Per-parent fee-input reservation**: each parent txid reserves the
  wallet UTXO(s) it has committed to. Reservations survive block
  boundaries and are released only when the parent is evicted
  (terminal state) or when the CPFP child never reaches the mempool
  (fallback / preflight reject / package error). A parent IS allowed
  to re-pick UTXOs from its own reserved set, because TRUC package
  RBF requires the new child to double-spend the previous child's fee
  input.
- **Wallet-level lease coordination**: every reserved fee UTXO is also
  leased via `Wallet.LeaseOutput` (caller-scoped `txconfirmLockID`)
  and released on eviction / fallback. Lease errors are soft — the
  in-memory reservation map is the source of truth — but the lease
  closes a narrow cross-subsystem race.
- **Child vsize is script-aware**: `estimateChildVSize` uses
  `input.TxWeightEstimator` with the actual fee-input and change
  pkScripts (P2TR, P2WKH, nested-P2WKH, …) to size the CPFP child,
  not a hard-coded constant. Unknown script classes fall back to
  P2WKH (which over-estimates for P2TR, safe for Rule 4).
- **Child fee input signals RBF** (`MaxTxInSequenceNum - 2 =
  0xfffffffd`) as belt-and-suspenders; the anchor input keeps the
  sentinel sequence value.
- **PSBT finalization matches by outpoint, not position**:
  `signCPFPChild` locates the wallet-owned input by
  `PreviousOutPoint`, so wallets that reorder inputs (e.g. BIP-69) or
  add fee-bump inputs do not panic or silently mis-wire witnesses.
- **Service-key symmetry**: `RegisterConfRequest` and
  `UnregisterConfRequest` both carry `PkScript` so chainsource's
  txid+script keyed service-actor lookup resolves symmetrically; one
  conf sub-actor per tracked tx.
- **Terminal eviction**: on Confirmed or Failed, the actor first delivers
  terminal notifications. If a subscriber is slow or transiently fails,
  the tracked entry is retained without a conf watch and retried on later
  actor ticks. Once every subscriber has been notified or cancelled, the
  actor stops the per-txid FSM goroutine, releases per-parent broadcaster
  state (fee-bump history + reservations + wallet leases), and deletes the
  tracked-tx entry. Late callers arriving after eviction re-register from
  scratch and receive an immediate `TxConfirmed` via the normal path if the
  tx is already on chain.

## Deep Docs

- [`doc.go`](doc.go) — Package-level literate-programming overview
  covering architecture, lifecycle, CPFP correctness invariants, PSBT
  finalization, service-key round trip, and eviction.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
