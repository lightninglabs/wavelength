# vtxo

## Purpose

Per-VTXO lifecycle management using a state machine that monitors expiry,
coordinates forfeit signing, and tracks cooperative and unilateral exit paths.
The VTXO FSM models lifecycle phases only (not business intent like
refresh vs leave); intent composition is handled by the wallet.

## Key Types

- `VTXOState` — Sealed interface for all states (Live, PendingForfeit, Forfeiting, Forfeited, UnilateralExit, Failed).
- `Descriptor` — Complete VTXO metadata: outpoint, amount, taproot key, CSV expiry, tree path to root, `ChainDepth` (OOR hop count from on-chain commitment).
- `Manager` — Actor managing per-VTXO FSM instances and their lifecycle. Handles both round-created (`VTXOCreatedNotification`) and OOR-materialized (`VTXOsMaterializedNotification`) VTXOs.
- `VTXOsMaterializedNotification` — Notifies the manager that VTXOs were already persisted by another actor (OOR receive) and only actor activation is needed.
- `VTXOEvent` — Inbound events (BlockEpochEvent, PendingForfeitEvent, ForfeitRequestEvent, ForfeitConfirmedEvent, ResumeVTXOEvent).
- `VTXOOutMsg` — Outbound messages (ForfeitRequest, ForfeitSignatureSubmission, ExpiringNotification, VTXOStatusUpdate, VTXOTerminatedNotification).

## Relationships

- **Depends on**: `baselib/protofsm` (FSM engine), `lib/tree` (tree paths).
- **Depended on by**: `round` (triggers forfeit requests), `oor` (incoming VTXOs), `db` (persistence), `darepod` (wiring).
- **Sends**:
  - → `round` (via manager relay): `RefreshVTXORequest` (auto-expiry path), `ForfeitSignatureSubmission`
  - → `db` (via outbox): `VTXOStatusUpdate`
  - → `vtxo` manager: `VTXOTerminatedNotification`, `RelayToRoundMsg`
- **Receives**:
  - ← `round`: `VTXOCreatedNotification`, `PendingForfeitEvent`, `ForfeitRequestEvent`, `ForfeitConfirmedEvent`, `BlockEpochEvent`
  - ← `oor`: `VTXOsMaterializedNotification` (already-persisted VTXOs needing actor activation)
  - ← `chainsource` (via Manager): `BlockEpochEvent`
  - ← API: `ResumeVTXOEvent`

## Invariants

- Forfeit transaction is not broadcast until the connector output's round confirms (atomic replacement).
- Refresh is auto-triggered at configurable height before expiry.
- Once ForfeitedState is reached, the old VTXO is unspendable; the new VTXO is available only after round confirmation.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
