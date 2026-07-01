# unroll

## Purpose

Durable per-target unilateral-exit subsystem. One `VTXOUnrollActor` per VTXO
outpoint owns the full exit lifecycle (proof assembly → proof-node
confirmation → CSV maturity → final sweep build → broadcast → confirmation)
on top of a pure `unrollplan.Planner` and the shared `txconfirm` actor. A
thin `UnrollRegistryActor` owns spawn / dedup / terminal bookkeeping and
persists a control-plane record per target to `db` so restart can restore
in-flight jobs.

## Key Types

For field-level detail, use `go doc github.com/lightninglabs/darepo-client/unroll.<Symbol>`.

### Per-target actor

- `VTXOUnrollActor` — one durable actor per target outpoint, wrapping
  `baselib/actor.DurableActor[Msg, Resp]`. Owns the FSM session, proof,
  planner, and cached sweep transaction for this VTXO. Runs on the durable
  Read/Commit (`TxBehavior`) path: each checkpoint write is a short,
  lock-releasing Stage ahead of the `txconfirm` IO, and the message is
  consumed in a single lease-fenced Commit so the SQLite writer is never held
  across a cross-actor Ask.
- `Config` — per-actor wiring. Notable: `TargetOutpoint`, `ActorID`,
  `DeliveryStore`, `ProofAssembler`, `VTXOStore`, `TxConfirmRef`,
  `ChainSource`, `Wallet` (`SweepWallet`),
  `MaxSweepFeeRateSatPerVByte`, `FraudCheckpointSafetyMargin int32`
  (overrides the fraud-triggered unroll backstop margin in blocks;
  zero falls back to the default), `RegistryRef`, `LedgerSink`
  (`fn.Option[ledger.Sink]`; mirrored down from `RegistryConfig.LedgerSink`
  by `registryBehavior.childConfig` so every spawned child can emit its own
  exit-cost event).
- `behavior` — actor behavior implementing `actor.TxBehavior[Msg, Resp,
  unrollTx]`. Holds `b.sweepTx` (restored from checkpoint on boot) so
  retries and replays converge on a single sweep txid / pkScript under
  `txconfirm`'s txid-keyed dedup. The `dispatch` method runs the full
  FSM pipeline including Stage writes; `Receive` owns the single
  lease-fenced Commit.
- `Msg` / `Resp` / `Event` / `OutboxEvent` — sealed durable-mailbox,
  response, FSM event, and FSM outbox interfaces.
- Mailbox messages: `StartUnrollRequest`, `ResumeUnrollRequest`,
  `HeightObservedMsg`, `TxConfirmedMsg`, `TxFailedMsg`,
  `SpendObservedMsg`, `GetStateRequest`. Each ships a per-message TLV
  codec (no JSON) with a pinned record-type layout; round-trips in
  `messages_test.go`.
- `StartTrigger` — `TriggerManual`, `TriggerCriticalExpiry`,
  `TriggerRestart`, `TriggerFraudSpend`.
- `Phase` — control-plane phase: `PhasePending`,
  `PhaseMaterializing`, `PhaseCSVPending`, `PhaseSweepBroadcast`,
  `PhaseSweepConfirmation`, `PhaseCompleted`, `PhaseFailed`.
- `JobState` — durable FSM state (height, trigger, planner state,
  `FailReason`, `SweepAttempts`).

### Registry

- `UnrollRegistryActor` — coordinator over the set of
  `VTXOUnrollActor`s. Handles `EnsureUnrollRequest` /
  `GetStatusRequest` admission, receives `UnrollTerminatedMsg` from
  children, persists records, and runs `RestoreNonTerminal` on boot.
- `RegistryConfig` — `Store`, `DeliveryStore`, `ProofAssembler`,
  `VTXOStore`, `TxConfirmRef`, `ChainSource`, `Wallet`,
  optional `LedgerSink`, `MaxSweepFeeRateSatPerVByte`,
  `ExitSpendPolicyResolver` (optional; reconstructs the exit spend policy
  from `(ExitPolicyKind, ExitPolicyRef)` after restart; nil means every child
  uses the standard VTXO timeout), and optional `VTXOExitObserver`
  (`fn.Option[actor.TellOnlyRef[vtxo.ManagerMsg]]`). When set, each child's
  terminal outcome is forwarded to the VTXO manager as a
  `vtxo.ExitOutcomeNotification` so VTXO lifecycle tracks the unroll's
  terminal on-chain result rather than the user's intent to exit
  (darepo-client#602): a clean failure (`!HadOnChainFootprint`) →
  `ExitOutcomeRecoverable` (roll back to live), a completed exit →
  `ExitOutcomeConfirmed` (retire to spent). `UnrollTerminatedMsg` carries
  `HadOnChainFootprint`, computed by `jobHadOnChainFootprint` (any
  confirmed/in-flight proof node or a non-pending sweep).
- `RegistryRecord` — control-plane row (`TargetOutpoint`, `ActorID`,
  `Phase`, `Trigger`, `FailReason`, `SweepTxid`, `ExitPolicyKind`,
  `ExitPolicyRef`).
- `RegistryStore` — `UpsertRecord`, `GetRecord`,
  `ListNonTerminalRecords`, `MarkTerminal`. `DBRegistryStore`
  (`db_store.go`) is production. Adapts to
  `db.UnilateralExitStore` via `statusForPhase`/`phaseFromDB` +
  `triggerToDB`/`triggerFromDB` (round-tripped in
  `db_store_test.go`).
- `EnsureUnrollRequest` / `EnsureUnrollResp` — admission API. Must
  include `ExitPolicyKind` and `ExitPolicyRef` for non-standard exits;
  `validateExitPolicyIdentity` checks consistency at admit time. Dedup
  runs against `r.active`, `r.pending`, AND `Store.GetRecord` so a
  repeat after termination returns `Created=false` with the historical
  `ActorID`, never clobbering the sweep txid / failure reason. The one
  exception is a **recoverable** terminal failure
  (`RecoverableFailure`): the prior exit failed cleanly with no on-chain
  footprint and the VTXO was rolled back to live (darepo-client#602), so
  a fresh `EnsureUnrollRequest` re-admits (spawns a new child,
  overwriting the stale record) instead of deduping — otherwise a
  recovered VTXO could never be unrolled again. Any existing unroll job
  for the same target must carry the same `(ExitPolicyKind,
  ExitPolicyRef)`; mismatches fail closed.
- `ExitSpendPolicyResolver` — interface for looking up the final spend
  policy by `(ExitPolicyKind, ExitPolicyRef)`. Implemented by
  `vhtlcrecovery/unrollpolicy.ExitSpendPolicyResolver`.

### Support

- `LocalProofAssembler` — assembles a `recovery.Proof` from the VTXO
  descriptor and its OOR artifact lineage; implements
  `ProofAssembler`. Also exposes `EnsureProofForHarness`, a
  terminal-tolerant sibling of `EnsureProof` that skips ONLY the
  descriptor's terminal-status arm so test harnesses (currently only
  `darepod.Server.GetVTXOLineageTx`) can walk the lineage of a
  Spent/Forfeited/Failed VTXO. Production code MUST keep using
  `EnsureProof`; the harness surface is gated by an explicit method
  name (not a flag) so a refactor cannot silently disable the guard.
- `DescriptorLineageResolver` — walks OOR checkpoint artifacts to
  produce the lineage transactions that must confirm before sweep.
  Implements `ResolveLineage` (shape + active-status validation) and
  `ResolveLineageHistorical` (shape only) through a shared
  `resolveValidatedLineage`; the only divergence is whether
  `validateProofDescriptorActive` runs. Iterates `desc.Ancestry` and
  appends every fragment's `TreePath` to `mat.TreePaths` so
  multi-commitment OOR VTXOs surface all required tree paths to the
  planner. `resolveOORArtifacts` cross-checks unresolved checkpoint
  inputs against a `treeTxids` index built from all tree paths
  before declaring fatal gaps — a checkpoint whose earliest parent is
  a tree node is correctly accepted.
- `SweepWallet` — `NewWalletPkScript`, `SignTaprootSpend`.
- `safeTxOutPkScript(tx, index)` — bounds-checking helper used at
  every `tx.TxOut[i].PkScript` site; surfaces retryable errors for
  malformed proof artifacts instead of panicking the actor.
- `ensureProofSpendWatches(ctx, txid, node)` — registers spend
  watches on proof-node outputs consumed by in-proof children.
  Neutrino can miss direct confirmation under load; a spend of the
  parent output proves parent confirmation. `proofSpendWatches` map
  dedups.
- `watchDeferredCheckpoint(ctx, txid, node)` — registers confirmation
  watch for fraud-triggered checkpoints while the actor waits for
  operator confirmation of the proof node.
- `proofNodeHeightHint = 1` — height hint for proof-node
  spend/confirmation watches (proof roots and intermediate ancestors
  can confirm before the target VTXO's creation height).

### Exit funding planning (`exit_plan.go`)

Pre-admission API, called by `darepod` RPC handlers so a caller can check
whether a VTXO exit is fundable and how much backing-wallet balance to stage
BEFORE calling `EnsureUnrollRequest`. Pure and read-only: it never touches
the registry or spawns an actor.

- `PlanExitFunding(desc *vtxo.Descriptor, feeRate btcutil.Amount, wallet
  ExitFundingSnapshot) ExitFundingPlan` — wraps `RecoveryTxCount` +
  `AssessExitFeasibility` (`feasibility.go`) and layers a user-facing funding
  projection on top of the feasibility verdict.
- `ExitFundingSnapshot` — caller-supplied wallet state:
  `WalletConfirmedSat`, `WalletUsableInputs`.
- `ExitFundingPlan` — result: `Feasibility` (the underlying
  `ExitFeasibility` verdict), `RequiredConfirmations`,
  `RecommendedUTXOAmountSat`, `RecommendedTotalFundingSat`,
  `FundingShortfallSat`.
- `RecommendedExitFeeInputAmount(verdict ExitFeasibility) btcutil.Amount` —
  derives the suggested per-UTXO funding amount from
  `verdict.CPFPFeeTotalSat` / `verdict.RequiredWalletInputs`, floored at
  `DefaultFeeInputMinAmountSat` (10,000 sat) and padded by
  `txconfirm.DustLimit`.
- `RequiredFeeInputConfirmations = 1` — confirmations a backing-wallet UTXO
  needs before it can fund unroll CPFP.
- `ExitFundingAddressBook` — caches one funding address per target key
  (`Address(ctx, key, newAddress)`) so polling a plan repeatedly does not
  advance the backing wallet's address index. Owned by `darepod.Server`
  (`exitFundingAddresses` field), not by the registry or any actor.

## Relationships

- **Depends on**: `baselib/actor` (`DurableActor`, `TLVMessage`,
  codec), `baselib/protofsm`, `lib/recovery`, `unrollplan` (pure
  planner + TLV state codec), `txconfirm` (broadcast + CPFP +
  confirmation), `chainsource` (best-height, spend watch, fee
  estimate), `vtxo` (`Descriptor`, `VTXOStore`), `db`
  (`UnilateralExitStore`, `RegistryRecord` shape), `lib/arkscript`.
- **Depended on by**: `darepod` (wires the registry via the lazy
  chain-resolver seam, PR #264; also calls `PlanExitFunding` and owns an
  `ExitFundingAddressBook` directly for exit-funding RPC endpoints, not just
  registry admission), `vhtlcrecovery/coordinator` (admission via
  `EnsureUnrollRequest`), `vhtlcrecovery/unrollpolicy` (implements
  `ExitSpendPolicyResolver` and `ExitSpendPolicy`).
- **Sends**:
  - → `txconfirm` (Ask): `EnsureConfirmedReq` per proof node and for
    the final sweep; txid dedup makes retries idempotent.
  - → `chainsource` (Ask): `RegisterSpendRequest` on the target
    outpoint to catch external spends, `BestHeightRequest`,
    `FeeEstimateRequest`.
  - → registry (Tell): `UnrollTerminatedMsg` from each child on
    terminal transition.
  - → `ledger` (Tell, via per-child `Config.LedgerSink`, mirrored down from
    `RegistryConfig.LedgerSink`): `ledger.ExitCostMsg` once the final sweep
    confirms. The amount is the proof target output value and the fee is
    derived from the persisted sweep tx outputs. A failed Tell defers the
    terminal registry handoff (see Invariants) instead of dropping the
    event.
  - → `vtxo` (indirect via chain-resolver seam, #264).
  - → `vtxo` manager (Tell, via `RegistryConfig.VTXOExitObserver`):
    `ExitOutcomeNotification` on each child's terminal outcome — the
    reverse feedback edge for darepo-client#602.
- **Receives**:
  - ← API (registry): `EnsureUnrollRequest`, `GetStatusRequest`
    (from `darepod` RPC via chain resolver).
  - ← registry (internal): `persistActiveRecordMsg`,
    `persistRecordResultMsg`, `UnrollTerminatedMsg`.
  - ← per-target mailbox: all messages listed under Per-target actor.
  - ← `txconfirm` subscriber: `TxConfirmed` / `TxFailed` mapped to
    `TxConfirmedMsg` / `TxFailedMsg`.
  - ← `chainsource` block epochs / spend notifications mapped to
    `HeightObservedMsg` / `SpendObservedMsg`.

## Multi-Tree Ancestry

- `LineageMaterial.TreePaths` is plural; the resolver iterates
  `desc.Ancestry` and appends every fragment's `TreePath` so
  cross-commitment multi-input OOR VTXOs contribute all required
  commitment trees to the planner.
- `validateProofDescriptorShape` checks `len(desc.Ancestry) == 0`
  (was `desc.TreePath == nil`) and validates each fragment
  individually (non-nil `TreePath`, non-nil root, non-zero
  `CommitmentTxID`, non-zero `TreeDepth`).
- `BuildProofFromMaterial` calls `addTreePathNodes` once per tree
  path, tolerating overlapping ancestry (duplicate txids silently
  deduped; conflicting duplicates — same txid, different bytes — are
  rejected).

## Invariants

- **Persist-before-broadcast.** `startSweep` calls
  `persistCheckpoint` (writing `b.sweepTx` into the TLV checkpoint)
  BEFORE `txconfirm.Ask`. Any handler retry or restart restores the
  same sweep tx, and `txconfirm`'s txid-keyed dedup makes the
  resubmit a benign no-op — never a second sweep with a freshly
  derived wallet pkScript racing the first on-chain.
- **Sweep tx reuse.** `startSweep` skips `buildSweepTx` when
  `b.sweepTx` is already set; every retry converges on the same
  sweep txid/pkScript and avoids burning BIP32 addresses on fee-spike
  retries.
- **Reissue fails hard on missing state.** The
  `ReissueInFlightTransactions` and `ReissueSweepConfirmation`
  outbox branches return errors on a missing proof node or nil
  `sweepTx`. A silent `continue` would strand the FSM with no
  pending `txconfirm` subscription.
- **Exit-cost emission is delivered before the terminal handoff, not
  raced with it.** `notifyRegistryIfTerminal` calls
  `emitExitCostIfCompleted` first; only once that returns `true` does it
  proceed to notify `RegistryRef` and set `terminalNotified`. A failed
  `LedgerSink.Tell` (e.g. mailbox backpressure) makes
  `emitExitCostIfCompleted` return `false`, which defers the whole
  terminal handoff — the child stays alive and subscribed so the next
  `HeightObservedMsg` retries the emission. Without this ordering the
  registry could stop a completed child (terminal records are not
  restored on boot) before the exit-cost event was durably accepted,
  losing it permanently. A deterministically un-buildable exit cost
  (missing proof/sweepTx, non-positive value) is logged at error level via
  `exitCostNotified` and let through so the handoff is never wedged
  forever on an internal inconsistency.
- **Registry dedup covers the whole trail.** `handleEnsure` checks
  `r.active`, `r.pending`, AND `Store.GetRecord` before spawning so
  a repeat for an already-terminal outpoint returns the historical
  `ActorID` and never overwrites stored sweep txid / failure reason.
  A **recoverable** terminal failure is the deliberate exception: the
  VTXO was rolled back to live (darepo-client#602), so `handleEnsure`
  falls through both the `r.pending` and `Store.GetRecord` arms to
  re-admit a fresh exit rather than strand the recovered coin.
- **Fail-closed on restore gaps.** `handleEnsure` validates restorable
  non-terminal records via `validateRestorableRecords` before re-admitting
  them; a record with an unrecognized `ExitPolicyKind` or missing ref fails
  closed rather than spawning an actor that cannot build the final spend.
  Late admission failures observed after the synchronous timeout window are
  delivered as `childAdmissionResultMsg` so they remain serialized with
  registry mutations.
- **Fail-closed admission write.** `handleEnsure` calls
  `Store.UpsertRecord` synchronously and only returns `Created=true`
  after the record is durable. On write failure the spawned child is
  stopped, removed from `r.active`, and the caller sees a wrapped
  error — no silent orphans. Subsequent updates stay on the async
  `requestPersist` path so the registry goroutine isn't held hostage
  by every transition.
- **Durable mailbox messages are TLV, not JSON.** Every message in
  `messages.go` implements `actor.TLVMessage` with a hand-written
  `Encode`/`Decode` driven by `tlv.Stream`. Inner record types start
  at 1 per message (the outer mailbox codec identifies which message).
  The checkpoint codec in `snapshot.go` is also TLV.
- **Checkpoint persists the sweep tx** via `wire.MsgTx.Serialize`
  under `checkpointSweepTxRecordType` so restore produces the exact
  same `b.sweepTx` as the pre-broadcast commit.
- **Phase ↔ DB status mapping is lossless.** `PhaseSweepBroadcast`
  maps to `UnilateralExitJobStatusSweepBroadcasting` (=6) and
  `PhaseSweepConfirmation` to `UnilateralExitJobStatusSweeping` (=3)
  — they used to collide and erase the "built but not yet broadcast"
  vs "broadcast awaiting conf" distinction. `TriggerFraudSpend`
  round-trips through a dedicated
  `UnilateralExitJobTriggerFraudSpend` constant instead of silently
  downgrading to `TriggerManual`.
- **Sweep tx is v3 (TRUC).** `buildSweepTx` creates the sweep with
  `wire.NewMsgTx(arktx.TxVersion)` (= 3). The shared `txconfirm` CPFP
  broadcaster gates parent submission on v3/BIP-431 semantics;
  CSV-relative timelocks work for any `version >= 2` but the
  anchor-detection heuristic requires v3.
- **FSM outbox events are side-effect-only.** `RequestSweepBuild`,
  `EnsureReadyTransactions`, `ReissueInFlightTransactions`, and
  `ReissueSweepConfirmation` never mutate `JobState`; they are
  routed by `behavior.routeOutbox` to `txconfirm.Ask` calls outside
  the FSM.
- **All TxOut indexing goes through `safeTxOutPkScript`** —
  operator-sourced OOR artifacts flow into proof assembly, so a
  zero- or short-output node maps to a retryable error rather than
  a goroutine panic.

## Deep Docs

- [docs/durable_actor_quickstart.md](../docs/durable_actor_quickstart.md)
  — `TLVMessage`, `ActorBehavior`, migration checklist.
- [docs/durable_actor_architecture.md](../docs/durable_actor_architecture.md)
  — CDC pattern and durable mailbox lifecycle.
- [unrollplan/CLAUDE.md](../unrollplan/CLAUDE.md) — pure planner.
- [txconfirm/CLAUDE.md](../txconfirm/CLAUDE.md) — broadcast + CPFP.
- [lib/recovery/CLAUDE.md](../lib/recovery/CLAUDE.md) — immutable
  proof graph.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — system-wide package map.
