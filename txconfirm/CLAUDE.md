# txconfirm

## Purpose

Generic "broadcast this signed tx, tell me when it confirms, and fee-bump it
via CPFP until it does" actor. Subsystem-neutral: no unroll/, vtxo/, oor/, or
round/ semantics leak in. Callers submit a signed v3/TRUC parent via
`EnsureConfirmedReq` and receive a reorg-aware lifecycle of `TxConfirmed`,
optional `TxReorged` (if the confirming block is reorged out), back to
`TxConfirmed` on re-mining, and a terminal `TxFinalized` once the
confirmation matures past the backend's reorg-safety depth. Failures land
on a terminal `TxFailed`. Dedup is by txid: two callers asking to confirm
the same txid share a single confirmation watch, broadcast attempt, and
CPFP child, but each still receives its own lifecycle notifications.

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
  primitives. Not safe for concurrent use; the outer `TxBroadcasterActor`
  serializes access. Constructed via `NewCPFPBroadcaster(BroadcasterConfig)`.
- `BroadcasterConfig` — Configuration for `CPFPBroadcaster`: `ChainSource`,
  `Wallet`, optional `Log`, `MaxFeeRateSatPerVByte` (default 100), 
  `IncrementalRelayFeeSatPerVByte` (default 1, should match the node's
  `-incrementalrelayfee`), and `PreSubmitTestMempoolAccept` (opt-in
  `testmempoolaccept` preflight before every broadcast).
- `BroadcastRequest` / `BroadcastResult` — Input/output for
  `CPFPBroadcaster.Submit`. Request carries the fully signed parent `Tx`
  and a `Label` for logging.
- `BuildCPFPChild` — Exported helper that constructs the CPFP fee-paying
  child for a given v3 parent anchor outpoint, fee input, change script,
  and fee amount. Useful for callers that need the broadcast primitive
  without the full actor harness.
- `EstimatePackageFee`, `EstimateWeight`, `SelectFeeInput` — Exported
  helpers for fee estimation and fee-input selection, usable standalone.
- `Wallet` — Wallet interface the broadcaster requires: `ListUnspent`,
  `NewWalletPkScript`, `FinalizePsbt`, plus `wallet.OutputLeaser`
  (`LeaseOutput` / `ReleaseOutput`) for cross-subsystem UTXO lock
  coordination.
- `EnsureConfirmedReq` / `EnsureConfirmedResp` — Public Ask API: register
  interest in a txid with a `TargetConfs`, `ConfirmationPkScript`, and a
  subscriber that receives the lifecycle notifications.
- `CancelInterestReq` / `CancelInterestResp` — Public Ask API: drop a
  subscriber; the last subscriber's cancel also tears down tracking.
- `TxConfirmed` / `TxReorged` / `TxFinalized` / `TxFailed` — `Notification`
  types delivered to each subscriber. `TxConfirmed` and `TxReorged` are
  reversible (the entry can move between Confirmed and AwaitingConfirmation
  any number of times before finality); only `TxFinalized` and `TxFailed`
  are terminal. Reversible deliveries are fire-and-forget (per-subscriber
  goroutine, bounded by `reversibleNotifyTimeout`) so a slow durable
  subscriber cannot pin the actor loop; dropped reversible events are
  recoverable from the next lifecycle transition. Terminal deliveries
  retain the subscriber on failure and retry on later actor ticks.
- `TxState` (`New`, `Broadcasting`, `AwaitingConfirmation`, `FeeBumping`,
  `Confirmed`, `Finalized`, `Failed`) — Public view of the per-txid
  protofsm state. Only `Finalized` and `Failed` are terminal; `Confirmed`
  is reorg-reversible. `Broadcasting` covers BOTH the initial attempt and
  the "reached no mempool, retrying" case; `AwaitingConfirmation` is
  reported only once the tx (or a redundant parent) is actually in a
  mempool.
- Sentinels: `ErrNonTRUCParent`, `ErrCPFPFeeInputUnavailable`,
  `ErrEnsureParamsMismatch`, `ErrFeeInputProducesDust`.
- `Config.BroadcastFailureAlertThreshold` — consecutive no-mempool
  failures before the operator escalation fires (default 3). Time to
  first alert ≈ threshold × `FeeBumpIntervalBlocks` blocks.

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
- **Depended on by**: `unroll` (plugs `TxBroadcasterActor` and
  `CPFPBroadcaster` into boarding sweep / unilateral exit flows),
  `btcwbackend` (fee-input selection helper), `darepod` (wiring),
  `db` (schema references).
- **Sends**:
  - → `chainsource` (Ask): `BestHeightRequest`, `SubscribeBlocksRequest`,
    `RegisterConfRequest` (reorg-aware mode, with `NotifyReorged` and
    `NotifyDone` mapped refs), `UnregisterConfRequest`,
    `BroadcastTxRequest`, `SubmitPackageRequest`,
    `TestMempoolAcceptRequest`, `FeeEstimateRequest`.
  - → `Wallet` (direct call): `ListUnspent`, `NewWalletPkScript`,
    `FinalizePsbt`, `LeaseOutput`, `ReleaseOutput`.
  - → Caller-supplied subscriber (Tell): `TxConfirmed`, `TxReorged`,
    `TxFinalized`, `TxFailed`.
- **Receives**:
  - ← `chainsource` (via mapped Tell refs): `BlockEpoch` (re-wrapped as
    `blockEpochObservedMsg`), `ConfirmationEvent` (re-wrapped as
    `confirmationObservedMsg`), `ConfReorgedEvent` (re-wrapped as
    `confirmationReorgedMsg`), `ConfDoneEvent` (re-wrapped as
    `confirmationDoneMsg`).
  - ← API: `EnsureConfirmedReq`, `CancelInterestReq`.

## Invariants

- **Never give up on a no-mempool tx**: a tx whose broadcast reached no
  mempool stays in `Broadcasting` and is re-attempted every
  `FeeBumpIntervalBlocks`, never transitioning to terminal `Failed`. This
  covers `ErrCPFPFeeInputUnavailable` and transient package-relay
  rejections (min-relay-fee on the zero-fee anchor parent, mempool-full,
  fee input spent mid-submit) — the conditions CPFP retry exists to
  overcome. Only a structurally permanent error
  (`isPermanentBroadcastError`, currently `ErrNonTRUCParent`) fails
  terminally; `ErrParentAlreadyBroadcast` advances to
  `AwaitingConfirmation` (a live parent exists on another path). Rationale:
  a fraud-response checkpoint must land before the counterparty's
  CSV-timeout path, so the actor escalates to operators rather than
  silently aborting.
- **Strict dedup check**: two `EnsureConfirmedReq` for the same txid
  must agree on `TargetConfs` and `ConfirmationPkScript`; mismatches
  return `ErrEnsureParamsMismatch` rather than silently reusing the
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
- **Reversible notifications are fire-and-forget**: `notifyConfirmed`
  and `notifyReorged` dispatch each subscriber on its own goroutine
  with a bounded `reversibleNotifyTimeout`. The actor does NOT wait
  for delivery to complete and does NOT retry on failure, because the
  next lifecycle event (re-`TxConfirmed` after a `TxReorged`,
  `TxFinalized`, or eventual `TxFailed`) supersedes any dropped
  reversible delivery. The same fire-and-forget path is used when
  `attachExistingSubscriber` replays the cached confirmation to a
  late-arriving subscriber.
- **Terminal eviction**: on `Finalized` or `Failed`, the actor first
  delivers terminal notifications synchronously (via the goroutine +
  timeout + idempotent-retry pattern in `notifyOneTerminal`). If a
  subscriber is slow or transiently fails, the tracked entry is retained
  without a conf watch and retried on later actor ticks. Once every
  subscriber has been notified or cancelled, the actor stops the
  per-txid FSM goroutine, releases per-parent broadcaster state
  (fee-bump history + reservations + wallet leases), and deletes the
  tracked-tx entry. Late callers arriving after eviction re-register
  from scratch and receive an immediate `TxConfirmed` via the normal
  path if the tx is already on chain.
- **Backend Done in non-Confirmed states is dropped**: `confirmationDoneMsg`
  for an entry that is not in `TxStateConfirmed` (e.g. mid-reorg
  AwaitingConfirmation) is logged at warn and dropped rather than advancing
  to Finalized, because finalization from a non-confirmed state is
  semantically incorrect. The realistic backends (chainntnfs, lndclient)
  do not fire Done during reorg gaps; if this warn ever fires in
  production, the right follow-up is to re-register the conf watch from
  txconfirm rather than relax the guard.

## Deep Docs

- [`doc.go`](doc.go) — Package-level literate-programming overview
  covering architecture, lifecycle, CPFP correctness invariants, PSBT
  finalization, service-key round trip, and eviction.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
