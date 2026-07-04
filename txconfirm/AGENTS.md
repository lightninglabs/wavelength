# txconfirm

## Purpose

Generic "broadcast this signed tx, tell me when it confirms, and fee-bump
it via CPFP until it does" actor. Subsystem-neutral: no unroll/, vtxo/,
oor/, or round/ semantics leak in. Callers submit a signed parent via
`EnsureConfirmedReq` — either a zero-fee ephemeral-anchor v3/TRUC parent
that always rides a CPFP package, or a funded (non-zero-value) anchor
parent of any version that pays its own fee and is CPFP-bumped only when
needed — and receive a terminal `TxConfirmed` or `TxFailed` notification.
Dedup is by txid: two callers asking to confirm the same txid share a
single confirmation watch, broadcast attempt, and CPFP child, but each
still receives its own terminal notification.

## Key Types

For field-level detail, use `go doc github.com/lightninglabs/darepo-client/txconfirm.<Symbol>`.

- `TxBroadcasterActor` (`actor.go`) — message-driven orchestrator. Holds
  a txid-keyed tracked-tx map, runs a protofsm lifecycle per txid, and
  fans chainsource callbacks (confirmations, block epochs) into per-txid
  transitions.
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
  `NewWalletPkScript`, `FinalizePsbt`, `FundPsbt`, plus
  `wallet.OutputLeaser` (`LeaseOutput` / `ReleaseOutput`) for
  cross-subsystem UTXO lock coordination.
- `EnsureConfirmedReq` / `EnsureConfirmedResp` — public Ask API:
  register interest in a txid with `TargetConfs`, `ConfirmationPkScript`,
  a subscriber, and (for a funded-anchor parent) `ParentFee` so a later
  CPFP bump lands the combined fee on the target rate instead of
  double-counting the parent's own fee.
- `CancelInterestReq` / `CancelInterestResp` — drop a subscriber; the
  last subscriber's cancel tears down tracking.
- `BumpNowReq` / `BumpNowResp` — operator Ask API: force an immediate
  CPFP fee bump of an already-tracked txid at a supplied
  `TargetFeeRateSatPerVByte` (clamped to the broadcaster's configured
  ceiling), rather than waiting for the next interval-paced bump.
- `TxConfirmed` / `TxFailed` — terminal `Notification` types delivered
  to each subscriber.
- `TxState` — `New`, `Broadcasting`, `AwaitingConfirmation`,
  `FeeBumping`, `Confirmed`, `Failed`. `Broadcasting` covers BOTH the
  initial attempt and the "reached no mempool, retrying" case;
  `AwaitingConfirmation` is reported only once the tx (or a redundant
  parent) is actually in a mempool.
- Sentinels: `ErrNonTRUCParent`, `ErrCPFPFeeInputUnavailable`,
  `ErrEnsureParamsMismatch`, `ErrFeeInputProducesDust`,
  `ErrParentFeeSufficient` (a funded-anchor bump would pay the child a
  sub-relay-fee share because the parent already meets the target).
- `Config.BroadcastFailureAlertThreshold` — consecutive no-mempool
  failures before the operator escalation fires (default 3). Time to
  first alert ≈ threshold × `FeeBumpIntervalBlocks` blocks.

## Relationships

- **Depends on**: `baselib/actor`, `baselib/protofsm`, `chainsource`
  (confirmation watches, block epochs, broadcast, package submission,
  fee estimation, preflight), `wallet` (`Utxo`, `OutputLeaser`,
  `LockID`), `lib/tx/arktx` (`TxVersion` constant, `IsP2AAnchorScript`,
  `IsFundedAnchorOutput`).
- **Depended on by**: `unroll`, `wallet` (boarding-sweep and
  wallet-sweep actors), `darepod`.
- **Sends → `chainsource`** (Ask): `BestHeightRequest`,
  `SubscribeBlocksRequest`, `RegisterConfRequest`,
  `UnregisterConfRequest`, `BroadcastTxRequest`,
  `SubmitPackageRequest`, `TestMempoolAcceptRequest`,
  `FeeEstimateRequest`.
- **Sends → `Wallet`** (direct): `ListUnspent`, `NewWalletPkScript`,
  `FinalizePsbt`, `FundPsbt` (fee-input fanout funding),
  `LeaseOutput`, `ReleaseOutput`.
- **Sends → caller subscriber** (Tell): `TxConfirmed`, `TxFailed`.
- **Receives ← `chainsource`** (mapped Tell refs): `BlockEpoch` →
  `blockEpochObservedMsg`; `ConfirmationEvent` →
  `confirmationObservedMsg`.
- **Receives ← API**: `EnsureConfirmedReq`, `CancelInterestReq`,
  `BumpNowReq`.

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
- **TRUC version gate applies only to zero-fee anchors**: a parent
  carrying a zero-value ephemeral anchor must be v3
  (`Tx.Version == arktx.TxVersion`) — `CPFPBroadcaster.Submit` rejects
  it otherwise with `ErrNonTRUCParent`, since package RBF and the
  zero-fee anchor are not policy-legal without TRUC semantics. A
  funded (non-zero-value) anchor is exempt: it pays its own fee, so
  `Submit` broadcasts it directly on the initial pass regardless of
  version, and the anchor is spent only when a bump is requested.
- **Funded-anchor parents pay their own way until bumped**: on the
  initial broadcast a funded-anchor parent goes out directly with no
  CPFP child and no fee-input reservation; the anchor is a spare
  handle spent only by a later `BumpNowReq` or interval-paced bump. The
  resulting CPFP child's own fee is the package-fee target minus
  `ParentFee` (floored at 1 sat), and the anchor's value is credited
  into the child's change, so the combined parent+child fee lands on
  the target rate instead of double-counting the parent's own fee. A
  bump whose target the parent already meets or exceeds is refused
  with `ErrParentFeeSufficient` rather than building a child guaranteed
  to fail the mempool's minimum relay fee.
- **Fee inputs that fail to sign are deprioritized, not excluded**:
  `CPFPBroadcaster` tracks `suspectFeeInputs` — wallet outpoints whose
  CPFP child previously failed at the child-signing stage.
  `selectFeeInput` only falls back to a suspect coin when no clean
  candidate qualifies, so one unsignable UTXO (e.g. an imported
  watch-only output that leaked through UTXO enumeration) cannot keep
  winning smallest-first selection while signable coins sit idle; the
  mark clears the moment the input signs successfully.
- **A confirmed fee-input shortfall drives an active fanout, not just a
  retry**: when a CPFP child broadcast fails because no confirmed
  wallet UTXO covers the demand, the actor's single long-lived fanout
  state machine (`feeBumpFSM`, one instance per `TxBroadcasterActor`)
  funds and broadcasts a plain transaction that mints right-sized
  fee-input outputs for every blocked anchor parent, watches it for
  confirmation, and retries every parent still stuck in `Broadcasting`
  once it lands. The FSM performs its own wallet/broadcast IO inside
  transitions — safe because the actor serializes every event fed to
  it — and a build or broadcast failure stashes on the FSM's
  environment rather than tearing the long-lived machine down.
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

## Deep Docs

- [`doc.go`](doc.go) — Package-level overview covering architecture,
  lifecycle, CPFP correctness invariants, PSBT finalization,
  service-key round trip, and eviction.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
