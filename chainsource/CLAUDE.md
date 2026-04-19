# chainsource

## Purpose

Chain backend abstraction layer providing a blockchain interface for fee
estimation, broadcasting, and event-driven monitoring of confirmations, spends,
and new blocks.

## Key Types

- `ChainBackend` — Interface: EstimateFee, BestBlock, BroadcastTx, TestMempoolAccept, RegisterConf/Spend/Blocks, and `SubmitPackage` for atomic parent+child V3 package relay.
- `ChainSourceActor` — Factory actor spawning sub-actors for each monitoring request.
- `ConfRegistration` / `SpendRegistration` / `BlockRegistration` — Structs with buffered channels and Cancel.
- `SubmitPackageRequest` / `SubmitPackageResponse` — Actor messages for atomic parent+child transaction package submission (V3 package relay).

## Relationships

- **Depends on**: `baselib/actor` (ActorSystem, ActorBehavior, ServiceKey).
- **Depended on by**: `round`, `vtxo`, `wallet` (monitoring), `chainbackends` (implements interface), `darepod` (wiring).

## Invariants

- `ChainBackend` is an interface; implementations live in `chainbackends`.
- Each monitoring request spawns a dedicated sub-actor (no shared state between monitors).
- Registration channels are buffered.
- `ConfActor.handleRegisterConf` applies a 10-second context timeout when
  calling `Backend.RegisterConf` to prevent hangs under heavy block load.
- `SubmitPackage` is required by all `ChainBackend` implementations; neutrino
  and unsupported backends return an explicit error.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
