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
- `RefreshVTXORequest` — Per-VTXO refresh registration carrying `Amount`, `VTXO`, `SigningKey`, and `OperatorFee int64`. The `OperatorFee` is quoted by the VTXO actor's `RefreshFeeQuoter` before emission; `buildVTXORequestFromRefresh` subtracts it from the new VTXO output amount and clamps to zero so a buggy quoter cannot produce a negative output.
- `VTXOIntent` — Pre-registration VTXO request carrying `OwnerKey`, `OperatorKey`. For directed sends, `OwnerKey` is the recipient's key (distinct from the sender's `SigningKey`). Ownership is determined at confirmation time via `OwnedScriptChecker` — there is no `IsOwner` flag on the wire or in local state.
- `RoundVTXORequest` — Pairs a `VTXOIntent` with an ephemeral `SigningKey` derived at registration time for MuSig2 tree construction.
- `OwnedScriptChecker` — Interface that answers "does this pkScript belong to the local wallet?" The `InputSigSent → Confirmed` transition calls this for every VTXO in the round to decide which entries `buildOwnedClientVTXOs` persists as spendable local balance. Backed in production by the OOR artifact store (owned receive scripts table).
- `OwnedScriptRegistrar` — Interface used by the round actor when building VTXO intents (refresh change, boarding change, directed-send change outputs) to persist the pkScript + owner key before the round registers. This ensures the `OwnedScriptChecker` recognizes the script when the round confirms. `handleRegisterIntent` also registers any VTXO in an incoming `RegisterIntentMsg` whose `OwnerKey.KeyLocator` is non-zero (remote recipient keys are left with a zero locator and skipped).
- `ForfeitSignaturesCollectingState` — State entered after VTXO tree signing when round includes refresh/leave VTXOs. Waits for all expected forfeit signatures before submitting to server.
- `ForfeitSignatureResponse` — Carries a VTXO's forfeit signature back from the VTXO actor.
- `ConnectorLeafInfo` — Maps a VTXO outpoint to its connector output index and leaf info for forfeit construction.
- `RoundClientConfig.LedgerSink` — Optional `fn.Option[ledger.Sink]` plumbed onto the round actor so `VTXOCreatedNotification` dispatch can fire-and-forget ledger messages. Gated on `fn.Some`; unit tests that do not register a ledger actor pass `fn.None`.
- `emitVTXOsReceived(ctx, n)` — Origin-routed emission invoked on `VTXOCreatedNotification` dispatch. Per owned VTXO it calls `emitOwnedVTXOLedgerEntry`, which switches on `ClientVTXO.Origin` (set by the wallet at intent composition): `RoundBoarding` → `VTXOReceivedMsg{Source=SourceRoundBoarding}`; `RoundRefresh` → paired `VTXOSentMsg{Outpoint}` + `VTXOReceivedMsg{Source=SourceRoundRefresh}` so the two legs cancel on `transfers_out`; `RoundTransfer` → `VTXOReceivedMsg{Source=SourceRoundTransfer}`; `Unknown` is a silent no-op (strictly safer than a default that would corrupt the chart of accounts). After the per-VTXO loop, `emitRoundFee` appends a single `FeePaidMsg{FeeType=FeeTypeRefresh}` when `OperatorFeeSat > 0` and at least one refresh-origin VTXO was present (boarding-fee emission deferred).
- `computeClientOperatorFee(intents, ownedVTXOs) int64` — Transition-side helper that derives the per-client operator fee as Σ(boarding input amounts) + Σ(forfeited VTXO amounts) − Σ(owned output VTXO amounts) − Σ(cooperative leave output values). Clamps to zero. Called inside the `InputSigSent → Confirmed` transition; the result is carried on `VTXOCreatedNotification.OperatorFeeSat` for the actor's emission path to read.

## Relationships

- **Depends on**: `baselib/protofsm` (FSM engine), `lib/tree` (Merkle trees), `lib/types` (shared domain types), `lib/arkscript` (policy-backed tapscript construction), `wallet` (types: `BoardingAddress`, `BoardingIntent`, `SelectedVTXO`), `ledger` (`Sink` + `VTXOReceivedMsg` / `Source*` constants), `google/uuid` (round ID parsing for ledger emission).
- **Depended on by**: `vtxo` (forfeit coordination), `db` (round persistence), `darepod` (wiring, owned-script adapters).
- **Sends**:
  - → `serverconn`: `JoinRoundRequest`, `SubmitNoncesRequest`, `SubmitPartialSigRequest`, `SubmitVTXOForfeitSigsToServer`
  - → `vtxo`: `ForfeitRequestEvent`, `ForfeitConfirmedEvent`, `BlockEpochEvent`, `PendingForfeitEvent`, `SpendReserveEvent`, `SpendCompletedEvent`, `ForfeitReleasedEvent`
  - → `vtxo` manager: `VTXOCreatedNotification`
  - → `wallet`: `RegisterConfirmationNotifierRequest`
  - → `timeout`: `ScheduleTimeoutRequest`, `CancelTimeoutRequest`
  - → `OwnedScriptRegistrar` (darepod adapter over OOR artifact store): `RegisterOwnedScript(pkScript, ownerKey)`
  - → `ledger` actor (via `ledger.Sink` Tell, when `fn.Some`), origin-routed per owned `ClientVTXO`:
    `VTXOReceivedMsg{Source=SourceRoundBoarding}` for boarding-origin VTXOs;
    paired `VTXOSentMsg{Outpoint}` + `VTXOReceivedMsg{Source=SourceRoundRefresh}` for refresh-origin VTXOs (legs cancel on transfers_out);
    `VTXOReceivedMsg{Source=SourceRoundTransfer}` for participant-transfer-origin VTXOs;
    one `FeePaidMsg{FeeType=FeeTypeRefresh}` per round when `OperatorFeeSat > 0` and any refresh-origin VTXO was emitted (boarding-fee emission deferred).
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
- After aggregated signatures are validated on `VTXOTreePaths`, they are
  propagated to extracted `ClientTrees` via `SubmitTreeSigs` + `VerifySigned`.
  This ensures persisted client trees contain valid signatures for unilateral
  exit (unrolling).
- Round state is checkpointed atomically after tree validation; crash before checkpoint means client has no record of sent signatures.
- Primary FSM handles interactive phases (through InputSigSent); a dedicated FSM per round handles confirmation monitoring.
- The round actor does not mark VTXOs as PendingForfeit — the wallet/manager admits VTXOs before sending RegisterIntentMsg.
- `ClientWallet` provides MuSig2 signing and key derivation; boarding address creation is handled by the wallet actor (not the round FSM).
- Persisted VTXO ownership uses `OwnerKey` (not `SigningKey`). For directed sends the sender's signing key participates in MuSig2 tree construction, but the recipient's owner key determines VTXO ownership.
- Local-balance persistence on confirmation is driven by `OwnedScriptChecker.IsOwnedScript(pkScript)`, not by any per-intent boolean. `buildOwnedClientVTXOs` skips any VTXO whose pkScript the checker does not recognize; the client still co-signs its tree path, so foreign recipients in a directed send still get a valid unroll proof. When the checker is nil (tests), every VTXO is treated as owned.
- VTXO pkScripts are registered with `OwnedScriptRegistrar` at intent-build time for change/refresh outputs, and inside `handleRegisterIntent` for any `RegisterIntentMsg` entry with a non-zero `KeyLocator`. Remote recipient keys in directed sends carry a zero `KeyLocator` and are intentionally left unregistered.
- Each client sub-tree in the commitment tree must contain exactly one non-anchor leaf. `buildOwnedClientVTXOs` fails the transition if a signing-key sub-tree yields anything other than one leaf.

## Deep Docs

- [round/README.md](README.md) — Full state machine walkthrough with diagrams.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
