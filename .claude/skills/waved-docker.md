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

`wavecli --network` defaults to `mainnet`, and it is what derives the default
TLS cert and macaroon paths under the data directory. A regtest daemon keeps
both under `data/regtest/`, so point a bare `wavecli` at one and it goes
looking in the wrong place and gives up before it ever dials:

```
unable to load TLS cert: open /root/.waved/data/mainnet/tls.cert: no such file or directory
```

`--network=regtest` is the whole fix on a stock daemon, which serves TLS with
macaroon auth and has the matching credentials sitting right there on disk:

```bash
# Inside the container:
docker exec ark-client wavecli --network=regtest \
    --rpcserver=localhost:10029 getinfo

docker exec ark-client wavecli --network=regtest \
    --rpcserver=localhost:10029 balance

# Interactive shell:
docker exec -it ark-client /bin/sh
```

`--rpcserver=localhost:10029` is already the default, so it can be dropped.
`--network=regtest` cannot.

### When the daemon runs plaintext

A daemon started with `--rpc.notls --rpc.no-macaroons` needs the matching
`--no-tls --no-macaroons` on the client. Those two travel as a pair: pass
`--no-tls` on its own and gRPC refuses to put per-RPC credentials on a
plaintext link (`the credentials require transport level security`).

With both set, wavecli reads nothing off disk at all, so `--network` stops
mattering for the connection. It is still worth passing so the same command
survives the daemon flipping back to TLS.

Do not reach for these by reflex. Against a stock TLS daemon they fail with
`error reading server preface: EOF`, which reads like a dead daemon rather
than a client too eager to speak plaintext.

### Connection errors and what they actually mean

Getting this half-right produces six distinct errors, none of which say
"you have a flag wrong":

| Error | Cause |
|-------|-------|
| `unable to load TLS cert: open .../data/mainnet/tls.cert: no such file...` | wavecli is on the default `--network=mainnet` and looked for the cert in the wrong directory. Pass `--network=regtest`. |
| `read macaroon: open .../data/mainnet/admin.macaroon: no such file...` | Same cause, one step later: `--no-tls` silenced the cert lookup but the macaroon path is still mainnet. Pass `--network=regtest` or `--no-macaroons`. |
| `error reading server preface: EOF` | A `--no-tls` client against a TLS listener. Drop `--no-tls`. |
| `authentication handshake failed: tls: first record does not look like a TLS handshake` | The reverse: a TLS client against a `--rpc.notls` daemon. Add `--no-tls`. |
| `grpc: the credentials require transport level security` | `--no-tls` without `--no-macaroons`. gRPC will not ship per-RPC credentials over a plaintext link, so the two dev flags travel as a pair. |
| `expected 1 macaroon, got 0` | The reverse of that: `--no-macaroons` against a daemon that still enforces them, so the connection is fine but the call carries no credential. Drop `--no-macaroons`, or turn enforcement off on the daemon with `--rpc.no-macaroons`. Adding `--macaroonpath` alongside `--no-macaroons` does nothing: `client.go` only reads the path when macaroons are enabled. |

## Logs

```bash
# Follow container logs (stdout).
docker logs -f ark-client

# With docker-compose:
docker-compose logs -f waved

# Increase verbosity:
# Set WAVED_DEBUG=trace in docker-compose.yml or pass --debuglevel=trace
```
