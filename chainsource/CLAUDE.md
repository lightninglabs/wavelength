# chainsource

## Purpose

Chain backend abstraction layer providing a blockchain interface for fee
estimation, broadcasting, and event-driven monitoring of confirmations, spends,
and new blocks. Exposes a full actor message hierarchy for bidirectional actor
communication alongside the raw registration API.

## Key Types

- `ChainBackend` — Interface: `EstimateFee`, `BestBlock`, `BroadcastTx`,
  `TestMempoolAccept`, `RegisterConf/Spend/Blocks`, `SubmitPackage`, `Start/Stop`.
- `ChainSourceActor` — Factory actor spawning sub-actors for each monitoring
  request. Registered under `ChainSourceKey`.
- `ChainSourceConfig` — Config struct: `Backend ChainBackend`, `System
  *actor.ActorSystem`, `Log fn.Option[btclog.Logger]`.
- `BlockEpochConfig` — Config for `BlockEpochActor`. Fields:
  `Backend ChainBackend`, `Log fn.Option[btclog.Logger]`,
  `ReconnectBackoff time.Duration` (initial reconnect delay when the
  backend stream closes; zero → 1s default),
  `MaxReconnectBackoff time.Duration` (exponential cap; zero → 30s
  default). Tests may lower both to speed up reconnect cycles.
- `ChainSourceMsg` / `ChainSourceResp` — Sealed actor message interfaces for
  requests and responses sent to the `ChainSourceActor`.
- `FeeEstimateRequest/Response`, `BestHeightRequest/Response`,
  `BroadcastTxRequest/Response`, `TestMempoolAcceptRequest/Response`,
  `SubmitPackageRequest/Response` — Request/response pairs implementing
  `ChainSourceMsg`/`ChainSourceResp`.
- `ConfMsg` / `ConfResp` — Sealed interfaces for confirmation sub-actor messages.
- `RegisterConfRequest/Response`, `UnregisterConfRequest/Response` — Request
  types for conf-actor lifecycle. `RegisterConfRequest` carries an optional
  `NotifyActor fn.Option[actor.TellOnlyRef[ConfirmationEvent]]` for async-mode
  notification without blocking on a Future.
- `SpendMsg` / `SpendResp` — Sealed interfaces for spend sub-actor messages.
- `RegisterSpendRequest/Response`, `UnregisterSpendRequest/Response` — Spend
  monitoring lifecycle.
- `EpochMsg` / `EpochResp` — Sealed interfaces for block-epoch sub-actor.
- `SubscribeBlocksRequest/Response`, `UnsubscribeBlocksRequest/Response` —
  Block subscription lifecycle.
- `ConfRegistration` / `SpendRegistration` / `BlockRegistration` — Structs with
  buffered notification channels and a `Cancel()` function.
- `ConfirmationEvent`, `SpendEvent`, `BlockEpoch` — Notification payload types.
- `MapBlockEpoch`, `MapConfirmationEvent`, `MapSpendEvent` — Generic helpers
  that wrap a target `TellOnlyRef[Out]` and a mapping function, producing a
  `TellOnlyRef` of the source event type for actor-to-actor notification wiring.

## Relationships

- **Depends on**: `baselib/actor` (ActorSystem, ActorBehavior, ServiceKey).
- **Depended on by**: `round`, `vtxo`, `wallet` (monitoring), `chainbackends`
  (implements `ChainBackend`), `btcwbackend`, `lwwallet` (also implement
  `ChainBackend`), `darepod` (wiring).

## Invariants

- `ChainBackend` is an interface; implementations live in `chainbackends`,
  `btcwbackend`, and `lwwallet`.
- Each monitoring request spawns a dedicated sub-actor (no shared state between
  monitors).
- Registration channels are buffered.
- Confirmation sub-actors support two notification modes: Future-based (blocking
  await) and actor-based (async `Tell` via `NotifyActor`). Callers use the actor
  mode when blocking inside a durable actor transaction is unsafe.
- `BlockEpochActor.monitorBlocks()` auto-reconnects when the backend
  stream closes (e.g. LND notifier restart). On channel close it
  cancels the old registration, waits for `currentBackoff`, then calls
  `RegisterBlocks` again. Backoff doubles on each failed re-registration
  and resets to `ReconnectBackoff` on success. The actor stops cleanly
  when its context is cancelled during a reconnect wait.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
