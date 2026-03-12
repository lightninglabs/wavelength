# vtxo

## Purpose

Per-VTXO lifecycle management using a state machine that monitors expiry,
coordinates refresh (forfeit + new issuance), coordinates forfeit
signing, and tracks cooperative and unilateral spending paths. The
Manager actor is the single admission gate for all VTXO operations. The
VTXO FSM models lifecycle phases only, not business intent like refresh
versus leave.

## Key Types

- `VTXOState` — Sealed interface for all states (Live, Spending, Spent, PendingForfeit, Forfeiting, Forfeited, UnilateralExit, Failed).
- `Descriptor` — Complete VTXO metadata: outpoint, amount, taproot key, CSV expiry, tree path to root.
- `Manager` — Actor managing per-VTXO FSM instances, lifecycle, and admission gating.
- `VTXOEvent` — Inbound events (BlockEpochEvent, ForfeitRequest, ForfeitConfirmed, SpendReserveEvent, SpendCompletedEvent, etc.).
- `VTXOOutMsg` — Outbound messages (ForfeitRequest, ExpiringNotify, StatusUpdate, Terminated).

## Relationships

- **Depends on**: `baselib/protofsm` (FSM engine), `lib/tree` (tree paths).
- **Depended on by**: `round` (triggers forfeit requests), `oor` (incoming VTXOs), `db` (persistence), `darepod` (wiring).
- **Sends**:
  - → `round` (via manager relay): `RefreshVTXORequest` (auto-expiry path), `ForfeitSignatureSubmission`
  - → `db` (via outbox): `VTXOStatusUpdate`
  - → `vtxo` manager: `VTXOTerminatedNotification`, `RelayToRoundMsg`
- **Receives**:
  - ← `round`: `ForfeitRequestEvent`, `ForfeitConfirmedEvent`, `BlockEpochEvent`
  - ← `manager` (admission): `SpendReserveEvent`, `SpendReleasedEvent`, `SpendCompletedEvent`, `PendingForfeitEvent`, `ForfeitReleasedEvent`
  - ← `chainsource` (via Manager): `BlockEpochEvent`

## Invariants

- VTXO actor state is the single source of truth for availability.
- Forfeit transaction is not broadcast until the connector output's round confirms (atomic replacement).
- Refresh is auto-triggered at configurable height before expiry.
- Once ForfeitedState is reached, the old VTXO is unspendable; the new VTXO is available only after round confirmation.
- SpendingState is persisted as VTXOStatusSpending and survives restarts.
- OOR completion transitions VTXOs to SpentState through the VTXO actor FSM, not by direct store writes.
- A VTXO in SpendingState cannot be admitted for cooperative consumption, and vice versa.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
