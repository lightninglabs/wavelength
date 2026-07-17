# batchcanon

## Purpose

Client-side **batch canonicality authority** for the reorg-safety epic
(lumos#454). This package is the sole client interpreter of how each batch
(commitment) transaction is faring against the best chain, and it produces the
**fail-closed lineage availability** the VTXO manager's admission gate and the
round/OOR producers consume.

It owns three things: the durable, reorg-aware **data model** (`Record`,
`ConsumedInput`, `ConsumerEdge`), the dependency-light **reducer** that derives
canonicality `State` and `Availability` from a complete current-chain
observation, and the **`Manager`** actor that arms reorg-aware watches, drives
the versioned snapshot/readiness restart barrier, and interprets chainsource
observations into state.

Observation (chainsource) → interpretation (this package) → admission (vtxo)
stays a strict split: chainsource reports raw reversible facts, this package
decides canonicality, and the VTXO manager remains the admission boundary.

## Key Types

- `State` — canonicality state: `StateUnseen`, `StateProvisional`,
  `StateFinalized`, `StateReorgedOut`, `StateConflictProvisional`,
  `StateConflictFinalized`. Every state is reorg-reversible; **no state is a
  terminal verdict** except a conflict that reached policy finality. Priority:
  `conflict_finalized > conflict_provisional > reorged_out >
  finalized/provisional > unseen`. Persisted as an append-only typed INTEGER —
  values must never be renumbered.
- `RegistrationStage` — crash-safe evidence lifecycle: `Registering` →
  `Reconciling` → `Complete`. Semantic `State` is **never** admissible unless
  the stage is `Complete` and `ReadyGeneration == ObservationGeneration`.
- `Record` — durable per-batch view keyed by **`BatchTxID`** (never
  `(txid, block hash)`; a reorg that re-mines the same tx is the same batch).
  Carries `BatchTx` (the serialized commitment tx, authenticated to hash to
  `BatchTxID`), `ObservationGeneration`/`ReadyGeneration`/`Revision`,
  `ConfirmationHeight`/`Block` (observation attributes; cleared on reorg),
  `CSVExpiryDelta`, `ConsumedInputs`, and `DependentVTXOs`. `Ready()` is true
  only with complete evidence + `Complete` stage + a matching ready generation.
  `EffectiveExpiry()` derives absolute expiry on demand
  (`ConfirmationHeight + CSVExpiryDelta`), so a reorg-and-reconfirm recomputes
  it instead of freezing it.
- `ConsumedInput` — one actual `TxIn` the batch spends, with `Value` +
  `PkScript` (required to arm the reorg-aware spend watch) and persisted
  `Conflicting`/`ConflictFinal` flags so restart reconciliation cannot
  transiently downgrade a persisted conflict.
- `ConsumerEdge` — the **logical value-lineage** edge (a VTXO consumed by a
  batch), separate from the on-chain `ConsumedInputs` graph. Carries
  `ExpectedRevision` and the full `CreatorLineage`; used by the terminal
  conditional-restore compare-and-swap, not mislabeled as a commitment input.
- `Availability` — derived (never persisted) VTXO-lineage spendability:
  `AvailableFinal`, `AvailableProvisional`, `AvailabilityUnknown`,
  `LineageReconciling`, `LimboReorg`, `LimboConflict`, `Invalidated`.
  **Fail-closed**: a missing record, a non-`Ready()` record, or an empty
  lineage all map to `LineageReconciling`; `Usable()` is true only for
  `AvailableFinal`/`AvailableProvisional`. `CombineAvailability` takes the
  worst across a multi-parent lineage.
- `AdmissionToken` — the linearizable guard returned by a successful lineage
  query, binding the observation generation + lineage revision; producers
  revalidate it before each critical effect.
- `Manager` — the actor interpreter. Arms one reorg-aware confirmation watch
  on the batch tx plus one reorg-aware spend watch per consumed input;
  registration cross-checks the serialized `BatchTx` (hash == `BatchTxID`,
  output/pkScript bound, every `TxIn` registered) before a row can reach
  `Ready`. `Reconcile(g)` runs the restart barrier: it opens a new observation
  generation, re-arms watches, requires an explicit current fact per subject,
  and only installs `Ready(g)` + derived state atomically — admission stays
  closed until then, so a persisted conflict can never transiently look usable.

## Relationships

- **Depends on**: `btcd/chainhash`, `btcd/wire`, `lnd/fn/v2` only (plus
  `baselib/actor` + `chainsource` for the `Manager`). The reducer/model
  (`state.go`, `availability.go`, `record.go`) is deliberately dependency-light
  so the **server can reuse the same reducer** (lumos#454 Server PR2).
- **Depended on by**: `db` (concrete `Store`), `vtxo` (admission gate), and the
  round/OOR producers (registration before exposure).

## Invariants

- Identity is by txid / outpoint, never `(txid, block hash)`.
- Fail-closed: missing / incomplete / unarmed / reconciling lineage is never
  usable. Registration completeness is part of the safety proof, not a
  compatibility hint.
- Registration authenticates the serialized commitment tx (hash + full `TxIn`
  set) before a record reaches `Ready`; an omitted or unauthenticated input
  keeps the record unavailable.
- Two graphs: the on-chain `ConsumedInputs` (`TxIn`) graph drives conflict
  observation; the logical `ConsumerEdge` graph drives inherited lineage and
  conditional restore.
- Expiry is never persisted as a standalone/terminal value; always derived.
- State enum integer values are append-only (persisted column).
- Restart increments the observation generation before any watch is armed;
  admission is closed until `Ready(g)` installs a complete snapshot.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
- `REORG_SAFETY_SPEC.md` (workspace root) — normative §3–§9 contracts.
