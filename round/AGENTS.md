# round

## Purpose

Client-side Ark round participation FSM implementing boarding (on-chain to
off-chain), refresh (VTXO rollover), and leave (off-chain to on-chain exit)
protocols with MuSig2 signing ceremonies.

## Key Types

- `ClientState` — Sealed interface for all 14 FSM states (Idle through Confirmed/ClientFailed), including `ForfeitSignaturesCollectingState` and `RecoveryInitiatedState`.
- `ClientEvent` — Inbound events triggering transitions (CommitmentTxBuilt, NoncesAggregated, OperatorSigned, ForfeitSignatureResponse, ForfeitCollectionTimedOut, etc.).
- `ClientOutMsg` — Outbound messages (JoinRoundRequest, SubmitNoncesRequest, SubmitPartialSigRequest, SubmitVTXOForfeitSigsToServer, VTXOCreatedNotification).
- `ClientEnvironment` — FSM environment providing storage access (boarding intents, round checkpoints, VTXO store).
- `ClientWallet` — Interface for client wallet operations (embeds `input.Signer` for MuSig2 signing, adds `DeriveNextKey` for VTXO signing keys).
- `BoardingIntent` — Represents a funded on-chain input to include in a round.
- `Intents` — Pools of boarding, VTXO, forfeit, and leave requests accumulated before registration.
- `IntentPackage` — FSM event wrapping `Intents` for atomic delivery to the round FSM.
- `RegisterIntentRequest` — Actor message carrying a pre-composed `IntentPackage` from the wallet.
- `VTXOIntent` — Pre-registration VTXO request carrying `OwnerKey`, `OperatorKey`, `IsOwner` flag. For directed sends, `OwnerKey` is the recipient's key (distinct from the sender's `SigningKey`).
- `RoundVTXORequest` — Pairs a `VTXOIntent` with an ephemeral `SigningKey` derived at registration time for MuSig2 tree construction.
- `ForfeitSignaturesCollectingState` — State entered after VTXO tree signing when round includes refresh/leave VTXOs. Waits for all expected forfeit signatures before submitting to server.
- `ForfeitSignatureResponse` — Carries a VTXO's forfeit signature back from the VTXO actor.
- `ConnectorLeafInfo` — Maps a VTXO outpoint to its connector output index and leaf info for forfeit construction.

## Relationships

- **Depends on**: `baselib/protofsm` (FSM engine), `lib/tree` (Merkle trees), `lib/types` (shared domain types), `lib/scripts` (taproot scripts), `wallet` (types: `BoardingAddress`, `BoardingIntent`, `SelectedVTXO`).
- **Depended on by**: `vtxo` (forfeit coordination), `db` (round persistence), `darepod` (wiring).
- **Sends**:
  - → `serverconn`: `JoinRoundRequest`, `SubmitNoncesRequest`, `SubmitPartialSigRequest`, `SubmitVTXOForfeitSigsToServer`
  - → `vtxo`: `ForfeitRequestEvent`, `ForfeitConfirmedEvent`, `BlockEpochEvent`, `PendingForfeitEvent`, `SpendReserveEvent`, `SpendCompletedEvent`, `ForfeitReleasedEvent`
  - → `vtxo` manager: `VTXOCreatedNotification`
  - → `wallet`: `RegisterConfirmationNotifierRequest`
  - → `timeout`: `ScheduleTimeoutRequest`, `CancelTimeoutRequest`
- **Receives**:
  - ← `serverconn`: `CommitmentTxBuilt`, `NoncesAggregated`, `OperatorSigned`, `RoundJoined`, `BoardingFailed`
  - ← `vtxo`: `ForfeitSignatureResponse` (relayed through manager)
  - ← `wallet` (via `lib/actormsg`): `RegisterIntentMsg` (cooperative intent packages pre-admitted by manager), `TriggerBoardMsg` (VTXO registration + registration trigger)
  - ← `wallet`: `BoardingUtxoConfirmedEvent`
  - ← `timeout`: `TimeoutMsg`
  - ← `chainsource`: `ConfirmationEvent`

## Invariants

- Tree signatures must be validated BEFORE boarding input signatures are released (security checkpoint at InputSigSent).
- Forfeit signatures are collected AFTER VTXO tree signing is complete (ForfeitSignaturesCollectingState), ensuring clients only forfeit old VTXOs after verifying new VTXOs are properly signed.
- Round state is checkpointed atomically after tree validation; crash before checkpoint means client has no record of sent signatures.
- Primary FSM handles interactive phases (through InputSigSent); a dedicated FSM per round handles confirmation monitoring.
- The round actor does not mark VTXOs as PendingForfeit — the wallet/manager admits VTXOs before sending RegisterIntentMsg.
- `ClientWallet` provides MuSig2 signing and key derivation; boarding address creation is handled by the wallet actor (not the round FSM).
- Persisted VTXO ownership uses `OwnerKey` (not `SigningKey`). For directed sends the sender's signing key participates in MuSig2 tree construction, but the recipient's owner key determines VTXO ownership.

## Deep Docs

- [round/README.md](README.md) — Full state machine walkthrough with diagrams.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
