# wallet

## Purpose

Manages on-chain boarding addresses (2-of-2 multisig with operator + CSV
timeout), monitors for confirmed boarding UTXOs, composes cooperative
intent packages, and gates round registration through the VTXO manager
admission APIs. The wallet actor owns VTXO selection and locking for
refresh, leave, OOR spend, and directed send flows.

## Key Types

- `Ark` — Main actor managing boarding addresses, UTXO enumeration, confirmation polling, admission forwarding, VTXO selection/locking, and the boarding-sweep subsystem. Holds a `ledgerSink` field (`fn.Option[ledger.Sink]`) used by the `emitUTXOCreated` helper to Tell `UTXOCreatedMsg` to the ledger actor whenever a confirmed wallet UTXO is observed.
- `NewArk` — Constructor; takes the `ledgerSink` as a **required** argument (`fn.Option[ledger.Sink]`) plus variadic `...ArkOption`. Production passes `fn.Some(ledger.NewSink(actorSystem))` and `wallet.WithBoardingSweep(...)` to wire the sweep subsystem; harnesses that do not register a ledger actor pass `fn.None[ledger.Sink]()`.
- `ArkOption` / `WithBoardingSweep(store BoardingSweepStore, signer SweepSigner, chainParams)` — Functional option that wires the boarding-sweep subsystem into the wallet actor. When omitted, boarding-sweep RPC paths return a clear "subsystem not initialised" error.
- `emitUTXOCreated(ctx, utxo, blockHeight, classification)` — Internal helper that null-safely builds a `ledger.UTXOCreatedMsg` from a wallet `Utxo` and Tells it to `ledgerSink`. Negative block heights clamp to `0` rather than wrapping under a direct `uint32` cast; nil `utxo` and `fn.None` sink are silent no-ops.
- `LockID` — Type alias for `walletcore.LockID` ([32]byte). The canonical declaration lives in `walletcore` to avoid the wallet→txconfirm import cycle.
- `OutputLeaser` — Type alias for `walletcore.OutputLeaser`. Same rationale.
- `Utxo` — Type alias for `walletcore.Utxo`. Same rationale.
- `SweepSigner` — Interface (`input.Signer` + `NewWalletPkScript`) needed to build and sign a boarding-timeout sweep transaction. Deliberately mirrors `unroll.SweepWallet` so the existing per-backend adapters satisfy it without modification.
- `BoardingSweepStore` — Persistence interface for aggregate boarding-sweep lifecycle. Methods: `CreatePendingBoardingSweep`, `MarkBoardingSweepPublished`, `MarkBoardingSweepFailed`, `MarkBoardingSweepInputSpent`, `ListBoardingSweeps`, `GetBoardingSweep`, `ListPendingBoardingSweeps`. Concrete implementation is `db.BoardingWalletStore`.
- `NewBoardingSweep` / `BoardingSweepRecord` / `BoardingSweepInputRecord` — Domain types for the boarding-sweep control plane. Lifecycle constants live here as `BoardingSweepStatus*` and `BoardingSweepInputStatus*` strings; `db` package imports these via type aliases.
- `SweepBoardingUTXOsRequest` / `SweepBoardingUTXOsResponse` — Ask-request/response for triggering a boarding timeout-path sweep. `Broadcast=false` returns a preview without persisting or broadcasting.
- `ResumeBoardingSweepsRequest` / `ResumeBoardingSweepsResponse` — Ask-request for re-arming spend watches and re-submitting pending sweeps to `txconfirm` on daemon restart. Must be issued AFTER `txconfirm.TxBroadcasterActor` is registered so `txconfirm.LookupRef` resolves correctly.
- `BoardingSweepSpendNotification` / `BoardingSweepTxNotification` / `BoardingSweepNotificationAck` — Internal notifications from chainsource spend watches and txconfirm back to the wallet actor for sweep state reconciliation.
- `BoardingBackend` — Interface for wallet integration (key derivation, taproot import, ListUnspent). `GetTransaction` returns `*TxInfo` (containing tx, block hash, and block height).
- `TxInfo` — Struct wrapping a confirmed transaction with its block hash and block height. Returned by `BoardingBackend.GetTransaction`.
- `BoardingStore` — Interface for persisting boarding addresses and intents.
- `VTXOReader` — Read-only interface for loading VTXO descriptors by outpoint. Wallet uses this to build intent packages without importing `vtxo` directly.
- `VTXODescriptor` — Wallet-level VTXO descriptor (outpoint, amount, pkscript, tree, expiry). Avoids direct dependency on `vtxo.Descriptor`.
- `SelectedVTXO` — Describes a VTXO selected and locked for use as a transfer input (outpoint, amount, pkscript). Breaks the vtxo → round → wallet import cycle.
- `CreateBoardingAddressRequest` / `CreateBoardingAddressResponse` — Ask-request for deriving new address.
- `BlockEpochNotification` — Tell-message from chain source triggering UTXO polling.
- `BoardingUtxoConfirmedEvent` — Tell-message sent when a VTXO confirms.
- `BoardRequest` / `BoardResponse` — Ask-request from RPC to trigger boarding flow.
- `RefreshVTXOsRequest` — Ask-request to select VTXOs for refresh and compose intent package. Carries `OperatorFees map[wire.OutPoint]btcutil.Amount`; when non-empty, the handler validates each fee is non-negative and below the VTXO amount, then subtracts it from the new VTXO output before registering with the round actor. Empty map is pre-#269 zero-fee behavior (tests, legacy paths).
- `SelectAndLockVTXOsRequest` — Ask-request to select and lock VTXOs for OOR spend.
- `LeaveVTXOsRequest` — Ask-request to select VTXOs for cooperative leave. Carries a singular `DestOutput *wire.TxOut` plus a per-outpoint `DestOutputs map[wire.OutPoint]*wire.TxOut` override map; the handler picks `DestOutputs[op]` when set and falls back to `DestOutput`. Per-input operator fees are no longer pre-quoted on the client — under the #270 seal-time fee handshake the server stamps the residual onto the IsChange=true leave output at seal time, so the wallet ships the full forfeited amount on each leave output.
- `CompleteSpendVTXOsRequest` — Tell-message to finalize spend and release locks.
- `UnlockVTXOsRequest` — Tell-message to release locked VTXOs on failure.
- `SendRecipient` — Describes a single directed send destination (pkscript, amount, recipient client key).
- `SendVTXOsRequest` / `SendVTXOsResponse` — Ask-request for in-round directed sends. Validates each recipient amount is within `(0, MaxSatoshi]` and that the running total never overflows `int64`, atomically selects and reserves VTXOs via `SelectAndReserveForfeitRequest`, builds forfeit + recipient VTXO intents, and registers with the round actor. Supports dry-run mode for previewing coin selection without committing. Reserved VTXOs are released via a deferred cleanup that uses `context.WithoutCancel` so cleanup survives caller disconnect; on success, a `committed` flag is set to skip the release.
- `GetConfirmedBoardingIntentsRequest` / `GetConfirmedBoardingIntentsResponse` — Ask-request to retrieve currently confirmed boarding intents (used by the RPC/CLI layer to report boarding balance with policy metadata).
- `GetBoardingBalanceResponse` — Carries `TotalBalance` (confirmed, round-eligible), `PendingSweepBalance` (in-flight sweep_pending UTXOs), and `SweptBalance` (historical swept total).
- `VTXODescriptor.EffectivePolicyTemplate` — Decodes the serialized `PolicyTemplate` field on the wallet-level VTXO descriptor using `lib/arkscript`.

## Relationships

- **Depends on**: `baselib/actor` (actor system), `chainsource` (block epoch notifications, spend watches), `lib/actormsg` (VTXO manager admission types), `ledger` (`Sink` alias for emission + `UTXOCreatedMsg` / `ClassificationDeposit` constants), `txconfirm` (boarding-sweep confirmation tracking via `txconfirm.LookupRef`), `walletcore` (canonical `LockID`, `Utxo`, `OutputLeaser`).
- **Depended on by**: `round` (boarding intents, types: `BoardingAddress`, `SelectedVTXO`), `db` (persistence; also imports wallet for sweep domain types), `darepod` (wiring).
- **Sends**:
  - → `round` (via registered notifier): `BoardingUtxoConfirmedEvent`
  - → `round` (via `lib/actormsg`): `TriggerBoardMsg` (VTXO amounts for
    boarding), `RegisterIntentMsg` (pre-composed cooperative intents with
    forfeits + VTXOs/leaves); `TriggerRegistration=true` for directed sends
    so the round FSM advances from `PendingRoundAssembly` immediately,
    `false` for refresh/leave batching
  - → `vtxo` manager (via `lib/actormsg`): `SelectAndReserveSpendRequest`, `ReleaseSpendRequest`, `CompleteSpendRequest`, `ReserveForfeitRequest`, `ReleaseForfeitRequest`, `SelectAndReserveForfeitRequest`
  - → `ledger` actor (via `ledger.Sink` Tell, when `fn.Some`): `UTXOCreatedMsg` on every processed confirmed wallet UTXO, tagged `ClassificationDeposit`; `FeePaidMsg{FeeType=FeeTypeOnchainSweep}` on boarding-sweep confirmation via `emitSweepConfirmedLedger`. (`UTXOSpentMsg` emission is a planned follow-up.)
  - → `round` (via `lib/types.VTXORequest.Origin`): wallet intent composition tags each locally-owned VTXO output with a `VTXOOrigin` classifier so the round actor's downstream ledger emission dispatches to the correct `Source`. Refresh outputs and directed-send self-change get `VTXOOriginRoundRefresh`; boarding-path tagging lives in `round.handleTriggerBoard` (`VTXOOriginRoundBoarding`).
- **Receives**:
  - ← `chainsource`: `BlockEpochNotification` (triggers UTXO polling)
  - ← `round`: `RegisterConfirmationNotifierRequest`, `UnregisterConfirmationNotifierRequest`
  - ← `chainsource`: `BoardingSweepSpendNotification` (per-input confirmed spend for boarding-sweep tracking)
  - ← `txconfirm`: `BoardingSweepTxNotification` (`TxConfirmed` / `TxFailed` for the aggregate sweep tx)
  - ← API: `CreateBoardingAddressRequest`, `GetActiveBoardingAddressesRequest`, `GetBoardingBalanceRequest`, `GetConfirmedBoardingIntentsRequest`, `RefreshVTXOsRequest`, `SelectAndLockVTXOsRequest`, `LeaveVTXOsRequest`, `BoardRequest`, `CompleteSpendVTXOsRequest`, `UnlockVTXOsRequest`, `SendVTXOsRequest`, `SweepBoardingUTXOsRequest`, `ResumeBoardingSweepsRequest`

## Invariants

- UTXO confirmation requires `MinBoardingConfs` (1) on-chain confirmations.
- `ListUnspent` queries are retried up to 3 times with 1 s delay on each block epoch (mitigates the race between block epoch arrival and wallet UTXO set update; neutrino backends can take over a second; Esplora-backed backends previously hit rate limits under higher burst).
- Notifier registration captures `minConf` parameter per actor; different actors can require different confirmation depths.
- Cooperative admission (refresh/leave) must reserve forfeit inputs through the VTXO manager before sending `RegisterIntentMsg` to the round actor.
- If round registration fails after successful admission, the wallet releases the forfeit reservation so VTXOs return to LiveState.
- Directed sends use `SelectAndReserveForfeitRequest` (cooperative forfeit path) rather than the OOR spend path. The wallet builds recipient VTXOs with the recipient's key as `OwnerKey` and derives a separate ephemeral `SigningKey` for MuSig2 tree construction.
- Local ownership of a round-produced VTXO is no longer tracked with a per-intent `IsOwner` flag. `types.VTXORequest` / `round.VTXOIntent` no longer carry `IsOwner`; at round confirmation time the round FSM asks a `round.OwnedScriptChecker` (backed in production by the OOR owned-receive-scripts store) which pkScripts to persist as local balance. The wallet's only job is to supply the correct `OwnerKey` per intent — local-origin owner keys keep their populated `KeyLocator` so `handleRegisterIntent` registers them via `OwnedScriptRegistrar`, while remote recipients carry a zero `KeyLocator` and are intentionally left unregistered.
- `handleSendVTXOs` uses a `defer`-based release rather than a `releaseAndFail` helper: any error path (including dry-run) falls through to the deferred release, and the `committed` flag is set only after the round actor accepts the intent. Context is preserved via `context.WithoutCancel` so cleanup is not dropped when the caller disconnects.
- `handleSendVTXOs` rejects pre-flight any directed send with multiple recipients and exactly-zero change residual under the #270 seal-time fee handshake. The server is the amount authority and absorbs the operator fee out of the designated `IsChange=true` slot; if there is no residual to absorb the fee against, the server has no slack to deduct fees without silently shifting them onto a recipient leg. The wallet refuses the request rather than letting the server pick the loser.
- `VTXOReader` / `VTXODescriptor` / `SelectedVTXO` break the vtxo → round → wallet import cycle by providing wallet-level types that don't reference `vtxo.Descriptor` directly.
- Per-subsystem logging via `build.LoggerFromContext` (no global mutable loggers).
- `ResumeBoardingSweepsRequest` must be dispatched AFTER `txconfirm.TxBroadcasterActor` registers with the receptionist. Sending it from `Ark.Start` would race the registrar; `darepod` explicitly Asks the wallet to resume after step 12 of `startWalletDependentActors`.
- The boarding-sweep subsystem is optional: when `WithBoardingSweep` is not passed, `SweepBoardingUTXOsRequest` and `ResumeBoardingSweepsRequest` return explicit errors rather than panicking.
- `buildBoardingSweepTx` (in `boarding_sweep.go`) iterates the vsize estimate up to three times until `SerializeSizeStripped` / `SerializeSize` converge, ensuring the returned `fee` and `txid` are accurate before the record is persisted.
- Boarding sweep records are persisted before broadcast. On broadcast failure the record is marked failed; on confirmation `emitSweepConfirmedLedger` emits `FeePaidMsg{FeeTypeOnchainSweep}` exactly once via the `idx_client_ledger_idempotent_key` dedup path.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
