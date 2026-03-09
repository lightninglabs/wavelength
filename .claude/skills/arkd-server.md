---
name: arkd-server
description: "This skill provides context for working on the arkd operator daemon server code. It should be used when modifying server startup, actor system wiring, subsystem lifecycle, admin/client RPC servers, config, or database setup. Triggers include working on server.go, config.go, adminrpcserver.go, rpcserver.go, server_rounds.go, server_oor.go, or any actor registration code."
---

# arkd — Ark Operator Daemon

## Overview

`arkd` is the Ark protocol operator (server) daemon. It drives round
lifecycle (registration, signing, broadcast, confirmation), manages VTXOs,
processes out-of-round (OOR) transfers, and serves client connections via
gRPC and mailbox transport.

## Building

```bash
make build          # produces bin/arkd and bin/arkcli
make lint-changed   # run linter on changed files vs base branch
make fmt            # format all Go source files
make rpc            # regenerate protobuf stubs
make sqlc           # regenerate type-safe DB queries
make unit pkg=<pkg> timeout=5m   # run unit tests for a package
```

## Starting the Daemon

### Regtest with lnd

```bash
./bin/arkd \
  --network=regtest \
  --lnd.host=localhost:10009 \
  --lnd.tlspath=~/.lnd/tls.cert \
  --lnd.macaroonpath=~/.lnd/data/chain/bitcoin/regtest/admin.macaroon \
  --db.backend=sqlite \
  --adminrpc.listenaddr=localhost:8081 \
  --rpc.listenaddr=localhost:10010 \
  --debuglevel=debug
```

### Key Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--network` | `regtest` | Bitcoin network (mainnet, testnet, regtest, simnet, signet) |
| `--lnd.host` | `localhost:10009` | lnd gRPC endpoint |
| `--lnd.tlspath` | `~/.lnd/tls.cert` | Path to lnd TLS certificate |
| `--lnd.macaroonpath` | (auto) | Path to lnd admin macaroon |
| `--lnd.rpctimeout` | `30s` | Timeout for lnd RPC calls |
| `--db.backend` | `sqlite` | Database backend (sqlite, postgres) |
| `--db.sqlitepath` | `~/.arkd/arkd.db` | SQLite database file path |
| `--db.postgresdsn` | | PostgreSQL connection string |
| `--adminrpc.listenaddr` | `localhost:8081` | Admin gRPC listen address |
| `--rpc.listenaddr` | `localhost:10010` | Client-facing gRPC listen address |
| `--rpc.tls.certpath` | | TLS certificate for client gRPC |
| `--rpc.tls.keypath` | | TLS key for client gRPC |
| `--debuglevel` | `info` | Log verbosity (trace, debug, info, warn, error) |

### Environment Variables

All flags can be set via environment variables with the `ARKD_` prefix,
dots replaced by underscores:

| Variable | Description |
|----------|-------------|
| `ARKD_NETWORK` | Bitcoin network override |
| `ARKD_LND_HOST` | lnd gRPC endpoint |
| `ARKD_DB_BACKEND` | Database backend |
| `ARKD_DEBUGLEVEL` | Log verbosity |
| `ARKD_ADMINRPC_LISTENADDR` | Admin gRPC listen address |
| `ARKD_RPC_LISTENADDR` | Client gRPC listen address |

## Architecture

### Startup Sequence

1. Connect to lnd (blocks until synced and unlocked)
2. Initialize actor system
3. Create and register chain source actor
4. Initialize database (sqlite or postgres)
5. Setup indexer subsystem (bridge, stores, operator)
6. Setup rounds subsystem (timeout, batch watcher, rounds actor, operator)
7. Setup OOR subsystem (session store, delivery store, driver, actor, operator)
8. Start admin RPC server
9. Start client RPC server + mailbox mux
10. Block until shutdown signal

### Subsystems

**Indexer**: Handles VTXO indexing and balance queries. Uses synchronous
ServeMux dispatch for 7 IndexerService RPCs.

**Rounds**: Drives the round FSM (registration, nonce exchange, signing,
broadcast, confirmation). Uses actor-based async dispatch: inbound client
requests go via `Tell` to the rounds actor, responses flow back through
outbox events via the bridge.

**OOR**: Manages out-of-round transfers between clients. Uses a
DurableActor for crash-safe session recovery. Inbound
SubmitPackage/FinalizePackage requests are dispatched to the OOR actor.

**Client Bridge**: `ClientsConnBridge` manages per-client connection
runtimes. Each client gets dispatchers for all services (indexer, rounds,
OOR) merged via `RegisterClientWithAllDispatchers`.

### Key Files

| File | Role |
|------|------|
| `server.go` | Main startup orchestration |
| `server_indexer.go` | Indexer subsystem setup |
| `server_rounds.go` | Rounds subsystem setup + dispatchers |
| `server_oor.go` | OOR subsystem setup + dispatchers |
| `rpcserver.go` | Client-facing gRPC (ArkService) |
| `adminrpcserver.go` | Admin gRPC (OperatorAdmin) |
| `config.go` | Config structs + validation |
| `indexer/operator.go` | Indexer dispatcher pattern |
| `rounds/operator.go` | Round dispatcher + actor bridge |
| `rounds/actor.go` | Round FSM actor |
| `oor/operator.go` | OOR dispatcher |
| `oor/actor.go` | OOR DurableActor |
| `clientconn/bridge.go` | Per-client connection bridge |

## Troubleshooting

| Problem | Fix |
|---------|-----|
| `unable to connect to lnd` | Verify lnd is running, TLS cert path, macaroon path |
| `wallet is locked` | Unlock lnd wallet first: `lncli unlock` |
| `chain not synced` | Wait for lnd to finish syncing (arkd blocks automatically) |
| `unable to open database` | Check `--db.sqlitepath` or postgres DSN |
| `address already in use` | Another process on admin/client RPC port |
| Rounds not progressing | Check `arkcli trigger-batch` to force a round |

## Regtest Workflow

1. Start bitcoind in regtest mode
2. Start lnd connected to bitcoind
3. Start arkd (see flags above)
4. Verify: `arkcli info --no-tls`
5. Start a client daemon (darepod) pointing at arkd
6. Fund client wallet, then join rounds or send OOR
