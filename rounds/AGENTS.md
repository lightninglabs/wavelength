# rounds

## Purpose

Server-side Ark round lifecycle FSM coordinating client registration, batch
transaction building, MuSig2 signing, finalization, and on-chain confirmation
monitoring.

## Key Concepts

Use `go doc rounds.<Symbol>` for signatures and exhaustive bullets. This map
covers the non-obvious wiring only.

- **FSM** — `Actor` drives one `State` per round through `Event` transitions;
  side effects emit `OutboxEvent`s. State (Created → IntentCollecting →
  QuoteSent → BatchBuilding → ServerSigning → Finalized → Confirmed/Failed) is
  checkpointed atomically; partial state never persists across crashes.
- **Seal-time fee handshake (#270)** — `SealEvent` (fired by tick predicate,
  registration timeout, or admin `TriggerBatch`) calls `computeSealTimeQuotes`
  to issue one `Quote` per client over `QuoteSentState`. Clients
  accept/reject; rejects/timeouts respin a fresh seal pass (capped by
  `Environment.MaxSealPasses` = 3 and `MaxClientRejects` = 3). Fee authority
  lives **only** in the quote builder — `validateOperatorFee` is gone.
  `Environment.SkipQuoteHandshake` short-circuits for pre-#270 tests.
- **Recurring ticks** — `Actor.scheduleRoundTick` arms one timeout entry per
  newly-created round at `ActorConfig.RoundTickInterval`. Each fire delivers
  `TickEvent` (drives empty-round skipping and seal-predicate evaluation) and
  emits `RoundTickFiredReq` carrying a `TickResult`. Scheduling lives in
  `newRoundFSM` (not `buildAndStartRoundFSM`) so restored rounds in
  `FinalizedState` don't arm a ticker with no cancel path. Tick IDs are
  namespaced by `TimeoutPhaseTick`; cancellation is dual-redundant
  (`RoundSealedReq` / `RoundFailedReq` at the actor, plus FSM emission of
  `CancelTimeoutReq`) and stale fires self-cancel.
- **Per-client admission limiting** — `allowJoinRequest(clientID)` gates
  `JoinRoundRequest` via a token bucket
  (`ActorConfig.JoinRequestRate`/`JoinRequestBurst`, defaults `DefaultJoinRequestRate` =
  1/2s and `DefaultJoinRequestBurst` = 3). Bursts return `ClientErrorResp` without
  touching the FSM; state is reaped when the client's registration is removed.
- **Validation surface** — `ValidateJoinRequestAtHeight` does structural
  checks only (signing keys, policy templates, change designation, pkScript
  presence). Fee authority is at seal time. Notable errors:
  `ErrSigningKeyMatchesOperator` (x-only collision would let one party forge
  the other's forfeit), `ErrSigningKeyMissing`, `ErrVTXOPkScriptMissing`,
  `ErrInvalidChangeDesignation`, `ErrTxProofFutureBlock`,
  `ErrExitDelayBelowSafetyMargin`. **`ErrDuplicateForfeitRequest`** fires when
  one join request lists the same outpoint twice;
  **`ErrDuplicateForfeitInRound`** fires when a join request references a
  forfeit input already registered by *another client* in the same round.
- **Boarding input locker** — `BoardingInputLocker` blocks concurrent rounds
  from racing on the same outpoint. `validateBoardingInput` inspects
  `lockedBy` so a same-round re-registration (replacement path in
  `IntentCollectingState`) accepts its own previously-locked inputs.
- **SweepKey persistence** — Operator's sweep `keychain.KeyDescriptor` is
  persisted per round (`sweep_key_family`/`sweep_key_index` columns in
  migration `000002`) and threaded `FinalizedState` → `ConfirmedState` →
  `RegisterBatchRequest`. `restoreSweepKey` distinguishes three states (see
  `actor.go`): full descriptor (new rows), pubkey + zero locator
  (pre-migration rows — "locator unknown", refuse to sign with the configured
  key), and zero descriptor (`round.SweepKey == nil`, very old test fixtures
  only).
- **Correlation keys** — Every round-bound `ClientMessage`
  (`ClientSuccessResp`, `ClientAwaitingInputSigsResp`, `ClientVTXOAggNonces`,
  `ClientVTXOAggSigs`, `ClientBatchInfo`) returns
  `roundClientCorrelationKey(clientID, roundID)` from `CorrelationKey()` to
  keep per-client/round delivery FIFO-ordered across transient failures.
  `ClientErrorResp` returns empty.
- **Admin observability** — `GetRoundStatusReq`/`Resp` Ask pair returns FSM
  state, intent count, quote-phase counters, seal pass, and quote expiry.
  Returns `RoundNotFound = true` for already-finalized or never-created
  rounds.
- **TxProof validation** — `Environment.HeaderVerifier` is required when no
  `ChainSource` is configured; both paths enforce `MinBoardingConfirmations`
  and `BoardingExitDelaySafetyMargin`.
- **Side-effect plumbing** — `VTXOEventPublisher` fans out `VTXO_CREATED` to
  the indexer; `Environment.LedgerRef` (optional) sends `RoundConfirmedMsg`
  and `VTXOsForfeitedMsg` to the ledger. Both are fire-and-forget Tells.

## Relationships

- **Depends on**: `batch` (tx building, MuSig2), `batchwatcher` (confirmation),
  `clientconn` (client egress), `vtxo` (lock store), `metrics`, `fees`
  (`Calculator` + `TreasuryTracker`, both required at `Actor.Start`),
  `ledger` (optional).
- **Depended on by**: `indexer`, `lndbackend`, root `darepo` wiring.
- **Messages**:
  - `JoinRoundRequest` / nonces / partial sigs ← `clientconn` (via
    `AddEnvelopeRoute`; proto conversion in `proto_convert.go`).
  - Round events / commitment tx / agg nonces → `clientconn` egress.
  - `RegisterBatchRequest` (with `SweepKey`) → `batchwatcher`; confirmation
    events ← `batchwatcher`.
  - `RoundSealedReq` → actor self (single emission point in `SealEvent`
    handler; triggers next-round spawn).
  - `PublishVTXOCreated` per leaf → indexer on confirmation (absolute
    `BatchExpiryHeight = cs.BlockHeight + Terms.SweepDelay`; origin
    hard-coded to `VTXO_ORIGIN_IN_ROUND`).
  - `RoundConfirmedMsg` (with funding/change outpoints, `BoardingNewSat`,
    `RefreshNewSat`) and `VTXOsForfeitedMsg` → ledger via `LedgerRef`.

## Invariants

- Tree signatures must validate **before** input signatures are released.
- VTXO locks acquired before batch building, released on failure.
- Boarding input signatures broadcast only after **all** forfeit sigs are
  collected.
- **Boarding PSBT keyspend-then-script swap**: boarding inputs are attached
  to the commitment PSBT with `TaprootKeySpendSignMethod` appearance so LND's
  `PsbtCoinSelect` accepts them (it rejects taproot script-spend external
  inputs in `EstimateInputWeight`). After `FundPsbt` returns, metadata is
  swapped to the real script-spend layout (`boardingPInputScriptSpend`,
  matched by `PreviousOutPoint` since LND may reorder).
- **Witness-weight delta clamped at dust**: LND under-charges fees by sizing
  each boarding input as `TaprootKeyPathWitnessSize` (~66wu); the real
  collab-tapscript witness is computed via
  `input.TxWeightEstimator.AddTapscriptInput` over a partial-reveal
  `*waddrmgr.Tapscript` per input, and `change` is reduced by one
  `feeRate.FeeForWeight(scriptW − keyW)` call clamped at the P2WKH dust floor
  (294 sats). Any unrecovered delta lands implicitly in the miner fee.
- **No-change boarding rounds proceed with a warning**: when `FundPsbt` puts
  the would-be change under dust, the witness-weight adjustment can't apply
  and the residual goes to miners (bounded by LND's dust threshold,
  ~294-546 sat, fee-rate-independent). `transitionToBatchBuilding` `WarnS`s
  and increments `metrics.RoundChangeRequiredForBoardingTotal`;
  `ErrChangeRequiredForBoarding` is retained for a future stricter policy.
- Exactly one `IsChange=true` marker per intent
  (`validateChangeDesignation`); quote builder stamps residual on it.
- `quote_id` binds every accept/reject/timeout to its seal pass; stale
  quote_ids drop silently at the FSM boundary.
- `FeeCalculator` requires `FeeEstimator`, `LedgerRef`, and
  `TreasuryTracker` all wired — enforced at `Actor.Start`.
- `Round.ChangeOutputIdx` defaults to `-1` (a zero is a valid PSBT index);
  pre-migration rows read back as `-1` via column default.
- `Round.ConnectorOutputIndices` are sorted before DB insert for deterministic
  row layout.
- `RoundConfirmedMsg` outpoint slices are populated **at**
  `ServerSigning → FinalizedState` while the PSBT is in scope; reloaded
  rounds carry zero slices and the ledger handler skips pre-insertion.
- `allClientsSubmitted` (`AwaitingInputSigsState`) requires every client with
  boarding **or** forfeit inputs to complete both submissions; clients with
  neither don't count.
- Forfeit signing: `SpendPath` non-nil before signing,
  `ensureForfeitSpendPathCommitsOperator` validated at both validation and
  sign time, witness cleared before signing (mirrors
  `batchsweeper/sweep.go`), witness deferred to a `forfeitVTXOSignResult`
  until both VTXO and connector inputs are signed (lndclient serializes
  existing witnesses into the PSBT, which remote-signer LND rejects),
  `verifyCompletedForfeitVTXOInput` runs the script engine after both
  witnesses attach.
- Forfeit penalty includes the connector leaf value
  (`VTXO.Amount + connector.Value`), per protocol spec. Nil `VTXO.Descriptor`
  is a hard error.
- `BoardingRequest.PolicyTemplate` is required; the boarding path derives
  the expected pkScript from the template rather than assuming the standard
  collab leaf.
- Seal predicates (`SealPredicate`, `MaxClients`, `MaxOutputAmount`,
  `AnySealPredicate`) are pure — no I/O, no state mutation. Evaluated after
  each join.
- `RoundSealedReq` is emitted from one canonical site (`SealEvent` handler).
- `ConnectorDustAmount > 0` (default 330 sats); zero breaks refresh
  commitment assembly.
- Per-round metrics: `RoundCreatedMsg`, `ClientJoinedRoundMsg`
  (suppressed by `IsReregistration` for replacement joins), `RoundSealedMsg`,
  `PhaseStartedMsg`/`PhaseEndedMsg`, `RoundCompletedMsg`,
  `RoundTickFiredMsg` (per `TickResult`).

## Deep Docs

- [rounds/README.md](README.md) — Full state machine walkthrough with
  diagrams.
- [client/docs/fee-change-model.md](../client/docs/fee-change-model.md) —
  Seal-time handshake scenario catalogue (proto contract, change rules, 11
  scenarios).
- [docs/authoritative_locking.md](../docs/authoritative_locking.md) —
  Server-side locking model.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide map.
