# chainsource

## Purpose

Chain backend abstraction layer providing a blockchain interface for fee
estimation, broadcasting, and event-driven monitoring of confirmations, spends,
and new blocks. Exposes a full actor message hierarchy for bidirectional actor
communication alongside the raw registration API.

## Key Types

- `ChainBackend` — Interface: `EstimateFee`, `BestBlock`, `BroadcastTx`,
  `TestMempoolAccept`, `RegisterConf/Spend/Blocks`, `SubmitPackage`,
  `Start/Stop`. Implemented by `chainbackends`, `btcwbackend`, `lwwallet`.
- `ChainSourceActor` — Actor registered under `ChainSourceKey`; dispatches
  each RegisterConf/RegisterSpend/SubscribeBlocks request to a dedicated
  sub-actor (`ConfActor`, `SpendActor`, `BlockEpochActor`).
- `ChainSourceMsg`/`ChainSourceResp`, `ConfMsg`/`ConfResp`,
  `SpendMsg`/`SpendResp`, `EpochMsg`/`EpochResp` — Sealed request/response
  interfaces for the top-level actor and its three sub-actor kinds
  (e.g. `RegisterConfRequest`, `SubscribeBlocksRequest`,
  `SubmitPackageRequest`, each with a matching `...Response`).
- `ConfirmationEvent`, `SpendEvent`, `BlockEpoch` — Notification payloads,
  delivered via a buffered channel on the returned `*Registration` or, when
  `NotifyActor` is set on the request, an actor `Tell`.
- `MapConfirmationEvent`/`MapSpendEvent`/`MapBlockEpoch` — Generic adapters
  wrapping a target `TellOnlyRef[Out]` so callers can subscribe with their
  own event type instead of the chainsource one.
- `IsIgnorableBroadcastError`, `IsIgnorableMempoolRejectReason` — Classify
  "already known/confirmed" rebroadcast errors/reject reasons as non-fatal.

## Relationships

- **Depends on**: `baselib/actor` (ActorSystem, ActorBehavior, ServiceKey),
  `build` (logger-from-context helper).
- **Depended on by**: `round`, `vtxo`, `wallet`, `fraud`, `txconfirm`,
  `unroll` (monitoring), `chainbackends`, `btcwbackend`, `lwwallet` (each
  implements `ChainBackend`), `darepod` (wiring).

## Invariants

- Each monitoring request spawns a dedicated sub-actor (no shared state between
  monitors).
- Registration channels are buffered.
- Confirmation sub-actors support two notification modes: Future-based (blocking
  await) and actor-based (async `Tell` via `NotifyActor`). Callers use the actor
  mode when blocking inside a durable actor transaction is unsafe.
- `BlockEpochActor` treats a closed backend block-epoch stream as transient
  once the initial subscription has succeeded: it cancels the closed
  registration and re-registers with capped exponential backoff
  (`BlockEpochConfig.ReconnectBackoff`/`MaxReconnectBackoff`, defaults 1s/30s)
  instead of terminating monitoring, so long-lived subscribers (e.g. the
  boarding wallet) keep receiving blocks across LND notifier churn/restart.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
