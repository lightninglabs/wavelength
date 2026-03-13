---
name: arkd-docker
description: "Run the full Ark protocol stack (bitcoind + lnd + arkd + darepod) in Docker for end-to-end development and testing. Use when building Docker images, running docker-compose, debugging container issues, or testing the system end-to-end."
---

# Ark Docker E2E Stack

## Quick Start

```bash
# Build and start the full stack (first run takes ~5 min for lnd build).
docker-compose up -d --build

# Fund both lnd wallets and verify connectivity.
./scripts/docker-regtest-setup.sh

# Follow logs.
docker-compose logs -f arkd darepod
```

## Architecture

```
bitcoind (regtest, ZMQ, txindex)
  |
  +-- lnd-server (operator wallet, gRPC :10009)
  |     |
  |     +-- arkd (operator daemon, client RPC :7070, admin RPC :8081)
  |
  +-- lnd-client (client wallet, gRPC :10010)
        |
        +-- darepod (client daemon, RPC :10029, connects to arkd:7070)
```

## Services & Ports

| Service | Container | Host Port | Internal Port | Purpose |
|---------|-----------|-----------|---------------|---------|
| bitcoind | ark-bitcoind | 18443 | 18443 | Bitcoin RPC |
| lnd-server | ark-lnd-server | 10009 | 10009 | Server LND gRPC |
| lnd-client | ark-lnd-client | 10010 | 10010 | Client LND gRPC |
| arkd | ark-server | 7070 | 7070 | Ark client-facing RPC |
| arkd | ark-server | 8081 | 8081 | Ark admin RPC |
| darepod | ark-client | 10029 | 10029 | Client daemon RPC |

## Common Operations

### Mining Blocks

```bash
docker exec ark-bitcoind bitcoin-cli -regtest -rpcuser=devuser -rpcpassword=devpass -generate 6
```

### Admin CLI (arkcli)

```bash
docker exec ark-server arkcli --rpcserver=localhost:8081 getinfo
docker exec ark-server arkcli --rpcserver=localhost:8081 listutxos
```

### Client CLI (darepocli)

```bash
docker exec ark-client darepocli --rpcserver=localhost:10029 getinfo
docker exec ark-client darepocli --rpcserver=localhost:10029 balance
```

### LND CLIs

```bash
docker exec ark-lnd-server lncli --network=regtest getinfo
docker exec ark-lnd-client lncli --network=regtest getinfo
```

### View Logs

```bash
docker-compose logs -f arkd          # Server logs
docker-compose logs -f darepod       # Client logs
docker-compose logs -f lnd-server    # LND server logs
docker-compose logs --tail=50 arkd   # Last 50 lines
```

## Debugging

### Exec Into Containers

```bash
docker exec -it ark-server /bin/sh
docker exec -it ark-client /bin/sh
```

### Rebuild Single Service

```bash
docker-compose up -d --build arkd      # Rebuild only arkd
docker-compose up -d --build darepod   # Rebuild only darepod
```

### Reset Everything

```bash
docker-compose down -v   # Remove containers and volumes
docker-compose up -d --build && ./scripts/docker-regtest-setup.sh
```

## Environment Variables

| Variable | Default | Purpose |
|----------|---------|---------|
| `LND_SOURCE` | (required) | Path to local lnd source tree |
| `LND_IMAGE` | `lnd:local` | LND Docker image name |
| `LND_DEBUG` | `debug` | LND log level |
| `ARKD_DEBUG` | `debug` | arkd log level |
| `DAREPOD_DEBUG` | `debug` | darepod log level |

## Building Images Standalone

```bash
# Server only.
docker build -t arkd:local .

# Client only (from client/ submodule).
docker build -t darepod:local ./client
```
