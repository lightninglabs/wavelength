# chainsource

## Purpose

Chain backend abstraction layer providing a blockchain interface for fee
estimation, broadcasting, and event-driven monitoring of confirmations, spends,
and new blocks.

## Key Types

- `ChainBackend` — Interface: EstimateFee, BestBlock, BroadcastTx, TestMempoolAccept, RegisterConf/Spend/Blocks, SubmitPackage. `SubmitPackage` atomically submits a parent+child transaction package for V3 package relay.
- `ChainSourceActor` — Factory actor spawning sub-actors for each monitoring request. Handles `SubmitPackageRequest` by delegating to `ChainBackend.SubmitPackage`.
- `ConfRegistration` / `SpendRegistration` / `BlockRegistration` — Structs with buffered channels and Cancel.
- `SubmitPackageRequest` / `SubmitPackageResponse` — Actor message pair for atomic package submission. Carries `Parents []*wire.MsgTx` and `Child *wire.MsgTx`.

## Relationships

- **Depends on**: `baselib/actor` (ActorSystem, ActorBehavior, ServiceKey).
- **Depended on by**: `round`, `vtxo`, `wallet` (monitoring), `chainbackends` (implements interface), `darepod` (wiring).

## Invariants

- `ChainBackend` is an interface; implementations live in `chainbackends`.
- Each monitoring request spawns a dedicated sub-actor (no shared state between monitors).
- Registration channels are buffered.
- `ConfActor.RegisterConf` wraps `ChainBackend.RegisterConf` in a 10-second timeout context to prevent hanging when LND is slow under block load.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
