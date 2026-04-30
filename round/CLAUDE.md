# round

## Purpose

Client-side Ark round participation FSM implementing boarding (on-chain to
off-chain), refresh (VTXO rollover), and leave (off-chain to on-chain exit)
protocols with MuSig2 signing ceremonies.

## Key Types

### FSM States (`states.go`)

- `ClientState` — Sealed interface for all 15 FSM states (Idle through
  Confirmed/ClientFailed). Concrete states: `Idle`,
  `PendingRoundAssembly`, `IntentSentState`, `QuoteReceivedState`,
  `RoundJoinedState`, `CommitmentTxReceivedState`,
  `CommitmentTxValidatedState`, `NoncesSentState`,
  `NoncesAggregatedState`, `PartialSigsSentState`,
  `ForfeitSignaturesCollectingState`, `InputSigSentState`,
  `ConfirmedState`, `ClientFailedState`, `RecoveryInitiatedState`.
- `IntentSentState` — State entered after `IntentRequested` (out of
  `PendingRoundAssembly`). Holds the client's `Intents` and an
  `AdmittedRoundID RoundID` field that is zero until the server's
  `RoundJoined` admission ack lands. `RoundJoined` is consumed as a
  watermark only — the actor layer re-keys the FSM from the ephemeral
  temp key to the server-assigned `RoundID`, but the FSM stays parked
  in `IntentSentState` (with `AdmittedRoundID` populated) until the
  seal-time `JoinRoundQuoteReceived` arrives.
- `QuoteReceivedState` — Entered on `JoinRoundQuoteReceived` between
  `IntentSentState` and `RoundJoinedState`. Carries `RoundID`, the
  `*ClientQuote`, and the cloned `Intents`. `evaluateQuote` compares
  `OperatorFeeSat` against `env.MaxOperatorFee`, validates echoed
  per-output amounts/pkScripts/recipient keys, and rejects expired
  quotes. On accept emits `JoinRoundAcceptOutbox`; on reject emits
  `JoinRoundRejectOutbox` and transitions to `ClientFailedState`. A
  `JoinRoundQuoteReceived` with a strictly higher `SealPass` replaces
  the in-state quote and re-evaluates; lower-or-equal deliveries
  self-loop as stale.
- `RoundJoinedState` — Entered after `QuoteAccepted`. Carries
  `RoundID`, the cloned `Intents` (with leave amounts captured on
  `Intents.QuotedLeaveAmounts`), and the accepted `*ClientQuote`.
  Awaits `CommitmentTxBuilt`. A `JoinRoundQuoteReceived` with a
  strictly higher `SealPass` walks the FSM back to
  `QuoteReceivedState` for re-evaluation.
- `ForfeitSignaturesCollectingState` — Entered after VTXO tree signing
  when the round includes refresh/leave VTXOs. Fields:
  `ExpectedForfeits map[wire.OutPoint]*ConnectorLeafInfo` and
  `CollectedForfeits map[wire.OutPoint]*ForfeitSignatureResponse`.
  Waits until all expected forfeit signatures are collected, then
  submits to server.
- `ClientQuote` — Client-side view of `roundpb.JoinRoundQuote`.
  Carries `QuoteID [32]byte`, `SealPass uint32`, `OperatorFeeSat
  int64`, positional `VTXOQuotes []VTXOQuoteEntry` / `LeaveQuotes
  []LeaveQuoteEntry` (server-decided amounts plus echoed pkScripts and
  recipient keys), `QuoteExpiresAt int64`, and `RejectReason
  roundpb.QuoteReason`. Stored on `QuoteReceivedState` and threaded
  forward through `RoundJoinedState` and `CommitmentTxReceivedState`.
- `VTXOQuoteEntry` — Per-VTXO positional entry in a `ClientQuote`.
  Fields: `PkScript []byte`, `AmountSat int64`, `RecipientKey []byte`.
  `evaluateQuote` cross-checks all three against the intent's
  positional `VTXORequest`.
- `LeaveQuoteEntry` — Per-leave positional entry in a `ClientQuote`.
  Fields: `PkScript []byte`, `AmountSat int64`. Echoed pkScript lets
  `evaluateQuote` cross-check positional agreement with intent's
  `LeaveRequests`.

### Events (`events.go`)

- `ClientEvent` — Sealed interface for all FSM inbound events.
- `JoinRoundQuoteReceived` — Carries `RoundID` and `*ClientQuote`.
  Transitions the FSM from `IntentSentState` to `QuoteReceivedState`.
  Also accepted in `QuoteReceivedState` (reseal, higher `SealPass`)
  and `RoundJoinedState` (reseal-after-accept, walks back to
  `QuoteReceivedState`).
- `QuoteAccepted` — Internal event fired by `QuoteReceivedState` after
  fee-cap check passes. Carries `RoundID` and `QuoteID [32]byte`.
- `QuoteRejected` — Internal event fired by `QuoteReceivedState` when
  fee cap is exceeded or server reject reason is non-OK. Carries
  `RoundID`, `QuoteID [32]byte`, and `Reason string`.
- `ForfeitCollectionTimedOut` — Emitted by the round actor when the
  forfeit signature collection window expires. Carries `RoundID`.
- `ForfeitSignatureResponse` — Carries a VTXO's forfeit signature back
  from the VTXO actor. Fields: `VTXOOutpoint`, `RoundID string`,
  `ForfeitTx *wire.MsgTx`, `Signature *schnorr.Signature`, `SpendPath
  *arkscript.SpendPath`.
- `ConnectorLeafInfo` — Maps a VTXO outpoint to its connector info for
  forfeit construction. Fields: `LeafIndex int` (not populated by
  `FromProto`), `ConnectorOutpoint wire.OutPoint`,
  `ConnectorPkScript []byte`, `ConnectorAmount int64`, `VTXOAmount
  btcutil.Amount`. `VTXOAmount` enables validation that the forfeit
  penalty output equals the correct forfeited amount.
- `IntentPackage` — Single FSM event embedding `Intents` for atomic
  delivery. Covers all pooled intent types (boarding, VTXO, forfeit,
  leave). `isEmpty()` guards against sending empty packages.

### Outbox Messages (`outbox_messages.go`)

- `ClientOutMsg` — Sealed interface for all FSM outbox messages.
- `JoinRoundAcceptOutbox` — Emitted after quote fee-cap check passes.
  Carries `RoundID` and `QuoteID [32]byte`; routed to
  `MethodAcceptQuote`. `ToProto()` encodes to `roundpb.JoinRoundAccept`.
- `JoinRoundRejectOutbox` — Emitted when client refuses the server's
  quote. Carries `RoundID`, `QuoteID [32]byte`, `Reason string`;
  routed to `MethodRejectQuote`. `ToProto()` encodes to
  `roundpb.JoinRoundReject`.
- `SubmitForfeitSigRequest` — Carries `RoundID` and `Signatures
  []*types.BoardingInputSignature`; routed to
  `MethodSubmitForfeitSigs`. `ToProto()` converts via
  `roundpb.BoardingInputSigToProto`.
- `StartTimeoutReq` — Asks the actor to schedule a timeout for a
  round phase. Fields: `RoundID`, `Phase TimeoutPhase`, `Duration
  time.Duration`.
- `CancelTimeoutReq` — Asks the actor to cancel a previously
  scheduled timeout. Fields: `RoundID`, `Phase TimeoutPhase`.
- `RoundCheckpointedNotification` — Emitted by the primary FSM when it
  reaches `InputSigSentState`, signalling the actor to migrate this
  round to a dedicated round FSM. Carries `RoundID`.
- `RoundCompletedNotification` — Emitted when an FSM reaches
  `ConfirmedState`. Carries `RoundID`, `TxID`, and `ConfInfo`. Signals
  actor to perform cleanup (remove from `activeRounds`, finalize
  storage).
- `RoundFailedNotification` — Emitted when an FSM transitions to
  `ClientFailedState`. Carries `RoundID fn.Option[RoundID]`, `Reason
  string`, `Recoverable bool`, `OriginalError error`.
- `ForfeitRequestToVTXO` — Emitted by the FSM to ask a VTXO actor to
  sign its forfeit tx. Fields: `VTXOOutpoint`, `RoundID string`,
  `ConnectorOutpoint`, `ConnectorPkScript`, `ConnectorAmount int64`,
  `ServerForfeitPkScript []byte`, `ForfeitSpend *arkscript.SpendPath`
  (overrides default collaborative leaf for custom-policy VTXOs).
- `ForfeitConfirmedToVTXO` — Emitted on commitment tx confirmation to
  signal old VTXO actors they are permanently forfeited. Fields:
  `VTXOOutpoint`, `CommitmentTxID`, `BlockHeight int32`.

### Interfaces (`interfaces.go`)

- `ClientEnvironment` — FSM environment providing storage access
  (boarding intents, round checkpoints, VTXO store).
- `ClientWallet` — Interface for client wallet operations (embeds
  `input.Signer` for MuSig2 signing, adds `DeriveNextKey` for VTXO
  signing keys).
- `OwnedScriptChecker` — Interface that answers "does this pkScript
  belong to the local wallet?" `IsOwnedScript(ctx, pkScript) →
  fn.Result[bool]`. The `InputSigSent → Confirmed` transition calls
  this for every VTXO in the round to decide which entries
  `buildOwnedClientVTXOs` persists. Backed in production by the OOR
  artifact store. When nil (tests), every VTXO is treated as owned.
- `OwnedScriptRegistrar` — `RegisterOwnedScript(ctx, pkScript,
  ownerKey)` called at intent-build time for change/refresh outputs
  and inside `handleRegisterIntent` for entries with a non-zero
  `KeyLocator`. Remote recipient keys carry a zero `KeyLocator` and
  are skipped.
- `VTXOStore` — Persistence for off-chain balance. Methods:
  `SaveVTXOs`, `ListVTXOs`, `GetVTXO`, `MarkVTXOSpent`.
- `RoundStore` — Persistence for round FSM state. Methods:
  `CommitState`, `FetchState`, `LookupRoundByCommitmentTx`,
  `ListActiveRounds`, `FinalizeRound`.

### Key Data Types (`interfaces.go`)

- `Intents` — Pools of boarding, VTXO, forfeit, and leave requests.
  Field `QuotedLeaveAmounts []int64` holds server-authoritative leave
  output amounts captured at `QuoteAccepted` time. `LeaveAmount(idx)`
  returns the authoritative value, falling back to intent target when
  no quote was accepted.
- `VTXOIntent` — Pre-registration VTXO request carrying `Amount`,
  `PolicyTemplate`, `PkScript`, `Expiry`, `OwnerKey`, `OperatorKey`.
  For directed sends, `OwnerKey` is the recipient's key.
- `RoundVTXORequest` — Pairs a `VTXOIntent` with an ephemeral
  `SigningKey keychain.KeyDescriptor` for MuSig2 tree construction.
  `ToVTXORequest()` converts to `types.VTXORequest`.
- `ClientVTXO` — Full VTXO descriptor owned by the client. Adds
  `Origin types.VTXOOrigin`, `CommitmentTxID`, `BatchExpiry int32`,
  and `CreatedHeight int32` on top of the basic VTXO fields.
- `BoardingIntent` — Embeds `wallet.BoardingIntent` plus a
  `Request types.BoardingRequest`.

### Actor-level Types (`actor_messages.go`, `actor.go`)

- `ClientMsg` — Marker interface for messages receivable by
  `RoundClientActor` (embeds `actormsg.RoundReceivable`).
- `ClientResp` — Sealed interface for responses from
  `RoundClientActor` (embeds `actormsg.RoundActorResp`).
- `ServerMsg` / `ServerResp` — Sealed interfaces for round server
  actor messages (stubs; server actor types TBD).
- `WalletBoardingConfirmed` — Wraps `wallet.BoardingIntent` to make
  boarding UTXO confirmations compatible with the `ClientMsg`
  interface.
- `ServerMessageNotification` — Delivers a `ClientEvent` from the
  server FSM outbox to the round actor.
- `ServerMessageResponse` — Acknowledges receipt of a server message.
- `GetClientStateRequest` / `GetClientStateResponse` — Query the
  current state of all FSMs. `GetClientStateResponse.States` maps FSM
  key strings to `FSMStateInfo{State, IsTemp, RoundID}`.
- `FSMStateInfo` — Bundles `State ClientState`, `IsTemp bool`, and
  `RoundID` for introspection without exposing the full FSM.
- `CancelRoundRequest` / `CancelRoundResponse` — Cancel participation
  in a round identified by `RoundKey fn.Option[RoundKeyStr]`.
- `RegisterVTXORequestsRequest` / `RegisterVTXORequestsResponse` —
  Inform the FSM of VTXO amounts to include in the next round.
- `ConfirmationEvent` — Wraps a chain confirmation from `ChainSource`.
  Fields: `Txid`, `BlockHeight int32`, `BlockHash`, `Confirmations
  uint32`, `Tx *wire.MsgTx`.
- `TimeoutMsg` — Sent to the round actor when a timeout fires. Carries
  `TimeoutID timeout.ID`.
- `RegisterIntentRequest` — Actor message carrying a pre-composed
  `IntentPackage` from the wallet.
- `RefreshVTXORequest` — Per-VTXO refresh registration carrying
  `Amount`, `VTXO`, `SigningKey`, and `OperatorFee int64`. The
  `OperatorFee` is quoted by the VTXO actor's `RefreshFeeQuoter` before
  emission; `buildVTXORequestFromRefresh` subtracts it from the new
  VTXO output amount and clamps to zero so a buggy quoter cannot
  produce a negative output.

### VTXO Actor Messages (`vtxo_messages.go`)

- `VTXOActorMsg` / `VTXOManagerMsg` — Marker interfaces for messages
  to VTXO actors and the VTXO manager.
- `BlockEpochEvent` — New block notification. Fields: `Height int32`,
  `Hash`, `Timestamp int64`.
- `ForfeitRequestEvent` — Asks a VTXO actor to sign its forfeit tx.
  Fields mirror `ForfeitRequestToVTXO` plus `ForfeitSpend
  *arkscript.SpendPath`.
- `ForfeitConfirmedEvent` — Commitment tx confirmed, forfeit final.
  Fields: `CommitmentTxID`, `BlockHeight int32`.
- `SpendReserveEvent` — Claims a VTXO for an OOR spend (LiveState
  only).
- `SpendReleasedEvent` — Releases a VTXO from spend reservation back
  to LiveState.
- `SpendCompletedEvent` — Marks a VTXO as fully spent via OOR tx.
- `ForfeitReleasedEvent` — Releases a VTXO from pending forfeit back
  to LiveState when round registration fails.
- `ForfeitSignedEvent` — Internal: forfeit tx signed and submitted.
  Carries `ForfeitTxID`.
- `VTXOFailedEvent` — Error during VTXO processing. Fields: `Reason`,
  `Error`, `Recoverable bool`.
- `ResumeVTXOEvent` — Sent during startup to restore a VTXO actor
  from persisted state.
- `PendingForfeitEvent` — Transitions a VTXO to `PendingForfeitState`
  when the round actor admits it for cooperative consumption.
- `VTXOTerminatedMsg` — Notifies the VTXO manager that a VTXO actor
  has reached a terminal state. Fields: `Outpoint`, `FinalState
  string`, `Reason string`.

### Timeout Types (`fsm_timeouts.go`)

- `TimeoutPhase string` — Identifies which FSM phase owns a timeout.
  Only defined value: `TimeoutPhaseForfeitCollection =
  "forfeit-collection"`.

### Proto Deserialization (`from_proto.go`)

- `MaxQuoteEntriesPerClient = 1024` — Bounds the per-quote VTXO/leave
  entry slices decoded from the server to cheaply reject malformed or
  malicious envelopes before allocating large backing slices.
- `FromProto` methods on `JoinRoundQuoteReceived`, `RoundJoined`,
  `CommitmentTxBuilt`, `AwaitingBoardingSigs`, `NoncesAggregated`,
  `OperatorSigned`, `BoardingFailed`, and `JoinRoundRequest`. All
  implement the private `inboundServerMessage` interface checked by
  compile-time assertions.

### Ledger Emission

- `RoundClientConfig.LedgerSink` — Optional `fn.Option[ledger.Sink]`
  plumbed onto the round actor so `VTXOCreatedNotification` dispatch
  can fire-and-forget ledger messages. Gated on `fn.Some`; unit tests
  that do not register a ledger actor pass `fn.None`.
- `emitVTXOsReceived(ctx, n)` — Origin-routed emission. Per owned
  VTXO switches on `ClientVTXO.Origin`: `RoundBoarding` →
  `VTXOReceivedMsg{Source=SourceRoundBoarding}`; `RoundRefresh` →
  paired `VTXOSentMsg{Outpoint}` + `VTXOReceivedMsg{Source=
  SourceRoundRefresh}`; `RoundTransfer` →
  `VTXOReceivedMsg{Source=SourceRoundTransfer}`; `Unknown` → silent
  no-op. After the per-VTXO loop, `emitRoundFee` appends one
  `FeePaidMsg{FeeType=FeeTypeRefresh}` when `OperatorFeeSat > 0` and
  at least one refresh-origin VTXO was emitted.
- `computeClientOperatorFee(intents, ownedVTXOs) int64` —
  Σ(boarding inputs) + Σ(forfeited VTXOs) − Σ(owned output VTXOs) −
  Σ(cooperative leave outputs). Clamps to zero. Called inside the
  `InputSigSent → Confirmed` transition; result is carried on
  `VTXOCreatedNotification.OperatorFeeSat`.

## Relationships

- **Depends on**: `baselib/protofsm` (FSM engine), `lib/tree` (Merkle
  trees), `lib/types` (shared domain types), `lib/arkscript`
  (policy-backed tapscript construction), `wallet` (types:
  `BoardingAddress`, `BoardingIntent`), `ledger` (`Sink` +
  `VTXOReceivedMsg` / `Source*` constants), `timeout` (timeout
  scheduling), `google/uuid` (round ID parsing).
- **Depended on by**: `vtxo` (forfeit coordination), `db` (round
  persistence), `darepod` (wiring, owned-script adapters).
- **Sends**:
  - → `serverconn`: `JoinRoundRequest`, `JoinRoundAcceptOutbox`,
    `JoinRoundRejectOutbox`, `SubmitNoncesRequest`,
    `SubmitPartialSigRequest`, `SubmitForfeitSigRequest`,
    `SubmitVTXOForfeitSigsToServer`
  - → `vtxo` actors: `ForfeitRequestToVTXO`, `ForfeitConfirmedToVTXO`,
    `ForfeitRequestEvent`, `ForfeitConfirmedEvent`, `BlockEpochEvent`,
    `PendingForfeitEvent`, `SpendReserveEvent`, `SpendCompletedEvent`,
    `ForfeitReleasedEvent`, `SpendReleasedEvent`
  - → `vtxo` manager: `VTXOCreatedNotification`, `VTXOTerminatedMsg`
  - → `wallet`: `RegisterConfirmationRequest`
  - → `timeout` (via `StartTimeoutReq` / `CancelTimeoutReq`)
  - → `OwnedScriptRegistrar` (darepod adapter over OOR artifact store):
    `RegisterOwnedScript(pkScript, ownerKey)`
  - → `ledger` actor (via `ledger.Sink` Tell, when `fn.Some`),
    origin-routed per owned `ClientVTXO`:
    `VTXOReceivedMsg{Source=SourceRoundBoarding}` for boarding-origin;
    paired `VTXOSentMsg{Outpoint}` +
    `VTXOReceivedMsg{Source=SourceRoundRefresh}` for refresh-origin
    (legs cancel on transfers_out);
    `VTXOReceivedMsg{Source=SourceRoundTransfer}` for transfer-origin;
    one `FeePaidMsg{FeeType=FeeTypeRefresh}` per round when
    `OperatorFeeSat > 0` and any refresh-origin VTXO was emitted.
- **Receives**:
  - ← `serverconn`: `CommitmentTxBuilt`, `NoncesAggregated`,
    `OperatorSigned`, `RoundJoined`, `BoardingFailed`,
    `JoinRoundQuoteReceived` (via `ServerMessageNotification`)
  - ← `vtxo`: `ForfeitSignatureResponse` (relayed through manager)
  - ← `wallet` (via `lib/actormsg`): `RegisterIntentMsg` (cooperative
    intent packages pre-admitted by manager), `TriggerBoardMsg` (VTXO
    registration + registration trigger)
  - ← `wallet` (via `WalletBoardingConfirmed`): boarding UTXO
    confirmations
  - ← `timeout`: `TimeoutMsg`
  - ← `chainsource`: `ConfirmationEvent`

## Invariants

- Tree signatures must be validated BEFORE boarding input signatures
  are released (security checkpoint at `InputSigSent`).
- Forfeit signatures are collected AFTER VTXO tree signing is complete
  (`ForfeitSignaturesCollectingState`), ensuring clients only forfeit
  old VTXOs after verifying new VTXOs are properly signed.
- After aggregated signatures are validated on `VTXOTreePaths`, they
  are propagated to extracted `ClientTrees` via `SubmitTreeSigs` +
  `VerifySigned`. This ensures persisted client trees contain valid
  signatures for unilateral exit (unrolling).
- Round state is checkpointed atomically after tree validation; crash
  before checkpoint means client has no record of sent signatures.
- Primary FSM handles interactive phases (through `InputSigSent`); a
  dedicated FSM per round handles confirmation monitoring.
- The round actor does not mark VTXOs as `PendingForfeit` — the
  wallet/manager admits VTXOs before sending `RegisterIntentMsg`.
- `ClientWallet` provides MuSig2 signing and key derivation; boarding
  address creation is handled by the wallet actor (not the round FSM).
- Persisted VTXO ownership uses `OwnerKey` (not `SigningKey`). For
  directed sends the sender's signing key participates in MuSig2 tree
  construction, but the recipient's owner key determines VTXO
  ownership.
- Local-balance persistence on confirmation is driven by
  `OwnedScriptChecker.IsOwnedScript(pkScript)`, not by any per-intent
  boolean. `buildOwnedClientVTXOs` skips any VTXO whose pkScript the
  checker does not recognize; the client still co-signs its tree path,
  so foreign recipients in a directed send still get a valid unroll
  proof. When the checker is nil (tests), every VTXO is treated as
  owned.
- VTXO pkScripts are registered with `OwnedScriptRegistrar` at
  intent-build time for change/refresh outputs, and inside
  `handleRegisterIntent` for any `RegisterIntentMsg` entry with a
  non-zero `KeyLocator`. Remote recipient keys in directed sends carry
  a zero `KeyLocator` and are intentionally left unregistered.
- Each client sub-tree in the commitment tree must contain exactly one
  non-anchor leaf. `buildOwnedClientVTXOs` fails the transition if a
  signing-key sub-tree yields anything other than one leaf.
- Seal-time fee handshake (#270): the server is the amount authority.
  When `QuoteReceivedState.Quote` is non-nil, it threads through
  `RoundJoinedState` → `CommitmentTxReceivedState`, and
  `CommitmentTxReceivedState` validates each VTXO leaf and leave
  output against the quote's positional amount (not the intent
  target). `env.MaxOperatorFee` is applied at `QuoteReceivedState` —
  each seal pass re-evaluates the cap independently. Quote-less
  harness paths fall back to intent targets so pre-#270 FSM tests
  keep working.
- RoundID identity is asserted at every server-pushed event that
  carries one. `IntentSentState` records the admitted `RoundID` from
  the `RoundJoined` ack onto `AdmittedRoundID` and cross-checks
  `JoinRoundQuoteReceived.RoundID` against it; `RoundJoinedState`
  cross-checks both `CommitmentTxBuilt.RoundID` and any
  reseal-after-accept `JoinRoundQuoteReceived.RoundID` against its
  own `RoundID`. The actor's routing map is keyed by the same RoundID,
  so under normal operation these checks agree by construction; the
  FSM-level assertion is defense-in-depth against future routing
  regressions.
- `ConnectorLeafInfo.VTXOAmount` is populated from local VTXO state
  (not from the server's proto), ensuring the forfeit penalty output
  equals the canonical local value rather than a server-supplied one.
- `MaxQuoteEntriesPerClient = 1024` is enforced in `FromProto` before
  allocating the `VTXOQuotes` / `LeaveQuotes` slices to prevent
  resource exhaustion from malformed server envelopes.
- `SubmitForfeitSigRequest` (boarding input signatures) is distinct
  from `SubmitVTXOForfeitSigsToServer` (VTXO forfeit signatures);
  these are separate mailbox methods with separate proto types.
- `ForfeitRequestToVTXO.ForfeitSpend` overrides the default
  standard-VTXO collaborative leaf when the live output uses a custom
  script policy; without it, the VTXO actor would construct a forfeit
  using the wrong tapscript branch.

## Deep Docs

- [round/README.md](README.md) — Full state machine walkthrough with diagrams.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
