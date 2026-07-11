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
  zero falls back to the default), `RegistryRef`, optional
  `LedgerSink fn.Option[ledger.Sink]` (propagated from
  `RegistryConfig.LedgerSink` via `childConfig`; receives the confirmed
  on-chain exit fee once the final sweep confirms).
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
- `GetStatusRequest.Detailed` / `GetStatusResp.State` — a detailed status
  probe (`Detailed: true`) Asks the live child (`detailedChildState`,
  bounded by `detailedStatusAskTimeout` = 3s) for its `*GetStateResp`
  (including `Progress`) and attaches it as `GetStatusResp.State`; the
  common coarse polling path (`Detailed: false`, the default) never Asks
  the child and leaves `State` nil. The child Ask is best-effort — a
  timeout or error falls back to the coarse cached/stored record rather
  than failing the probe.
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
  confirmed/in-flight proof node or a non-pending sweep). It also carries
  the child's `ExitPolicyKind`, which the child stamps from its own durable
  exit policy (`exitPolicyKind`), so the terminal message is self-contained:
  it stays authoritative after the registry has evicted its in-memory
  `pending` record, which a completed async terminal persist can drop before
  the terminal handoff lands. `handleTerminated` prefers the message's
  `ExitPolicyKind` over the cached record and threads it to the manager via
  `notifyVTXOExit`, so a recovery-only target (a non-standard policy such as
  a vHTLC refund) is held in unilateral-exit rather than relived as a live
  coin on a recoverable failure. The kind is forwarded only to the manager,
  never stamped onto the persisted registry record — the message has no
  policy ref, so stamping its kind would drop the store's durable
  `(ExitPolicyKind, ExitPolicyRef)` identity.
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

### Feasibility & Funding

- `AssessExitFeasibility(ExitFeasibilityInput) ExitFeasibility` —
  up-front verdict folding wallet-funded CPFP cost (recovery-tx
  ancestry) and VTXO-funded sweep cost into one check, so admission can
  refuse an exit that would leave a dust sweep or burn more in fees
  than the VTXO is worth (darepo-client#608) instead of stranding it
  in an exit state after a min-relay-fee broadcast failure.
- `PlanExitFunding(desc, mat, feeRate, ...) ExitFundingPlan` —
  derives the wallet fee-input amount an operator/caller should fund
  before starting the exit; `RecommendedExitFeeInputAmount` reads the
  verdict for the same number.
- `ExitProgress` (`GetStateResp.Progress`, surfaced on a detailed status
  probe via `GetStatusResp.State`) — the planner-derived, human-facing
  progress summary computed by `behavior.exitProgress` at the actor's
  current best height: `ConfirmedTxs`/`InFlightTxs`/`ReadyTxs`/
  `BlockedTxs`/`TotalTxs` counts over the proof graph, `CurrentLayer`/
  `TotalLayers` frontier position, `TargetConfirmed`/`AllProofConfirmed`
  flags, `CSV fn.Option[unrollplan.CSVInfo]` (populated once the target
  confirms), `BestCaseBlocksRemaining` (optimistic lower-bound block
  count via `bestCaseBlocksRemaining`, gated on the longer of the CSV
  wait and the exit policy's cached `requiredLockTime`), and
  `ActualSweepFeeSat fn.Option[int64]` (populated once the sweep tx is
  built, via `actualSweepFeeSat`). `stateResponse` returns nil `Progress`
  when the planner/proof is not yet loaded.

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
- `behavior.proofNodeConfHeightHint(ctx, txid)` — confirmation-watch height
  floor for proof-graph nodes. Primary floor is `commitmentHeightFloor()`:
  `min(Descriptor.Ancestry[i].CommitmentHeight)`, but only when EVERY fragment
  has a known (>0) height — a single unknown fragment returns 0 and defers to
  the fallback (a min over only the known fragments would not bound an unknown
  fragment's ancestor). Nothing in a VTXO's proof graph confirms before its
  commitment tx, so once all fragments are known this is a tight, provable
  floor (min, never max — a lower floor only widens the safe rescan window).
  When no commitment height is known (legacy persisted VTXOs, empty-ancestry
  round VTXOs, or a server not yet populating `commitment_height`), it falls
  back to `proofNodeHeightHint(currentHeight)` =
  `max(1, currentHeight - proofNodeHeightHintLookback)` (10000 blocks) —
  identical to the pre-commitment-height behaviour. Roots/intermediate
  ancestors can confirm before the target VTXO's creation height, so the
  fallback anchors to tip minus a lookback (never `CreatedHeight`), bounding
  the neutrino historical rescan instead of scanning to genesis
  (darepo-client#884). The fallback path warns when the VTXO's age exceeds the
  lookback (the floor may then miss an already-confirmed ancestor).

## Relationships

- **Depends on**: `baselib/actor` (`DurableActor`, `TLVMessage`,
  codec), `baselib/protofsm`, `lib/recovery`, `unrollplan` (pure
  planner + TLV state codec), `txconfirm` (broadcast + CPFP +
  confirmation), `chainsource` (best-height, spend watch, fee
  estimate), `vtxo` (`Descriptor`, `VTXOStore`), `db`
  (`UnilateralExitStore`, `RegistryRecord` shape), `lib/arkscript`.
- **Depended on by**: `darepod` (wires the registry via the lazy
  chain-resolver seam, PR #264), `vhtlcrecovery/coordinator` (admission via
  `EnsureUnrollRequest`), `vhtlcrecovery/unrollpolicy` (implements
  `ExitSpendPolicyResolver` and `ExitSpendPolicy`), `fraud` (admits a
  `TriggerFraudSpend` unroll via `EnsureUnrollRequest` when it detects a
  counterparty spend racing our forfeit).
- **Sends**:
  - → `txconfirm` (Ask): `EnsureConfirmedReq` per proof node and for
    the final sweep; txid dedup makes retries idempotent.
  - → `chainsource` (Ask): `RegisterSpendRequest` on the target
    outpoint to catch external spends, `BestHeightRequest`,
    `FeeEstimateRequest`.
  - → registry (Tell): `UnrollTerminatedMsg` from each child on
    terminal transition.
  - → `ledger` actor (Tell, via `RegistryConfig.LedgerSink`):
    `ExitCostMsg` after the final sweep confirms. The amount is the proof
    target output value and the fee is derived from the persisted sweep tx
    outputs.
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

- **Terminal handoff waits for the ledger exit-cost Tell to land.**
  `notifyRegistryIfTerminal` calls `emitExitCostIfCompleted` before
  notifying the registry. On `PhaseCompleted` with a set `LedgerSink` and
  no prior emission (`!b.exitCostNotified`), it derives a
  `ledger.ExitCostMsg` via `exitCostMsg` (target output value from the
  proof minus the persisted sweep tx's output total; a non-positive
  result or missing proof/sweep tx is an unbuildable-but-terminal
  condition, logged at error and allowed through) and Tells the sink on a
  detached context. A Tell error defers the ENTIRE terminal handoff (the
  registry stops the child right after it, and terminal records are not
  restored on boot, so there is no later retry point) so the next height
  tick retries; a successful Tell sets `b.exitCostNotified` and is a safe
  at-least-once replay target since the ledger handler dedups by target
  outpoint.
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
