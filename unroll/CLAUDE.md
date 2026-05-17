# unroll

## Purpose

Per-target unilateral-exit subsystem. One in-memory `VTXOUnrollActor` per VTXO
outpoint owns the full exit lifecycle: proof assembly, ancestor confirmation,
CSV maturity, sweep build, sweep broadcast, and sweep confirmation.

The package keeps the actor abstraction for local concurrency and reasoning,
but does not use durable actors or durable mailboxes. Restart safety comes
from target-keyed SQL rows in the unroll job/effect/control-plane tables.

## Component Split

- `unrollplan.Planner` - pure planner. Given a recovery proof plus durable
  state, it decides what transactions are ready, what is blocked, whether CSV
  has matured, and whether the sweep should be built.
- `VTXOUnrollActor` - one in-memory actor per target outpoint. It owns the
  FSM session, proof, planner state, cached sweep transaction, chain watches,
  txconfirm calls, and SQL job snapshot.
- `UnrollRegistryActor` - in-memory coordinator for spawn, dedup, restore,
  terminal bookkeeping, and coarse control-plane persistence.
- `EffectWorker` - SQL effect poller. It claims due `unroll_effects` rows and
  asks the registry to resume the target; the per-target actor then derives
  the work to reissue from `unroll_jobs`, `unroll_tx_progress`, and
  `unroll_watches`. The worker validates `effect_type`, but replay remains
  target-derived so FSM dispatch logic stays in one place.
- `LocalProofAssembler` / `DescriptorLineageResolver` - reconstruct immutable
  recovery proof material from locally persisted VTXO and OOR artifact state.
- `buildSweepTx` / `snapshot.go` - build, sign, encode, and decode the final
  timeout-path sweep state.

## Key Types

### Per-target actor

- `JobStore` - SQL persistence surface for one target's detailed FSM state:
  job row, planner state, deferred checkpoints, sweep tx, watches, tx
  progress, and effect completion. Immutable proof material is reconstructed
  from local VTXO and OOR artifact lineage, not stored as a standalone job
  blob.
- `Config` - target outpoint, `JobStore`, proof assembler, VTXO store,
  txconfirm ref, chain source, wallet, logger, fee clamp, fraud checkpoint
  margin, and optional registry notification ref.
- `VTXOUnrollActor` - wrapper around `actor.Actor[Msg, Resp]`.
- `Msg` / `Resp` - in-memory message surface. Messages embed
  `actor.BaseMessage`; they are not encoded for a durable mailbox.
- `StartUnrollRequest` - starts a target from a proof/trigger.
- `ResumeUnrollRequest` - resumes a target from SQL after restart or effect
  replay.
- `HeightObservedMsg`, `TxConfirmedMsg`, `TxFailedMsg`, `SpendObservedMsg` -
  external observations fed into the FSM.
- `GetStateRequest` - reads the current detailed state.
- `StartTrigger` - manual, critical-expiry, restart, or fraud-spend trigger.
- `Phase` - coarse lifecycle phase for control-plane visibility.
- `JobState` - durable FSM state: height, trigger, planner state, fail
  reason, deferred checkpoints, and sweep attempts.

### Registry

- `RegistryStore` - SQL control-plane persistence for target admission,
  restore, and terminal status.
- `RegistryRecord` - coarse row keyed by target outpoint: actor ID, trigger,
  phase, fail reason, and sweep txid.
- `RegistryConfig` - registry store plus the child actor dependencies it uses
  to respawn targets.
- `UnrollRegistryActor` - actor owning active children, pending terminal
  persistence, restore, and persistence retry backoff.
- `EnsureUnrollRequest` - deduped admission. It checks active children,
  pending records, and SQL before spawning.
- `GetStatusRequest` - status API backed by active child state when available
  and SQL control-plane state otherwise.

## Relationships

- **Depends on**: `baselib/actor` as an in-memory actor runtime,
  `baselib/protofsm`, `unrollplan`, `txconfirm`, `chainsource`, `vtxo`,
  `lib/recovery`, `lib/arkscript`, and `db`.
- **Depended on by**: `darepod` unilateral-exit wiring and chain-resolver
  surfaces.
- **Sends**:
  - To `txconfirm`: `EnsureConfirmedReq` for proof nodes and final sweep.
  - To `chainsource`: best-height, fee-estimate, and spend-watch requests.
  - To registry: terminal child notifications.
- **Receives**:
  - From API/registry: ensure, resume, and status requests.
  - From `txconfirm`: confirmation/failure notifications.
  - From `chainsource`: block height and spend observations.
  - From `EffectWorker`: target resume requests for stranded effects.

## Durability Model

- Actors are restartable workers, not durable state containers.
- Every meaningful state transition writes SQL via `persistJob` before the
  actor issues side effects that depend on that state.
- `unroll_jobs` is the source of truth for target state, planner state, sweep
  tx bytes, deferred checkpoints, and failure/completion. Proof material is
  reconstructed from local lineage/artifact tables through
  `LocalProofAssembler`.
- Child tables store tx progress, watches, and effects so restart can
  re-create txconfirm subscriptions and chain watches.
- `unroll_effects` rows are typed retry handles. The worker validates the
  persisted type against the SQL enum and resumes the target actor; the actor
  derives the exact pending work from SQL.
- Registry records are coarse control-plane state used for admission dedup and
  `RestoreNonTerminal`.

## Invariants

- Persist before broadcast. `startSweep` saves the sweep tx before asking
  `txconfirm` to broadcast so retries reuse the same txid and pkScript.
- Sweep tx reuse is mandatory. If `b.sweepTx` already exists, the actor must
  not rebuild with a fresh wallet script.
- Admission fails closed. `handleEnsure` persists a control-plane row before
  returning `Created=true`.
- Registry dedup checks active children, pending terminal persistence, and SQL
  before spawning.
- Reissue paths fail hard on missing state. A missing proof node, watch, or
  sweep tx must surface as an error rather than silently stranding the FSM.
- Deferred checkpoint deadlines are durable as part of `JobState` and are
  reloaded from SQL on restart.
- The FSM emits side-effect descriptions; the actor behavior performs IO and
  persistence around it.
- All `TxOut` indexing goes through `safeTxOutPkScript`.
- External spends of the target outpoint fail the job unless the spender is a
  known proof node or the stored sweep.

## Restart Flow

On daemon start, the registry calls `RestoreNonTerminal`, lists non-terminal
records from SQL, respawns one child actor per target, and sends
`ResumeUnrollRequest`. The child loads its SQL job snapshot, reconstructs the
FSM, and reissues pending txconfirm/chain-source work. `txconfirm` dedupes by
txid, so re-submission is idempotent.

## Deep Docs

- [unrollplan/CLAUDE.md](../unrollplan/CLAUDE.md) - pure planner.
- [txconfirm/CLAUDE.md](../txconfirm/CLAUDE.md) - broadcast/confirmation
  actor.
- [lib/recovery/CLAUDE.md](../lib/recovery/CLAUDE.md) - immutable proof graph.
- [db/CLAUDE.md](../db/CLAUDE.md) - SQL job/effect stores.
- [ARCHITECTURE.md](../ARCHITECTURE.md) - system-wide package map.
