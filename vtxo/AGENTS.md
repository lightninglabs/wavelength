# vtxo

## Purpose

Per-VTXO lifecycle management using a state machine that monitors expiry,
coordinates refresh (forfeit + new issuance), and tracks cooperative and
unilateral spending paths.

## Key Types

- `VTXOState` — Sealed interface for all states (Live, RefreshRequested, Forfeiting, Forfeited, Expiring, Failed).
- `Descriptor` — Complete VTXO metadata: outpoint, amount, taproot key, CSV expiry, tree path to root.
- `Manager` — Actor managing per-VTXO FSM instances and their lifecycle.
- `VTXOEvent` — Inbound events (BlockEpochEvent, ForfeitRequest, ForfeitConfirmed, ResumeVTXOEvent).
- `VTXOOutMsg` — Outbound messages (ForfeitRequest, ExpiringNotify, StatusUpdate, Terminated).

## Relationships

- **Depends on**: `baselib/protofsm` (FSM engine), `lib/tree` (tree paths).
- **Depended on by**: `round` (triggers forfeit requests), `oor` (incoming VTXOs), `db` (persistence), `darepod` (wiring).
- **Messages to/from**:
  - Receives `ForfeitRequest` ← `round` (during round participation).
  - Sends `ForfeitRequest` → `round` (initiating refresh).
  - Receives `BlockEpochEvent` ← `chainsource` (via Manager, for expiry monitoring).

## Invariants

- Forfeit transaction is not broadcast until the connector output's round confirms (atomic replacement).
- Refresh is auto-triggered at configurable height before expiry.
- Once ForfeitedState is reached, the old VTXO is unspendable; the new VTXO is available only after round confirmation.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
