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

### Optional: walletdkrpc

The "user-facing" verbs in the CLI (`balance`, `recv`, `send`, `activity`,
`create`, `unlock`) route through the `walletdkrpc` subserver. That code
is gated behind the `walletdkrpc` build tag; the default `make build` does
**not** enable it. Without the tag, those verbs return:

```
daemon was not built with -tags walletdkrpc;
rebuild with `make build-walletdkrpc` or see docs/walletdkrpc_build.md
```

Two options:

1. **Use the `ark *` and `dev *` subtrees** — same RPCs as the
   top-level verbs but exposed under power-user parents. Works with
   the default build.
2. **Build with the tag enabled** when you want the top-level verbs:
   ```bash
   make build-walletdkrpc       # produces walletdkrpc-enabled darepod
   make install-walletdkrpc
   ```

See [walletdkrpc_build.md](walletdkrpc_build.md) for more.

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
| `--network` | `mainnet` | Bitcoin network: mainnet, testnet, testnet4, regtest, simnet, signet |
| `--debuglevel` | `info` | Logging verbosity: trace, debug, info, warn, error, critical |
| `--logdir` | `~/.darepod/logs/<network>` | Directory for persistent daemon logs |
| `--allow-mainnet` | `false` | Required to run on mainnet (safety guard) |
| `--wallet.type` | `lwwallet` | Wallet backend: `lwwallet`, `lnd`, or `btcwallet` |
| `--wallet.esploraurl` | | Esplora REST API URL (lwwallet only) |
| `--wallet.feeurl` | | Fee-estimate JSON endpoint URL (btcwallet only) |
| `--wallet.btcwallet_blockheaderssource` | | Block header import source for btcwallet fast sync |
| `--wallet.btcwallet_filterheaderssource` | | Filter header import source for btcwallet fast sync |
| `--wallet.pollinterval` | `5s` | Esplora poll interval (lwwallet only) |
| `--wallet.recoverywindow` | `100` | Address look-ahead window (lwwallet only) |
| `--wallet.password_file` | | Auto-unlock password file path (lwwallet/btcwallet) |
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

The `create` and `unlock` CLI commands require a daemon built with
`walletdkrpc` (see Installation above). For the default build, configure
auto-unlock via `--wallet.password_file` and skip the CLI step.

### Step 1: Create the Wallet (walletdkrpc only)

In lwwallet mode, wallet creation generates a new aezeed mnemonic
and creates the wallet database with its key material encrypted under
your password. Record the mnemonic offline: it is shown once and is
the only backup.

```bash
# Via environment variable (recommended for automation)
DAREPOD_WALLET_PASSWORD=your_password darepocli create

# Via stdin pipe
echo -n 'your_password' | darepocli create

# Via password file
darepocli create \
  --wallet_password_file=/path/to/password_file

# Interactive (prompts for password on TTY)
darepocli create
```

**Important:** The mnemonic is displayed on stderr during creation.
Write it down and store it securely -- it is your only backup.

### Step 2: Unlock the Wallet (walletdkrpc only)

Each time the daemon restarts, the wallet must be unlocked:

```bash
DAREPOD_WALLET_PASSWORD=your_password darepocli unlock
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

### Authentication

`darepocli` authenticates to the daemon over TLS with the daemon's admin
macaroon. By default it derives both from the daemon data directory:

- TLS cert: `<datadir>/data/<network>/tls.cert`
- Macaroon: `<datadir>/data/<network>/admin.macaroon`

where `--datadir` defaults to `~/.darepod` and `--network` to `mainnet`. A
daemon run on a **non-default datadir or network must be matched on the CLI**,
otherwise `darepocli` looks under `~/.darepod/data/mainnet/` and fails with
`read macaroon: ... no such file`. For a signet instance under
`~/.darepod-signet`:

```bash
darepocli --network=signet --datadir=~/.darepod-signet getinfo
```

A macaroon cannot ride an unencrypted connection, so `--no-tls` and the
macaroon are mutually exclusive. There are two working modes:

- **TLS + macaroon (default).** Point `--datadir` / `--network` at the daemon,
  or override `--tlscertpath` / `--macaroonpath` directly.
- **Plaintext (regtest / dev).** Pass **both** `--no-tls` and `--no-macaroons`.
  Passing only `--no-tls` fails with `credentials require transport level
  security`, because the CLI still tries to attach the macaroon.

The command examples below omit these connection flags. Set them once via an
alias:

```bash
# signet instance over TLS
alias da='darepocli --network=signet --datadir=~/.darepod-signet'

# local regtest over plaintext
alias da='darepocli --no-tls --no-macaroons'
```

### Command tree

```
darepocli
├── getinfo                   — daemon status (no walletdkrpc)
├── balance                   — wallet balances (walletdkrpc)
├── create / unlock           — wallet bring-up (walletdkrpc)
├── recv                      — boarding address / Lightning invoice (walletdkrpc)
├── send                      — Lightning invoice / onchain leave (walletdkrpc)
├── activity                  — unified wallet activity feed (walletdkrpc)
├── exit [status]             — unilateral exit a VTXO
├── mcp serve                 — MCP server for AI agents (walletdkrpc)
├── schema                    — JSON dump of CLI methods
├── ark                       — power-user parent (no walletdkrpc)
│   ├── board                 — board confirmed boarding UTXOs
│   ├── vtxos {list|refresh|leave}
│   ├── oor {receive|get|list}
│   ├── send {oor|inround}
│   ├── rounds {get|list|watch}
│   ├── sweep [list]
│   ├── fees {estimate|history}
│   └── listtransactions
└── dev                       — generated low-level RPC (no walletdkrpc)
    └── daemon <Method>       — call any daemonrpc.DaemonService method
```

### Global Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--rpcserver` | `localhost:10029` | Daemon gRPC address |
| `--datadir` | `~/.darepod` | Daemon data dir; TLS cert + macaroon are derived from `<datadir>/data/<network>/` |
| `--network` | `mainnet` | Network segment of the derived cert / macaroon paths |
| `--tlscertpath` | | Explicit daemon TLS cert path (overrides `--datadir`) |
| `--macaroonpath` | | Explicit admin macaroon path (overrides `--datadir`) |
| `--no-tls` | `false` | Disable TLS (regtest / dev); requires `--no-macaroons` |
| `--no-macaroons` | `false` | Disable macaroon auth (required alongside `--no-tls`) |
| `--json` | | Raw JSON request payload (overrides bespoke flags) |

### `getinfo`

Display daemon status information.

```bash
darepocli getinfo
```

### `balance` (walletdkrpc) / `dev daemon GetBalance` (no walletdkrpc)

Display wallet balance (boarding, VTXO, total, onchain) in satoshis.

```bash
darepocli balance                   # requires walletdkrpc
darepocli dev daemon GetBalance     # always available
```

### `recv` (walletdkrpc) / `dev daemon NewAddress` (no walletdkrpc)

Allocate an inbound payment surface.

| Flag | Type | Description |
|------|------|-------------|
| `--offchain` | bool | Returns a BOLT-11 invoice via the swap subsystem (default) |
| `--onchain` | bool | Returns a fresh boarding address |
| `--amt` | uint | Required for `--offchain`; ignored for `--onchain` |
| `--amt_hint` | uint | Optional expected amount for `--onchain` (accounting only) |
| `--memo` | string | Optional memo embedded in the offchain invoice |

```bash
darepocli recv --onchain                  # boarding address
darepocli recv --offchain --amt 5000 --memo coffee

# No-walletdkrpc equivalent for the boarding-address case:
darepocli dev daemon NewAddress
```

### `ark board` / `dev daemon Board`

Trigger the client to join the next round with any confirmed boarding
UTXOs.

| Flag | Type | Description |
|------|------|-------------|
| `--target-vtxo-count` | uint32 | Fan boarded balance into N VTXOs (default 1) |
| `--no-persist` | bool | Skip restart-safe replay (testing only) |

```bash
darepocli ark board
darepocli ark board --target-vtxo-count 4
```

### `ark oor receive` — incoming OOR pubkey

Allocate a fresh out-of-round receive script backed by a newly derived
wallet key. Returns `pk_script_hex`, `pubkey_xonly_hex`, and the wallet
key locator.

| Flag | Type | Description |
|------|------|-------------|
| `--label` | string | Optional indexer registration label |

```bash
darepocli ark oor receive
```

### `ark vtxos list`

List VTXOs known to the wallet with optional filters.

| Flag | Type | Description |
|------|------|-------------|
| `--status` | string | Filter: live, pending_forfeit, forfeiting, forfeited, spent, unilateral_exit, failed, spending |
| `--min_amount` | int64 | Minimum amount in sats |
| `--fields` | string | Comma-separated field names to include |
| `--ndjson` | bool | Emit one JSON object per VTXO (newline-delimited) |

```bash
# All VTXOs
darepocli ark vtxos list

# Live VTXOs above 10k sats, only outpoint and amount
darepocli ark vtxos list --status live --min_amount 10000 \
  --fields outpoint,amount_sat

# Streaming NDJSON for piping to jq
darepocli ark vtxos list --ndjson | jq '.amount_sat'
```

### `ark vtxos refresh`

Queue VTXOs for refresh in the next round and (by default) join that
round immediately. Pass `--no_join` to leave the intent queued in
`PendingRoundAssembly` so it can batch with subsequent refresh / leave
RPCs; commit the batch later with `ark rounds join`.

| Flag | Type | Description |
|------|------|-------------|
| `--outpoint` | string[] | VTXO outpoint(s) to refresh (txid:index) |
| `--all` | bool | Refresh all live VTXOs |
| `--dry_run` | bool | Validate without queuing |
| `--no_join` | bool | Skip the implicit `ark rounds join` follow-up |

```bash
# Explicit outpoints (auto-joins the next round)
darepocli ark vtxos refresh --outpoint <txid:idx>

# Batch with other intents — explicitly join later
darepocli ark vtxos refresh --outpoint <txid:idx> --no_join
darepocli ark vtxos leave   --outpoint <txid:idx> --no_join \
  --address bcrt1p...
darepocli ark rounds join
```

### `ark send inround`

Send via in-round refresh (waits for next round to commit).

| Flag | Type | Description |
|------|------|-------------|
| `--to` | string[] | Recipient address(es) (bech32m) |
| `--amount` | int64[] | Amount(s) in sats (one per --to) |
| `--dry_run` | bool | Validate without submitting |

```bash
darepocli ark send inround --to bcrt1p... --amount 50000

# Multiple recipients
darepocli ark send inround \
  --to bcrt1p...addr1 --amount 50000 \
  --to bcrt1p...addr2 --amount 30000 \
 

# Via JSON input
darepocli ark send inround --json '{
  "recipients": [
    {"address":"bcrt1p...","amount_sat":50000},
    {"address":"bcrt1p...","amount_sat":30000}
  ]
}'
```

### `ark send oor`

Send via out-of-round transfer (immediate, through operator).

| Flag | Type | Description |
|------|------|-------------|
| `--to` | string | Recipient address (one of `--to` / `--pubkey`) |
| `--pubkey` | string | Recipient 32-byte x-only pubkey hex |
| `--amount` | int64 | Amount in sats |
| `--idempotency_key` | string | Caller-provided key for retry-safe sends |
| `--dry_run` | bool | Validate without initiating |

```bash
darepocli ark send oor --pubkey <pubkey_xonly_hex> --amount 25000
darepocli ark send oor --pubkey <hex> --amount 25000 \
  --idempotency_key my-attempt-1
```

### `send <invoice-or-address>` (walletdkrpc)

Unified send for Lightning invoice (`--offchain`, default) or onchain
leave (`--onchain`). Onchain v1 has whole-VTXO sweep semantics —
selected VTXOs are swept in full, so the actual outflow (echoed in
`actual_amount_sat`) may exceed `--amt`.

| Flag | Type | Description |
|------|------|-------------|
| `--offchain` | bool | BOLT-11 dispatch via swap subsystem (default) |
| `--onchain` | bool | Cooperative leave via `LeaveVTXOs` |
| `--amt` | uint | Amount in sats (required for onchain unless `--sweep-all`) |
| `--max_fee` | uint | Max fee in sats |
| `--note` | string | Caller-supplied label |
| `--sweep-all` | bool | Onchain only: drain wallet; `--amt` must be 0 |

```bash
darepocli send lnbcrt... --offchain
darepocli send bcrt1... --onchain --amt 1000
darepocli send bcrt1... --onchain --sweep-all
```

### `exit` (unilateral exit, formerly `unroll`)

Start the on-chain recovery process for a VTXO.

| Flag | Type | Description |
|------|------|-------------|
| `--outpoint` | string | VTXO outpoint to exit (txid:vout) |

```bash
darepocli exit --outpoint <txid:vout>
darepocli exit status --outpoint <txid:vout>
```

The job survives daemon restarts; the command only submits the request.
Status enum (`UNROLL_JOB_STATUS_*`) still uses the old "unroll" naming.

### `ark listtransactions`

List local transaction history from the daemon's persisted accounting
and boarding-sweep records.

| Flag | Type | Description |
|------|------|-------------|
| `--from` | string | Include entries at or after this ISO 8601 timestamp |
| `--to` | string | Include entries at or before this ISO 8601 timestamp |
| `--limit` | uint32 | Maximum entries to return |
| `--offset` | uint32 | Number of filtered entries to skip |
| `--type` | string | Optional filter: boarding, round, oor, or sweep |

```bash
darepocli ark listtransactions --limit 25

darepocli ark listtransactions \
  --type oor \
  --from 2026-05-01T00:00:00Z \
  --to 2026-05-08T23:59:59Z \
 
```

### `activity` (walletdkrpc)

The merged wallet activity feed: send / recv / deposit / exit history.

| Flag | Default | Purpose |
|------|---------|---------|
| `--pending` | | Only entries still in flight |
| `--kind` | | Filter by kind (`send,recv,deposit,exit`); repeatable |
| `--limit` | daemon default | Page size |
| `--offset` | `0` | Pagination offset |
| `--format` | `table` | Output format (`table`, `expanded`/`x`, `json`) |

```bash
darepocli activity
darepocli activity --pending --kind send,recv
darepocli activity --format json
darepocli activity inspect <id>
```

The VTXO inventory and onchain history are not part of the activity feed.
Use the `ark` subtree for those:

```bash
darepocli ark vtxos list          # live VTXO inventory
darepocli ark listtransactions    # raw transaction / onchain history
darepocli ark sweep list          # boarding-timeout sweep records
```

### `schema`

Introspect available CLI commands and their parameters.

```bash
darepocli schema
darepocli schema --method ark.vtxos.list
```

### `mcp serve` (walletdkrpc)

Start an MCP (Model Context Protocol) server on stdio for AI agent
integration. Exposes daemon RPCs as typed tool calls.

```bash
darepocli mcp serve
```

**Note:** Wallet management tools (`create`, `unlock`, genseed) are
intentionally excluded from MCP to prevent sensitive material from
transiting the protocol. Use the CLI directly for wallet operations.
`receive_script` is exposed because it only allocates a fresh
wallet-derived receive target and does not reveal seed material.

## Regtest Quickstart

A complete end-to-end workflow on regtest using the default (no
`walletdkrpc`) build. With a `walletdkrpc`-enabled daemon, swap the
relevant commands per the references above.

```bash
# 1. Start a regtest bitcoind + Esplora (e.g., via Nigiri)
nigiri start

# 2. Start the daemon. Auto-unlock so you don't need walletdkrpc to
#    create/unlock from the CLI.
darepod \
  --network=regtest \
  --wallet.type=lwwallet \
  --wallet.esploraurl=http://localhost:3000 \
  --wallet.password_file=/path/to/password_file \
  --server.host=localhost:10010 \
  --server.insecure \
  --server.localmailboxid=client1 \
  --server.remotemailboxid=server \
  --rpc.listenaddr=localhost:10029

# 2b. Alias the CLI for this regtest daemon: plaintext transport (no TLS,
#     no macaroon) on the regtest network. See the Authentication section
#     above for why both --no-tls and --no-macaroons are needed.
alias da='darepocli --no-tls --no-macaroons --network=regtest'

# 3. Get a boarding address.
ADDR=$(da dev daemon NewAddress | jq -r .address)

# 4. Fund it on-chain and confirm.
bitcoin-cli -regtest sendtoaddress "$ADDR" 0.01
bitcoin-cli -regtest -generate 6

# 5. Verify balance.
da dev daemon GetBalance

# 6. Board into the next round.
da ark board

# 7. After the round confirms, list the new VTXO.
da ark vtxos list

# 8. Send funds (in-round to a peer's bech32m address).
da ark send inround --to bcrt1p... --amount 5000
```

For per-client manual testing under the `arktest` harness, see
[`MANUAL_TESTING.md`](../../MANUAL_TESTING.md) at the server repo root.

## Password Handling

Wallet passwords are never accepted as CLI arguments. The priority
order for password resolution:

1. **stdin pipe** -- `echo -n 'pass' | darepocli unlock`
2. **Environment variable** -- `DAREPOD_WALLET_PASSWORD=pass`
3. **Password file** -- `--wallet_password_file=/path/to/file`
4. **Interactive prompt** -- prompted on TTY if none of the above

For production deployments, use the password file approach with
restrictive file permissions (`chmod 600`).

## Troubleshooting

| Problem | Solution |
|---------|----------|
| `daemon was not built with -tags walletdkrpc` | Rebuild with `make build-walletdkrpc`, or use the `ark *` / `dev *` subtrees |
| `connection refused` | Daemon not running or wrong `--rpcserver` address |
| `wallet not ready` | Run `darepocli unlock` (requires walletdkrpc) or restart the daemon with `--wallet.password_file` |
| `wallet already exists` | Wallet was already created; use `unlock` instead |
| `GenSeed: lwwallet mode only` | Switch daemon to `--wallet.type=lwwallet` |
| `read macaroon: ... no such file` | The CLI is looking under the wrong data dir/network. Pass `--datadir` / `--network` to match the daemon (see [Authentication](#authentication)), or `--macaroonpath` directly |
| `credentials require transport level security` | A macaroon can't ride a plaintext connection. Use TLS (drop `--no-tls`), or add `--no-macaroons` alongside `--no-tls` |
| TLS certificate errors | Point `--datadir` / `--network` at the daemon's cert, set `--tlscertpath`, or use `--no-tls --no-macaroons` for regtest |
| `password must be at least 8 bytes` | Wallet password minimum is 8 characters |
| `decryption failed: wrong password` | Incorrect password or corrupted seed file |
