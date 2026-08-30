# round

## Purpose

Client-side Ark round participation FSM implementing boarding (on-chain to
off-chain), refresh (VTXO rollover), and leave (off-chain to on-chain exit)
protocols with MuSig2 signing ceremonies.

## Key Types

For field-level detail, use `go doc github.com/lightninglabs/wavelength/round.<Symbol>`.
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
  `QuoteRejected`, `ForfeitCollectionTimedOut`, `RegistrationTimedOut`,
  `ForfeitSignatureResponse`, `ConnectorLeafInfo`, `IntentPackage` (atomic
  delivery of all intent types), `StatusReconcileTimedOut` and
  `RoundStatusReported` (the wavelength#844 status reconcile, below).
- `ClientOutMsg` — sealed outbox interface. Members:
  `JoinRoundRequest`, `JoinRoundAcceptOutbox`, `JoinRoundRejectOutbox`,
  `SubmitNoncesRequest`, `SubmitPartialSigRequest`,
  `SubmitForfeitSigRequest`, `SubmitVTXOForfeitSigsToServer`,
  `RegisterConfirmationRequest`, `StartTimeoutReq`, `CancelTimeoutReq`,
  `ReleaseForfeitReservation`, `DropCustomForfeitReservation`,
  `QueryRoundStatusOutbox`, `VTXOCreatedNotification`,
  `RoundCheckpointedNotification`, `RoundCompletedNotification`,
  `RoundFailedNotification`, `TerminalJobFailedNotification`,
  `ForfeitRequestToVTXO`, `ForfeitConfirmedToVTXO`.

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
  exactly one `IsChange=true` across the assembled intent. Expiry-driven
  auto-refreshes additionally carry `BatchExpiry` (the maintenance cohort
  key), `ExpandCohort` (local-only coordination provenance), the leader's
  operator-key snapshot, `PolicyTemplate`, and `OwnerKey`.
- `RefreshVTXOCohortRequest` — local-only message that atomically adds a
  manager-coordinated maintenance cohort (`Requests
  []*RefreshVTXORequest`) to a single assembling round, so cohort members
  cannot be split across rounds or pay per-member operator fees.
- `ConfirmationEvent`, `TimeoutMsg` — chain confirmation / timeout
  delivery to the actor.
- VTXO-actor messages (`vtxo_messages.go`): `ForfeitRequestEvent`,
  `ForfeitConfirmedEvent`, `BlockEpochEvent`, `SpendReserveEvent`,
  `SpendReleasedEvent`, `SpendCompletedEvent`, `ForfeitReleasedEvent`,
  `ForfeitSignedEvent`, `VTXOFailedEvent`, `ResumeVTXOEvent`,
  `PendingForfeitEvent`, `VTXOTerminatedMsg`.

### Misc

- `TimeoutPhase` (`fsm_timeouts.go`) — four phases:
  `TimeoutPhaseForfeitCollection` (forfeit-signature collection window),
  `TimeoutPhaseRegistration` (IntentSentState admission window; on expiry
  the FSM fails the round recoverably and emits
  `ReleaseForfeitReservation` so forfeit-reserved inputs are not
  stranded — wavelength#653), `TimeoutPhaseRefreshRegistration` (the quiet
  period that coalesces expiry-driven refreshes into one round), and
  `TimeoutPhaseStatusReconcile` (the wavelength#844 probe window). Timeout
  outbox messages (`StartTimeoutReq`/`CancelTimeoutReq`) key on
  `RoundKeyStr` so temp-keyed rounds (pre-admission) can be timed, and the
  distinct phase keys let two timers coexist on one round.
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
  stranded when the server never responds (wavelength#653).
- `RoundClientConfig.StatusReconcileTimeout` — window a forfeit-bearing
  round in `InputSigSentState` waits before probing the operator with
  `QueryRoundStatusOutbox`; doubles as the retry interval between probes
  (exponential backoff capped at 16× the base). Zero selects
  `defaultStatusReconcileTimeout` (90 s); non-positive opts out entirely.
- `RoundClientConfig.WalletAskTimeout` — bounds *every* Ask this actor
  makes into the wallet actor, covering both the enqueue and the Await.
  Zero selects `defaultWalletAskTimeout` (30 s). Applied by the
  `askWallet` helper.
- `RoundClientConfig.AutoRefreshFeeFloor` / `AutoRefreshFeeRatePPM` —
  the unattended-maintenance fee budget curve (floor plus proportional
  rate over preserved value). See `autoRefreshFeeBudget` under
  [Invariants](#invariants). Both zero disables the curve and falls back
  to `MaxOperatorFee` alone.
- `defaultRefreshRegistrationDelay` (500 ms) — quiet period used to
  coalesce back-to-back expiry-driven refreshes (one per VTXO actor on a
  block epoch) into a single round registration.
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
- **Depended on by**: `vtxo`, `db`, `waved`.
- **Sends → `serverconn`**: `JoinRoundRequest`,
  `JoinRoundAcceptOutbox`, `JoinRoundRejectOutbox`,
  `SubmitNoncesRequest`, `SubmitPartialSigRequest`,
  `SubmitForfeitSigRequest`, `SubmitVTXOForfeitSigsToServer`,
  `QueryRoundStatusOutbox` (→ `roundpb.MethodQueryRoundStatus`).
- **Sends → `vtxo`**: forfeit/spend/block-epoch events listed above;
  manager-level `VTXOCreatedNotification`, `VTXOTerminatedMsg`.
- **Sends → `wallet`**: `RegisterConfirmationRequest`.
- **Sends → `OwnedScriptRegistrar`** (waved adapter over the OOR
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
  `RoundJoined`, `BoardingFailed`, `JoinRoundQuoteReceived`,
  `RoundStatusReported`.
- **Receives ← `vtxo`**: `ForfeitSignatureResponse` (via manager),
  `RefreshVTXORequest` (per-VTXO expiry refresh) and
  `RefreshVTXOCohortRequest` (manager-coordinated maintenance cohort).
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
  never see it (wavelength#602). Sweeping at the next assembly still
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
  (`validateConnectorAncestry`, wavelength#681). In
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
- **Forfeit release past `PartialSigsSent` goes through a status
  reconcile, never a blind release** (wavelength#844).
  `releaseForfeitsOnFailure` (the #653 rollback wrapper) deliberately
  stops at `PartialSigsSentState`, because beyond that point the operator
  may hold fully-signed forfeit txs and an unconditional release risks a
  double-spend. A `BoardingFailed` landing in `InputSigSentState` with
  forfeits at stake therefore parks in `InputSigSentState.PendingFailure`
  while `QueryRoundStatusOutbox` probes the operator. Only a
  `ROUND_STATUS_DEAD` answer — proof the commitment can never confirm,
  since the operator persists a finalized round atomically with its VTXOs
  *before* broadcasting — releases the reservations. Any other answer
  holds them.
- The `TimeoutPhaseStatusReconcile` timer never releases anything on its
  own; expiry only re-emits the probe. It exists to cover the silent
  operator (lumos#618): a crashed operator sends no failure at all, so
  without the timer the FSM would wait forever for an event that is not
  coming. It is armed when forfeit signatures are emitted, re-armed per
  probe, and re-armed on restart reload. `InputSigSentState.PendingFailure`
  and `ReconcileProbes` are **in-memory only** — a restart resets the probe
  count and relies on the re-armed timer.
- **Arm status reconciliation before any fallible outbox effect.**
  `processOutbox` stops at the first failed external effect, so a transient
  failure while submitting forfeit signatures or registering the chain
  watch would otherwise skip arming the timeout *after* signatures had
  already left the client, stranding reserved inputs with no reconciliation
  path. The forfeit-collection outbox order is: cancel the
  forfeit-collection timeout, arm status reconciliation, then VTXO forfeit
  signatures → optional boarding signatures → chain registration.
- **Exactly one commitment-confirmation watch per round.** The FSM outbox
  emits `RegisterConfirmationRequest` after committing the durable
  `InputSigSent` state; checkpoint handling only installs the routing
  index, and restart recovery re-registers from durable state. Registering
  again from the checkpoint notification produced a second subscription
  under a different caller ID — the first completed the round and the
  second reported the tx as no longer indexed. The FSM request uses the
  validated batch-output script and the operator's minimum confirmation
  target, matching restart recovery.
- **`SweepDelay` must survive a restart** (`rounds.sweep_delay`). The
  confirmation handler derives each new VTXO's absolute batch expiry as
  `confirmation_height + SweepDelay`, and a round checkpointed at
  `input_sig_sent` can confirm long afterwards. The upsert only adopts a
  non-zero incoming delay — the value is fixed for the life of a round, so
  a later checkpoint must never clear what an earlier one recorded. Rounds
  checkpointed before the migration have no recorded delay; for those the
  confirmation path leaves the expiry **unstamped** (logging at error
  level) rather than stamping `BatchExpiry == CreatedHeight`, which the
  wallet would read back as already expired. An unstamped expiry
  classifies as `ExpiryStatusUnknown`, so the VTXO stays live and spendable
  with only expiry monitoring disabled.
- **Every round → wallet `Ask` is bounded** by `askWallet`, which applies
  one `WithTimeout` covering both the enqueue and the `Await`. The wallet's
  own handlers Ask the round actor back, so two full mailboxes could
  otherwise produce a circular wait that parks the receive loop forever.
  Bounding one direction breaks the cycle; the wallet → round direction is
  left unbounded **deliberately**, because an Ask's context becomes the
  receiver's turn context and a deadline there would bound money-path FSM
  registration work. The actor lifecycle context stays the parent so
  shutdown still cancels a pending Ask.
- Call sites that scan every tracked round share **one** deadline per scan
  rather than paying the per-query budget per FSM. With N rounds
  mid-transition a single message could otherwise spend N × the per-query
  budget, and `OnStop` could burn its whole cleanup budget on queries and
  strand external-signer session cleanup. Every scanning caller already
  degrades by skipping an unreadable round, so the shared budget changes
  worst-case latency, not behaviour.
- **Automatic (expiry-driven) refreshes are cohort-scoped.** Members of
  one `BatchExpiry` cohort are handed to a round atomically via
  `RefreshVTXOCohortRequest`, reusing the leader's operator-key snapshot so
  a single registration cannot mix operator terms or spend its deadline on
  repeated `GetInfo` calls. If the round handoff fails, the leader is
  released together with its reserved siblings.
- **Auto-refresh fee budget** (`autoRefreshFeeBudget`): the allowance is
  `max(AutoRefreshFeeFloor, autoValue × AutoRefreshFeeRatePPM / 1e6)`,
  clamped to `MaxOperatorFee`, which remains the hard global ceiling. The
  floor covers fixed costs for small VTXOs without giving large cohorts an
  unbounded allowance. It **fails closed**: with a configured policy and a
  non-positive automatic value (malformed or overflowed intents) the budget
  is zero, not unlimited. The denominator counts only automatic-maintenance
  outputs, but the resulting cap applies to the entire realised round fee.
  A `ratePPM > 1_000_000` is rejected outright.

## Deep Docs

- [round/README.md](README.md) — Full state machine walkthrough with
  diagrams.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
