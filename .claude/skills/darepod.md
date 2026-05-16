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
# Create (password via env var for automation). Build with walletrpc:
#   make build-walletrpc
DAREPOD_WALLET_PASSWORD=testpass darepocli create --no-tls

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

The CLI surface is three tiers:

1. **Seven top-level wallet verbs (implicit, no parent)** — the everyday
   surface. Backed by `walletrpc.WalletService` (build with
   `make build-walletrpc`).
2. **Daemon introspection at root** — `getinfo`, `schema`, `mcp`, `dev`.
3. **Advanced subtrees** — `ark.*` (raw daemonrpc) and `swap.*`
   (`swapruntime` build only).

If the daemon is built without the `walletrpc` tag, the seven top-level
verbs return a structured error pointing at `docs/walletrpc_build.md`.

### Top-level wallet verbs

```bash
# Status
darepocli getinfo --no-tls

# Create + unlock (password from env, never argv).
DAREPOD_WALLET_PASSWORD=testpass darepocli create --no-tls
DAREPOD_WALLET_PASSWORD=testpass darepocli unlock --no-tls

# Balance: confirmed_sat / pending_in_sat / pending_out_sat
darepocli balance --no-tls

# Receive: offchain (invoice) or onchain (boarding address)
darepocli recv --offchain --amt 5000 --memo "coffee" --no-tls
darepocli recv --onchain --no-tls

# Send: --offchain (default) = invoice; --onchain = cooperative leave.
# The CLI does NOT sniff the destination string; pick the direction
# explicitly.
darepocli send lnbcrt... --offchain --no-tls
darepocli send bcrt1... --onchain --amt 1000 --no-tls
darepocli send bcrt1... --onchain --sweep-all --no-tls

# List: pick a view (activity = default).
darepocli list --no-tls                            # activity
darepocli list --view vtxos --no-tls               # VTXO inventory
darepocli list --view onchain --limit 100 --no-tls # on-chain history
darepocli list --pending --kind send,recv --no-tls # filter (activity)

# Exit: trigger and query a unilateral exit (unroll).
darepocli exit --outpoint TXID:VOUT --no-tls
darepocli exit status --outpoint TXID:VOUT --no-tls
```

### Advanced (`ark.*`) commands

The everyday top-level verbs compose `walletrpc` end-to-end; `ark.*`
surfaces the raw daemonrpc methods underlying them.

```bash
# Raw VTXO inventory + lifecycle
darepocli ark vtxos list --no-tls
darepocli ark vtxos list --status live --min_amount 10000 --no-tls
darepocli ark vtxos refresh --all --no-tls

# Raw transaction history (superseded for the wallet shape by
# `list --view onchain`)
darepocli ark listtransactions --no-tls

# Raw send paths
darepocli ark send inround --to tb1p... --amount 50000 --no-tls
darepocli ark send oor --to tb1p... --amount 25000 --no-tls

# JSON input for complex requests
darepocli ark send inround --no-tls --json '{
  "recipients": [
    {"address": "tb1p...", "amount_sat": 50000},
    {"address": "tb1p...", "amount_sat": 30000}
  ]
}'
```

### Password Input (Never as CLI args)

Priority order (matches `readPassword` in `wallet_password.go`):
1. `DAREPOD_WALLET_PASSWORD` env var (highest priority — wins even
   when stdin is also piped, so automated REPLs do not race).
2. `--wallet_password_file` flag (file is read and the trailing
   newline is stripped).
3. Piped stdin (non-TTY).
4. Interactive TTY prompt (lowest priority).

The optional aezeed seed passphrase is read with the same priority,
from `DAREPOD_SEED_PASSPHRASE` and `--seed_passphrase_file`. The
seed passphrase is NOT accepted via CLI args either — both secrets
stay out of `argv`.

```bash
# Env var
DAREPOD_WALLET_PASSWORD=pass darepocli unlock --no-tls

# File
darepocli unlock --wallet_password_file=/tmp/pass --no-tls

# Pipe
echo -n 'pass' | darepocli unlock --no-tls
```

## Regtest Workflow

1. Start a regtest bitcoin node + esplora.
2. Start darepod in lwwallet mode (see above) built with
   `make build-walletrpc`.
3. Create + unlock the wallet via CLI:
   `DAREPOD_WALLET_PASSWORD=testpass darepocli create --no-tls`
   `DAREPOD_WALLET_PASSWORD=testpass darepocli unlock --no-tls`
4. Get a boarding address: `darepocli recv --onchain --no-tls`
5. Fund it: `bitcoin-cli sendtoaddress <addr> 0.01`
6. Mine a block: `bitcoin-cli generatetoaddress 1 <miner_addr>`
7. Check balance: `darepocli balance --no-tls`
8. List VTXOs (once boarding completes):
   `darepocli list --view vtxos --no-tls`

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
| `wallet not ready` | Run `darepocli unlock` first |
| `wallet already exists` | Wallet was already created; use `unlock` |
| `daemon was not built with -tags walletrpc` | Rebuild with `make build-walletrpc`; the seven top-level verbs require the walletrpc tag |
| `--sweep-all requires --amt=0` | On `send --onchain`: pass `--sweep-all` for "drain wallet", or set `--amt N` |
| `--offchain and --onchain are mutually exclusive` | Pick one direction on `send` / `recv` |
| `GenSeed: lwwallet mode only` | Switch daemon to `--wallet.type=lwwallet` |
| TLS errors | Use `--no-tls` for regtest, or set `--tlscertpath` |
