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
  *actor.ActorSystem`, `Log fn.Option[btclog.Logger]`, `FinalityDepth uint32`.
  `FinalityDepth` is forwarded to each spawned conf/spend sub-actor and
  configures height-based synthesis of `Done` for backend transports that
  cannot deliver one themselves (notably lndclient over gRPC). Zero disables
  synthesis; `DefaultFinalityDepth` is six — the conventional Bitcoin
  reorg-safety threshold.
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
  notification without blocking on a Future, plus optional
  `NotifyReorged fn.Option[actor.TellOnlyRef[ConfReorgedEvent]]` and
  `NotifyDone fn.Option[actor.TellOnlyRef[ConfDoneEvent]]` refs for the
  reorg-aware lifecycle. Reorg/Done refs require `NotifyActor` (a Future
  can only complete once).
- `SpendMsg` / `SpendResp` — Sealed interfaces for spend sub-actor messages.
- `RegisterSpendRequest/Response`, `UnregisterSpendRequest/Response` — Spend
  monitoring lifecycle. `RegisterSpendRequest` carries the same
  `NotifyReorged`/`NotifyDone` reorg-aware refs (typed
  `SpendReorgedEvent`/`SpendDoneEvent`).
- `EpochMsg` / `EpochResp` — Sealed interfaces for block-epoch sub-actor.
- `SubscribeBlocksRequest/Response`, `UnsubscribeBlocksRequest/Response` —
  Block subscription lifecycle.
- `ConfRegistration` / `SpendRegistration` / `BlockRegistration` — Structs with
  buffered notification channels and a `Cancel()` function. `ConfRegistration`
  and `SpendRegistration` carry optional `Reorged` and `Done` channels backends
  use to surface the reversible lifecycle; `SpendDetail` carries
  `SpendingBlockHash` so callers can match spends to specific blocks.
- `ConfirmationEvent`, `SpendEvent`, `BlockEpoch` — Positive-event payload
  types. `ConfReorgedEvent`, `ConfDoneEvent`, `SpendReorgedEvent`,
  `SpendDoneEvent` — Lifecycle payloads delivered to the matching
  `NotifyReorged` / `NotifyDone` refs.
- `MapBlockEpoch`, `MapConfirmationEvent`, `MapSpendEvent`,
  `MapConfReorgedEvent`, `MapConfDoneEvent`, `MapSpendReorgedEvent`,
  `MapSpendDoneEvent` — Generic helpers that wrap a target `TellOnlyRef[Out]`
  and a mapping function, producing a `TellOnlyRef` of the source event type
  for actor-to-actor notification wiring.

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
- Future-mode watches are single-shot: the sub-actor exits after the first
  positive event. Actor-mode watches without `NotifyReorged`/`NotifyDone`
  retain the same single-shot contract for backwards compatibility. Actor-mode
  watches with at least one reorg/done ref are multi-shot: the sub-actor
  continues running, forwarding the full
  `Confirmed/Spend -> Reorged -> Confirmed/Spend -> Done` lifecycle to the
  configured refs, and releases the backend registration on `Done`.
- Admission rejects a `NotifyReorged` or `NotifyDone` ref without
  `NotifyActor` (a Future can only complete once; a missing ref would
  silently drop every re-event after the first).
- When `ChainSourceConfig.FinalityDepth > 0`, reorg-aware sub-actors arm a
  block-epoch subscription on the first positive event and synthesize a
  `Done` once they observe a block at `eventHeight + FinalityDepth - 1`. A
  reorg resets the depth counter so the next re-confirmation/re-spend on
  the new tip restarts the window cleanly. This closes the lndclient gap
  where `ConfirmationEvent.Done` / `SpendEvent.Done` are allocated but
  never written across the gRPC transport.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
