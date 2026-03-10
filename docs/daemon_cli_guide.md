# darepod / darepocli User Guide

This guide covers installing, configuring, and operating the Ark client
daemon (`darepod`) and its CLI (`darepocli`).

## Installation

### From Source

Requires Go 1.22+.

```bash
git clone https://github.com/lightninglabs/darepo-client.git
cd darepo-client

# Build both binaries into bin/
make build

# Or install to $GOPATH/bin
make install
```

After building, two binaries are produced:

- `bin/darepod` -- the long-running daemon process
- `bin/darepocli` -- the CLI for controlling the daemon

## Daemon Configuration

`darepod` supports two wallet backends: **lwwallet** (standalone,
in-process) and **lnd** (uses an existing lnd node).

### lwwallet Mode (Standalone)

The lightweight wallet requires only an Esplora API endpoint for chain
access. No external lnd node is needed.

```bash
darepod \
  --network=regtest \
  --wallet.type=lwwallet \
  --wallet.esploraurl=http://localhost:3000 \
  --server.host=localhost:10010 \
  --server.insecure \
  --server.localmailboxid=client1 \
  --server.remotemailboxid=server \
  --rpc.listenaddr=localhost:10029
```

### lnd Mode

Uses an existing lnd node for signing and key derivation. Point the
daemon at the lnd gRPC interface with the TLS cert and admin macaroon.

```bash
darepod \
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

### Daemon Flags Reference

| Flag | Default | Description |
|------|---------|-------------|
| `--datadir` | `~/.darepod` | Root data directory for all daemon state |
| `--network` | `mainnet` | Bitcoin network: mainnet, testnet, regtest, simnet, signet |
| `--debuglevel` | `info` | Logging verbosity: trace, debug, info, warn, error, critical |
| `--allow-mainnet` | `false` | Required to run on mainnet (safety guard) |
| `--wallet.type` | `lwwallet` | Wallet backend: `lwwallet` or `lnd` |
| `--wallet.esploraurl` | | Esplora REST API URL (lwwallet only) |
| `--wallet.pollinterval` | `5s` | Esplora poll interval (lwwallet only) |
| `--wallet.recoverywindow` | `100` | Address look-ahead window (lwwallet only) |
| `--wallet.password_file` | | Auto-unlock password file path (lwwallet only) |
| `--lnd.host` | `localhost:10009` | lnd gRPC address |
| `--lnd.tlspath` | | Path to lnd TLS certificate |
| `--lnd.macaroonpath` | | Path to lnd admin macaroon |
| `--lnd.rpctimeout` | `30s` | Timeout for lnd RPC calls |
| `--server.host` | `localhost:10010` | Ark operator mailbox address |
| `--server.insecure` | `false` | Disable TLS for server connection |
| `--server.tlscertpath` | | Operator TLS certificate path |
| `--server.localmailboxid` | | This client's mailbox ID |
| `--server.remotemailboxid` | | Server's mailbox ID |
| `--rpc.listenaddr` | `localhost:10029` | Daemon gRPC listen address |
| `--rpc.tlscertpath` | | Custom TLS cert for daemon RPC |
| `--rpc.tlskeypath` | | Custom TLS key for daemon RPC |

### Environment Variables

All flags can be set via environment variables with the `DAREPOD_`
prefix and dots replaced by underscores:

| Variable | Description |
|----------|-------------|
| `DAREPOD_WALLET_PASSWORD` | Wallet password for create/unlock |
| `DAREPOD_LWWALLET_SEED` | Hex-encoded raw seed (dev/CI only) |
| `DAREPOD_NETWORK` | Bitcoin network override |
| `DAREPOD_WALLET_TYPE` | Wallet backend type |
| `DAREPOD_WALLET_ESPLORAURL` | Esplora URL |
| `DAREPOD_SERVER_HOST` | Operator server address |

## Initial Wallet Setup

After starting the daemon, the wallet must be created and unlocked
before any operations can proceed.

### Step 1: Create the Wallet

In lwwallet mode, wallet creation generates a new aezeed mnemonic,
encrypts the seed with your password, and saves it to
`~/.darepod/<network>/wallet_seed.enc`.

```bash
# Via environment variable (recommended for automation)
DAREPOD_WALLET_PASSWORD=your_password darepocli wallet create --no-tls

# Via stdin pipe
echo -n 'your_password' | darepocli wallet create --no-tls

# Via password file
darepocli wallet create \
  --wallet_password_file=/path/to/password_file \
  --no-tls

# Interactive (prompts for password on TTY)
darepocli wallet create --no-tls
```

**Important:** The mnemonic is displayed on stderr during creation.
Write it down and store it securely -- it is your only backup.

### Step 2: Unlock the Wallet

Each time the daemon restarts, the wallet must be unlocked:

```bash
DAREPOD_WALLET_PASSWORD=your_password darepocli wallet unlock --no-tls
```

### Auto-Unlock

To skip the manual unlock step, provide the password file at daemon
startup:

```bash
darepod \
  --wallet.type=lwwallet \
  --wallet.password_file=/path/to/password_file \
  ...
```

The daemon reads the file, decrypts the seed, and starts the wallet
automatically.

## CLI Reference

`darepocli` connects to the daemon's gRPC server. All output is JSON.

### Global Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--rpcserver` | `localhost:10029` | Daemon gRPC address |
| `--tlscertpath` | | Daemon TLS cert path |
| `--no-tls` | `false` | Disable TLS (use for regtest) |

### Commands

#### `getinfo`

Display daemon status information.

```bash
darepocli getinfo --no-tls
```

#### `wallet create`

Create a new wallet from a fresh seed.

| Flag | Type | Description |
|------|------|-------------|
| `--wallet_password_file` | string | Path to file containing wallet password |
| `--seed_passphrase` | string | Optional aezeed passphrase |
| `--json` | string | Raw InitWalletRequest proto-JSON |

#### `wallet unlock`

Unlock an existing wallet.

| Flag | Type | Description |
|------|------|-------------|
| `--wallet_password_file` | string | Path to file containing wallet password |
| `--json` | string | Raw UnlockWalletRequest proto-JSON |

#### `wallet balance`

Display wallet balance (boarding, VTXO, total) in satoshis.

```bash
darepocli wallet balance --no-tls
```

#### `wallet newaddress`

Generate a new taproot boarding address for receiving on-chain funds.

```bash
darepocli wallet newaddress --no-tls
```

#### `vtxos list`

List VTXOs known to the wallet with optional filters.

| Flag | Type | Description |
|------|------|-------------|
| `--status` | string | Filter by status: live, refresh_requested, forfeiting, forfeited, spent, expiring, failed |
| `--min_amount` | int64 | Minimum amount in sats |
| `--fields` | string | Comma-separated field names to include |
| `--ndjson` | bool | Emit one JSON object per VTXO (newline-delimited) |

```bash
# All VTXOs
darepocli vtxos list --no-tls

# Live VTXOs above 10k sats, only outpoint and amount
darepocli vtxos list --status live --min_amount 10000 \
  --fields outpoint,amount_sat --no-tls

# Streaming NDJSON for piping to jq
darepocli vtxos list --ndjson --no-tls | jq '.amount_sat'
```

#### `vtxos refresh`

Queue VTXOs for refresh in the next round (extends expiry).

| Flag | Type | Description |
|------|------|-------------|
| `--outpoint` | string[] | VTXO outpoint(s) to refresh (txid:index) |
| `--all` | bool | Refresh all live VTXOs |
| `--dry_run` | bool | Validate without queuing |

```bash
# Refresh all
darepocli vtxos refresh --all --no-tls

# Dry run first
darepocli vtxos refresh --all --dry_run --no-tls
```

#### `send inround`

Send via in-round refresh (waits for next round to commit).

| Flag | Type | Description |
|------|------|-------------|
| `--to` | string[] | Recipient address(es) |
| `--amount` | int64[] | Amount(s) in sats (one per --to) |
| `--dry_run` | bool | Validate without submitting |

```bash
# Single recipient
darepocli send inround --to tb1p... --amount 50000 --no-tls

# Multiple recipients
darepocli send inround \
  --to tb1p...addr1 --amount 50000 \
  --to tb1p...addr2 --amount 30000 \
  --no-tls

# Via JSON input
darepocli send inround --no-tls --json '{
  "recipients": [
    {"address": "tb1p...", "amount_sat": 50000},
    {"address": "tb1p...", "amount_sat": 30000}
  ]
}'
```

#### `send oor`

Send via out-of-round transfer (immediate, through operator).

| Flag | Type | Description |
|------|------|-------------|
| `--to` | string | Recipient address |
| `--amount` | int64 | Amount in sats |
| `--dry_run` | bool | Validate without initiating |

```bash
darepocli send oor --to tb1p... --amount 25000 --no-tls
```

#### `schema`

Introspect available CLI commands and their parameters.

```bash
# List all methods
darepocli schema --no-tls

# Show specific method details
darepocli schema --method vtxos.list --no-tls
```

#### `mcp serve`

Start an MCP (Model Context Protocol) server on stdio for AI agent
integration. Exposes daemon RPCs as typed tool calls.

```bash
darepocli mcp serve --no-tls
```

**Note:** Wallet management tools (create, unlock, genseed) are
intentionally excluded from MCP to prevent sensitive material from
transiting the protocol. Use the CLI directly for wallet operations.

## Regtest Quickstart

A complete end-to-end workflow on regtest:

```bash
# 1. Start a regtest bitcoind + Esplora (e.g., via Nigiri)
nigiri start

# 2. Start the daemon
darepod \
  --network=regtest \
  --wallet.type=lwwallet \
  --wallet.esploraurl=http://localhost:3000 \
  --server.host=localhost:10010 \
  --server.insecure \
  --server.localmailboxid=client1 \
  --server.remotemailboxid=server \
  --rpc.listenaddr=localhost:10029

# 3. Create and unlock the wallet
DAREPOD_WALLET_PASSWORD=testpass \
  darepocli wallet create --no-tls

# 4. Get a boarding address
darepocli wallet newaddress --no-tls

# 5. Fund it
bitcoin-cli -regtest sendtoaddress <addr> 0.01
bitcoin-cli -regtest generatetoaddress 1 <miner_addr>

# 6. Verify balance
darepocli wallet balance --no-tls

# 7. List VTXOs
darepocli vtxos list --no-tls

# 8. Send funds
darepocli send inround --to tb1p... --amount 5000 --no-tls
```

## Password Handling

Wallet passwords are never accepted as CLI arguments. The priority
order for password resolution:

1. **stdin pipe** -- `echo -n 'pass' | darepocli wallet unlock`
2. **Environment variable** -- `DAREPOD_WALLET_PASSWORD=pass`
3. **Password file** -- `--wallet_password_file=/path/to/file`
4. **Interactive prompt** -- prompted on TTY if none of the above

For production deployments, use the password file approach with
restrictive file permissions (`chmod 600`).

## Troubleshooting

| Problem | Solution |
|---------|----------|
| `connection refused` | Daemon not running or wrong `--rpcserver` address |
| `wallet not ready` | Run `darepocli wallet unlock` first |
| `wallet already exists` | Wallet was already created; use `unlock` instead |
| `GenSeed: lwwallet mode only` | Switch daemon to `--wallet.type=lwwallet` |
| TLS certificate errors | Use `--no-tls` for regtest, or set `--tlscertpath` |
| `password must be at least 8 bytes` | Wallet password minimum is 8 characters |
| `decryption failed: wrong password` | Incorrect password or corrupted seed file |
