# chainsource

## Purpose

Chain backend abstraction layer providing a blockchain interface for fee
estimation, broadcasting, and event-driven monitoring of confirmations, spends,
and new blocks.

## Key Types

- `ChainBackend` — Interface: EstimateFee, BestBlock, BroadcastTx, TestMempoolAccept, SubmitPackage, RegisterConf/Spend/Blocks, Start/Stop.
- `ChainSourceActor` — Factory actor spawning sub-actors for each monitoring request.
- `ConfRegistration` / `SpendRegistration` / `BlockRegistration` — Structs with buffered channels and Cancel.
- `SubmitPackageRequest` / `SubmitPackageResponse` — Actor message pair for atomic parent+child package relay; `SubmitPackageRequest` carries `Parents []*wire.MsgTx` and `Child *wire.MsgTx`.

## Relationships

- **Depends on**: `baselib/actor` (ActorSystem, ActorBehavior, ServiceKey).
- **Depended on by**: `round`, `vtxo`, `wallet` (monitoring), `chainbackends` (implements interface), `darepod` (wiring).

## Invariants

- `ChainBackend` is an interface; implementations live in `chainbackends`.
- Each monitoring request spawns a dedicated sub-actor (no shared state between monitors).
- Registration channels are buffered.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
