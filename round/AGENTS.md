# round

## Purpose

Client-side Ark round participation FSM implementing boarding (on-chain to
off-chain), refresh (VTXO rollover), and leave (off-chain to on-chain exit)
protocols with MuSig2 signing ceremonies.

## Key Types

- `ClientState` — Sealed interface for all 15 FSM states (Idle through Confirmed/ClientFailed).
- `ClientEvent` — Inbound events triggering transitions (CommitmentTxBuilt, NoncesAggregated, OperatorSigned, etc.).
- `ClientOutMsg` — Outbound messages (JoinRoundRequest, SubmitNoncesRequest, SubmitPartialSigRequest, VTXOCreatedNotification).
- `ClientEnvironment` — FSM environment providing storage access (boarding intents, round checkpoints, VTXO store).
- `BoardingIntent` — Represents a funded on-chain input to include in a round.

## Relationships

- **Depends on**: `baselib/protofsm` (FSM engine), `lib/tree` (Merkle trees), `lib/types` (shared domain types), `lib/scripts` (taproot scripts).
- **Depended on by**: `vtxo` (forfeit coordination), `db` (round persistence), `darepod` (wiring).
- **Sends**:
  - → `serverconn`: `JoinRoundRequest`, `SubmitNoncesRequest`, `SubmitPartialSigRequest`, `SubmitForfeitSigRequest`, `SubmitVTXOForfeitSigsToServer`
  - → `vtxo`: `ForfeitRequestEvent`, `ForfeitConfirmedEvent`, `RefreshAcknowledgedEvent`, `BlockEpochEvent`
  - → `vtxo` manager: `VTXOCreatedNotification`
  - → `wallet`: `RegisterConfirmationNotifierRequest`
  - → `timeout`: `ScheduleTimeoutRequest`, `CancelTimeoutRequest`
- **Receives**:
  - ← `serverconn`: `CommitmentTxBuilt`, `NoncesAggregated`, `OperatorSigned`, `RoundJoined`, `BoardingFailed`
  - ← `vtxo`: `RefreshVTXORequest`, `LeaveVTXORequest`, `ForfeitSignatureSubmission`
  - ← `wallet`: `BoardingUtxoConfirmedEvent`
  - ← `timeout`: `TimeoutMsg`
  - ← `chainsource`: `ConfirmationEvent`

## Invariants

- Tree signatures must be validated BEFORE boarding input signatures are released (security checkpoint at InputSigSent).
- Round state is checkpointed atomically after tree validation; crash before checkpoint means client has no record of sent signatures.
- Primary FSM handles interactive phases (through InputSigSent); a dedicated FSM per round handles confirmation monitoring.
- Forfeit signatures must be collected BEFORE boarding inputs are signed (atomic replacement guarantee).

## Deep Docs

- [round/README.md](README.md) — Full state machine walkthrough with diagrams.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
