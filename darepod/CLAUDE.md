# darepod

## Purpose

Top-level daemon orchestrator that wires wallet backend, mailbox transport,
chain backend, database, and all domain actors into a running system with a
gRPC API.

## Key Types

- `Server` — Main daemon owning wallet, DB, chainsource actor, gRPC server, and ActorSystem.
- `RPCServer` — Implements the gRPC `DaemonService` API (Board, ListRounds, WatchRounds, NewOORReceiveScript, etc.).
- `Config` — Daemon configuration (data dir, network, RPC host, wallet type, etc.).
- `WalletState` — Enum (None/Locked/Ready) for wallet lifecycle.
- `serverDurableUnaryBuilder` — Implements `serverconn.DurableUnaryRequestBuilder` by delegating to the indexer client with proof-of-control credentials.
- `NewOwnedReceiveScriptSigner` — Indexer signer that resolves the wallet key for any persisted owned receive script, then delegates signing to the backend-specific signer.
- `EnsureDefaultOORReceiveScript` / `CreateOORReceiveScript` — Receive-key lifecycle: derive, register with indexer (proof-of-control), persist ownership record.
- `ResolveIncomingMetadataFromIndexer` — Resolves authoritative VTXO lineage metadata from the indexer's `ListVTXOsByScripts` response for incoming materialization.

## Relationships

- **Depends on**: `baselib/actor` (ActorSystem), `chainbackends`, `chainsource`, `lib/actormsg`, `db`, `round`, `vtxo`, `wallet`, `oor`, `serverconn`, `indexer`, `arkrpc`.
- **Depended on by**: `cmd/darepod` (main entry point).

## Invariants

- Server owns ActorSystem lifetime; shutdown stops all subsystems.
- Wallet transitions None → Locked → Ready (or direct to Ready if seed provided).
- Two wallet modes: LND-backed or lightweight (`lwwallet`).
- Board RPC is non-blocking: delegates to wallet actor and returns immediately.
- ListRounds splits pending (in-memory from actor) and persisted (SQL with cursor pagination) rounds.
- Server holds a `roundStore` reference for direct SQL queries from the RPC layer.
- Actor startup order: VTXO manager starts before round actor and OOR actor, so the manager ref is available for both. The round actor ref in the VTXO manager is lazy (service-key-based, resolved at Tell time).
- `mapRoundVTXOManagerMsg` bridges `round.VTXOManagerMsg` → `vtxo.ManagerMsg` via `MapInputRef`. Compile-time assertions enforce that all `round.VTXOManagerMsg` implementors satisfy `vtxo.ManagerMsg`.
- OOR receive-key is derived once at startup via `EnsureDefaultOORReceiveScript` and persisted for restart-safe re-registration. The `DurableUnaryBuilder` is wired through `serverconn.ConnectorConfig` so all indexer queries flow through the durable transport path.
- `ensureRoundExists` in `db/vtxo_store.go` uses check-then-insert (not upsert) because `InsertRound`'s `ON CONFLICT DO UPDATE` would overwrite richer round state.

## Deep Docs

- [docs/daemon_cli_guide.md](../docs/daemon_cli_guide.md) — Installation, configuration, CLI reference.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
