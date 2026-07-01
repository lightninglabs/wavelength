# txconfirm

## Purpose

Generic "broadcast this signed tx, tell me when it confirms, and fee-bump
it via CPFP until it does" actor. Subsystem-neutral: no unroll/, vtxo/,
oor/, or round/ semantics leak in. Callers submit a signed v3/TRUC parent
via `EnsureConfirmedReq` and receive a terminal `TxConfirmed` or `TxFailed`
notification. Dedup is by txid: two callers asking to confirm the same
txid share a single confirmation watch, broadcast attempt, and CPFP child,
but each still receives its own terminal notification.

When a CPFP child cannot find a confirmed wallet fee input, the actor
drives an internal fee-input fanout state machine that mints right-sized
fee-input UTXOs via a wallet-funded fanout transaction, so a confirmed
fee-input shortfall resolves itself instead of stalling every anchor
parent indefinitely.

## Key Types

For field-level detail, use `go doc github.com/lightninglabs/darepo-client/txconfirm.<Symbol>`.

- `TxBroadcasterActor` (`actor.go`) — message-driven orchestrator. Holds
  a txid-keyed tracked-tx map, runs a protofsm lifecycle per txid, and
  fans chainsource callbacks (confirmations, block epochs) into per-txid
  transitions. Also owns the single long-lived `feeBumpFSM` (below) and
  drives it via `driveFeeBump`.
- `CPFPBroadcaster` (`broadcaster.go`) — actor-free helper for broadcast
  mechanics: direct submission for txs without anchors, CPFP child
  construction for anchor parents, fee estimation, script-aware child
  vsize estimation, fee-input selection / reservation, BIP-125 Rule 3/4
  floor enforcement, and optional `TestMempoolAccept` preflight. Usable
  standalone for callers needing only the broadcast primitives. **Not
  safe for concurrent use** — `TxBroadcasterActor` serializes access.
  Constructed via `NewCPFPBroadcaster(BroadcasterConfig)`. Also backs
  the fee-bump FSM: `parentState`/`parentStates` is the reservation map
  the FSM reads and writes directly (predicted vs. used fee inputs).
- `BroadcasterConfig` — `ChainSource`, `Wallet`, optional `Log`,
  `MaxFeeRateSatPerVByte` (default 100),
  `IncrementalRelayFeeSatPerVByte` (default 1; should match the node's
  `-incrementalrelayfee`), `PreSubmitTestMempoolAccept` (opt-in).
- `BroadcastRequest` / `BroadcastResult` — `CPFPBroadcaster.Submit`
  I/O. Request carries the signed parent `Tx` and a `Label`.
- Exported helpers usable standalone: `BuildCPFPChild`,
  `EstimatePackageFee`, `EstimateWeight`, `SelectFeeInput`.
- `Wallet` — interface required by the broadcaster: `ListUnspent`,
  `NewWalletPkScript`, `FinalizePsbt`, `FundPsbt` (funds/signs/finalizes
  a PSBT template under a caller-supplied lock namespace — backs the
  fee-input fanout build path), plus `wallet.OutputLeaser`
  (`LeaseOutput` / `ReleaseOutput`) for cross-subsystem UTXO lock
  coordination.
- `EnsureConfirmedReq` / `EnsureConfirmedResp` — public Ask API:
  register interest in a txid with `TargetConfs`, `ConfirmationPkScript`,
  and a subscriber.
- `CancelInterestReq` / `CancelInterestResp` — drop a subscriber; the
  last subscriber's cancel tears down tracking.
- `TxConfirmed` / `TxFailed` — terminal `Notification` types delivered
  to each subscriber.
- `TxState` — `New`, `Broadcasting`, `AwaitingConfirmation`,
  `FeeBumping`, `Confirmed`, `Failed`. `Broadcasting` covers BOTH the
  initial attempt and the "reached no mempool, retrying" case;
  `AwaitingConfirmation` is reported only once the tx (or a redundant
  parent) is actually in a mempool.
- Sentinels: `ErrNonTRUCParent`, `ErrCPFPFeeInputUnavailable`,
  `ErrEnsureParamsMismatch`, `ErrFeeInputProducesDust`, and the
  unexported `errCPFPFeeInputShortfall` (double-wrapped with
  `ErrCPFPFeeInputUnavailable` on the "no confirmed wallet UTXOs"
  path only — the one shortfall a fanout can actually resolve).
- `Config.BroadcastFailureAlertThreshold` — consecutive no-mempool
  failures before the operator escalation fires (default 3). Time to
  first alert ≈ threshold × `FeeBumpIntervalBlocks` blocks.

### Fee-input fanout subsystem (`fee_bump_fsm_types.go`,
`fee_bump_fsm_logic.go`, `fee_input_actor.go`)

A single long-lived `feeBumpStateMachine` (protofsm instance, one per
`TxBroadcasterActor`) tracks at most one in-flight fee-input fanout
transaction at a time. Unlike most protofsm instances in this codebase,
its transitions do their own blocking wallet/chainsource IO directly
(safe because the actor serializes every event fed to it — see
`docs/durable_actor_architecture.md` for the general pattern this
deviates from).

- `feeBumpStateIdle` / `feeBumpStateFanoutPending` — the two FSM states.
  Idle self-loops on stale confirm/evict events; fanout-pending carries
  the in-flight `pendingFeeInputFanout` (txid, funded tx, watch script,
  per-parent output assignments, last broadcast height).
- `feeBumpEnvironment` — FSM execution context: holds the
  `*CPFPBroadcaster` (for the shared reservation map and
  wallet/chainsource helpers) and stashes the last operational error in
  `lastErr` rather than returning it from `ProcessEvent` (a returned
  error would tear the long-lived FSM down; a transient fanout failure
  must not). The actor reads it back via `takeLastErr`.
- Events in (`feeBumpEvent`): `feeBumpDemandsObserved` (fresh demand
  set + fee rate + height, built from tracked anchor parents that hit a
  fee-input shortfall), `feeBumpFanoutConfirmedEvent`,
  `feeBumpParentEvicted`.
- Outbox events out (`feeBumpOutboxEvent`), applied by the actor's
  `driveFeeBump`: `feeBumpWatchFanout` / `feeBumpUnwatchFanout`
  (register/unregister a chainsource confirmation watch on the fanout's
  output script) and `feeBumpRetryParents` (re-attempt every tracked tx
  stuck in `Broadcasting` once the fanout confirms).
- `feeInputDemand` — one blocked parent's `minAmount` (package fee +
  dust limit); `pendingFeeInputFanout.assignments` maps parent txid →
  fanout output outpoints reserved for it as `PredictedFeeInputs`
  (`parentBumpState.PredictedFeeInputs`, distinct from
  `UsedFeeInputs`/`UsedFeeOutpoints` until the fanout confirms).
- `fee_input_actor.go` holds the actor-side glue:
  `maybeEnsureFeeInputSupply` (the single entry point from a failed
  broadcast into the fanout subsystem — filters for
  `errCPFPFeeInputShortfall`), `driveFeeBump`, `activeFeeInputDemands`,
  `registerFanoutConfWatch` / `unregisterFanoutConfWatch`,
  `handleFanoutConfirmed`, `retryBroadcastingParents`.

## Relationships

- **Depends on**: `baselib/actor`, `baselib/protofsm`, `chainsource`
  (confirmation watches, block epochs, broadcast, package submission,
  fee estimation, preflight), `wallet` (`Utxo`, `OutputLeaser`,
  `LockID`), `lib/tx/arktx` (`TxVersion` constant, `IsAnchorOutput`).
- **Depended on by**: `unroll`, `btcwbackend` (fee-input selection
  helper), `darepod`, `db`.
- **Sends → `chainsource`** (Ask): `BestHeightRequest`,
  `SubscribeBlocksRequest`, `RegisterConfRequest`,
  `UnregisterConfRequest`, `BroadcastTxRequest`,
  `SubmitPackageRequest`, `TestMempoolAcceptRequest`,
  `FeeEstimateRequest`. The fee-bump FSM also sends `BroadcastTxRequest`
  directly (fanout broadcast/rebroadcast) and the actor sends
  `RegisterConfRequest` / `UnregisterConfRequest` on the FSM's behalf
  for the fanout's own confirmation watch.
- **Sends → `Wallet`** (direct): `ListUnspent`, `NewWalletPkScript`,
  `FinalizePsbt`, `LeaseOutput`, `ReleaseOutput`. The fee-bump FSM also
  calls `FundPsbt` (fanout build path) and `ListUnspent`/`LeaseOutput`
  (fanout supply check and input locking).
- **Sends → caller subscriber** (Tell): `TxConfirmed`, `TxFailed`.
- **Receives ← `chainsource`** (mapped Tell refs): `BlockEpoch` →
  `blockEpochObservedMsg`; `ConfirmationEvent` →
  `confirmationObservedMsg` (also used for the fanout's own
  confirmation watch — routed to `handleFanoutConfirmed` before the
  normal tracked-tx path).
- **Receives ← API**: `EnsureConfirmedReq`, `CancelInterestReq`.
- **Internal actor ↔ fee-bump FSM** (in-process `AskEvent`, not a
  cross-package boundary but message-shaped): actor → FSM
  `feeBumpDemandsObserved`, `feeBumpFanoutConfirmedEvent`,
  `feeBumpParentEvicted`; FSM → actor (outbox) `feeBumpWatchFanout`,
  `feeBumpUnwatchFanout`, `feeBumpRetryParents`.

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
  boundaries and release only on eviction (terminal state) or when
  the CPFP child never reaches the mempool (fallback / preflight
  reject / package error). A parent IS allowed to re-pick UTXOs from
  its own reserved set — TRUC package RBF requires the new child to
  double-spend the previous child's fee input.
- **Wallet-level lease coordination**: every reserved fee UTXO is
  also leased via `Wallet.LeaseOutput` (caller-scoped
  `txconfirmLockID`) and released on eviction/fallback. Lease errors
  are soft — the in-memory reservation map is the source of truth —
  but the lease closes a narrow cross-subsystem race.
- **Child vsize is script-aware**: `estimateChildVSize` uses
  `input.TxWeightEstimator` with the actual fee-input and change
  pkScripts (P2TR, P2WKH, nested-P2WKH, …). Unknown script classes
  fall back to P2WKH (which over-estimates for P2TR, safe for Rule 4).
- **Child fee input signals RBF** (`MaxTxInSequenceNum - 2 =
  0xfffffffd`) belt-and-suspenders; the anchor input keeps the
  sentinel sequence value.
- **PSBT finalization matches by outpoint, not position**:
  `signCPFPChild` locates the wallet-owned input by
  `PreviousOutPoint`, so wallets that reorder inputs (e.g. BIP-69) or
  add fee-bump inputs do not panic or silently mis-wire witnesses.
- **Service-key symmetry**: `RegisterConfRequest` and
  `UnregisterConfRequest` both carry `PkScript` so chainsource's
  txid+script keyed service-actor lookup resolves symmetrically.
- **At most one fanout in flight**: the fee-bump FSM is a single
  long-lived instance per actor; a fresh demand set never starts a
  second fanout while one is pending — it either rebuilds from scratch
  (idle) or rebroadcasts/extends the existing one (fanout-pending).
- **Fanout failures never tear down the FSM**: a transition that hits
  an operational error (wallet fund failure, rejected broadcast,
  wallet-rewritten output) stashes it on `feeBumpEnvironment.lastErr`
  and self-loops to a safe state instead of returning the error —
  returning would tear the whole protofsm instance down. The actor
  surfaces the stashed error via `takeLastErr` purely for logging; the
  next demand observation simply tries again.
- **Predicted vs. used fee inputs are distinct until confirmation**:
  a fanout's outputs are reserved as `PredictedFeeInputs` (unconfirmed,
  excluded from other parents' selection and from fanout double-spend)
  the moment the fanout is broadcast, but only promoted to
  `UsedFeeInputs`/`UsedFeeOutpoints` (spendable by a CPFP child) once
  the fanout itself confirms (`promoteConfirmedFanout`).
- **Reserved-input fallback can replace a parent's own fee input**:
  when the wallet has no free confirmed supply, the fanout may re-spend
  fee inputs already reserved (and confirmed) by the blocked parents
  themselves, immediately replacing them with predicted outputs from the
  fanout. This mirrors the CPFP child's own "re-pick from own reserved
  set" allowance and keeps the ownership boundary inside txconfirm.
- **Fanout output integrity is verified, not trusted**: after
  `FundPsbt`/`FinalizePsbt`, `verifyFanoutOutputs` checks that the
  wallet did not change the value or pkScript of any output the fanout
  build path asked for; a mismatch fails the build rather than
  reserving predicted inputs against outputs that do not exist as
  intended.
- **Parent eviction prunes fanout assignments, not vice versa**: when a
  tracked parent reaches a terminal state or is cancelled, the actor
  evicts it from both the broadcaster's `parentStates` and the fee-bump
  FSM (`feeBumpParentEvicted`); when a pending fanout's last assigned
  parent is pruned this way, the fanout itself is released (predicted
  outputs freed, wallet leases released, watch torn down) rather than
  left orphaned.
- **Terminal eviction**: on Confirmed or Failed, the actor delivers
  terminal notifications first. If a subscriber is slow or transiently
  fails, the tracked entry is retained without a conf watch and
  retried on later actor ticks. Once every subscriber is notified or
  cancelled, the actor stops the per-txid FSM goroutine, releases
  per-parent broadcaster state (fee-bump history + reservations +
  wallet leases), and deletes the tracked-tx entry. Late callers
  after eviction re-register from scratch and receive an immediate
  `TxConfirmed` via the normal path if the tx is already on chain.

## Deep Docs

- [`doc.go`](doc.go) — Package-level overview covering architecture,
  lifecycle, CPFP correctness invariants, PSBT finalization,
  service-key round trip, and eviction.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
