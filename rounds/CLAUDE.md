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
- `RoundSealedReq` — Outbox event emitted by `SealEvent` handler when
  registration closes. Signals the actor to spawn a new round for the next
  registration window.
- `ActorMsg` — Messages sent to the round actor (JoinRoundRequest, nonces, sigs).
- `JoinRoundRequestFromProto`, `NoncesFromProto`, `PartialSigsFromProto`,
  etc. — Exported proto→domain conversion helpers in `proto_convert.go`,
  called from `server_rounds.go` `AddEnvelopeRoute` Adapt closures.
- `BoardingInputLocker` — Interface for locking boarding inputs to prevent
  double-spending across concurrent rounds. Implemented by
  `inMemoryBoardingLocker` in the root package.
- `Environment.HeaderVerifier` — `proof.HeaderVerifier` for TxProof SPV
  validation when no `ChainSource` is available. Wired from
  `lndbackend.NewLndHeaderVerifier`.
- `SealEvent` — Canonical internal event that transitions
  `IntentCollectingState` -> `QuoteSentState` (via
  `sealRoundWithQuotes`) and emits `RoundSealedReq` on pass 0.
  Fired by registration timeout, seal predicate, or admin
  `TriggerBatch` RPC. Single emission point prevents duplicate
  round creation.
- `QuoteSentState` — Post-seal state that fans out per-client
  `JoinRoundQuoteOutbox` envelopes and waits for every client to
  accept (`ClientQuoteAcceptEvent`), reject
  (`ClientQuoteRejectEvent`), or time out (`QuoteTimeoutEvent`).
  Advances to `BatchBuildingState` on all-accepted, reseals via a
  fresh `SealEvent` over survivors on any reject/timeout (capped
  by `Environment.MaxSealPasses`), or fails the round if zero
  clients survive. See `docs/fee-model.md` for the full lifecycle.
- `Quote` — Server-side per-client seal-time result: binding
  VTXO / leave output amounts, operator fee, breakdown, and a
  32-byte `QuoteID` derived from
  `sha256(round_id || seal_pass || client_id)` that every
  downstream accept/reject/timeout echoes. Produced by
  `computeSealTimeQuotes` in `seal_time_fee_builder.go`.
- `computeSealTimeQuotes` — Pure function that turns a
  `ClientRegistrations` map + live market inputs (fee rate,
  treasury utilization, chain height) into one `Quote` per
  client, or a `QuoteReasonInsufficientResidual` /
  `QuoteReasonInvalidChangeDesignation` classifier when the
  client's intent cannot be admitted at the current pass. Sole
  fee authority under #270 — `validateOperatorFee` is removed.
- `Environment.QuoteTTL` / `MaxSealPasses` / `MaxClientRejects` —
  Governance knobs for the quote handshake. Defaults are 10s, 3
  passes, 3 rejects per client. Timeouts do not count against
  the reject cap (network flakes should not drop honest clients);
  only explicit rejects do.
- `Environment.SkipQuoteHandshake` — Test-only flag that
  short-circuits the quote fan-out and transitions
  `SealEvent` → `BatchBuildingState` directly. Set by pre-#270
  tests that don't exercise the handshake; production leaves
  this false.
- `SealPredicate` — Pure function `func(regs) bool` evaluated after each
  client join to decide if the round should seal early (before registration
  timeout). Defined in `seal_policy.go`. When a predicate fires, it emits
  `SealEvent`.
- `MaxClients(limit)` — Seal predicate that fires when `len(regs) >= limit`.
- `MaxOutputAmount(threshold)` — Seal predicate that fires when total output
  value reaches a satoshi threshold.
- `AnySealPredicate(preds...)` — Composite predicate returning true when any
  sub-predicate fires (logical OR).
- `TickEvent` — Internal periodic FSM event delivered at
  `ActorConfig.RoundTickInterval` cadence to the active round's FSM.
  `CreatedState` handles it by recording `skipped_empty` and staying
  open for the first client. `IntentCollectingState` defers to
  `SealPredicate` when configured, otherwise emits `SealEvent` to close
  registration. Each fire also emits a `RoundTickFiredReq` carrying a
  `TickResult` so operators can alert on stuck rounds (sustained
  `skipped_empty`) or measure tick-driven seal cadence.
- `TimeoutPhaseTick` — Timeout phase suffix used in
  `makeTimeoutID(roundID, TimeoutPhaseTick)` to namespace recurring-tick
  scheduling separately from the registration timeout. Lets a single
  timeout actor track both per-round entries without ID collision.
- `TickFiredMsg` (rounds package) — Actor message produced by
  `timeout.MapTickFired` when the timeout actor's recurring entry
  fires. The actor's `handleTickFired` parses the composite tick ID,
  drops stale fires for rounds it no longer tracks (best-effort
  self-cancel), and otherwise injects a `TickEvent` into the live FSM.
- `RoundTickFiredReq` — Outbox event emitted by the FSM on every
  TickEvent regardless of branch. Carries a `TickResult` (one of
  `TickResultSkippedEmpty`, `TickResultSkippedPredicate`,
  `TickResultSealed`). The actor forwards each fire to the metrics
  actor as a `RoundTickFiredMsg` for the per-result counter.
- `ActorConfig.RoundTickInterval` — Operator-configured cadence at
  which the actor schedules a recurring `TickEvent` for each newly-
  created round. Zero disables ticks (event-driven seals only). Read
  only at the actor scheduling layer (`scheduleRoundTick` and the
  gate in `newRoundFSM`); the FSM never reads the cadence — it only
  receives `TickEvent` messages.
- `cancelRoundTick(ctx, roundID)` — Actor helper that Tells a
  `CancelTimeoutRequest` for `makeTimeoutID(roundID, TimeoutPhaseTick)`.
  Centralizes the three cancel sites (`RoundSealedReq`,
  `RoundFailedReq`, stale-tick-for-unknown-round in `handleTickFired`).
  The timeout actor's `Cancel` is a no-op for unscheduled IDs, so the
  helper is safe regardless of whether ticks were configured.
- `VTXOEventPublisher` — Interface for publishing `VTXO_CREATED` lifecycle
  events to the indexer after a round confirms. `PublishVTXOCreated` takes
  the leaf pkScript, outpoint, value, round ID, **absolute** batch expiry
  height, relative expiry, origin, and commitment txid. Wired to the
  indexer layer via `vtxoEventPublisherAdapter` in `server_rounds.go`.
- `ValidateVTXORequest` — Exported function in `validation.go` that validates
  a client VTXO request (amount bounds, unique signing key, policy template
  decoding, pkScript presence). Dispatches to `validateStandardVTXOTemplate`
  or `validateCustomVTXOPolicy` based on the policy shape. Returns the
  resolved `*tree.VTXODescriptor` on success.
- `ValidateJoinRequestAtHeight(ctx, env, req, currentBlockHeight, existingRegCount)` —
  Validates a join request with explicit chain height and
  existing-registration count. Validation is structural only
  (signing keys, policy templates, change designation); fee
  authority lives in `computeSealTimeQuotes` at seal time, not at
  admission. The `existingRegCount` is preserved on the call signature
  for parity with the seal-time builder's batch-size divisor.
- `Environment.FeeCalculator` — `*fees.Calculator` consumed by
  `computeSealTimeQuotes` to compute boarding and forfeit fee
  components dynamically (amount, batch size, VTXO lifetime, current
  fee rate, treasury utilization). Required at `Actor.Start` under
  the seal-time handshake — there is no flat-fee fallback post-#270.
- `Environment.TreasuryTracker` — `*fees.TreasuryTracker` consumed by
  the fee calculator. Required at `Actor.Start` alongside
  `FeeCalculator` so quotes reflect the real capital position.
- `Environment.LedgerRef` — Optional `actor.TellOnlyRef[ledger.LedgerMsg]`
  wired by root. When set, the actor sends `RoundConfirmedMsg` (with
  `FundingOutpoints`, `ChangeOutpoints`, `BoardingNewSat`, `RefreshNewSat`)
  and `VTXOsForfeitedMsg` to the ledger actor via fire-and-forget Tell.
  `FundingOutpoints` and `ChangeOutpoints` are populated from the
  commitment PSBT at the `ServerSigning → FinalizedState` transition.
- `GetRoundStatusReq` / `GetRoundStatusResp` — Admin observability snapshot
  Ask/response pair. `GetRoundStatusReq` carries a `RoundID` to query; the
  actor responds with `GetRoundStatusResp` populated from the live FSM: state
  name, intent count, quote-phase counters (sent/accepted/rejected/timed-out),
  current seal pass, and quote-expiry timestamp. `RoundNotFound` is true when
  no live FSM exists for the given round (already finalized or never created).
  Wired via `AdminRPCServer.GetRoundStatus` which issues a synchronous Ask.
- `VTXO.BatchExpiry` — Absolute block height at which the source batch becomes
  sweepable. Populated at load time as `confirmation_height + csv_delay` from
  the source round (via `GetVTXOWithRoundExpiry`). OOR-derived VTXOs carry a
  persisted `batch_expiry` column stamped from `min(parent.batch_expiry)` at
  materialization. The seal-time fee builder reads this to compute
  `remainingBlocks = BatchExpiry - currentHeight` for the liquidity-fee leg.
  Zero when the source round is not yet confirmed or the load path does not
  populate it.
- `ClientRegistration.IntentVTXOReqs` / `IntentLeaveReqs` — Ordered slices of
  the original `VTXORequest` / `LeaveRequest` protos from the client intent.
  Preserves `IsChange` markers and positional order so `computeSealTimeQuotes`
  can locate the designated change output and stamp residual amounts in the
  same sequence the client submitted.
- `Round.ChangeOutputIdx` — FinalTx output index where `FundPsbt` put the
  wallet change, or -1 when no change was produced. Persisted in the
  `rounds` table (migration 000013) and restored on restart so the ledger
  classifier can short-circuit external_deposit booking for the change
  output without re-deriving it from the PSBT.
- `Round.ConnectorOutputIndices` — Sorted set of FinalTx output indices for
  operator-controlled connector outputs (dust outputs spent by forfeit txs).
  Persisted in `round_connector_outputs` (migration 000013) and carried
  through all FSM states so the classifier can attribute connector dust.
- `ConnectorTreeDescriptor.Radix` — Branching factor of the connector tree as
  it was built at round finalization time. Persisted to `round_connector_descriptors`
  so the fraud responder can reconstruct the exact connector path years after
  the fact regardless of config rotations. Loaded and stored by `db.RoundStoreDB`.
- `ErrVTXOBelowMinViable` — Returned by `quoteForClient` when a VTXO
  amount is below the economic viability threshold and
  `Schedule.MinViablePolicy` is set to `"reject"`. Surfaces as a
  `QuoteReasonInsufficientResidual` reject reason on the
  `JoinRoundQuote` so the client drops the intent.

## Relationships

- **Depends on**: `batch` (tx building, MuSig2 coordination), `batchwatcher`
  (confirmation monitoring), `clientconn` (outbound events to clients),
  `vtxo` (VTXO locking during rounds), `metrics` (round lifecycle
  instrumentation), `fees` (`Calculator`, `TreasuryTracker` for dynamic fee
  validation), `ledger` (optional `LedgerMsg` for accounting notifications).
  Interaction with `batchsweeper` is indirect:
  `rounds` registers batches with `batchwatcher`, which in turn notifies
  `batchsweeper`. Depends on the `rounds.VTXOEventPublisher` interface (not
  the indexer package itself); the adapter is provided by the root package.
- **Depended on by**: `indexer` (round event subscription), `lndbackend`
  (chain queries), root `darepo` (wiring).
- **Messages to/from**:
  - Receives JoinRoundRequest, nonces, partial sigs <- `clientconn` via
    `AddEnvelopeRoute` (fire-and-forget Tell from clients).
  - Sends round events, commitment tx, aggregated nonces -> `clientconn` (to
    clients via bridge egress).
  - Sends batch build requests -> `batch`.
  - Receives confirmation events <- `batchwatcher`.
  - Emits `RoundSealedReq` from `SealEvent` handler -> actor (triggers new
    round creation).
  - On confirmation, calls `VTXOEventPublisher.PublishVTXOCreated` per VTXO
    tree leaf -> indexer (fans out `IncomingVTXOEvent` to non-participant
    recipients so directed-send targets can discover and materialize their
    VTXOs).
  - Sends `RoundConfirmedMsg` (with `FundingOutpoints`, `ChangeOutpoints`,
    `BoardingNewSat`, `RefreshNewSat`) -> `ledger` on round confirmation
    (fire-and-forget via `LedgerRef`).
  - Sends `VTXOsForfeitedMsg` -> `ledger` when refresh VTXOs are forfeited
    (fire-and-forget via `LedgerRef`).
  - Proto→domain conversion helpers exported in `proto_convert.go` for use
    by server wiring layer (`server_rounds.go`).

## Invariants

- Tree signatures must be validated BEFORE input signatures are released.
- VTXO locks must be acquired before batch building and released on failure.
- Round state is checkpointed atomically; crash before checkpoint means no
  partial state persists.
- Boarding input signatures are only broadcast after all forfeit signatures
  are collected.
- **Boarding inputs are pre-added to the commitment PSBT with P2TR
  key-spend appearance before `FundPsbt`.** LND's `PsbtCoinSelect` path
  rejects taproot script-spend external inputs in
  `EstimateInputWeight` (`ErrScriptSpendFeeEstimationUnsupported`), so
  `buildCommitmentTx` initially attaches each boarding input with the
  real `WitnessUtxo`/`TaprootInternalKey`/`TaprootMerkleRoot` but an
  empty `TaprootBip32Derivation[0].LeafHashes` and no
  `TaprootLeafScript`. LND treats it as `TaprootKeySpendSignMethod`,
  counts the value in `inputSum`, and only adds wallet inputs to cover
  `outputs − Σboarding + fees`. After `FundPsbt` returns, the metadata
  is swapped (by `PreviousOutPoint` lookup, since LND may reorder) to
  the real script-spend layout via `boardingPInputScriptSpend`.
- **Witness-weight delta is billed against change, clamped at dust.**
  LND under-charges fees because it estimates each boarding input as
  `TaprootKeyPathWitnessSize` (~66 wu), but the real collab-tapscript
  witness is computed at runtime via
  `input.TxWeightEstimator.AddTapscriptInput`, fed a partial-reveal
  `*waddrmgr.Tapscript` built from each boarding input's actual leaf
  script and merkle inclusion proof
  (`boardingScriptSpendTapscript`). The change output is reduced by a
  single `feeRate.FeeForWeight(scriptW − keyW)` call (one truncation,
  never two) so the implicit miner fee lands at the script-spend level
  once the real witnesses are attached at finalization. The
  subtraction is clamped at `change.Value − P2WKH_dust_floor (294)`
  so a tight change output (multi-input round at high fee rate) can
  never be driven below dust or negative; any unrecovered delta lands
  implicitly in the miner fee.
- **No-change boarding rounds proceed with a warning, not an error.**
  When `FundPsbt` produces no change output for a boarding round
  (LND's coin selection determined the would-be change was below
  dust), the witness-weight-delta adjustment cannot be applied and
  the residual goes implicitly to miners as overpay. LND's
  coin-selection invariant bounds this overpay to its dust threshold
  (`changeAmt < dust_limit ≈ 294-546 sat`) — the bound is
  fee-rate-independent. The caller in `transitionToBatchBuilding`
  emits a `WarnS` and increments
  `metrics.RoundChangeRequiredForBoardingTotal` so operators can
  observe the case (e.g., as a signal to increase wallet liquidity)
  without the round failing. The `ErrChangeRequiredForBoarding`
  error type is kept defined for a future stricter fee policy to
  re-enable the failure path.
- TxProof validation (when no ChainSource) requires a non-nil `HeaderVerifier`
  and enforces `MinBoardingConfirmations` and `BoardingExitDelaySafetyMargin`
  checks matching the ChainSource path.
- `ValidateBoardingRequest` takes a `currentHeight` parameter for
  confirmation depth checks in both ChainSource and TxProof paths.
- `ErrTxProofFutureBlock` is returned when a TxProof block height exceeds
  the current chain height; this prevents a uint32 underflow in the
  confirmation subtraction.
- `ErrExitDelayBelowSafetyMargin` is returned when the policy exit delay is
  not strictly greater than the boarding safety margin, preventing a uint32
  underflow in the delay-path window check.
- `ErrVTXOPkScriptMissing` is returned when a VTXO request omits the
  pkScript; the server cross-checks client and server-derived taproot outputs
  even though it also derives the pkScript from the policy template.
- **Fee authority is the seal-time quote builder.** Fees are no
  longer validated at submit time. At `SealEvent`,
  `computeSealTimeQuotes` computes a binding `Quote` per client
  using `FeeCalculator.ComputeBoardingFee` / `ComputeForfeitFee`
  with live chain rate, treasury utilization, and true round
  occupancy. Clients accept (`JoinRoundAccept`) or reject
  (`JoinRoundReject`) explicitly; timeouts are treated as
  rejects. The FSM does not construct the PSBT / VTXO tree until
  every outstanding quote has resolved — nonces cannot stand in
  for acceptance because the tree does not yet exist.
- **Exactly one `IsChange=true` marker per intent.** The intent
  must carry one change-bearing output across its VTXORequests +
  LeaveRequests (or a single-output intent, where change is
  implicit). Enforced at admission in
  `validateChangeDesignation`; violations return
  `ErrInvalidChangeDesignation` as a `ClientErrorResp`. The quote
  builder stamps the residual
  (`Σin − Σ(fixed targets) − operator_fee`) on the designated
  change output and echoes verbatim target amounts for the
  others.
- **quote_id binds every downstream response to a specific seal
  pass.** `JoinRoundAccept`, `JoinRoundReject`, and
  `QuoteTimeoutEvent` carry the 32-byte quote_id the server
  issued. Stale quote_ids (from a prior pass after a reseal) are
  dropped silently at the FSM boundary.
- **Reseal cap = 3, per-client reject cap = 3.** Any reject or
  timeout fires a fresh `SealEvent` over the surviving accepted
  set; after `MaxSealPasses` the round finalizes with the last
  pass's accepted set. A client that sends more than
  `MaxClientRejects` explicit rejects is permanently dropped
  (locks released).
- **`FeeCalculator` requires `FeeEstimator`, `LedgerRef`, and
  `TreasuryTracker` all to be wired.** The actor enforces this at `Start`
  (fails fast rather than nil-deref on the first join).
- **`ChangeOutputIdx` defaults to -1, never 0.** A zero `ChangeOutputIdx`
  is a valid commitment tx output index (the VTXO tree root). Pre-migration
  rows read back as -1 via the column default, which the classifier treats
  as "no change output" — the safe fail-open behavior for restarts.
- **`ConnectorOutputIndices` are sorted before DB insertion** to keep the
  on-disk row layout deterministic and the PRIMARY KEY order reproducible.
- **Outpoint slices in `RoundConfirmedMsg` are populated from the PSBT** at
  the `ServerSigning → FinalizedState` transition where the PSBT is still in
  scope. Reloaded rounds (post-restart `FinalizedState`) have zero slices and
  the ledger handler skips pre-insertion for those messages.
- `allClientsSubmitted` in `AwaitingInputSigsState` requires every client
  that registered boarding inputs OR forfeit inputs to have completed both
  submissions before the round advances. Clients that have neither are not
  counted.
- **Recurring ticks are scheduled only from `newRoundFSM`** — the
  scheduling call deliberately lives outside `buildAndStartRoundFSM`
  because that helper is also called from `loadRoundFSM` to restore
  persisted rounds in `FinalizedState`, which has no `TickEvent`
  handler. Restoring a finalized round must not arm a ticker that
  would fire forever with no cancel path on the boot trajectory.
- **Tick cancellation is dual-redundant.** The actor cancels on
  `RoundSealedReq` and `RoundFailedReq` (terminal outbox events,
  authoritative). The FSM also emits a `CancelTimeoutReq` for
  `TimeoutPhaseTick` from the tick-driven seal branch in
  `IntentCollectingState` as defense in depth so the FSM stays
  self-consistent even if the actor is racing. Both cancels are
  no-ops on the timeout actor when the ID is not tracked.
- **Stale tick fires self-cancel.** `handleTickFired` for a roundID
  no longer in `a.rounds` (e.g. a fire that crossed a seal/fail)
  Tells a best-effort `CancelTimeoutRequest` so the underlying
  recurring entry stops, then drops the event. The FSM is never
  invoked for unknown rounds.
- Seal predicates are pure functions — they must not perform I/O or modify
  state. They are evaluated inside FSM transitions after each successful
  join.
- Side effects (batch building, signing, persistence) are inlined in FSM
  transition functions, not dispatched through a separate handler.
- Single-client refresh settlement: when only one client participates in a
  refresh round, the settlement path must still produce valid outputs and
  not skip signing.
- `RoundSealedReq` is emitted from a single canonical location (`SealEvent`
  handler in `RegistrationState`). No other code path emits this message.
- `ConnectorDustAmount` must be > 0 in round terms (default: 330 sats). Wired
  from config -> `batch.Terms`. Zero value causes refresh commitment
  assembly to fail (invalid connector leaf outputs).
- Round lifecycle is instrumented via metrics actor: `RoundCreatedMsg`,
  `ClientJoinedRoundMsg`, `RoundSealedMsg`, `PhaseStartedMsg`/`PhaseEndedMsg`,
  `RoundCompletedMsg`.
- Aggregated MuSig2 sigs are persisted on server VTXOTrees so they survive
  restarts and support batch sweep transactions.
- Swept batches transition VTXOs to Expired status via `batchsweeper` -> `db`.
- `publishVTXOEvents` runs after the batch watcher is registered on
  confirmation, iterates every non-nil VTXO tree, and emits one
  `IncomingVTXOEvent` per leaf. Publisher failures are logged at warn level
  and do not abort confirmation.
- `BatchExpiryHeight` passed to the publisher is **absolute**
  (`cs.BlockHeight + Terms.SweepDelay`), not the relative sweep delay.
- `CommitmentTxid` is taken from the signed `cs.FinalTx.TxHash()` so clients
  can distinguish the commitment tx from individual leaf txids.
- `Origin` is hard-coded to `arkrpc.VTXOOrigin_VTXO_ORIGIN_IN_ROUND` for
  confirmed round leaves.
- Forfeit spend paths must be non-nil (`ForfeitTxSig.SpendPath`); the
  `completeForfeitTxs` call rejects nil paths before any signing occurs.
- `signForfeitVTXOInput` validates the spend path against the VTXO's AST via
  `ensureForfeitSpendPathCommitsOperator` both at validation time and at sign
  time (defense in depth). The operator will not sign a path that does not
  commit its key in the AST. **Returns a `forfeitVTXOSignResult` (witness +
  midstate) rather than attaching the witness to the tx directly** — the
  witness is deferred until after `signForfeitConnectorInput` also returns, so
  the tx remains witness-free across both `SignOutputRaw` calls (lndclient
  serializes existing witnesses into the PSBT, which remote-signer LND rejects).
- `verifyCompletedForfeitVTXOInput` runs the assembled forfeit witness through
  the script engine after **both** witnesses are attached; rejection before
  broadcast.
- **Forfeit tx witness is cleared before signing** (`ftx.TxIn[i].Witness = nil`
  for all inputs). This mirrors `batchsweeper/sweep.go`'s `signSweepInputs`
  to prevent remote-signer LND from rejecting witness-bearing PSBT inputs.
- Forfeit penalty output amount validation now includes the connector leaf
  output value (`VTXO.Amount + connector.Value`) per the protocol spec, not
  only the VTXO amount. A nil `VTXO.Descriptor` is now a hard validation error.
- `BoardingRequest.PolicyTemplate` must be non-empty; the boarding validation
  path derives the expected pkScript from the policy template rather than
  assuming the standard VTXO collab leaf shape.

## Deep Docs

- [rounds/README.md](README.md) — Full state machine walkthrough with diagrams.
- [client/docs/fee-change-model.md](../client/docs/fee-change-model.md) — Seal-time fee handshake scenario catalogue (proto contract, change-designation rules, 11 protocol scenarios). Source-of-truth narrative for `computeSealTimeQuotes` and `resolveChangeDesignation`.
- [docs/authoritative_locking.md](../docs/authoritative_locking.md) — Server-side locking model: ownership rules, FSM ordering, recovery invariants.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
