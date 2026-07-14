# waved / wavecli User Guide

This guide covers installing, configuring, and operating the Ark client
daemon (`waved`) and its CLI (`wavecli`).

## Installation

### From Source

Requires Go 1.26+ (see `go.mod`).

```bash
git clone https://github.com/lightninglabs/wavelength.git
cd wavelength

# Build both binaries into bin/
make build

# Or install to $GOPATH/bin
make install
```

After building, two binaries are produced:

- `bin/waved` -- the long-running daemon process
- `bin/wavecli` -- the CLI for controlling the daemon

### Optional: wavewalletrpc

The "user-facing" verbs in the CLI (`balance`, `recv`, `send`, `activity`,
`create`, `unlock`) route through the `wavewalletrpc` subserver. That code
is gated behind the `wavewalletrpc` build tag; the default `make build` does
**not** enable it. Without the tag, those verbs return:

```
daemon was not built with -tags wavewalletrpc;
rebuild with `make build-wavewalletrpc` or see docs/wavewalletrpc_build.md
```

Two options:

1. **Use the `ark *` and `dev *` subtrees** — same RPCs as the
   top-level verbs but exposed under power-user parents. Works with
   the default build.
2. **Build with the tag enabled** when you want the top-level verbs:
   ```bash
   make build-wavewalletrpc       # produces wavewalletrpc-enabled waved
   make install-wavewalletrpc
   ```

See [wavewalletrpc_build.md](wavewalletrpc_build.md) for more.

## Daemon Configuration

`waved` supports two wallet backends: **lwwallet** (standalone,
in-process) and **lnd** (uses an existing lnd node).

### lwwallet Mode (Standalone)

The lightweight wallet requires only an Esplora API endpoint for chain
access. No external lnd node is needed.

```bash
waved \
  --network=regtest \
  --wallet.type=lwwallet \
  --wallet.esploraurl=http://localhost:3000 \
  --server.host=localhost:10010 \
  --server.insecure \
  --rpc.listenaddr=localhost:10029
```

### lnd Mode

Uses an existing lnd node for signing and key derivation. Point the
daemon at the lnd gRPC interface with the TLS cert and admin macaroon.

```bash
waved \
  --network=regtest \
  --wallet.type=lnd \
  --lnd.host=localhost:10009 \
  --lnd.tlspath=~/.lnd/tls.cert \
  --lnd.macaroonpath=~/.lnd/data/chain/bitcoin/regtest/admin.macaroon \
  --server.host=localhost:10010 \
  --server.insecure \
  --rpc.listenaddr=localhost:10029
```

### Daemon Flags Reference

| Flag | Default | Description |
|------|---------|-------------|
| `--datadir` | `~/.waved` | Root data directory for all daemon state |
| `--network` | `mainnet` | Bitcoin network: mainnet, testnet, testnet4, regtest, simnet, signet |
| `--debuglevel` | `info` | Logging verbosity: trace, debug, info, warn, error, critical |
| `--logdir` | `~/.waved/logs/<network>` | Directory for persistent daemon logs |
| `--allow-mainnet` | `false` | Required to run on mainnet (safety guard) |
| `--wallet.type` | `lwwallet` | Wallet backend: `lwwallet`, `lnd`, or `btcwallet` |
| `--wallet.esploraurl` | | Esplora REST API URL (lwwallet only) |
| `--wallet.feeurl` | | Fee-estimate JSON endpoint URL (btcwallet only) |
| `--wallet.btcwallet_blockheaderssource` | | Block header import source for btcwallet fast sync |
| `--wallet.btcwallet_filterheaderssource` | | Filter header import source for btcwallet fast sync |
| `--wallet.pollinterval` | `30s` | Esplora poll interval (lwwallet only) |
| `--wallet.recoverywindow` | `100` | Address look-ahead window (lwwallet only) |
| `--wallet.password_file` | | Auto-unlock password file path (lwwallet/btcwallet) |
| `--lnd.host` | `localhost:10009` | lnd gRPC address |
| `--lnd.tlspath` | | Path to lnd TLS certificate |
| `--lnd.macaroonpath` | | Path to lnd admin macaroon |
| `--lnd.rpctimeout` | `30s` | Timeout for lnd RPC calls |
| `--server.host` | network default | Ark operator address override for the selected transport |
| `--server.transport` | `grpc` | Ark operator transport: `grpc` or `rest` |
| `--server.insecure` | `false` | Disable TLS for server connection |
| `--server.tlscertpath` | | Operator TLS certificate path |
| `--rpc.listenaddr` | `localhost:10029` | Daemon gRPC listen address |
| `--rpc.tlscertpath` | | Custom TLS cert for daemon RPC |
| `--rpc.tlskeypath` | | Custom TLS key for daemon RPC |
| `--swap.serveraddress` | network default | Swap server address override for swapruntime builds |
| `--swap.servertransport` | `grpc` | Swap server transport: `grpc` or `rest` |

Empty Ark and swap addresses resolve from the selected network and transport.
See [signet.md](signet.md) for the testnet3, testnet4, and signet endpoints and
override examples.

There is no mailbox-ID flag: the client and compound server mailbox IDs are
derived automatically from the client's identity key at connect time.

### Environment Variables

All flags can be set via environment variables with the `WAVED_`
prefix and dots replaced by underscores:

| Variable | Description |
|----------|-------------|
| `WAVED_WALLET_PASSWORD` | Wallet password for create/unlock |
| `WAVED_LWWALLET_SEED` | Hex-encoded raw seed (dev/CI only) |
| `WAVED_NETWORK` | Bitcoin network override |
| `WAVED_WALLET_TYPE` | Wallet backend type |
| `WAVED_WALLET_ESPLORAURL` | Esplora URL |
| `WAVED_SERVER_HOST` | Operator server address |

## Initial Wallet Setup

After starting the daemon, the wallet must be created and unlocked
before any operations can proceed.

The `create` and `unlock` CLI commands require a daemon built with
`wavewalletrpc` (see Installation above). For the default build, configure
auto-unlock via `--wallet.password_file` and skip the CLI step.

### Step 1: Create the Wallet (wavewalletrpc only)

In lwwallet mode, wallet creation generates a new aezeed mnemonic
and creates the wallet database with its key material encrypted under
your password. Record the mnemonic offline: it is shown once and is
the only backup.

```bash
# Via environment variable (recommended for automation)
WAVED_WALLET_PASSWORD=your_password wavecli create

# Via stdin pipe
echo -n 'your_password' | wavecli create

# Via password file
wavecli create \
  --wallet_password_file=/path/to/password_file

# Interactive (prompts for password on TTY)
wavecli create
```

**Important:** The mnemonic is displayed on stderr during creation.
Write it down and store it securely -- it is your only backup.

### Step 2: Unlock the Wallet (wavewalletrpc only)

Each time the daemon restarts, the wallet must be unlocked:

```bash
WAVED_WALLET_PASSWORD=your_password wavecli unlock
```

### Auto-Unlock

To skip the manual unlock step, provide the password file at daemon
startup:

```bash
waved \
  --wallet.type=lwwallet \
  --wallet.password_file=/path/to/password_file \
  ...
```

The daemon reads the file, decrypts the seed, and starts the wallet
automatically.

## CLI Reference

`wavecli` connects to the daemon's gRPC server. All output is JSON.

### Authentication

`wavecli` authenticates to the daemon over TLS with the daemon's admin
macaroon. By default it derives both from the daemon data directory:

- TLS cert: `<datadir>/data/<network>/tls.cert`
- Macaroon: `<datadir>/data/<network>/admin.macaroon`

where `--datadir` defaults to `~/.waved` and `--network` to `mainnet`. A
daemon run on a **non-default datadir or network must be matched on the CLI**,
otherwise `wavecli` looks under `~/.waved/data/mainnet/` and fails with
`read macaroon: ... no such file`. For a signet instance under
`~/.waved-signet`:

```bash
wavecli --network=signet --datadir=~/.waved-signet getinfo
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
alias wave='wavecli --network=signet --datadir=~/.waved-signet'

# local regtest over plaintext
alias wave='wavecli --no-tls --no-macaroons'
```

### Command tree

The everyday wallet verbs and daemon introspection make up the default
`--help` face. The advanced `ark`, `recovery`, and `dev` subtrees are
hidden from `--help` (set `WAVELENGTH_DEV=1` to reveal them under an "Advanced"
group) but stay fully runnable in every build — `wavecli ark …` works
with or without the env var. `WAVELENGTH_DEV` only changes visibility; it never
gates execution.

The `swap` subtree was retired — `send`/`recv --offchain` and `activity`
cover it, and a stale `wavecli swap …` fails with a hint toward
`send`/`recv`. The `swapruntime` daemon runtime that powers the offchain
verbs is unchanged.

```
wavecli
├── getinfo                   — daemon status (no wavewalletrpc)
├── balance                   — wallet balances (wavewalletrpc)
├── create / unlock           — wallet bring-up (wavewalletrpc)
├── recv                      — boarding address / Lightning invoice (wavewalletrpc)
├── send                      — Lightning invoice / onchain leave (wavewalletrpc)
├── activity [inspect]        — unified wallet activity feed (wavewalletrpc)
├── exit {status|summary|plan} — cooperative leave by default, forced unroll (wavewalletrpc)
├── wallet-sweep              — sweep backing wallet to a destination (wavewalletrpc)
├── mcp serve                 — MCP server for AI agents (wavewalletrpc)
├── schema                    — JSON dump of CLI methods
├── ark                       — power-user parent (hidden; no wavewalletrpc)
│   ├── board                 — board confirmed boarding UTXOs
│   ├── vtxos {list|refresh|leave}
│   ├── oor {receive|get|list}
│   ├── send {oor|inround}
│   ├── rounds {get|list|join|watch}
│   ├── sweep [list]
│   ├── fees {estimate|history}
│   └── listtransactions
├── recovery {list|status|escalate|cancel} — daemon-owned vHTLC recovery rows (hidden)
└── dev                       — generated low-level RPC (hidden; no wavewalletrpc)
    └── daemon <Method>       — call any waverpc.DaemonService method
```

### Global Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--rpcserver` | `localhost:10029` | Daemon gRPC address |
| `--datadir` | `~/.waved` | Daemon data dir; TLS cert + macaroon are derived from `<datadir>/data/<network>/` |
| `--network` | `mainnet` | Network segment of the derived cert / macaroon paths |
| `--tlscertpath` | | Explicit daemon TLS cert path (overrides `--datadir`) |
| `--macaroonpath` | | Explicit admin macaroon path (overrides `--datadir`) |
| `--no-tls` | `false` | Disable TLS (regtest / dev); requires `--no-macaroons` |
| `--no-macaroons` | `false` | Disable macaroon auth (required alongside `--no-tls`) |
| `--json` | | Raw JSON request payload (overrides bespoke flags) |

### `getinfo`

Display daemon status information.

```bash
wavecli getinfo
```

### `balance` (wavewalletrpc) / `dev daemon GetBalance` (no wavewalletrpc)

Display wallet balance (boarding, VTXO, total, onchain) in satoshis.

```bash
wavecli balance                   # requires wavewalletrpc
wavecli dev daemon GetBalance     # always available
```

### `recv` (wavewalletrpc) / `dev daemon NewAddress` (no wavewalletrpc)

Allocate an inbound payment surface.

| Flag | Type | Description |
|------|------|-------------|
| `--offchain` | bool | Returns a BOLT-11 invoice via the swap subsystem (default) |
| `--onchain` | bool | Returns a fresh boarding address |
| `--amt` | uint | Required for `--offchain`; ignored for `--onchain` |
| `--amt_hint` | uint | Optional expected amount for `--onchain` (accounting only) |
| `--memo` | string | Optional memo embedded in the offchain invoice |

```bash
wavecli recv --onchain                  # boarding address
wavecli recv --offchain --amt 5000 --memo coffee

# No-wavewalletrpc equivalent for the boarding-address case:
wavecli dev daemon NewAddress
```

### `ark board` / `dev daemon Board`

Trigger the client to join the next round with any confirmed boarding
UTXOs.

| Flag | Type | Description |
|------|------|-------------|
| `--target-vtxo-count` | uint32 | Fan boarded balance into N VTXOs (default 1) |
| `--no-persist` | bool | Skip restart-safe replay (testing only) |

```bash
wavecli ark board
wavecli ark board --target-vtxo-count 4
```

### `ark oor receive` — incoming OOR pubkey

Allocate a fresh out-of-round receive script backed by a newly derived
wallet key. Returns `pk_script_hex`, `pubkey_xonly_hex`, and the wallet
key locator.

| Flag | Type | Description |
|------|------|-------------|
| `--label` | string | Optional indexer registration label |

```bash
wavecli ark oor receive
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
wavecli ark vtxos list

# Live VTXOs above 10k sats, only outpoint and amount
wavecli ark vtxos list --status live --min_amount 10000 \
  --fields outpoint,amount_sat

# Streaming NDJSON for piping to jq
wavecli ark vtxos list --ndjson | jq '.amount_sat'
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
wavecli ark vtxos refresh --outpoint <txid:idx>

# Batch with other intents — explicitly join later
wavecli ark vtxos refresh --outpoint <txid:idx> --no_join
wavecli ark vtxos leave   --outpoint <txid:idx> --no_join \
  --address bcrt1p...
wavecli ark rounds join
```

### `ark send inround`

Send via in-round refresh (waits for next round to commit).

| Flag | Type | Description |
|------|------|-------------|
| `--to` | string[] | Recipient address(es) (bech32m) |
| `--pubkey` | string[] | Recipient x-only pubkey hex(es); paired after `--to` entries |
| `--amount` | int64[] | Amount(s) in sats (one per recipient, `--to` then `--pubkey` order) |
| `--dry_run` | bool | Validate without submitting |

```bash
wavecli ark send inround --to bcrt1p... --amount 50000

# Multiple recipients
wavecli ark send inround \
  --to bcrt1p...addr1 --amount 50000 \
  --to bcrt1p...addr2 --amount 30000

# Via JSON input
wavecli ark send inround --json '{
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
wavecli ark send oor --pubkey <pubkey_xonly_hex> --amount 25000
wavecli ark send oor --pubkey <hex> --amount 25000 \
  --idempotency_key my-attempt-1
```

### `send <invoice-or-address>` (wavewalletrpc)

Unified send for Lightning invoice (`--offchain`, default) or onchain
send (`--onchain`). Onchain sends are atomic: the destination receives
exactly `--amt` sats and any residual lands back in the wallet as a
change VTXO. Use `--sweep-all` with `--amt=0` to drain every live VTXO
to the destination instead.

By default `send` blocks until the payment reaches a terminal state
(printing the Lightning preimage on success); pass `--no-wait` to
return as soon as the send is dispatched. Non-interactive callers must
pass `--force` or `--yes` to skip the confirmation prompt.

| Flag | Type | Description |
|------|------|-------------|
| `--offchain` | bool | BOLT-11 dispatch via swap subsystem (default) |
| `--onchain` | bool | Atomic onchain send via `SendOnChain` |
| `--amt` | uint | Amount in sats (required for onchain unless `--sweep-all`) |
| `--max_fee` | uint | Max swap fee in sats (invoice sends only) |
| `--note` | string | Caller-supplied label |
| `--sweep-all` | bool | Onchain only: drain wallet; `--amt` must be 0 |
| `--force` / `--yes` | bool | Skip the interactive confirmation prompt |
| `--no-wait` | bool | Return once dispatched instead of blocking to a terminal state |
| `--dry-run` | bool | Prepare and print the preview without dispatching |

```bash
wavecli send lnbcrt... --offchain --force
wavecli send bcrt1... --onchain --amt 1000 --force
wavecli send bcrt1... --onchain --sweep-all --force
```

### `exit` (cooperative by default, unilateral with `--force-unroll-ack`)

Cooperatively exit a VTXO by default; queues the outpoint for
cooperative leave and joins the next round. Unilateral (on-chain)
unroll only starts when `--force-unroll-ack` is set to the exact
string `I_KNOW_WHAT_I_AM_DOING`. `exit` replaces the legacy `unroll`
verb at the user surface.

| Flag | Type | Description |
|------|------|-------------|
| `--outpoint` | string | VTXO outpoint to exit (txid:vout) |
| `--onchain-address` | string | Cooperative leave destination; omitted generates a fresh wallet address |
| `--force-unroll-ack` | string | Must be exactly `I_KNOW_WHAT_I_AM_DOING` to force unilateral unroll |
| `--dry-run` | bool | Validate locally and print the preview without dispatching |

```bash
wavecli exit --outpoint <txid:vout>
wavecli exit --outpoint <txid:vout> --force-unroll-ack I_KNOW_WHAT_I_AM_DOING
wavecli exit status --outpoint <txid:vout>
wavecli exit summary
wavecli exit plan --outpoint <txid:vout>
```

`exit status` reports progress for a forced unilateral unroll job (it
survives daemon restarts); `exit summary` aggregates all in-progress
exits; `exit plan` previews backing-wallet funding readiness before
forcing an exit. Status enum (`UNROLL_JOB_STATUS_*`) still uses the old
"unroll" naming.

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
wavecli ark listtransactions --limit 25

wavecli ark listtransactions \
  --type oor \
  --from 2026-05-01T00:00:00Z \
  --to 2026-05-08T23:59:59Z
```

### `activity` (wavewalletrpc)

The merged wallet activity feed: send / recv / deposit / exit history.

| Flag | Default | Purpose |
|------|---------|---------|
| `--pending` | | Only entries still in flight |
| `--kind` | | Filter by kind (`send,recv,deposit,exit`); repeatable |
| `--limit` | daemon default | Page size |
| `--cursor` | | Page token from a prior page's `next_cursor` |
| `--format` | `table` | Output format (`table`, `expanded`/`x`, `json`) |

```bash
wavecli activity
wavecli activity --pending --kind send,recv
wavecli activity --format json
wavecli activity --cursor <next_cursor>
wavecli activity inspect <id>
```

The VTXO inventory and onchain history are not part of the activity feed.
Use the `ark` subtree for those:

```bash
wavecli ark vtxos list          # live VTXO inventory
wavecli ark listtransactions    # raw transaction / onchain history
wavecli ark sweep list          # boarding-timeout sweep records
```

### `schema`

Introspect available CLI commands and their parameters.

```bash
wavecli schema
wavecli schema ark.vtxos.list
wavecli schema --all
```

### `mcp serve` (wavewalletrpc)

Start an MCP (Model Context Protocol) server on stdio for AI agent
integration. Exposes daemon RPCs as typed tool calls.

```bash
wavecli mcp serve
```

**Note:** Wallet management tools (`create`, `unlock`, genseed) are
intentionally excluded from MCP to prevent sensitive material from
transiting the protocol. Use the CLI directly for wallet operations.
`ark.oor.receive` is exposed because it only allocates a fresh
wallet-derived receive target and does not reveal seed material.

## Regtest Quickstart

A complete end-to-end workflow on regtest using the default (no
`wavewalletrpc`) build. With a `wavewalletrpc`-enabled daemon, swap the
relevant commands per the references above.

```bash
# 1. Start a regtest bitcoind + Esplora (e.g., via Nigiri)
nigiri start

# 2. Start the daemon. Auto-unlock so you don't need wavewalletrpc to
#    create/unlock from the CLI.
waved \
  --network=regtest \
  --wallet.type=lwwallet \
  --wallet.esploraurl=http://localhost:3000 \
  --wallet.password_file=/path/to/password_file \
  --server.host=localhost:10010 \
  --server.insecure \
  --rpc.listenaddr=localhost:10029

# 2b. Alias the CLI for this regtest daemon: plaintext transport (no TLS,
#     no macaroon) on the regtest network. See the Authentication section
#     above for why both --no-tls and --no-macaroons are needed.
alias wave='wavecli --no-tls --no-macaroons --network=regtest'

# 3. Get a boarding address.
ADDR=$(wave dev daemon NewAddress | jq -r .address)

# 4. Fund it on-chain and confirm.
bitcoin-cli -regtest sendtoaddress "$ADDR" 0.01
bitcoin-cli -regtest -generate 6

# 5. Verify balance.
wave dev daemon GetBalance

# 6. Board into the next round.
wave ark board

# 7. After the round confirms, list the new VTXO.
wave ark vtxos list

# 8. Send funds (in-round to a peer's bech32m address).
wave ark send inround --to bcrt1p... --amount 5000
```

For per-client manual testing under the `arktest` harness, see
`MANUAL_TESTING.md` at the server (wavelength) repo root.

## Password Handling

Wallet passwords are never accepted as CLI arguments. The priority
order for password resolution:

1. **Environment variable** -- `WAVED_WALLET_PASSWORD=pass`
2. **Password file** -- `--wallet_password_file=/path/to/file`
3. **stdin pipe** -- `echo -n 'pass' | wavecli unlock`
4. **Interactive prompt** -- prompted on TTY if none of the above

For production deployments, use the password file approach with
restrictive file permissions (`chmod 600`).

## Troubleshooting

| Problem | Solution |
|---------|----------|
| `daemon was not built with -tags wavewalletrpc` | Rebuild with `make build-wavewalletrpc`, or use the `ark *` / `dev *` subtrees |
| `connection refused` | Daemon not running or wrong `--rpcserver` address |
| `wallet not ready` | Run `wavecli unlock` (requires wavewalletrpc) or restart the daemon with `--wallet.password_file` |
| `wallet already exists` | Wallet was already created; use `unlock` instead |
| `GenSeed is only available in lwwallet/btcwallet mode` | Switch daemon to `--wallet.type=lwwallet` or `btcwallet` |
| `read macaroon: ... no such file` | The CLI is looking under the wrong data dir/network. Pass `--datadir` / `--network` to match the daemon (see [Authentication](#authentication)), or `--macaroonpath` directly |
| `credentials require transport level security` | A macaroon can't ride a plaintext connection. Use TLS (drop `--no-tls`), or add `--no-macaroons` alongside `--no-tls` |
| TLS certificate errors | Point `--datadir` / `--network` at the daemon's cert, set `--tlscertpath`, or use `--no-tls --no-macaroons` for regtest |
| `password must be at least 8 bytes` | Wallet password minimum is 8 characters |
| `decryption failed: wrong password` | Incorrect password or corrupted seed file |
