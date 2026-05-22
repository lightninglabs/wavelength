# batchwatcher

## Purpose

On-chain batch transaction monitoring and VTXO spend detection. Watches
confirmed batch transactions for sweeps, spends, and expiry; reports state
changes to the round and sweep subsystems.

## Key Concepts

Use `go doc batchwatcher.<Symbol>` for signatures.

- **`Actor`** — Durable actor monitoring batches on-chain. `ActorConfig`
  carries optional `SpendRecoveryStore` (VTXO/forfeit lookups) and
  `CheckpointLookup` (OOR checkpoint-by-input) seams wired by adapters in
  `server_batchwatcher_adapters.go`. Both are `fn.Option` to avoid import
  cycles.
- **`BatchTreeState` / `StateStore`** — Aggregate per-batch on-chain state +
  in-memory runtime tracker (rebuilt on restart). `BatchIDForRoundOutput`
  derives a deterministic batch ID from a round UUID + output index;
  callers outside the package (e.g., `harness`) use this to construct IDs
  for `GetTreeStateRequest`.
- **`Output`** — Tracked output. `LeafDescriptor.CoSignerKey` (renamed from
  `SigningKey`). `IsCheckpoint` flags frontier outputs from finalized OOR
  checkpoints (paired with `CheckpointInput`, `CheckpointMaturityHeight`,
  `CheckpointSweepRequestedHeight`).
- **Checkpoint frontier retention** — `Output.RatchetSpendingTx` /
  `RatchetSpendingHeight` / `RatchetRetryRequestedHeight`: when a
  checkpoint output is spent but the fraud-response ratchet step fails
  (DB or txconfirm error), the watcher stores the spending tx + height
  instead of dropping the output. Restart re-enters
  `handleCheckpointOutputSpend` with the retained tx; `ratchetRetryBlocks
  = 1` gates retry gap.
- **Spend classification** — `SpendClassification` discriminates
  fraud-response flows: `MissedBranchTx`, `ForfeitedLeaf`,
  `OORCheckpointLeaf`, `SpentLeaf`, `ExpiredLeaf`, `InFlightLeaf`. The
  fraud detector switches on this value. `spendDispositionExpiredSubtreeSweep`
  is the new classification for a batch output spent at or after
  `ExpiryHeight` (not full-batch sweep) — triggers
  `BatchSubtreeSweptNotification`, removes the output from tracking, and
  fires `notifyTreeStateChanged`.
- **Recovery store seams** — `SpendRecoveryStore` (`GetVTXO`,
  `GetForfeitInfo`, `MarkVTXOUnrolledByClient`) gates leaf-spend
  classification. `CheckpointLookup` (`LoadCheckpointTxByInput`) resolves
  a broadcastable OOR checkpoint by spent input (backed by
  `oor.DBSessionStore`). Narrow views: `RecoveryVTXO` (outpoint +
  `VTXOStatus`) and `RecoveryForfeitInfo` (`ForfeitTx`).
- **`SweepKey`** — `RegisterBatchRequest.SweepKey` and
  `BatchTreeState.SweepKey keychain.KeyDescriptor` thread the historical
  operator key descriptor from the rounds actor into the batch sweeper so
  timeout spends sign with the exact locator that derived the sweep
  tapleaf — survives configured-key rotations. Interpretation: full
  descriptor (new rows), pubkey + zero locator (pre-migration rows —
  caller must refuse to fall back to the configured key), zero descriptor
  / `PubKey == nil` (very old test fixtures with no `SweepKey` at all —
  see `rounds.restoreSweepKey`).
- **Notifications**:
  - `UnexpectedSpendNotification` carries `Classification`, `ResponseTxID`
    (txid for the fraud detector to broadcast), and `ResponseTx`
    (pre-populated broadcastable tx for forfeit + OOR-checkpoint paths
    so the fraud detector skips redundant store lookups).
  - `CheckpointSweepNotification` (`BatchID`, `InputOutpoint`,
    `CheckpointOutpoint`, `MaturityHeight`) — emitted by
    `handleNewBlockReceived` once per maturity block per checkpoint
    (per-block dedup; retries after `checkpointSweepRetryBlocks` on
    transient failure).
  - `BatchSubtreeSweptNotification` (`BatchID`, `SubtreeRoot`) — sent to
    `batchsweeper` for mid-tree branch sweeps, distinct from
    `BatchExpiredNotification` (full sweep).
- **`VTXOStatus`** — Lifecycle subset used here: `live`, `in_flight`,
  `forfeited`, `unrolled_by_client`, `expired`, `spent`.

## Relationships

- **Depends on** (optional seams): `oor` indirectly via `CheckpointLookup`,
  `rounds` indirectly via `SpendRecoveryStore`.
- **Depended on by**: `rounds` (confirmation), `batchsweeper` (sweep
  eligibility), `harness` (`GetBatchTreeState`).
- **Messages**:
  - ← `rounds`: `RegisterBatchRequest`, `GetTreeStateRequest`,
    `UnregisterBatchRequest`.
  - → fraud detector: `VTXOOnChainNotification`,
    `UnexpectedSpendNotification`, `CheckpointSweepNotification`.
  - → `batchsweeper`: `BatchExpiredNotification`,
    `TreeStateChangedNotification`, `BatchSubtreeSweptNotification`.

## Invariants

- Must detect **all** spends of tracked outputs; missed spends produce
  incorrect VTXO state.
- Batch state persists before notifications fire.
- After full-batch sweep, the watcher self-unregisters and notifies the
  sweeper via `BatchSweptEvent` — prevents duplicate monitoring.
- Leaf VTXO outputs are watched for spend (not only branch outputs).
  Client-owned spend paths (forfeit, OOR, CSV timeout) classify via
  `SpendRecoveryStore` lookups before any state mutation;
  classification errors preserve tracking state.
- `SpendClassification` is set on every `UnexpectedSpendNotification`;
  `SpendClassificationUnknown` must never be emitted in production.
- `UnexpectedSpendNotification.ResponseTx` is non-nil for `ForfeitedLeaf`
  and `OORCheckpointLeaf`.
- `CheckpointSweepNotification` is emitted at most once per maturity block
  per output; `CheckpointSweepRequestedHeight` updates after each
  successful handoff so a stranded mature output doesn't wait for restart.
- An `in_flight` leaf spend is "client wins the race": hand off any
  checkpoint to the fraud detector + `MarkVTXOUnrolledByClient` to
  release the lock.
- Failed checkpoint ratchet steps **retain** the frontier in
  `ExistingOutputs`; `RatchetRetryRequestedHeight` prevents duplicate
  retry requests within `ratchetRetryBlocks`.
- `RegisterBatchRequest` with `SweepKey.PubKey == nil` is accepted
  (test-fixture compatibility), but the sweeper logs the gap and is
  responsible for refusing to silently fall back when the pubkey is set
  but the locator is zero.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide map.
