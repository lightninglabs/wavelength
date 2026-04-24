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
- `SealEvent` — Canonical internal event that transitions `RegistrationState`
  -> `BatchBuildingState` and emits `RoundSealedReq`. Fired by registration
  timeout, seal predicate, or admin `TriggerBatch` RPC. Single emission point
  prevents duplicate round creation.
- `SealPredicate` — Pure function `func(regs) bool` evaluated after each
  client join to decide if the round should seal early (before registration
  timeout). Defined in `seal_policy.go`. When a predicate fires, it emits
  `SealEvent`.
- `MaxClients(limit)` — Seal predicate that fires when `len(regs) >= limit`.
- `MaxOutputAmount(threshold)` — Seal predicate that fires when total output
  value reaches a satoshi threshold.
- `AnySealPredicate(preds...)` — Composite predicate returning true when any
  sub-predicate fires (logical OR).
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
  Validates a join request with explicit chain height and existing-registration
  count. The `existingRegCount` feeds the `batchSize = existingRegCount + 1`
  divisor for at-cost fee sizing under the default non-subsidy mode. Replaces
  the old 4-arg form (callers must now pass the current registration count).
- `Environment.FeeCalculator` — Optional `*fees.Calculator`; when set,
  `validateOperatorFee` computes the required fee dynamically for both boarding
  and forfeit inputs (amount, batch size, VTXO lifetime, current fee rate,
  treasury utilization). When nil, falls back to flat `Terms.MinOperatorFee`.
- `Environment.SubsidizeThinRounds` — When true, `validateOperatorFee` sizes
  the on-chain cost share against `MaxVTXOsPerTree` (legacy pre-#268 subsidy
  behavior). When false (default), it charges at the actual registered
  participant count (`existingRegCount + 1`), so thin rounds pay full per-input
  cost. Propagated from `ActorConfig.SubsidizeThinRounds` into each round's
  `Environment`.
- `Environment.TreasuryTracker` — Optional `*fees.TreasuryTracker`; required
  when `FeeCalculator` is set. Feeds current utilization into congestion
  pricing so quotes reflect the real capital position.
- `Environment.LedgerRef` — Optional `actor.TellOnlyRef[ledger.LedgerMsg]`
  wired by root. When set, the actor sends `RoundConfirmedMsg` (with
  `FundingOutpoints`, `ChangeOutpoints`, `BoardingNewSat`, `RefreshNewSat`)
  and `VTXOsForfeitedMsg` to the ledger actor via fire-and-forget Tell.
  `FundingOutpoints` and `ChangeOutpoints` are populated from the
  commitment PSBT at the `ServerSigning → FinalizedState` transition.
- `Round.ChangeOutputIdx` — FinalTx output index where `FundPsbt` put the
  wallet change, or -1 when no change was produced. Persisted in the
  `rounds` table (migration 000013) and restored on restart so the ledger
  classifier can short-circuit external_deposit booking for the change
  output without re-deriving it from the PSBT.
- `Round.ConnectorOutputIndices` — Sorted set of FinalTx output indices for
  operator-controlled connector outputs (dust outputs spent by forfeit txs).
  Persisted in `round_connector_outputs` (migration 000013) and carried
  through all FSM states so the classifier can attribute connector dust.
- `ErrVTXOBelowMinViable` — Returned by `validateOperatorFee` when a VTXO
  amount is below the economic viability threshold and `Schedule.MinViablePolicy`
  is set to `"reject"`. Dynamic fee path only; flat-fee path does not check
  per-VTXO viability.

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
- **Fee validation is dynamic when `FeeCalculator` is configured.** The
  `validateOperatorFee` function computes the required fee for **both boarding
  and forfeit inputs**: boarding via `FeeCalculator.ComputeBoardingFee`, forfeits
  via `FeeCalculator.ComputeForfeitFee`. Refresh, leave, and directed-send rounds
  all carry forfeit inputs and now pay dynamic fees (#269 closes the pre-existing
  gap where forfeit-only rounds skipped fee validation). Each VTXO is also
  checked against `MinViableAmount`; when `MinViablePolicy=reject`, sub-viable
  VTXOs return `ErrVTXOBelowMinViable`. When `FeeCalculator` is nil, the flat
  `Terms.MinOperatorFee` is used as a backward-compatible fallback.
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
  commit its key in the AST.
- `verifyCompletedForfeitVTXOInput` runs the assembled forfeit witness through
  the script engine after signing; a witness that fails script validation is
  rejected before broadcast.
- `BoardingRequest.PolicyTemplate` must be non-empty; the boarding validation
  path derives the expected pkScript from the policy template rather than
  assuming the standard VTXO collab leaf shape.

## Deep Docs

- [rounds/README.md](README.md) — Full state machine walkthrough with diagrams.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
