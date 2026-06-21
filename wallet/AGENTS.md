# wallet

## Purpose

Manages on-chain boarding addresses (2-of-2 multisig with operator + CSV
timeout), monitors for confirmed boarding UTXOs, composes cooperative
intent packages, and gates round registration through the VTXO manager
admission APIs. The wallet actor owns VTXO selection and locking for
refresh, leave, OOR spend, and directed send flows.

## Key Types

- `Ark` — Main actor managing boarding addresses, UTXO enumeration, confirmation polling, admission forwarding, and VTXO selection/locking. Holds a `ledgerSink` field (`fn.Option[ledger.Sink]`) used by the `emitUTXOCreated` helper to Tell `UTXOCreatedMsg` to the ledger actor whenever a confirmed wallet UTXO is observed.
- `NewArk` — Constructor; takes the `ledgerSink` as a **required** argument (`fn.Option[ledger.Sink]`) rather than a setter, so every call site is forced to make an explicit emission choice. Production passes `fn.Some(ledger.NewSink(actorSystem))`; harnesses and unit tests that do not register a ledger actor pass `fn.None[ledger.Sink]()`.
- `emitUTXOCreated(ctx, utxo, blockHeight, classification)` — Internal helper that null-safely builds a `ledger.UTXOCreatedMsg` from a wallet `Utxo` and Tells it to `ledgerSink`. Negative block heights clamp to `0` rather than wrapping under a direct `uint32` cast; nil `utxo` and `fn.None` sink are silent no-ops.
- `LockID` — `[32]byte` caller-scoped output lease identifier used to associate leased UTXOs with a specific subsystem (`txconfirmLockID` in `txconfirm`, etc.).
- `OutputLeaser` — Interface for UTXO output leasing: `LeaseOutput(ctx, outpoint, lockID, expiry)` and `ReleaseOutput(ctx, outpoint, lockID)`. Implemented by all three `BoardingBackend` implementations (`btcwbackend`, `lndbackend`, `lwwallet`) to coordinate cross-subsystem UTXO reservation.
- `BoardingBackend` — Interface for wallet integration (key derivation, taproot import, ListUnspent). `GetTransaction` returns `*TxInfo` (containing tx, block hash, and block height).
- `TxInfo` — Struct wrapping a confirmed transaction with its block hash and block height. Returned by `BoardingBackend.GetTransaction`.
- `BoardingStore` — Interface for persisting boarding addresses and intents. Embeds `PendingIntentStore`.
- `PendingIntent` / `PendingIntentKind` / `PendingIntentID` / `PendingIntentPayload` / `PendingIntentStore` — the generic restart-safe intent outbox (`pending_intent.go`). `Payload` is a sealed `PendingIntentPayload` interface holding the concrete kind-specific struct (not bytes); the db layer stores its fields as typed columns. An intent is persisted BEFORE its round-actor publish, anchored to the outpoints the round consumes on adoption (confirmed boarding outpoints for `board`, reserved forfeit outpoints for `send_onchain`); the round checkpoint clears anchors transactionally. `NewPendingIntentID(payload, anchors)` derives the idempotency key as `sha256(kind ‖ sorted anchors ‖ payload-canonical-digest)`.
- `PendingIntentReplayer` — per-kind replay strategy registered on the `Ark` actor (`intentReplayers`). `boardIntentReplayer` reconciles anchors against still-Confirmed boarding intents and self-Tells one `BoardRequest`; `sendOnChainIntentReplayer` self-Tells one `ReplaySendOnChainIntent` per persisted intent, whose handler re-reserves the EXACT anchors (never re-running selection), rebuilds the package via `buildSendOnChainIntentPackage`, and registers with `TriggerRegistration=true`. A reservation failure marks a send intent stale and deletes it.
- `BoardIntentPayload` / `SendOnChainIntentPayload` — concrete `PendingIntentPayload` implementations (`pending_intent.go`); each contributes a deterministic `writeIDDigest` field encoding (for the intent ID only) and is stored as typed columns by the db layer.
- `ReplayPendingIntentsRequest` / `ReplayPendingIntentsResponse` — daemon startup Ask that walks the replayer registry once every dependent actor is on the receptionist (replaces the Board-only `ReplayPendingBoardRequest`).
- `ReplaySendOnChainIntent` — internal self-Tell carrying one persisted send intent from the replay pass into the wallet mailbox.
- `VTXOReader` — Read-only interface for loading VTXO descriptors by outpoint. Wallet uses this to build intent packages without importing `vtxo` directly.
- `VTXODescriptor` — Wallet-level VTXO descriptor (outpoint, amount, pkscript, tree, expiry). Avoids direct dependency on `vtxo.Descriptor`.
- `SelectedVTXO` — Describes a VTXO selected and locked for use as a transfer input (outpoint, amount, pkscript). Breaks the vtxo → round → wallet import cycle.
- `CreateBoardingAddressRequest` / `CreateBoardingAddressResponse` — Ask-request for deriving new address.
- `BlockEpochNotification` — Tell-message from chain source triggering UTXO polling.
- `BoardingUtxoConfirmedEvent` — Tell-message sent when a VTXO confirms.
- `BoardRequest` / `BoardResponse` — Ask-request from RPC to trigger boarding flow.
- `GetBoardingBalanceResponse` — Balance breakdown with fields: `Balance` (confirmed), `UtxoCount`, `UnconfirmedBalance` (zero-conf), `UnconfirmedUtxoCount`, `AdoptedBalance` (accepted into round, VTXOs not yet live), `PendingSweepBalance`, `SweepPendingCount`.
- `RefreshVTXOsRequest` — Ask-request to select VTXOs for refresh and compose intent package. Carries `OperatorFees map[wire.OutPoint]btcutil.Amount`; when non-empty, the handler validates each fee is non-negative and below the VTXO amount, then subtracts it from the new VTXO output before registering with the round actor. Empty map is pre-#269 zero-fee behavior (tests, legacy paths).
- `SelectAndLockVTXOsRequest` — Ask-request to select and lock VTXOs for OOR spend. `MinChangeAmount`, when positive, asks selection to avoid a non-zero residual below that amount (exact spends still valid).
- `LeaveVTXOsRequest` — Ask-request to select VTXOs for cooperative leave. Carries a singular `DestOutput *wire.TxOut` plus a per-outpoint `DestOutputs map[wire.OutPoint]*wire.TxOut` override map; the handler picks `DestOutputs[op]` when set and falls back to `DestOutput`. Per-input operator fees are no longer pre-quoted on the client — under the #270 seal-time fee handshake the server stamps the residual onto the IsChange=true leave output at seal time, so the wallet ships the full forfeited amount on each leave output.
- `CompleteSpendVTXOsRequest` — Tell-message to finalize spend and release locks.
- `UnlockVTXOsRequest` — Tell-message to release locked VTXOs on failure.
- `SendRecipient` — Describes a single directed send destination (pkscript, amount, recipient client key).
- `SendVTXOsRequest` / `SendVTXOsResponse` — Ask-request for in-round directed sends. Validates each recipient amount is within `(0, MaxSatoshi]` and that the running total never overflows `int64`, atomically selects and reserves VTXOs via `SelectAndReserveForfeitRequest`, builds forfeit + recipient VTXO intents, and registers with the round actor. Supports dry-run mode for previewing coin selection without committing. Reserved VTXOs are released via a deferred cleanup that uses `context.WithoutCancel` so cleanup survives caller disconnect; on success, a `committed` flag is set to skip the release.
- `SendOnChainRequest` — Ask-request to plan and submit an atomic on-chain payment from VTXOs. Supports two modes: bounded send (`TargetAmountSat` > 0, empty `SweepOutpoints`) and sweep-all (`SweepOutpoints` non-empty). Bounded mode selects VTXOs with headroom for `OperatorFee + DustLimit` and creates a change VTXO. Sweep-all drains the exact outpoints to the destination with no change. Supports `DryRun` mode.
- `SendOnChainResponse` — Response to `SendOnChainRequest` carrying the selected outpoints, total amount, operator fee, and leave output details.
- `SendOnChainStatus` — Terminal outcome enum: `SendOnChainStatusSubmitted` (intent queued for next round), `SendOnChainStatusDryRun` (dry-run preview, no commitment).
- `GetConfirmedBoardingIntentsRequest` / `GetConfirmedBoardingIntentsResponse` — Ask-request to retrieve currently confirmed boarding intents (used by the RPC/CLI layer to report boarding balance with policy metadata).
- `VTXODescriptor.EffectivePolicyTemplate` — Decodes the serialized `PolicyTemplate` field on the wallet-level VTXO descriptor using `lib/arkscript`.
- `WalletBackingSweeper` — Interface for the general backing-wallet sweep backend: lists confirmed UTXOs, leases selected inputs, signs and finalizes the aggregate sweep PSBT. Satisfied by the same per-backend adapter wired as the boarding-sweep signer.
- `SweepWalletFundsRequest` / `SweepWalletFundsResponse` — Ask-request to preview (and optionally broadcast) a general sweep of all confirmed backing-wallet UTXOs to a single destination address. `DestinationAddress`, optional `FeeRateSatPerVByte` (capped at the actor's configured max), optional `ConfTarget` (falls back to default). Response carries `Inputs`, `TotalAmountSat`, `FeeAmountSat`, `NetAmountSat`, `CanBroadcast`, and `Txid` (populated on successful broadcast). `FailureReason` is set (and the actor response itself succeeds) for application-level failures so the RPC sees them as domain errors rather than transport errors.
- `WalletSweepInputInfo` — Per-UTXO sweep detail: `Outpoint`, `PkScript`, `AmountSat`.
- `WalletSweepTxNotification` — Tell carrying a txconfirm terminal notification (`TxConfirmed` or `TxFailed`) for a general wallet sweep tx, re-wrapped via `txconfirm.MapNotification`.
- `WithWalletSweep(sweeper, maxFeeRate)` — Option to wire the `WalletBackingSweeper` into the actor; when omitted `SweepWalletFundsRequest` returns a clear "subsystem not initialised" error.

## Relationships

- **Depends on**: `baselib/actor` (actor system), `chainsource` (block epoch notifications), `lib/actormsg` (VTXO manager admission types), `ledger` (`Sink` alias for emission + `UTXOCreatedMsg` / `ClassificationDeposit` constants).
- **Depended on by**: `round` (boarding intents, types: `BoardingAddress`, `SelectedVTXO`), `db` (persistence), `darepod` (wiring).
- **Sends**:
  - → `round` (via registered notifier): `BoardingUtxoConfirmedEvent`
  - → `round` (via `lib/actormsg`): `TriggerBoardMsg` (VTXO amounts for
    boarding), `RegisterIntentMsg` (pre-composed cooperative intents with
    forfeits + VTXOs/leaves); `TriggerRegistration=true` for directed sends
    so the round FSM advances from `PendingRoundAssembly` immediately,
    `false` for refresh/leave batching
  - → `vtxo` manager (via `lib/actormsg`): `SelectAndReserveSpendRequest`, `ReleaseSpendRequest`, `CompleteSpendRequest`, `ReserveForfeitRequest`, `ReleaseForfeitRequest`, `SelectAndReserveForfeitRequest`
  - → `ledger` actor (via `ledger.Sink` Tell, when `fn.Some`): `UTXOCreatedMsg` on every processed confirmed wallet UTXO, tagged `ClassificationDeposit`. `handleUTXOCreated` expands this into both a `wallet_utxo_log` audit row AND a double-entry deposit leg (debit `wallet_balance`, credit `opening_balance`). (`UTXOSpentMsg` emission is a planned follow-up.)
  - → `round` (via `lib/types.VTXORequest.Origin`): wallet intent composition tags each locally-owned VTXO output with a `VTXOOrigin` classifier so the round actor's downstream ledger emission dispatches to the correct `Source`. Refresh outputs and directed-send self-change get `VTXOOriginRoundRefresh`; boarding-path tagging lives in `round.handleTriggerBoard` (`VTXOOriginRoundBoarding`).
- **Receives**:
  - ← `chainsource`: `BlockEpochNotification` (triggers UTXO polling)
  - ← `round`: `RegisterConfirmationNotifierRequest`, `UnregisterConfirmationNotifierRequest`
  - ← API: `CreateBoardingAddressRequest`, `GetActiveBoardingAddressesRequest`, `GetBoardingBalanceRequest`, `GetConfirmedBoardingIntentsRequest`, `RefreshVTXOsRequest`, `SelectAndLockVTXOsRequest`, `LeaveVTXOsRequest`, `BoardRequest`, `CompleteSpendVTXOsRequest`, `UnlockVTXOsRequest`, `SendVTXOsRequest`, `SendOnChainRequest`, `SweepWalletFundsRequest`
  - ← `txconfirm` (Tell): `WalletSweepTxNotification` (terminal outcome for a submitted general wallet sweep tx)

## Invariants

- UTXO confirmation requires `MinBoardingConfs` (1) on-chain confirmations.
- `ListUnspent` runs at most once per tip-tick against the latest known chain tip; a backend whose UTXO reporting lags past one tick interval surfaces the missing UTXO on the next chain advance (whichever tick processes the new tip re-runs the scan). The per-block path no longer carries an inline retry budget — the tick loop is the retry seam.
- Notifier registration captures `minConf` parameter per actor; different actors can require different confirmation depths.
- Cooperative admission (refresh/leave) must reserve forfeit inputs through the VTXO manager before sending `RegisterIntentMsg` to the round actor.
- Persist-before-publish: `handleBoard` and `handleSendOnChain` write their pending intent to the outbox BEFORE the round-actor publish, so every crash window either leaves no row (clean retry) or a replayable row. `handleSendOnChain` deletes the row when registration fails synchronously — the caller saw an error, so the send must not silently resurrect on the next start.
- `WithEagerRoundJoin(true)` opts the wallet into "drive round-joining without a second RPC" semantics. Two sites change: `handleBlockEpoch` inline-calls `handleBoard` after at least one new boarding UTXO confirms in the block (one `TriggerBoardMsg` per block, not per UTXO), and `handleLeaveVTXOs` forwards its `RegisterIntentMsg` with `TriggerRegistration=true` so the leave moves the round FSM out of `PendingRoundAssembly` immediately. Default off preserves the operator-driven batched semantics that `darepocli` and server hosts rely on; `sdk/walletdk` flips it on via `darepod.Config.EagerRoundJoin` for wallet-shaped SDK hosts.
- If round registration fails after successful admission, the wallet releases the forfeit reservation so VTXOs return to LiveState.
- Directed sends use `SelectAndReserveForfeitRequest` (cooperative forfeit path) rather than the OOR spend path. The wallet builds recipient VTXOs with the recipient's key as `OwnerKey` and derives a separate ephemeral `SigningKey` for MuSig2 tree construction.
- Local ownership of a round-produced VTXO is no longer tracked with a per-intent `IsOwner` flag. `types.VTXORequest` / `round.VTXOIntent` no longer carry `IsOwner`; at round confirmation time the round FSM asks a `round.OwnedScriptChecker` (backed in production by the OOR owned-receive-scripts store) which pkScripts to persist as local balance. The wallet's only job is to supply the correct `OwnerKey` per intent — local-origin owner keys keep their populated `KeyLocator` so `handleRegisterIntent` registers them via `OwnedScriptRegistrar`, while remote recipients carry a zero `KeyLocator` and are intentionally left unregistered.
- `handleSendVTXOs` uses a `defer`-based release rather than a `releaseAndFail` helper: any error path (including dry-run) falls through to the deferred release, and the `committed` flag is set only after the round actor accepts the intent. Context is preserved via `context.WithoutCancel` so cleanup is not dropped when the caller disconnects.
- `handleSendVTXOs` rejects pre-flight any directed send with multiple recipients and exactly-zero change residual under the #270 seal-time fee handshake. The server is the amount authority and absorbs the operator fee out of the designated `IsChange=true` slot; if there is no residual to absorb the fee against, the server has no slack to deduct fees without silently shifting them onto a recipient leg. The wallet refuses the request rather than letting the server pick the loser.
- `VTXOReader` / `VTXODescriptor` / `SelectedVTXO` break the vtxo → round → wallet import cycle by providing wallet-level types that don't reference `vtxo.Descriptor` directly.
- Per-subsystem logging via `build.LoggerFromContext` (no global mutable loggers).

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
