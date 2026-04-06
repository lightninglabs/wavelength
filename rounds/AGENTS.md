# rounds

## Purpose

Server-side Ark round lifecycle FSM coordinating client registration, batch
transaction building, MuSig2 signing ceremonies, finalization, and on-chain
confirmation monitoring.

## Key Types

- `Actor` — Durable round FSM driver, processes messages and persists state.
- `RoundID` — UUID-based round identifier.
- `State` — Sealed interface for all FSM states (Created through Confirmed/Failed).
- `Event` — Inbound events triggering state transitions (ClientJoinRequest, BuildBatchTx, etc.).
- `OutboxEvent` — Outbound side effects (ClientSuccessResp, BroadcastRoundReq, RoundSealedReq, etc.).
- `RoundSealedReq` — Outbox event emitted by `SealEvent` handler when registration closes. Signals the actor to spawn a new round for the next registration window.
- `ActorMsg` — Messages sent to the round actor (JoinRoundRequest, nonces, sigs).
- `JoinRoundRequestFromProto`, `NoncesFromProto`, `PartialSigsFromProto`, etc. — Exported proto→domain conversion helpers in `proto_convert.go`, called from `server_rounds.go` `AddEnvelopeRoute` Adapt closures.
- `BoardingInputLocker` — Interface for locking boarding inputs to prevent double-spending across concurrent rounds. Implemented by `inMemoryBoardingLocker` in the root package.
- `Environment.HeaderVerifier` — `proof.HeaderVerifier` for TxProof SPV validation when no `ChainSource` is available. Wired from `lndbackend.NewLndHeaderVerifier`.
- `SealEvent` — Canonical internal event that transitions `RegistrationState` -> `BatchBuildingState` and emits `RoundSealedReq`. Fired by registration timeout, seal predicate, or admin `TriggerBatch` RPC. Single emission point prevents duplicate round creation.
- `SealPredicate` — Pure function `func(regs) bool` evaluated after each client join to decide if the round should seal early (before registration timeout). Defined in `seal_policy.go`. When a predicate fires, it emits `SealEvent`.
- `MaxClients(limit)` — Seal predicate that fires when `len(regs) >= limit`.
- `MaxOutputAmount(threshold)` — Seal predicate that fires when total output value reaches a satoshi threshold.
- `AnySealPredicate(preds...)` — Composite predicate returning true when any sub-predicate fires (logical OR).

## Relationships

- **Depends on**: `batch` (tx building, MuSig2 coordination), `batchwatcher`
  (confirmation monitoring), `clientconn` (outbound events to clients),
  `vtxo` (VTXO locking during rounds), `metrics` (round lifecycle
  instrumentation). Interaction with `batchsweeper` is indirect:
  `rounds` registers batches with `batchwatcher`, which in turn notifies
  `batchsweeper`.
- **Depended on by**: `indexer` (round event subscription), `lndbackend` (chain queries), root `darepo` (wiring).
- **Messages to/from**:
  - Receives JoinRoundRequest, nonces, partial sigs <- `clientconn` via `AddEnvelopeRoute` (fire-and-forget Tell from clients).
  - Sends round events, commitment tx, aggregated nonces -> `clientconn` (to clients via bridge egress).
  - Sends batch build requests -> `batch`.
  - Receives confirmation events <- `batchwatcher`.
  - Emits `RoundSealedReq` from `SealEvent` handler -> actor (triggers new round creation).
  - Proto→domain conversion helpers exported in `proto_convert.go` for use by server wiring layer (`server_rounds.go`).

## Invariants

- Tree signatures must be validated BEFORE input signatures are released.
- VTXO locks must be acquired before batch building and released on failure.
- Round state is checkpointed atomically; crash before checkpoint means no partial state persists.
- Boarding input signatures are only broadcast after all forfeit signatures are collected.
- TxProof validation (when no ChainSource) requires a non-nil `HeaderVerifier` and enforces `MinBoardingConfirmations` and `BoardingExitDelaySafetyMargin` checks matching the ChainSource path.
- `ValidateBoardingRequest` takes a `currentHeight` parameter for confirmation depth checks in both ChainSource and TxProof paths.
- Seal predicates are pure functions — they must not perform I/O or modify state. They are evaluated inside FSM transitions after each successful join.
- Side effects (batch building, signing, persistence) are inlined in FSM transition functions, not dispatched through a separate handler.
- Single-client refresh settlement: when only one client participates in a refresh round, the settlement path must still produce valid outputs and not skip signing.
- `RoundSealedReq` is emitted from a single canonical location (`SealEvent` handler in `RegistrationState`). No other code path emits this message.
- `ConnectorDustAmount` must be > 0 in round terms (default: 330 sats). Wired from config -> `batch.Terms`. Zero value causes refresh commitment assembly to fail (invalid connector leaf outputs).
- Round lifecycle is instrumented via metrics actor: `RoundCreatedMsg`,
  `ClientJoinedRoundMsg`, `RoundSealedMsg`, `PhaseStartedMsg`/`PhaseEndedMsg`,
  `RoundCompletedMsg`.
- Aggregated MuSig2 sigs are persisted on server VTXOTrees so they survive
  restarts and support batch sweep transactions.
- Swept batches transition VTXOs to Expired status via `batchsweeper` -> `db`.

## Deep Docs

- [rounds/README.md](README.md) — Full state machine walkthrough with diagrams.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
