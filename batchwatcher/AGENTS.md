# batchwatcher

## Purpose

On-chain batch transaction monitoring and VTXO spend detection. Watches
confirmed batch transactions for sweeps, spends, and expiry, reporting state
changes to the round and sweep subsystems.

## Key Types

- `Actor` — Durable actor monitoring batch transactions on-chain.
- `ActorConfig` — Configuration container for actor initialization. Now
  includes optional `SpendRecoveryStore` (VTXO/forfeit lookups) and
  `CheckpointLookup` (OOR checkpoint by input) seams wired from the root
  package via adapter types in `server_batchwatcher_adapters.go`.
- `BatchID` — Identifier for a confirmed batch.
- `Output` — Tracked output within a batch tree. `LeafDescriptor.CoSignerKey`
  (renamed from `SigningKey`) is the co-signer public key for the leaf.
- `BatchTreeState` — Aggregate state of a batch's VTXO tree on-chain.
- `StateStore` — In-memory runtime state for tracked batch trees (rebuilt on actor restart).
- `BatchWatcherMsg` / `BatchWatcherResp` — Sealed message/response interfaces for actor protocol.
- `RegisterBatchRequest` / `GetTreeStateRequest` / `UnregisterBatchRequest` — Inbound actor messages.
- `FraudDetectorMsg` — Interface for messages sent to fraud detector
  (`VTXOOnChainNotification`, `UnexpectedSpendNotification`).
- `UnexpectedSpendNotification` — Sent to fraud detector on unexpected spends.
  Carries `Classification` (`SpendClassification`), `ResponseTxID` (txid the
  fraud detector should broadcast), and `ResponseTx` (the broadcastable
  transaction for forfeit and OOR-checkpoint responses, pre-populated so the
  fraud detector avoids repeat recovery-store lookups).
- `CheckpointSweepNotification` — Sent to the fraud detector after a checkpoint
  output remains unspent until `CheckpointMaturityHeight` (CSV maturity). Carries
  `BatchID`, `InputOutpoint`, `CheckpointOutpoint`, and `MaturityHeight`. Emitted
  by batchwatcher when an `Output.IsCheckpoint` frontier output has matured.
- `SpendClassification` — Discriminates between fraud-response flows:
  `MissedBranchTx`, `ForfeitedLeaf`, `OORCheckpointLeaf`, `SpentLeaf`,
  `ExpiredLeaf`, `InFlightLeaf`. The fraud detector switches on this value.
- `SpendRecoveryStore` — Interface for VTXO/forfeit lookups during leaf-spend
  classification. Methods: `GetVTXO`, `GetForfeitInfo`,
  `MarkVTXOUnrolledByClient`. Implemented by an adapter in the root package.
- `CheckpointLookup` — Interface for resolving a broadcastable OOR checkpoint
  by spent VTXO input. Method: `LoadCheckpointTxByInput`. Implemented by an
  adapter backed by `oor.DBSessionStore` in the root package.
- `RecoveryVTXO` — Minimal VTXO view (outpoint + `VTXOStatus`) needed for leaf
  spend classification.
- `RecoveryForfeitInfo` — Minimal forfeit metadata (`ForfeitTx`) for
  constructing a fraud response.
- `VTXOStatus` — Lifecycle state subset used by the batchwatcher: `live`,
  `in_flight`, `forfeited`, `unrolled_by_client`, `expired`, `spent`.
- `BatchSweeperMsg` — Interface for messages sent to batch sweeper (`BatchExpiredNotification`, `TreeStateChangedNotification`).
- `Output.IsCheckpoint` — Flag marking a frontier output as checkpoint output 0
  from a finalized OOR checkpoint. When set, `CheckpointInput`,
  `CheckpointMaturityHeight`, and `CheckpointSweepRequestedHeight` are also
  populated. `handleNewBlockReceived` emits a `CheckpointSweepNotification` once
  per maturity block (suppressing per-block duplicates, retrying after
  `checkpointSweepRetryBlocks` on transient failures).
- `Output.RatchetSpendingTx` / `RatchetSpendingHeight` /
  `RatchetRetryRequestedHeight` — Checkpoint frontier retention fields. When a
  checkpoint output is spent but the fraud-response ratchet step fails (e.g.,
  DB or txconfirm error), the watcher stores the spending transaction and height
  here rather than dropping the output. On restart, `handleCheckpointOutputSpend`
  is re-entered with the retained spending tx, retrying the ratchet step.
  `ratchetRetryBlocks = 1` governs the retry-request gap between attempts.
- `BatchSubtreeSweptNotification` — Sent to `batchsweeper` when a mid-tree
  branch output is swept by the operator's expired-subtree path
  (`spendDispositionExpiredSubtreeSweep`). Carries `BatchID` and `SubtreeRoot`.
  Distinct from `BatchExpiredNotification` (full batch sweep) so the sweeper
  can update tracking state for partial tree sweeps.
- `RegisterBatchRequest.SweepKey` / `BatchTreeState.SweepKey
  keychain.KeyDescriptor` — Historical operator key descriptor threaded from
  the rounds actor through `RegisterBatchRequest` into `BatchTreeState`. The
  batch sweeper uses this to sign timeout spends with the exact locator that
  derived the tree's sweep tapleaf, surviving configured-key rotations. A zero
  descriptor (`PubKey == nil`) signals "locator unknown" (pre-migration round).
- `spendDispositionExpiredSubtreeSweep` — New spend classification for a batch
  tree output spent at or after the batch's `ExpiryHeight`. Triggers
  `notifyBatchSubtreeSwept` and removes the output from tracking without
  marking the full batch as swept.

## Relationships

- **Depends on**: `oor` indirectly via `CheckpointLookup` (OOR checkpoint
  lookup seam wired from root package); `rounds` indirectly via
  `SpendRecoveryStore` (VTXO/forfeit state lookup seam). Both seams are
  optional (`fn.Option`) to avoid import cycles.
- **Depended on by**: `rounds` (confirmation monitoring), `batchsweeper`
  (sweep eligibility), `harness` (test inspection via `GetBatchTreeState`).
- **Messages to/from**:
  - Receives `RegisterBatchRequest` <- `rounds` (register confirmed batch for monitoring).
  - Receives `GetTreeStateRequest` <- `rounds` (query on-chain tree state).
  - Sends `VTXOOnChainNotification`, `UnexpectedSpendNotification`,
    and `CheckpointSweepNotification` -> fraud detector.
  - Receives `UnregisterBatchRequest` <- `rounds` (stop monitoring a batch).
  - Sends `BatchExpiredNotification`, `TreeStateChangedNotification`,
    `BatchSubtreeSweptNotification` -> `batchsweeper`.

## Invariants

- Must detect all spends of tracked outputs; missed spends can cause incorrect VTXO state.
- Batch state must be persisted before notifications are sent.
- After a batch is fully swept, the watcher self-unregisters the batch and
  notifies the sweeper via `BatchSweptEvent`, preventing duplicate monitoring.
- Leaf VTXO outputs are now watched for spend (not only branch outputs).
  Client-owned spend paths (forfeit, OOR, CSV timeout) are classified via
  `SpendRecoveryStore` lookups before in-memory state is mutated.
  Classification errors preserve tracking state — no mutation occurs unless
  classification succeeds.
- `SpendClassification` must be set on every `UnexpectedSpendNotification`;
  `SpendClassificationUnknown` (zero value) must never be emitted in
  production. The fraud detector switches on this value exclusively.
- `UnexpectedSpendNotification.ResponseTxID` is the txid the fraud detector
  should broadcast; its meaning is determined by `Classification`. For
  `ExpiredLeaf` and `InFlightLeaf` without a checkpoint it is zero.
- `UnexpectedSpendNotification.ResponseTx` is non-nil for `ForfeitedLeaf`
  and `OORCheckpointLeaf` classifications; it is the broadcastable transaction
  pre-populated so the fraud detector skips redundant store lookups.
- `CheckpointSweepNotification` is emitted at most once per maturity block per
  checkpoint output. `CheckpointSweepRequestedHeight` is updated after each
  successful handoff; `checkpointSweepRetryBlocks` governs the retry window on
  transient failure so a stranded mature output does not wait for daemon restart.
- An `in_flight` leaf spend is a race won by the client: the watcher hands
  off any checkpoint to the fraud detector and calls
  `SpendRecoveryStore.MarkVTXOUnrolledByClient` to release the lock.
- `BatchIDForRoundOutput` derives a deterministic batch ID from a round UUID
  and output index; callers outside the package (e.g., `harness`) use this
  function to construct batch IDs for `GetTreeStateRequest`.
- **Failed checkpoint ratchet steps retain the frontier.** When
  `handleCheckpointOutputSpend` encounters a DB or txconfirm error, the output
  is NOT removed from `ExistingOutputs`. Instead, `RatchetSpendingTx` and
  `RatchetSpendingHeight` record the spending transaction so daemon restart
  can re-enter the handler and retry. `RatchetRetryRequestedHeight` prevents
  duplicate retry requests within `ratchetRetryBlocks` blocks.
- **Mid-tree branch sweeps classified as `spendDispositionExpiredSubtreeSweep`.**
  A batch output spent at or after `ExpiryHeight` (not full-batch sweep) is
  classified as an expired subtree sweep, triggers `BatchSubtreeSweptNotification`,
  removes the output from tracking, and fires `notifyTreeStateChanged`. This
  handles the case where the operator sweeps a branch output before the entire
  tree is swept.
- **`SweepKey` propagation is required for non-zero sweep key locators.** A
  `RegisterBatchRequest` with `SweepKey.PubKey == nil` is accepted (pre-migration
  compatibility) but the sweeper must fall back to its configured key and log
  the gap. Callers must populate `SweepKey` from the persisted round data.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
