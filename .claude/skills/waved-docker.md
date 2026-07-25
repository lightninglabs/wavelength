---
name: waved-docker
description: "Build and run the waved Ark client daemon in Docker. Use when building the client Docker image, configuring container-based deployment, or debugging client-side Docker issues."
---

# waved Docker

## Building the Image

```bash
docker build -t waved:local .
```

The Dockerfile uses a multi-stage build:
1. Builder stage: compiles `waved` and `wavecli` from source
2. Runtime stage: minimal alpine image with the two binaries

## Running Standalone

```bash
docker run --rm -p 10029:10029 waved:local \
    --network=regtest \
    --wallet.type=lnd \
    --lnd.host=<lnd-host>:10009 \
    --lnd.tlspath=/path/to/tls.cert \
    --lnd.macaroonpath=/path/to/admin.macaroon \
    --server.host=<lumosd-host>:7070 \
    --server.insecure=true \
    --rpc.listenaddr=0.0.0.0:10029
```

Two legs of transport security get confused here, so keep them apart.
`--server.insecure` is the **outbound** leg, waved dialing the Ark operator.
waved's **own** RPC listener is governed by `--rpc.notls` and
`--rpc.no-macaroons`, and neither is set above, so the daemon serves TLS with
macaroon auth and writes both credentials under the network's data directory
(`/root/.waved/data/regtest/`). Setting one leg says nothing about the other.

## Running with Full Stack

The server repo (`lumos`) includes a `docker-compose.yml` that
orchestrates the complete environment: bitcoind + 2x lnd + lumosd + waved.

```bash
# From the lumos (server) repo root:
docker-compose up -d --build
./scripts/docker-regtest-setup.sh
```

## Configuration Flags

| Flag | Default | Purpose |
|------|---------|---------|
| `--network` | `mainnet` | Bitcoin network (mainnet, testnet, testnet4, regtest, simnet, signet); use `regtest` for dev |
| `--wallet.type` | `lwwallet` | Wallet backend (`lnd`, `lwwallet`, or `btcwallet`) |
| `--lnd.host` | `localhost:10009` | LND gRPC address |
| `--lnd.tlspath` | | Path to LND TLS cert |
| `--lnd.macaroonpath` | | Path to LND admin macaroon |
| `--server.host` | (per-network) | Ark operator address; see below |
| `--server.transport` | `grpc` | Ark operator RPC transport (`grpc` or `rest`) |
| `--server.insecure` | `false` | Disable TLS for the outbound server connection (dev only) |
| `--rpc.listenaddr` | `localhost:10029` | Daemon gRPC listen address |
| `--rpc.notls` | `false` | Disable TLS on the daemon's own RPC listener (dev only) |
| `--rpc.no-macaroons` | `false` | Disable macaroon auth on the daemon's RPC listener (dev only) |
| `--debuglevel` | `info` | Log verbosity (trace/debug/info/warn/error/critical) |

`--server.host` has no baked-in default. When it is empty the address is
resolved from a network+transport table at startup: `localhost:10010` on
mainnet, regtest, and simnet, and the Lightning Labs deployment on testnet,
testnet4, and signet. Running on mainnet at all also requires
`--allow-mainnet`, and pairing mainnet with `--rpc.notls` or
`--rpc.no-macaroons` requires `--allow-insecure-mainnet` on top of that.

`--server.localmailboxid` and `--server.remotemailboxid` no longer exist.
The daemon derives both mailbox identifiers itself, and passing either one
now aborts startup with `unknown flag: --server.localmailboxid`. Anything
still handing those to waved (lumos's `docker-compose.yml` on `master`
included) needs them deleted, not renamed.

Environment variables use `WAVED_` prefix with dots replaced by underscores:
`WAVED_NETWORK=regtest`, `WAVED_WALLET_TYPE=lnd`, `WAVED_DEBUGLEVEL=trace`,
etc.

## CLI Access

```bash
# Inside the container:
docker exec ark-client wavecli --rpcserver=localhost:10029 getinfo
docker exec ark-client wavecli --rpcserver=localhost:10029 balance

# Interactive shell:
docker exec -it ark-client /bin/sh
```

## Logs

```bash
# Follow container logs (stdout).
docker logs -f ark-client

# With docker-compose:
docker-compose logs -f waved

# Increase verbosity:
# Set WAVED_DEBUG=trace in docker-compose.yml or pass --debuglevel=trace
```
