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
  `ForfeitConfirmedToVTXO`, `ReleaseForfeitReservation`,
  `DropCustomForfeitReservation`.

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
- `RoundClientConfig.MetricsSink` — optional `fn.Option[metrics.Sink]`;
  when `fn.Some`, `emitRoundJoined` (from `createNewRound`) and
  `emitRoundCompleted` (from `processOutbox` on
  `RoundCompletedNotification`/`RoundFailedNotification`) fire-and-forget
  `metrics.RoundJoinedMsg` / `metrics.RoundCompletedMsg{Status: "confirmed"|"failed"}`
  so `darepod_rounds_joined_total` / `darepod_rounds_completed_total`
  reflect reality. Mirrors `LedgerSink`; a Tell failure is logged at
  debug and never fails the enclosing dispatch.
- `RoundClientConfig.DropCustomForfeitSigningContexts` — optional
  `func(ctx, []wire.OutPoint) error` callback that clears daemon-local
  signing metadata for custom refresh inputs when a round fails before
  the connector-bound forfeit signing request is produced. When nil,
  only the VTXO manager's custom forfeit actors are dropped (via
  `actormsg.DropCustomForfeitInputsRequest`).
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
  (compile-time-asserted). `CommitmentTxBuilt.FromProto` also decodes
  the round's `TreeCosignKey`, `ConnectorOperatorKey`, `SweepKey`,
  `SweepDelay`, and `ForfeitKey` (all optional, nil/zero for servers
  that predate them); `BoardingFailed.FromProto` decodes an optional
  `RoundID` (validated against `roundIDLen`) so failures that arrive
  after admission route deterministically instead of via the
  sole-round heuristic.
- `RoundClientConfig.LedgerSink` — optional `fn.Option[ledger.Sink]`
  plumbed onto the round actor; `emitVTXOsReceived` and `emitRoundFee`
  fire-and-forget messages when `fn.Some`. `emitVTXOsReceived` also
  emits a `ledger.VTXOSentMsg` per `VTXOCreatedNotification.Outflows`
  entry (foreign directed-send recipients, cooperative leave outputs)
  so recipient value is not misreported as operator fee.
  `roundOperatorFeeType` classifies the fee as `FeeTypeBoarding` when
  the round carries any boarding intent (or a boarding-origin VTXO),
  else `FeeTypeRefresh`, carried on
  `VTXOCreatedNotification.OperatorFeeType`.
- `validateVTXOTreeBinding` / `verifyVTXOTreeRoot` (`transitions.go`)
  — see [Invariants](#invariants) (darepo-client#680).
- `confirmationWatchScript(commitmentTx, vtxoTrees)` (`transitions.go`)
  — picks the pkScript the actor asks the chain backend to watch for
  round confirmation: the lowest-indexed proven VTXO batch output, not
  always output 0. Falls back to output 0 for refresh-less/harness
  rounds with no `VTXOTreePaths`.
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

- **Depends on**: `baselib/protofsm` (FSM engine), `lib/tree`,
  `lib/types`, `lib/arkscript`, `wallet`, `ledger` (`Sink` +
  `VTXOReceivedMsg` / `Source*` constants), `metrics` (`Sink` +
  `RoundJoinedMsg` / `RoundCompletedMsg`), `timeout`, `google/uuid`.
- **Depended on by**: `vtxo`, `db`, `darepod`.
- **Sends → `serverconn`**: `JoinRoundRequest`,
  `JoinRoundAcceptOutbox`, `JoinRoundRejectOutbox`,
  `SubmitNoncesRequest`, `SubmitPartialSigRequest`,
  `SubmitForfeitSigRequest`, `SubmitVTXOForfeitSigsToServer`.
- **Sends → `vtxo`**: forfeit/spend/block-epoch events listed above;
  manager-level `VTXOCreatedNotification`, `VTXOTerminatedMsg`,
  `actormsg.DropCustomForfeitInputsRequest` (custom forfeit-actor
  teardown on round failure).
- **Sends → `wallet`**: `RegisterConfirmationRequest`.
- **Sends → `OwnedScriptRegistrar`** (darepod adapter over the OOR
  artifact store): `RegisterOwnedScript(pkScript, ownerKey)`.
- **Sends → `ledger`** (when `LedgerSink` is `fn.Some`), origin-routed
  per owned `ClientVTXO`: `VTXOReceivedMsg{Source=SourceRoundBoarding}`;
  paired `VTXOSentMsg{Outpoint}` +
  `VTXOReceivedMsg{Source=SourceRoundRefresh}`;
  `VTXOReceivedMsg{Source=SourceRoundTransfer}`; one
  `VTXOSentMsg{AmountSat}` per `VTXOCreatedNotification.Outflows` entry
  (non-owned recipients/leave outputs). One `FeePaidMsg` per round when
  `OperatorFeeSat > 0`, typed via `roundOperatorFeeType` as
  `FeeTypeBoarding` (any boarding intent, or a boarding-origin VTXO) or
  `FeeTypeRefresh` (otherwise) — no longer gated on seeing a
  refresh-origin VTXO.
- **Sends → `metrics`** (when `MetricsSink` is `fn.Some`):
  `RoundJoinedMsg` (from `createNewRound`, every assembled round);
  `RoundCompletedMsg{Status="confirmed"|"failed"}` (from
  `RoundCompletedNotification` / `RoundFailedNotification` handling).
- **Receives ← `serverconn`** (via `ServerMessageNotification`):
  `CommitmentTxBuilt`, `NoncesAggregated`, `OperatorSigned`,
  `RoundJoined`, `BoardingFailed`, `JoinRoundQuoteReceived`.
- **Receives ← `vtxo`**: `ForfeitSignatureResponse` (via manager).
- **Receives ← `wallet`** (via `lib/actormsg`): `RegisterIntentMsg`
  (pre-admitted intent packages), `TriggerBoardMsg`. Boarding UTXO
  confirmations arrive wrapped in `WalletBoardingConfirmed`.
  `TriggerBoardMsg.Outpoints` (optional) restricts `handleTriggerBoard`
  to exactly those confirmed boarding inputs instead of all confirmed
  intents; `TriggerBoardMsg.Change` (optional) adds a leave output that
  pays clipped boarding balance back to a wallet-owned boarding script.
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
- **Every VTXO tree is proven rooted in this round's commitment tx**
  (`validateVTXOTreeBinding` / `verifyVTXOTreeRoot`, darepo-client#680,
  the VTXO-tree counterpart to #681). Internal tree self-consistency
  (`ValidatePath`/`ValidateAnchors`) is not sufficient — a
  self-consistent tree rooted at the wrong commitment output would
  still pass those checks and get co-signed, leaving the client's new
  VTXOs unrecoverable once the real commitment tx confirms. Run in
  `CommitmentTxReceivedState` before any per-VTXO validation trusts
  tree contents: for each `(outputIdx, tree)` pair it asserts the
  tree's `BatchOutpoint` names this commitment txid, that `outputIdx`
  equals `BatchOutpoint.Index` and is in range, that the committed
  output byte-matches `BatchOutput`, and (implicitly, via the cosigner
  set) that the committed script traces back to the tree root. A
  binding failure is non-recoverable (`failBeforeForfeitSigning`,
  `false`) since a mis-rooted tree is a structural defect, not a
  transient condition.
- `confirmationWatchScript` (used by `registerCommitmentConfirmation`
  and the forfeit-collection/checkpoint outboxes) watches the
  lowest-indexed key of `VTXOTreePaths` — the output proven by
  `validateVTXOTreeBinding` to actually receive this client's funds —
  rather than assuming commitment-tx output 0. Falls back to output 0
  for refresh-less rounds or harness paths with no `VTXOTreePaths`.
- **Sweep delay vs. VTXO exit delay is validated per round, not once at
  actor construction.** `SweepDelay` is no longer a global operator
  term (`OperatorTerms`) — the server delivers it per round on
  `CommitmentTxBuilt`/`CommitmentTxReceivedState`. `NewRoundClientActor`
  no longer calls `ValidateDelayParameters`; the check
  (`SweepDelay > VTXOExitDelay`, so the operator has time to respond to
  a unilateral exit before the batch sweep window opens) now runs in
  `CommitmentTxReceivedState.processEvent` against each round's
  delivered `SweepDelay`, failing the round (non-recoverable) on
  violation.
- `MaxQuoteEntriesPerClient = 1024` is enforced in `FromProto` before
  allocating quote slices to prevent resource exhaustion.
- `SubmitForfeitSigRequest` (boarding input signatures) is distinct
  from `SubmitVTXOForfeitSigsToServer` (VTXO forfeit signatures) —
  separate mailbox methods, separate proto types.
- `ForfeitRequestToVTXO.ForfeitSpend` overrides the default
  collaborative leaf when the live output uses a custom script policy;
  without it the VTXO actor would build a forfeit against the wrong
  tapscript branch.
- `types.ForfeitTxSig.ParticipantVTXOSigs` (keyed non-operator
  signatures for custom multi-participant spend paths) is accepted as
  an alternative to `ClientVTXOSig` throughout the forfeit-signing
  path — `SubmitVTXOForfeitSigsToServer.ToProto` requires at least one
  of the two to be present per outpoint, and both are forwarded to the
  server as `ForfeitParticipantSig` entries alongside the legacy
  single-signer field.
- On round failure before connector-bound forfeit signatures are sent,
  `rollbackOutbox`/`releaseForfeitsOnFailure` split reserved forfeit
  inputs by kind: standard wallet VTXOs get `ReleaseForfeitReservation`
  (returned to `LiveState`), while caller-supplied custom refresh
  inputs get `DropCustomForfeitReservation` (their `PendingForfeit`
  signer actors, and any `DropCustomForfeitSigningContexts`-backed
  daemon-local signing metadata, are dropped instead — they are not
  wallet VTXOs and must never be returned to `LiveState`).
- `roundLedgerOutflows`/`VTXOCreatedNotification.Outflows` account for
  round value the client paid out without gaining a locally owned
  VTXO — non-local-owner `VTXORequest`s (`!HasLocalOwner()`, e.g.
  directed-send recipients) and leave outputs. Each entry carries an
  `IdempotencyKey` derived from `(RoundID, kind, index)` so multiple
  outflows in one round persist as distinct ledger rows instead of
  colliding on the round-level idempotency index.

## Deep Docs

- [round/README.md](README.md) — Full state machine walkthrough with
  diagrams.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
