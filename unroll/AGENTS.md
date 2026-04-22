# unroll

## Purpose

Durable per-target unilateral-exit subsystem. One `VTXOUnrollActor` per VTXO
outpoint owns the full exit lifecycle (proof assembly → proof-node confirmation
→ CSV maturity → final sweep build → broadcast → confirmation) on top of a
pure `unrollplan.Planner` and the shared `txconfirm` actor. A thin
`UnrollRegistryActor` owns spawn / dedup / terminal bookkeeping and persists a
control-plane record per target to `db` so restart can restore in-flight jobs.

## Key Types

### Per-target actor
- `VTXOUnrollActor` — One durable actor per target outpoint. Wraps a
  `baselib/actor.DurableActor[Msg, Resp]` and owns the FSM session, proof,
  planner, and cached sweep transaction for this one VTXO.
- `Config` — Per-actor wiring: `TargetOutpoint`, `ActorID`, `DeliveryStore`,
  `ProofAssembler`, `VTXOStore`, `TxConfirmRef`, `ChainSource`, `Wallet`
  (`SweepWallet`), `MaxSweepFeeRateSatPerVByte`, and a `RegistryRef` for
  terminal notifications.
- `behavior` — Actor behavior. Holds `b.sweepTx` (restored from checkpoint on
  boot) so retries and replays converge on a single sweep txid / pkScript
  under `txconfirm`'s txid-keyed dedup.
- `Msg` / `Resp` / `Event` / `OutboxEvent` — Sealed durable-mailbox,
  response, FSM event, and FSM outbox surfaces.
- `StartUnrollRequest` / `ResumeUnrollRequest` / `HeightObservedMsg` /
  `TxConfirmedMsg` / `TxFailedMsg` / `SpendObservedMsg` / `GetStateRequest` —
  Durable mailbox messages. Each ships a per-message TLV codec (no JSON) with
  a pinned record-type layout; round-trip tests live in
  `messages_test.go`.
- `StartTrigger` — What caused the job to start: `TriggerManual`,
  `TriggerCriticalExpiry`, `TriggerRestart`, `TriggerFraudSpend`.
- `Phase` — Coarse derived phase for control-plane visibility:
  `PhasePending` / `PhaseMaterializing` / `PhaseCSVPending` /
  `PhaseSweepBroadcast` / `PhaseSweepConfirmation` / `PhaseCompleted` /
  `PhaseFailed`.
- `JobState` — Durable FSM state (height, trigger, planner state,
  `FailReason`, `SweepAttempts`).

### Registry
- `UnrollRegistryActor` — Thin coordinator over the set of
  `VTXOUnrollActor`s. Handles `EnsureUnrollRequest` / `GetStatusRequest`
  admission, receives `UnrollTerminatedMsg` from children, persists records,
  and `RestoreNonTerminal` on boot.
- `RegistryConfig` — Store, `DeliveryStore`, `ProofAssembler`, `VTXOStore`,
  `TxConfirmRef`, `ChainSource`, `Wallet`, `MaxSweepFeeRateSatPerVByte`.
- `RegistryRecord` — Control-plane row: `TargetOutpoint`, `ActorID`,
  `Phase`, `Trigger`, `FailReason`, `SweepTxid`.
- `RegistryStore` — Persistence surface: `UpsertRecord`, `GetRecord`,
  `ListNonTerminalRecords`, `MarkTerminal`. `DBRegistryStore` in
  `db_store.go` is the production implementation; it adapts to the
  `db.UnilateralExitStore` enum through `statusForPhase` / `phaseFromDB`
  and `triggerToDB` / `triggerFromDB` which are locked in by round-trip
  tests in `db_store_test.go`.
- `EnsureUnrollRequest` / `EnsureUnrollResp` — Admission API. Dedup runs
  against `r.active`, `r.pending`, and `Store.GetRecord` — a repeat request
  after a child terminated returns `Created=false` with the historical
  `ActorID` rather than spawning a fresh actor and clobbering the sweep txid
  / failure reason.

### Support
- `LocalProofAssembler` — Assembles a `recovery.Proof` from the VTXO
  descriptor and its OOR artifact lineage. Implements `ProofAssembler`.
- `DescriptorLineageResolver` — Walks OOR checkpoint artifacts to produce
  the list of lineage transactions that must be confirmed before sweep.
- `SweepWallet` — Wallet interface: `NewWalletPkScript`,
  `SignTaprootSpend`.
- `safeTxOutPkScript(tx, index)` — Bounds-checking helper used at every
  `tx.TxOut[i].PkScript` site so malformed proof artifacts (operator-sourced
  OOR inputs) surface as retryable errors instead of goroutine panics.

## Relationships

- **Depends on**: `baselib/actor` (`DurableActor`, `TLVMessage`, codec),
  `baselib/protofsm` (FSM engine), `lib/recovery` (immutable proof graph),
  `unrollplan` (pure planner + TLV state codec), `txconfirm` (broadcast +
  CPFP + confirmation), `chainsource` (best-height + spend watch + fee
  estimate), `vtxo` (`Descriptor`, `VTXOStore`), `db` (`UnilateralExitStore`,
  `RegistryRecord` DB shape), `lib/arkscript` (timeout-path spend info).
- **Depended on by**: `darepod` (wires the registry into the daemon via the
  lazy chain-resolver seam; wiring lives in PR #264).
- **Sends**:
  - → `txconfirm` (Ask): `EnsureConfirmedReq` — one per proof node and one
    for the final sweep. Dedup by txid makes retried sends idempotent.
  - → `chainsource` (Ask): `RegisterSpendRequest` on the target outpoint to
    catch external spends, `BestHeightRequest`, `FeeEstimateRequest`.
  - → registry (Tell): `UnrollTerminatedMsg` from each child on terminal
    transition.
  - → `vtxo` (indirect via chain-resolver seam, wired in #264):
    control-plane callbacks.
- **Receives**:
  - ← API (registry): `EnsureUnrollRequest`, `GetStatusRequest`
    (from `darepod` RPC layer via chain resolver).
  - ← registry (internal): `persistActiveRecordMsg`,
    `persistRecordResultMsg`, `UnrollTerminatedMsg`.
  - ← per-target actor (mailbox): `StartUnrollRequest`, `ResumeUnrollRequest`,
    `HeightObservedMsg`, `TxConfirmedMsg`, `TxFailedMsg`, `SpendObservedMsg`,
    `GetStateRequest`.
  - ← `txconfirm` notification subscriber: `TxConfirmed`, `TxFailed`
    → mapped to `TxConfirmedMsg` / `TxFailedMsg`.
  - ← `chainsource` block epochs: re-wrapped as `HeightObservedMsg`.
  - ← `chainsource` spend notifications: re-wrapped as `SpendObservedMsg`.

## Invariants

- **Persist-before-broadcast.** `startSweep` calls `persistCheckpoint`
  (writing `b.sweepTx` into the TLV checkpoint) BEFORE `txconfirm.Ask`. Any
  handler-level retry or crash-restart restores the same sweep tx, and
  `txconfirm`'s txid-keyed dedup makes the re-submit a benign no-op instead
  of broadcasting a second sweep with a freshly-derived wallet pkScript that
  races the first on chain.
- **Sweep tx reuse.** `startSweep` skips `buildSweepTx` when `b.sweepTx` is
  already set (either from a prior attempt this actor lifetime or restored
  from the checkpoint). This converges every retry on a single sweep
  txid / pkScript and avoids burning BIP32 wallet addresses on fee-spike
  retries.
- **Reissue must fail hard on missing state.** The `ReissueInFlightTransactions`
  and `ReissueSweepConfirmation` outbox branches return an error on a missing
  proof node or nil `sweepTx`. A silent `continue` would strand the FSM in
  `AwaitingMaterialization` or `AwaitingSweepConfirmation` with no pending
  `txconfirm` subscription and no way to advance.
- **Registry deduplication covers the whole trail.** `handleEnsure` checks
  `r.active`, `r.pending`, AND `Store.GetRecord` before spawning — a repeat
  request for an already-terminal outpoint returns the historical `ActorID`
  and does not overwrite the stored sweep txid or failure reason.
- **Fail-closed admission write.** `handleEnsure` calls `Store.UpsertRecord`
  synchronously and only returns `Created=true` after the record is durable.
  If the initial write fails, the spawned child is stopped, removed from
  `r.active`, and the caller sees a wrapped error instead of a silent
  orphan. Without this invariant, a crash between admission and the former
  async persist would leave the child unknown to `RestoreNonTerminal` on
  reboot, silently losing the job. Subsequent updates stay on the async
  `requestPersist` path so the registry goroutine is not held hostage by
  every state transition.
- **Durable mailbox messages are TLV, not JSON.** Every message in
  `messages.go` implements `actor.TLVMessage` with a hand-written
  `Encode`/`Decode` pair driven by `tlv.Stream`. Inner record types start at
  1 per message (the outer mailbox codec identifies which message). The
  checkpoint codec in `snapshot.go` is also TLV.
- **Checkpoint persists the sweep tx** via
  `wire.MsgTx.Serialize` under `checkpointSweepTxRecordType` so restore
  produces the exact same `b.sweepTx` that the pre-broadcast commit wrote.
- **Phase ↔ DB status mapping is lossless.** `PhaseSweepBroadcast` maps to
  `UnilateralExitJobStatusSweepBroadcasting` (=6) and
  `PhaseSweepConfirmation` maps to `UnilateralExitJobStatusSweeping` (=3) —
  the two used to collapse onto the same DB value and silently erase the
  "sweep built but not yet broadcast" vs "sweep broadcast awaiting conf"
  distinction. `TriggerFraudSpend` round-trips through a dedicated
  `UnilateralExitJobTriggerFraudSpend` constant instead of silently
  downgrading to `TriggerManual`.
- **FSM outbox events are side-effect-only.** `RequestSweepBuild`,
  `EnsureReadyTransactions`, `ReissueInFlightTransactions`, and
  `ReissueSweepConfirmation` never mutate `JobState`; they are routed by
  `behavior.routeOutbox` to `txconfirm.Ask` calls outside the FSM.
- **All TxOut indexing goes through `safeTxOutPkScript`.** Operator-sourced
  OOR artifacts flow into proof assembly; a zero-output or short-output
  proof node is mapped to a retryable error rather than panicking the actor
  goroutine.

## Deep Docs

- [docs/durable_actor_quickstart.md](../docs/durable_actor_quickstart.md) —
  `TLVMessage`, `ActorBehavior`, migration checklist.
- [docs/durable_actor_architecture.md](../docs/durable_actor_architecture.md) —
  CDC pattern and durable mailbox lifecycle.
- [unrollplan/CLAUDE.md](../unrollplan/CLAUDE.md) — Pure planner that this
  actor drives.
- [txconfirm/CLAUDE.md](../txconfirm/CLAUDE.md) — Shared broadcast + CPFP
  actor.
- [lib/recovery/CLAUDE.md](../lib/recovery/CLAUDE.md) — Immutable proof
  graph.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
