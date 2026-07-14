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
docker run --rm waved:local \
    --network=regtest \
    --wallet.type=lnd \
    --lnd.host=<lnd-host>:10009 \
    --lnd.tlspath=/path/to/tls.cert \
    --lnd.macaroonpath=/path/to/admin.macaroon \
    --server.host=<arkd-host>:7070 \
    --server.insecure=true \
    --rpc.listenaddr=0.0.0.0:10029
```

## Running with Full Stack

The server repo (`darepo`) includes a `docker-compose.yml` that orchestrates
the complete environment: bitcoind + 2x lnd + arkd + waved.

```bash
# From the darepo (server) repo root:
docker-compose up -d --build
./scripts/docker-regtest-setup.sh
```

## Configuration Flags

| Flag | Default | Purpose |
|------|---------|---------|
| `--network` | `mainnet` | Bitcoin network (use `regtest` for dev) |
| `--wallet.type` | `lwwallet` | Wallet backend (`lnd` or `lwwallet`) |
| `--lnd.host` | `localhost:10009` | LND gRPC address |
| `--lnd.tlspath` | | Path to LND TLS cert |
| `--lnd.macaroonpath` | | Path to LND admin macaroon |
| `--server.host` | `localhost:10010` | Ark operator server address |
| `--server.insecure` | `false` | Disable TLS for server connection |
| `--rpc.listenaddr` | `localhost:10029` | Daemon gRPC listen address |
| `--debuglevel` | `info` | Log verbosity (trace/debug/info/warn/error) |

Environment variables use `WAVED_` prefix with dots replaced by underscores:
`WAVED_NETWORK=regtest`, `WAVED_WALLET_TYPE=lnd`, etc.

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
