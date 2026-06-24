# txconfirm

## Purpose

Generic "broadcast this signed tx, tell me when it confirms, and fee-bump
it via CPFP until it does" actor. Subsystem-neutral: no unroll/, vtxo/,
oor/, or round/ semantics leak in. Callers submit a signed v3/TRUC parent
via `EnsureConfirmedReq` and receive a terminal `TxConfirmed` or `TxFailed`
notification. Dedup is by txid: two callers asking to confirm the same
txid share a single confirmation watch, broadcast attempt, and CPFP child,
but each still receives its own terminal notification.

## Key Types

For field-level detail, use `go doc github.com/lightninglabs/darepo-client/txconfirm.<Symbol>`.

- `TxBroadcasterActor` (`actor.go`) — message-driven orchestrator. Holds
  a txid-keyed tracked-tx map, runs a protofsm lifecycle per txid, and
  fans chainsource callbacks (confirmations, block epochs) into per-txid
  transitions. Also owns the single `feeBumpStateMachine` instance and
  drives it via `driveFeeBump`.
- `CPFPBroadcaster` (`broadcaster.go`) — actor-free helper for broadcast
  mechanics: direct submission for txs without anchors, CPFP child
  construction for anchor parents, fee estimation, script-aware child
  vsize estimation, fee-input selection / reservation, BIP-125 Rule 3/4
  floor enforcement, and optional `TestMempoolAccept` preflight. Usable
  standalone for callers needing only the broadcast primitives. **Not
  safe for concurrent use** — `TxBroadcasterActor` serializes access.
  Constructed via `NewCPFPBroadcaster(BroadcasterConfig)`.
- `BroadcasterConfig` — `ChainSource`, `Wallet`, optional `Log`,
  `MaxFeeRateSatPerVByte` (default 100),
  `IncrementalRelayFeeSatPerVByte` (default 1; should match the node's
  `-incrementalrelayfee`), `PreSubmitTestMempoolAccept` (opt-in).
- `BroadcastRequest` / `BroadcastResult` — `CPFPBroadcaster.Submit`
  I/O. Request carries the signed parent `Tx` and a `Label`.
- Exported helpers usable standalone: `BuildCPFPChild`,
  `EstimatePackageFee`, `EstimateWeight`, `SelectFeeInput`.
- `Wallet` — interface required by the broadcaster: `ListUnspent`,
  `NewWalletPkScript`, `FinalizePsbt`, plus `wallet.OutputLeaser`
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
  `ErrEnsureParamsMismatch`, `ErrFeeInputProducesDust`.
- `Config.BroadcastFailureAlertThreshold` — consecutive no-mempool
  failures before the operator escalation fires (default 3). Time to
  first alert ≈ threshold × `FeeBumpIntervalBlocks` blocks.
- `FanoutController` FSM (`fee_bump_fsm_types.go`,
  `fee_bump_fsm_logic.go`) — `feeBumpStateMachine` wraps a
  `protofsm.StateMachine` with two states:
  - `feeBumpStateIdle` — resting state; on `feeBumpDemandsObserved`
    checks the wallet for free supply, and if blocked demands remain
    builds and broadcasts a fanout transaction via `buildFeeInputFanoutTx`
    (free supply) or `buildReservedInputFanoutTx` (re-spends already-
    reserved confirmed inputs). Transitions to `feeBumpStateFanoutPending`
    and emits `feeBumpWatchFanout` for the actor to arm a conf watch.
  - `feeBumpStateFanoutPending` — fanout is on-wire awaiting
    confirmation. Rebroadcasts the same tx on `feeBumpDemandsObserved`
    once `FeeBumpIntervalBlocks` have elapsed; a hard reject clears and
    rebuilds via internal event. `feeBumpFanoutConfirmedEvent` promotes
    predicted outputs to used fee inputs and emits `feeBumpUnwatchFanout`
    + `feeBumpRetryParents`. `feeBumpParentEvicted` drops that parent's
    assignments; when all assignments are gone the fanout is released.
  - Transitions do their own wallet/broadcast IO via `feeBumpEnvironment`
    (safe because the actor serializes all FSM events). Operational
    failures are stashed on `feeBumpEnvironment.lastErr` rather than
    returned from `ProcessEvent`, keeping the long-lived FSM alive across
    transient fanout failures.
- `feeBumpEnvironment` — execution context for fanout transitions:
  holds a `*CPFPBroadcaster` reference (for shared `parentStates` and
  wallet helpers) and `lastErr` (stashed per-turn failure).
- `feeInputDemand` / `pendingFeeInputFanout` — demand record (parent
  txid + min UTXO amount) and in-flight fanout state (txid, funded tx,
  watch script, per-parent output assignments, last broadcast height).
- `feeBumpWatchFanout` / `feeBumpUnwatchFanout` / `feeBumpRetryParents`
  — outbox events the FSM emits for the actor to apply: register or tear
  down a chainsource conf watch, and retry all stuck `Broadcasting`
  parents once supply arrives.

## Relationships

- **Depends on**: `baselib/actor`, `baselib/protofsm`, `chainsource`
  (confirmation watches, block epochs, broadcast, package submission,
  fee estimation, preflight), `wallet` (`Utxo`, `OutputLeaser`,
  `LockID`), `lib/tx/arktx` (`TxVersion` constant, `IsAnchorOutput`).
- **Depended on by**: `unroll`, `btcwbackend` (fee-input selection
  helper), `darepod`, `db`, `wallet` (general backing-wallet sweep
  submits through the shared txconfirm broadcaster path).
- **Sends → `chainsource`** (Ask): `BestHeightRequest`,
  `SubscribeBlocksRequest`, `RegisterConfRequest`,
  `UnregisterConfRequest`, `BroadcastTxRequest`,
  `SubmitPackageRequest`, `TestMempoolAcceptRequest`,
  `FeeEstimateRequest`.
- **Sends → `Wallet`** (direct): `ListUnspent`, `NewWalletPkScript`,
  `FinalizePsbt`, `FundPsbt`, `LeaseOutput`, `ReleaseOutput`.
- **Sends → caller subscriber** (Tell): `TxConfirmed`, `TxFailed`.
- **Receives ← `chainsource`** (mapped Tell refs): `BlockEpoch` →
  `blockEpochObservedMsg`; `ConfirmationEvent` →
  `confirmationObservedMsg` (also used for fanout confirmation, matched
  against the in-flight fanout txid by `handleFanoutConfirmed`).
- **Receives ← API**: `EnsureConfirmedReq`, `CancelInterestReq`.

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
- **Terminal eviction**: on Confirmed or Failed, the actor delivers
  terminal notifications first. If a subscriber is slow or transiently
  fails, the tracked entry is retained without a conf watch and
  retried on later actor ticks. Once every subscriber is notified or
  cancelled, the actor stops the per-txid FSM goroutine, releases
  per-parent broadcaster state (fee-bump history + reservations +
  wallet leases), and deletes the tracked-tx entry. Late callers
  after eviction re-register from scratch and receive an immediate
  `TxConfirmed` via the normal path if the tx is already on chain.
  Terminal eviction also fires a `feeBumpParentEvicted` event into
  the fanout FSM so the evicted parent's output assignments are dropped;
  when no parents remain, the in-flight fanout is released and the FSM
  returns to idle.
- **Single long-lived fanout FSM**: `TxBroadcasterActor` holds one
  `feeBumpStateMachine` instance for its entire lifetime. The FSM is
  lazily started on the first `driveFeeBump` call (via
  `ensureFeeBumpStarted`) and cycles between `feeBumpStateIdle` and
  `feeBumpStateFanoutPending` for subsequent demand/confirmation events.
  At most one fanout tx is ever in flight; new demand observations while
  a fanout is pending rebroadcast the existing tx rather than building a
  second one.
- **Fanout FSM survives operational failures**: transition errors (a
  rejected broadcast, a wallet fund failure, a rewritten fanout output)
  are stashed on `feeBumpEnvironment.lastErr` rather than returned from
  `ProcessEvent`. `protofsm` tears the FSM down on a `ProcessEvent`
  error, which would destroy the long-lived single instance; stashing
  and self-looping keeps the machine alive for the next demand
  observation. The actor reads the stashed error back via
  `env.takeLastErr` after each `AskEvent` returns and surfaces it as a
  warning log.
- **No-free-supply fallback**: when the wallet has no unencumbered
  confirmed UTXOs but the blocked parents already hold confirmed reserved
  fee inputs, the idle state falls back to `buildReservedInputFanoutTx`,
  which re-spends those inputs to mint new right-sized fanout outputs.
  This is the TRUC package RBF path: the new child double-spends the
  previous child's fee input and clears relay policy via the replacement
  fee floor.
- **Fanout output assignment is value-matched**: `fanoutWatchScript` and
  `reservePredictedFanoutOutputs` locate each demand's output by
  comparing `txOut.Value == demand.minAmount`. The wallet signer must not
  reorder or alter the leading fanout outputs; `verifyFanoutOutputs`
  asserts this before the FSM transitions to fanout-pending.

## Deep Docs

- [`doc.go`](doc.go) — Package-level overview covering architecture,
  lifecycle, CPFP correctness invariants, PSBT finalization,
  service-key round trip, and eviction.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
</content>
</invoke>
