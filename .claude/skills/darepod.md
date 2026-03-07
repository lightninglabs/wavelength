# darepod / darepocli — Ark Client Daemon

## Overview

`darepod` is the Ark protocol client daemon. It connects to an Ark
operator server via a mailbox transport, manages VTXOs (virtual
transaction outputs), and exposes a gRPC API for wallet operations.

`darepocli` is the CLI for driving the daemon. Output is always JSON.
All commands accept `--json` for raw proto-JSON request payloads.

## Building

```bash
make build          # produces bin/darepod and bin/darepocli
make install        # installs to $GOPATH/bin
make lint           # run linter
make unit pkg=darepod  # run unit tests for a package
```

## Starting the Daemon

### lwwallet Mode (Standalone — No lnd Required)

```bash
./bin/darepod \
  --network=regtest \
  --wallet.type=lwwallet \
  --wallet.esploraurl=http://localhost:3000 \
  --server.host=localhost:10010 \
  --server.insecure \
  --server.localmailboxid=client1 \
  --server.remotemailboxid=server \
  --rpc.listenaddr=localhost:10029
```

Then create and unlock the wallet:

```bash
# Create (password via env var for automation)
DAREPOD_WALLET_PASSWORD=testpass darepocli wallet create --no-tls

# Or auto-unlock at startup:
DAREPOD_WALLET_PASSWORD=testpass ./bin/darepod \
  --wallet.type=lwwallet \
  --wallet.password_file=/path/to/password \
  ...
```

### lnd Mode (Existing lnd Node)

```bash
./bin/darepod \
  --network=regtest \
  --wallet.type=lnd \
  --lnd.host=localhost:10009 \
  --lnd.tlspath=~/.lnd/tls.cert \
  --lnd.macaroonpath=~/.lnd/data/chain/bitcoin/regtest/admin.macaroon \
  --server.host=localhost:10010 \
  --server.insecure \
  --server.localmailboxid=client1 \
  --server.remotemailboxid=server \
  --rpc.listenaddr=localhost:10029
```

## CLI Quick Reference

All commands connect to the daemon at `--rpcserver` (default
`localhost:10029`). Use `--no-tls` for regtest.

### Status

```bash
darepocli getinfo --no-tls
```

### Wallet

```bash
# Create wallet
DAREPOD_WALLET_PASSWORD=testpass darepocli wallet create --no-tls

# Unlock wallet
DAREPOD_WALLET_PASSWORD=testpass darepocli wallet unlock --no-tls

# Check balance
darepocli wallet balance --no-tls

# New boarding address
darepocli wallet newaddress --no-tls
```

### VTXOs

```bash
# List all VTXOs
darepocli vtxos list --no-tls

# List live VTXOs above 10k sats
darepocli vtxos list --status live --min-amount 10000 --no-tls

# Refresh all VTXOs (extend expiry)
darepocli vtxos refresh --all --no-tls
```

### Send

```bash
# In-round send (waits for next round)
darepocli send inround --to tb1p... --amount 50000 --no-tls

# Out-of-round send (immediate, via operator)
darepocli send oor --to tb1p... --amount 25000 --no-tls

# Agent-friendly JSON input for complex sends
darepocli send inround --no-tls --json '{
  "recipients": [
    {"address": "tb1p...", "amount_sat": 50000},
    {"address": "tb1p...", "amount_sat": 30000}
  ]
}'
```

### Password Input (Never as CLI args)

Priority order: stdin pipe > `DAREPOD_WALLET_PASSWORD` env >
`--wallet_password_file` flag > interactive prompt.

```bash
# Pipe
echo -n 'pass' | darepocli wallet unlock --no-tls

# Env var
DAREPOD_WALLET_PASSWORD=pass darepocli wallet unlock --no-tls

# File
darepocli wallet unlock --wallet_password_file=/tmp/pass --no-tls
```

## Regtest Workflow

1. Start a regtest bitcoin node + esplora.
2. Start darepod in lwwallet mode (see above).
3. Create wallet via CLI.
4. Get a boarding address: `darepocli wallet newaddress --no-tls`
5. Fund it: `bitcoin-cli sendtoaddress <addr> 0.01`
6. Mine a block: `bitcoin-cli generatetoaddress 1 <miner_addr>`
7. Check balance: `darepocli wallet balance --no-tls`
8. List VTXOs: `darepocli vtxos list --no-tls`

## Environment Variables

| Variable | Description |
|----------|-------------|
| `DAREPOD_WALLET_PASSWORD` | Wallet password for create/unlock |
| `DAREPOD_NETWORK` | Bitcoin network override |
| `DAREPOD_WALLET_TYPE` | Wallet backend type override |
| `DAREPOD_WALLET_ESPLORAURL` | Esplora URL override |

All daemon config flags can be set via env vars with the `DAREPOD_`
prefix, dots replaced by underscores (e.g., `DAREPOD_SERVER_HOST`).

## Troubleshooting

| Problem | Fix |
|---------|-----|
| `connection refused` | Daemon not running or wrong `--rpcserver` |
| `wallet not ready` | Run `darepocli wallet unlock` first |
| `wallet already exists` | Wallet was already created; use `unlock` |
| `GenSeed: lwwallet mode only` | Switch daemon to `--wallet.type=lwwallet` |
| TLS errors | Use `--no-tls` for regtest, or set `--tlscertpath` |
