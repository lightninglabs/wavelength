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
- `Descriptor` — Complete VTXO metadata: `Outpoint`, `Amount`, `PkScript`, `OwnerKey` (keychain.KeyDescriptor), `OperatorKey`, `TapScript`, `TreePath`, `RoundID`, `CommitmentTxID`, `BatchExpiry`, `RelativeExpiry`, `TreeDepth`, `ChainDepth` (OOR hop count), `CreatedHeight`, `Status`.
- `Manager` — Actor managing per-VTXO FSM instances, lifecycle, and admission gating. Configured via `ManagerConfig`.
- `ManagerConfig` — Configuration holding Store, Wallet, ChainSource, ActorSystem, ExpiryConfig, RoundActor ref, ChainResolver ref, and optional `Log`.
- `VTXOEvent` — Inbound events (BlockEpochEvent, ForfeitRequest, ForfeitConfirmed, SpendReserveEvent, SpendCompletedEvent, etc.).
- `VTXOOutMsg` — Outbound messages (ForfeitRequest, ExpiringNotify, StatusUpdate, Terminated).
- `FilterOptions` / `FilterDescriptors` — VTXO filtering by expiry status, spend state, etc.
- `GetActiveVTXOCountRequest` / `GetActiveVTXOCountResponse` — Ask-message for querying active VTXO count from the manager.
- `ManagerMsg` / `ManagerResp` — Type aliases for `actormsg.VTXOManagerMsg` / `actormsg.VTXOManagerResp` (admission types live in `lib/actormsg` to avoid import cycles).

## Relationships

- **Depends on**: `baselib/protofsm` (FSM engine), `baselib/actor` (actor system), `lib/tree` (tree paths), `lib/actormsg` (admission message types), `chainsource` (block epochs).
- **Depended on by**: `round` (triggers forfeit requests), `oor` (incoming VTXOs), `wallet` (admission gating), `db` (persistence), `darepod` (wiring).
- **Sends**:
  - → `round` (via manager relay): `RelayToRoundMsg` wrapping `ForfeitSignatureSubmission`
  - → `db` (via outbox): `VTXOStatusUpdate`
  - → `vtxo` manager: `VTXOTerminatedNotification`, `RelayToRoundMsg`
- **Receives**:
  - ← `round`: `ForfeitRequestEvent`, `ForfeitConfirmedEvent`, `ForfeitSignedEvent`, `ForfeitReleasedEvent`, `BlockEpochEvent`, `PendingForfeitEvent`, `SpendReserveEvent`, `SpendReleasedEvent`, `SpendCompletedEvent`, `ResumeVTXOEvent`
  - ← `wallet` (via `lib/actormsg`): `SelectAndReserveSpendRequest`, `ReleaseSpendRequest`, `CompleteSpendRequest`, `ReserveForfeitRequest`, `ReleaseForfeitRequest`, `SelectAndReserveForfeitRequest`
  - ← `chainsource` (via Manager): `BlockEpochEvent`

## Invariants

- VTXO actor state is the single source of truth for availability.
- Forfeit transaction is not broadcast until the connector output's round confirms (atomic replacement).
- Refresh is auto-triggered at configurable height before expiry.
- Once ForfeitedState is reached, the old VTXO is unspendable; the new VTXO is available only after round confirmation.
- SpendingState is persisted as VTXOStatusSpending and survives restarts.
- OOR completion transitions VTXOs to SpentState through the VTXO actor FSM, not by direct store writes.
- A VTXO in SpendingState cannot be admitted for cooperative consumption, and vice versa.
- Admission types (`SelectAndReserveSpendRequest`, `SelectAndReserveForfeitRequest`, `ReserveForfeitRequest`, etc.) are defined in `lib/actormsg` and re-exported as type aliases to avoid wallet → vtxo → round → wallet import cycles.
- `selectAndReserveVTXOs` is a shared helper parameterized by `reserveParams` that serves both the OOR spend and cooperative forfeit coin selection paths, avoiding code duplication.
- Per-subsystem logging: `ManagerConfig.Log` provides an optional instance logger; falls back to `build.LoggerFromContext` (no global mutable loggers).

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
