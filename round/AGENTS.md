# round

## Purpose

Client-side Ark round participation FSM implementing boarding (on-chain to
off-chain), refresh (VTXO rollover), and leave (off-chain to on-chain exit)
protocols with MuSig2 signing ceremonies.

## Key Types

For field-level detail, use `go doc github.com/lightninglabs/darepo-client/round.<Symbol>`.
This section lists types with the one-line gist plus any non-obvious wiring;
state transitions and validation rules live under [Invariants](#invariants).

### FSM (`states.go`, `events.go`, `outbox_messages.go`)

- `ClientState` — sealed interface for the 15 FSM states: `Idle`,
  `PendingRoundAssembly`, `IntentSentState`, `QuoteReceivedState`,
  `RoundJoinedState`, `CommitmentTxReceivedState`,
  `CommitmentTxValidatedState`, `NoncesSentState`, `NoncesAggregatedState`,
  `PartialSigsSentState`, `ForfeitSignaturesCollectingState`,
  `InputSigSentState`, `ConfirmedState`, `ClientFailedState`,
  `RecoveryInitiatedState`.
- `ClientEvent` — sealed inbound event interface. Notable members:
  `JoinRoundQuoteReceived` (carries reseal `SealPass`), `QuoteAccepted`,
  `QuoteRejected`, `ForfeitCollectionTimedOut`, `ForfeitSignatureResponse`,
  `ConnectorLeafInfo`, `IntentPackage` (atomic delivery of all intent
  types).
- `ClientOutMsg` — sealed outbox interface. Members:
  `JoinRoundAcceptOutbox`, `JoinRoundRejectOutbox`,
  `SubmitForfeitSigRequest`, `StartTimeoutReq`, `CancelTimeoutReq`,
  `RoundCheckpointedNotification`, `RoundCompletedNotification`,
  `RoundFailedNotification`, `ForfeitRequestToVTXO`,
  `ForfeitConfirmedToVTXO`.

### Quote & Intent (`interfaces.go`, `events.go`)

- `ClientQuote` — client-side view of `roundpb.JoinRoundQuote`. Threaded
  through `QuoteReceivedState` → `RoundJoinedState` →
  `CommitmentTxReceivedState`. Carries positional `VTXOQuotes` /
  `LeaveQuotes` (server-decided amounts, echoed pkScripts and recipient
  keys), `SealPass`, `OperatorFeeSat`, `QuoteExpiresAt`, `RejectReason`.
- `VTXOQuoteEntry` / `LeaveQuoteEntry` — positional quote entries
  cross-checked by `evaluateQuote` against the intent's positional
  `VTXORequest` / `LeaveRequests`.
- `Intents` — pools of boarding/VTXO/forfeit/leave requests. Field
  `QuotedLeaveAmounts []int64` holds server-authoritative leave amounts
  captured at `QuoteAccepted`; `LeaveAmount(idx)` returns it, falling
  back to the intent target when no quote was accepted.
- `VTXOIntent`, `RoundVTXORequest`, `BoardingIntent`, `ClientVTXO` —
  pre-registration request / signing wrapper / boarding wrapper / full
  owned-VTXO descriptor (the latter carries `Origin`, `CommitmentTxID`,
  `BatchExpiry`, `CreatedHeight`, and `Ancestry []types.Ancestry`).

### Persistence & Wallet Interfaces (`interfaces.go`)

- `ClientEnvironment` — FSM environment (storage access).
- `ClientWallet` — embeds `input.Signer` for MuSig2; adds
  `DeriveNextKey` for VTXO signing keys.
- `OwnedScriptChecker` — `IsOwnedScript(ctx, pkScript) → fn.Result[bool]`.
  Used by the `InputSigSent → Confirmed` transition to decide which
  VTXOs `buildOwnedClientVTXOs` persists. Backed by the OOR artifact
  store in production; `nil` in tests treats every VTXO as owned.
- `OwnedScriptRegistrar` — `RegisterOwnedScript(ctx, pkScript, ownerKey)`.
  Called at intent-build time for change/refresh outputs and inside
  `handleRegisterIntent` for entries with a non-zero `KeyLocator`.
- `VTXOStore`, `RoundStore` — VTXO and round FSM persistence.

### Actor Layer (`actor.go`, `actor_messages.go`, `vtxo_messages.go`)

- `ClientMsg` / `ClientResp` — marker interfaces for `RoundClientActor`
  inbox/outbox (embed `actormsg.RoundReceivable` /
  `actormsg.RoundActorResp`).
- `WalletBoardingConfirmed` — wraps `wallet.BoardingIntent` so boarding
  confirmations implement `ClientMsg`.
- `ServerMessageNotification` / `ServerMessageResponse` — server-event
  delivery and ack.
- `GetClientStateRequest/Response`, `CancelRoundRequest/Response`,
  `RegisterVTXORequestsRequest/Response`, `RegisterIntentRequest` —
  introspection and command messages.
- `RefreshVTXORequest` — per-VTXO refresh. Under the seal-time fee
  handshake (#270) `OperatorFee` is **advisory only**: the FSM does NOT
  subtract it from `Amount`. The actor's `designateChangeMarker` stamps
  exactly one `IsChange=true` across the assembled intent.
- `ConfirmationEvent`, `TimeoutMsg` — chain confirmation / timeout
  delivery to the actor.
- VTXO-actor messages (`vtxo_messages.go`): `ForfeitRequestEvent`,
  `ForfeitConfirmedEvent`, `BlockEpochEvent`, `SpendReserveEvent`,
  `SpendReleasedEvent`, `SpendCompletedEvent`, `ForfeitReleasedEvent`,
  `ForfeitSignedEvent`, `VTXOFailedEvent`, `ResumeVTXOEvent`,
  `PendingForfeitEvent`, `VTXOTerminatedMsg`.

### Misc

- `TimeoutPhase` (`fsm_timeouts.go`) — `TimeoutPhaseForfeitCollection`
  (forfeit-signature collection window) and `TimeoutPhaseRegistration`
  (IntentSentState admission window; on expiry the FSM fails the round
  recoverably and emits `ReleaseForfeitReservation` so forfeit-reserved
  inputs are not stranded — darepo-client#653). Timeout outbox messages
  (`StartTimeoutReq`/`CancelTimeoutReq`) key on `RoundKeyStr` so temp-keyed
  rounds (pre-admission) can be timed.
- `MaxQuoteEntriesPerClient = 1024` (`from_proto.go`) — bounds quote
  entry decoding to reject malformed envelopes before allocating slices.
- `FromProto` methods on `JoinRoundQuoteReceived`, `RoundJoined`,
  `CommitmentTxBuilt`, `AwaitingBoardingSigs`, `NoncesAggregated`,
  `OperatorSigned`, `BoardingFailed`, `JoinRoundRequest` — all
  satisfy the private `inboundServerMessage` interface
  (compile-time-asserted).
- `RoundClientConfig.LedgerSink` — optional `fn.Option[ledger.Sink]`
  plumbed onto the round actor; `emitVTXOsReceived` and `emitRoundFee`
  fire-and-forget messages when `fn.Some`.
- `RoundClientConfig.RegistrationTimeout` — max wall-clock duration to wait in
  `IntentSentState` for the server's `RoundJoined` admission watermark. Zero
  selects `defaultRegistrationTimeout` (60 s); negative disables the timeout
  (round waits indefinitely). Bounds how long forfeit-reserved inputs sit
  stranded when the server never responds (darepo-client#653).
- `computeClientOperatorFee(intents, ownedVTXOs) int64` —
  Σ(boarding inputs) + Σ(forfeited VTXOs) − Σ(owned output VTXOs) −
  Σ(cooperative leave outputs), clamped to zero. Carried on
  `VTXOCreatedNotification.OperatorFeeSat`.

## Relationships

- **Depends on**: `baselib/protofsm` (FSM engine), `baselib/actor` (actor
  primitives: `ActorRef`, `ActorSystem`, `BaseMessage`), `lib/actormsg`
  (mailbox marker interfaces), `lib/tree`, `lib/types`, `lib/arkscript`,
  `lib/bip322` (join-round BIP-322 auth signing), `rpc/roundpb` (wire proto
  types via `FromProto`), `wallet`, `ledger` (`Sink` + `VTXOReceivedMsg` /
  `Source*` constants), `timeout`, `google/uuid`.
- **Depended on by**: `vtxo`, `db`, `darepod`.
- **Sends → `serverconn`**: `JoinRoundRequest`,
  `JoinRoundAcceptOutbox`, `JoinRoundRejectOutbox`,
  `SubmitNoncesRequest`, `SubmitPartialSigRequest`,
  `SubmitForfeitSigRequest`, `SubmitVTXOForfeitSigsToServer`.
- **Sends → `vtxo`**: forfeit/spend/block-epoch events listed above;
  manager-level `VTXOCreatedNotification`, `VTXOTerminatedMsg`.
- **Sends → `wallet`**: `RegisterConfirmationRequest`.
- **Sends → `OwnedScriptRegistrar`** (darepod adapter over the OOR
  artifact store): `RegisterOwnedScript(pkScript, ownerKey)`.
- **Sends → `ledger`** (when `LedgerSink` is `fn.Some`), origin-routed
  per owned `ClientVTXO`: `VTXOReceivedMsg{Source=SourceRoundBoarding}`;
  paired `VTXOSentMsg{Outpoint}` +
  `VTXOReceivedMsg{Source=SourceRoundRefresh}`;
  `VTXOReceivedMsg{Source=SourceRoundTransfer}`. One
  `FeePaidMsg{FeeType=FeeTypeRefresh}` per round when
  `OperatorFeeSat > 0` and any refresh-origin VTXO was emitted.
- **Receives ← `serverconn`** (via `ServerMessageNotification`):
  `CommitmentTxBuilt`, `NoncesAggregated`, `OperatorSigned`,
  `RoundJoined`, `BoardingFailed`, `JoinRoundQuoteReceived`.
- **Receives ← `vtxo`**: `ForfeitSignatureResponse` (via manager).
- **Receives ← `wallet`** (via `lib/actormsg`): `RegisterIntentMsg`
  (pre-admitted intent packages), `TriggerBoardMsg`. Boarding UTXO
  confirmations arrive wrapped in `WalletBoardingConfirmed`.
- **Receives ← `timeout`**: `TimeoutMsg`.
- **Receives ← `chainsource`**: `ConfirmationEvent`.

## Invariants

- Tree signatures are validated **before** boarding input signatures
  are released (security checkpoint at `InputSigSent`).
- Forfeit signatures are collected **after** VTXO tree signing
  completes — clients only forfeit old VTXOs after verifying the new
  VTXOs are properly signed.
- Aggregated signatures validated on `VTXOTreePaths` are propagated to
  extracted `ClientTrees` via `SubmitTreeSigs` + `VerifySigned`, so
  persisted client trees carry valid signatures for unilateral exit.
- Round state is checkpointed atomically after tree validation — a
  crash before checkpoint means the client has no record of sent
  signatures.
- Primary FSM handles interactive phases (through `InputSigSent`); a
  dedicated FSM per round handles confirmation monitoring.
- The round actor does **not** mark VTXOs as `PendingForfeit` — the
  wallet/manager admits VTXOs before sending `RegisterIntentMsg`.
- A round that settles in the terminal `ClientFailedState` (admission
  timeout, server rejection, quote rejection, forfeit-collection timeout,
  etc.) is reaped from the actor's `rounds` map by `reapFailedRounds`,
  swept at the start of the next assembly (`createNewRound`) rather than on
  entry. Deferring the reap keeps the failure observable: `GetClientState`
  (and the `ListRounds` RPC it backs) must be able to report a round as
  FAILED until the client moves on to a fresh round — reaping on entry made
  the terminal state vanish within the same actor turn, so a poller could
  never see it (darepo-client#602). Sweeping at the next assembly still
  bounds accumulation to the failures since the last new round, mirroring
  `onRoundComplete` (success) and `handleCancelRound` (explicit cancel).
  Nothing reuses a failed round — `findAssemblingRound` only returns
  `Idle`/`PendingRoundAssembly` rounds, and the FSM's recovery transitions
  have no production producer — so deferred reaping is safe.
- `ClientWallet` provides MuSig2 signing and key derivation; boarding
  address creation is handled by the wallet actor (not the round FSM).
- Persisted VTXO ownership uses `OwnerKey` (not `SigningKey`). For
  directed sends, the sender's signing key participates in MuSig2 tree
  construction but the recipient's owner key determines ownership.
- Local-balance persistence on confirmation is driven by
  `OwnedScriptChecker.IsOwnedScript(pkScript)` — `buildOwnedClientVTXOs`
  skips any VTXO whose pkScript the checker does not recognize. The
  client still co-signs its tree path, so foreign recipients in a
  directed send still get a valid unroll proof. `nil` checker (tests)
  treats every VTXO as owned.
- VTXO pkScripts are registered with `OwnedScriptRegistrar` at
  intent-build time for change/refresh outputs, and inside
  `handleRegisterIntent` for any `RegisterIntentMsg` entry with a
  non-zero `KeyLocator`. Remote recipient keys in directed sends carry
  a zero `KeyLocator` and are intentionally left unregistered.
- Each client sub-tree in the commitment tree must contain exactly one
  non-anchor leaf; `buildOwnedClientVTXOs` fails the transition
  otherwise.
- **Seal-time fee handshake (#270)**: the server is the amount
  authority. When `QuoteReceivedState.Quote` is non-nil, it threads
  through `RoundJoinedState` → `CommitmentTxReceivedState`, which
  validates each VTXO leaf and leave output against the quote's
  positional amount (not the intent target). `env.MaxOperatorFee` is
  applied at `QuoteReceivedState` and re-evaluated on every seal pass.
  Quote-less harness paths fall back to intent targets so pre-#270 FSM
  tests keep working.
- **RoundID identity** is asserted at every server-pushed event that
  carries one. `IntentSentState` records the admitted `RoundID` from
  the `RoundJoined` ack onto `AdmittedRoundID` and cross-checks
  `JoinRoundQuoteReceived.RoundID`; `RoundJoinedState` cross-checks
  both `CommitmentTxBuilt.RoundID` and any reseal-after-accept
  `JoinRoundQuoteReceived.RoundID`. The actor routing map is keyed by
  the same RoundID, so these checks agree by construction under normal
  operation — they are defense-in-depth against future routing
  regressions.
- A `JoinRoundQuoteReceived` with a strictly higher `SealPass` replaces
  the current quote and re-evaluates (in `QuoteReceivedState`) or walks
  the FSM back from `RoundJoinedState` to `QuoteReceivedState`. Lower
  or equal `SealPass` is self-loop / stale.
- `ConnectorLeafInfo.VTXOAmount` is populated from local VTXO state
  (not from the server's proto), so the forfeit penalty output equals
  the canonical local value rather than a server-supplied one.
- **Connector ancestry is proven before any forfeit is signed**
  (`validateConnectorAncestry`, darepo-client#681). In
  `CommitmentTxReceivedState`, after VTXO-tree validation, each assigned
  connector leaf is checked by deterministically reconstructing its
  connector tree via `tree.BuildConnectorTree` from the commitment-tx
  output at `ConnectorLeafInfo.RootOutputIndex`, the operator key, and
  the server-supplied `NumLeaves`/`Radix`, then asserting the assigned
  leaf is the one at `LeafIndex` (outpoint + output). Because the leaf is
  rebuilt on top of a real commitment output, the connector is only
  spendable once the commitment tx confirms, preserving round atomicity.
  No connector transactions cross the wire — only the four scalars.
- `MaxQuoteEntriesPerClient = 1024` is enforced in `FromProto` before
  allocating quote slices to prevent resource exhaustion.
- `SubmitForfeitSigRequest` (boarding input signatures) is distinct
  from `SubmitVTXOForfeitSigsToServer` (VTXO forfeit signatures) —
  separate mailbox methods, separate proto types.
- `ForfeitRequestToVTXO.ForfeitSpend` overrides the default
  collaborative leaf when the live output uses a custom script policy;
  without it the VTXO actor would build a forfeit against the wrong
  tapscript branch.

## Deep Docs

- [round/README.md](README.md) — Full state machine walkthrough with
  diagrams.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
