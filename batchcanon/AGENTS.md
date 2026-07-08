# batchcanon

## Purpose

Client-side **batch canonicality data model** for the reorg-safety epic
(darepo#454, task C2). Holds the durable, reorg-aware record of how each batch
(commitment) transaction is faring against the best chain: its canonicality
state, current confirmation observation, recompute inputs for effective
expiry, the inputs it consumes, the VTXOs it anchors, and the reverse
dependencies needed to restore a provisionally consumed VTXO.

This package is **data + query/update interface only**. It contains no
interpretation, no chain watching, and no admission behavior — those belong to
the (later) `BatchCanonicalityManager` and the VTXO manager. Keeping the model
in its own package, separate from `chainsource` (raw observation) and `vtxo`
(admission), preserves the epic's observation → interpretation → action split.

## Key Types

- `State` — canonicality state enum: `StateUnseen`, `StateProvisional`,
  `StateFinalized`, `StateReorgedOut`, `StateConflictProvisional`,
  `StateConflictFinalized`. Reorg-reversible; **no state is a terminal
  verdict** at this layer. Persisted as an append-only typed INTEGER column —
  values must never be renumbered.
- `PolicyState` — reserved policy classification slot (`PolicyStateDefault`
  only); persisted and round-tripped, no business meaning yet.
- `Record` — per-batch record keyed by `BatchTxID`. Identity is by **txid**,
  never `(txid, block hash)`; `ConfirmationBlock` is an observation attribute
  only. `EffectiveExpiry()` derives the absolute expiry as
  `ConfirmationHeight + CSVExpiryDelta`, returning `None` when unconfirmed —
  the structural guarantee that expiry is recomputed on every
  reconfirmation rather than frozen.
- `ProvisionalConsumer` — reverse-dependency edge (consumed VTXO → consumer
  batch) enabling VTXO restore if a consumer batch never becomes canonical.
- `Availability` — derived (never persisted) VTXO-lineage spendability:
  `AvailableFinal`, `AvailableProvisional`, `AvailabilityUnknown`,
  `LimboReorg`, `LimboConflict`, `Invalidated`. `AvailabilityForState`
  maps one batch's `State`; `CombineAvailability` takes the worst across a
  multi-parent lineage; `Usable()` is true only for confirmed lineage.
  `LineageAvailability`/`LineageBlocked` load each parent batch from the
  `Store` and produce the combined availability / block decision the VTXO
  manager's admission gate (C5 wiring) calls per candidate. The gate is
  permissive: unseen / not-yet-registered lineage does not block — only
  limbo/invalidated lineage does.
- `Store` — behavior-free durable query/update interface. Implemented by
  `db.BatchCanonicalityPersistenceStore` over the `000020`/`000021` schema;
  backfilled from existing VTXOs via
  `db.BatchCanonicalityPersistenceStore.BackfillFromVTXOs`.
- `Manager` — the actor that interprets chain observation into canonicality
  state (the sole client-side interpreter). Registered under
  `ManagerServiceKey`. `RegisterBatchRequest` arms one reorg-aware
  confirmation watch on the batch tx and one reorg-aware spend watch per
  consumed input (deduped per batch, idempotent — repeats merge dependent
  VTXOs). It maps chainsource `ConfirmationEvent`/`ConfReorgedEvent`/
  `ConfDoneEvent` and `SpendEvent`/`SpendReorgedEvent`/`SpendDoneEvent` onto
  its own mailbox and derives `State` per the priority
  `conflict_finalized > conflict_provisional > reorged_out >
  finalized/provisional > unseen`. `Reconcile` re-arms watches for non-final
  batches after restart without downgrading persisted state.
  `GetBatchStateRequest` reads the persisted record. `NewManager` returns the
  behavior; the caller registers it, then calls `SetSelfRef(ref.TellRef())`
  and `Reconcile`.

## Relationships

- **Depends on**: `btcd/chaincfg/chainhash`, `btcd/wire`, `lnd/fn/v2` only.
- **Depended on by**: `db` (concrete store), and — in later tasks — the
  batch canonicality manager and `vtxo` admission.

## Invariants

- Identity is by txid / outpoint, never by `(txid, block hash)`.
- Expiry is never persisted as a standalone or terminal value; it is always
  derived from `CSVExpiryDelta` + the current confirmation observation.
- State enum integer values are append-only (persisted column).

## Expiry-as-terminal audit (darepo#454 C2)

C2 requires auditing every site that treats `BatchExpiry`/`Expired` as a
one-way terminal fact. These are flagged for rework when the
BatchCanonicalityManager (task C3/C4) rewires expiry consumers onto
`Record.EffectiveExpiry()`; **no behavior is changed by C2**:

- `vtxo/transitions.go` (`ExpiryStatusExpired → FailedState{Recoverable:
  false}`, and the Critical/Expired escalations) — the primary offender: a
  reorg that lowers the confirmation height could otherwise push a VTXO
  permanently into non-recoverable `Failed`.
- `vtxo/expiry.go` (`CheckExpiry`, `BlocksUntilExpiry`) — compute from the
  frozen absolute `vtxo.BatchExpiry`; must consume effective (recomputable)
  expiry instead.
- `vtxo/actor.go` — schedules on the frozen absolute `BatchExpiry`.
- `waved/vhtlc_recovery_target.go` — folds multiple roots into a
  most-restrictive absolute `batchExpiry`.
- `unroll/proof_assembler.go` (`BatchExpiry == 0`) — treats zero as "unset",
  not terminal; benign, documented for completeness.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
</content>
