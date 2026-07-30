# waved / wavecli: Ark Client Daemon

## Overview

`waved` is the Ark protocol client daemon. It connects to an Ark
operator server via a mailbox transport, manages VTXOs (virtual
transaction outputs), and exposes a gRPC API for wallet operations.

`wavecli` is the CLI for driving the daemon. Almost everything it prints is
already JSON, so an agent can parse the default output: `getinfo`, `balance`,
`recv`, `send`, the `ark` verbs and the rest all render through one
`printJSON` helper with no format switch at all.

Two commands are the exception, and they are the only two that read the
global `--json` bool: `activity` (default `--format table`) and `activity
inspect` (default `--format expanded`). For those, pass `--json` (or
`--format json`) to get machine-readable output. Passing `--json` anywhere
else is silently inert.

Input is a separate flag: `--request-json` takes a raw proto-JSON request
payload.

## Building

```bash
make build          # produces bin/waved and bin/wavecli, default tag set
make install        # installs to $GOPATH/bin
make lint           # run linter
make unit pkg=waved  # run unit tests for a package
```

`make build` uses the **default tag set**, which leaves out both
`swapruntime` and `wavewalletrpc`. That is enough for the raw Ark RPCs but
not for the everyday wallet verbs, which need the two tags together, so most
local work wants:

```bash
make build-wavewalletrpc    # -tags "wavewalletrpc swapruntime"
make install-wavewalletrpc  # same, into $GOPATH/bin
```

In Docker the same thing is a build argument:

```bash
docker build --build-arg GOTAGS="wavewalletrpc swapruntime" -t waved:wallet .
```

Full detail on the tags and what each one turns on:
[`docs/wavewalletrpc_build.md`](../../docs/wavewalletrpc_build.md).

## Starting the Daemon

### lwwallet Mode (Standalone, No lnd Required)

```bash
./bin/waved \
  --network=regtest \
  --wallet.type=lwwallet \
  --wallet.esploraurl=http://localhost:3000 \
  --server.host=localhost:10010 \
  --server.insecure \
  --rpc.listenaddr=localhost:10029
```

Then create and unlock the wallet:

```bash
# Create (password via env var for automation). Needs a daemon built with
# make build-wavewalletrpc.
WAVED_WALLET_PASSWORD=testpass wavecli create --no-tls

# Or auto-unlock at startup:
WAVED_WALLET_PASSWORD=testpass ./bin/waved \
  --wallet.type=lwwallet \
  --wallet.password_file=/path/to/password \
  ...
```

### lnd Mode (Existing lnd Node)

```bash
./bin/waved \
  --network=regtest \
  --wallet.type=lnd \
  --lnd.host=localhost:10009 \
  --lnd.tlspath=~/.lnd/tls.cert \
  --lnd.macaroonpath=~/.lnd/data/chain/bitcoin/regtest/admin.macaroon \
  --server.host=localhost:10010 \
  --server.insecure \
  --rpc.listenaddr=localhost:10029
```

`--server.localmailboxid` and `--server.remotemailboxid` used to appear in
both of the above. They are gone: the daemon derives its mailbox identifiers
itself, and passing either one now aborts startup with
`unknown flag: --server.localmailboxid`.

Neither example disables transport security on waved's own listener, so both
daemons serve TLS with macaroon auth and drop the credentials under
`~/.waved/data/regtest/`. `--server.insecure` is the outbound leg to the
operator and does not change that; the listener is governed by `--rpc.notls`
and `--rpc.no-macaroons`, which are covered below.

## CLI Quick Reference

All commands connect to the daemon at `--rpcserver` (default
`localhost:10029`). Every example below passes `--network=regtest`, which is
not optional: it defaults to `mainnet` and is what derives the default TLS
cert and macaroon paths, so without it wavecli reads from the wrong data
directory and fails before dialing:

```
unable to load TLS cert: open ~/.waved/data/mainnet/tls.cert: no such file or directory
```

That is all a stock daemon needs, since it serves TLS with macaroon auth and
the credentials are already on disk under `~/.waved/data/regtest/`.

If the daemon was started with `--rpc.notls --rpc.no-macaroons`, add the
matching `--no-tls --no-macaroons`. Those two travel as a pair: `--no-tls`
alone fails with `grpc: the credentials require transport level security`,
because gRPC will not ship per-RPC credentials over a plaintext link. With
both set nothing is read off disk, so `--network` no longer matters for the
connection. Against a stock TLS daemon they instead produce `error reading
server preface: EOF`, so do not pass them by default.

The CLI surface is three tiers:

1. **Eight top-level wallet verbs (implicit, no parent)**: the everyday
   surface. Backed by `wavewalletrpc.WalletService`, which needs the
   `wavewalletrpc` **and** `swapruntime` tags together (build with
   `make build-wavewalletrpc`, which sets both).
2. **Daemon introspection at root**: `getinfo`, `schema`, `mcp`.
3. **Advanced subtrees**: `ark.*` (raw waverpc), `recovery.*` (daemon-owned
   vHTLC recovery rows), and `dev.*` (generated low-level daemon RPCs). The
   old `swap.*` subtree is gone; `wavecli swap ...` now exits with
   `unknown command "swap" for "wavecli"`.

The advanced subtrees are hidden from `--help` unless `WAVELENGTH_DEV=1` is
set. That only changes CLI visibility: they are registered and dispatchable
either way, which is separate from whether the daemon serves the RPC behind
them.

If the daemon is built without the tags, the eight top-level verbs return a
structured error pointing at `docs/wavewalletrpc_build.md`. Tier 2 and the
`ark` and `recovery` subtrees are unaffected, since they ride on
`waverpc.DaemonService`, which every build registers.

`dev.*` is the one tier-3 subtree that is not all-or-nothing: it is a
generated registry spanning six services, so availability is per service.

| `dev` service | Default-tag daemon |
|---|---|
| `dev daemon` (`waverpc.DaemonService`) | works |
| `dev wallet` (`wavewalletrpc.WalletService`) | `UNIMPLEMENTED: unknown service wavewalletrpc.WalletService` |
| `dev wallet-inspection` (`wavewalletrpc.WalletInspectionService`) | same, for `WalletInspectionService` |
| `dev swapclient` (`swapclientrpc.SwapClientService`) | `daemon was built without swapruntime support; rebuild waved with tags="swapruntime"` |

The two `wavewalletrpc` services need both tags:
`cmd/waved/wavewalletrpc_stub.go` is built under
`!wavewalletrpc || !swapruntime`, so either tag alone still gets the stub,
which registers nothing.

### Top-level wallet verbs

```bash
# Status (works against any daemon build)
wavecli --network=regtest getinfo

# Create + unlock (password from env, never argv).
WAVED_WALLET_PASSWORD=testpass wavecli --network=regtest create
WAVED_WALLET_PASSWORD=testpass wavecli --network=regtest unlock

# Balance: confirmed_sat / pending_in_sat / pending_out_sat
wavecli --network=regtest balance

# Receive: offchain (invoice) or onchain (boarding address)
wavecli --network=regtest recv --offchain --amt 5000 --memo "coffee"
wavecli --network=regtest recv --onchain

# Send: --offchain (default) = invoice; --onchain = cooperative leave.
# The CLI does NOT sniff the destination string; pick the direction
# explicitly.
wavecli --network=regtest send lnbcrt... --offchain
wavecli --network=regtest send bcrt1... --onchain --amt 1000
wavecli --network=regtest send bcrt1... --onchain --sweep-all

# Activity: merged send/recv/deposit/exit feed.
wavecli --network=regtest activity                            # all activity
wavecli --network=regtest activity --pending --kind send,recv # filter
wavecli --network=regtest activity --format json              # JSON output
# VTXO inventory and on-chain history are not part of the activity feed;
# use the `ark` subtree: `ark vtxos list` (live VTXOs),
# `ark listtransactions` (raw tx / onchain history),
# `ark sweep list` (boarding-timeout sweep records).

# Exit: trigger and query a unilateral exit (unroll).
wavecli --network=regtest exit --outpoint TXID:VOUT
wavecli --network=regtest exit status --outpoint TXID:VOUT
```

Every verb in this block except `getinfo` needs a `wavewalletrpc` daemon.

### Advanced (`ark.*`) commands

The everyday top-level verbs compose `wavewalletrpc` end-to-end; `ark.*`
surfaces the raw waverpc methods underlying them.

```bash
# Raw VTXO inventory + lifecycle
wavecli --network=regtest ark vtxos list
wavecli --network=regtest ark vtxos list --status live --min-amount 10000
# A real refresh is fee-gated: preview with --dry-run, consent with
# --yes (required on non-interactive stdin).
wavecli --network=regtest ark vtxos refresh --all --dry-run
wavecli --network=regtest ark vtxos refresh --all --yes

# Raw transaction history (the wallet-shaped feed is `activity`)
wavecli --network=regtest ark listtransactions

# Raw send paths
wavecli --network=regtest ark send inround --to tb1p... --amount 50000
wavecli --network=regtest ark send oor --to tb1p... --amount 25000

# Raw JSON request payloads use --request-json, not --json (which is the
# output-format bool, and inert on this command).
wavecli --network=regtest ark send inround --request-json '{
  "recipients": [
    {"address": "tb1p...", "amount_sat": 50000},
    {"address": "tb1p...", "amount_sat": 30000}
  ]
}'
```

Flag names are canonically kebab-case (`--min-amount`, `--dry-run`,
`--wallet-password-file`). A global normalizer folds the snake_case spelling
onto the same flag, so older `--min_amount` style invocations still work.

### Password Input (Never as CLI args)

Priority order (matches `readWalletPassword` in `wallet_password.go`):
1. `WAVED_WALLET_PASSWORD` env var (highest priority, so callers with
   something already on stdin do not have to fight over it).
2. `--wallet-password-file` flag (file is read and the trailing
   newline is stripped).
3. `--password-stdin`, which must be passed explicitly.
4. Interactive TTY prompt (lowest priority).

Stdin is never consumed implicitly. A bare pipe with no
`--password-stdin` is an error, not a fallback:

```
wallet password input required: set WAVED_WALLET_PASSWORD, use
--wallet-password-file, or explicitly pass --password-stdin
```

The optional aezeed seed passphrase is read from
`WAVED_SEED_PASSPHRASE`, then `--seed-passphrase-file`. The seed
passphrase is NOT accepted via CLI args either, so both secrets stay out
of `argv`.

```bash
# Env var
WAVED_WALLET_PASSWORD=pass wavecli --network=regtest unlock

# File
wavecli --network=regtest unlock --wallet-password-file=/tmp/pass

# Pipe, which needs the explicit opt-in
echo -n 'pass' | wavecli --network=regtest unlock --password-stdin
```

## Regtest Workflow

1. Start a regtest bitcoin node + esplora.
2. Start waved in lwwallet mode (see above) built with
   `make build-wavewalletrpc`.
3. Create + unlock the wallet via CLI:
   `WAVED_WALLET_PASSWORD=testpass wavecli --network=regtest create`
   `WAVED_WALLET_PASSWORD=testpass wavecli --network=regtest unlock`
4. Get a boarding address: `wavecli --network=regtest recv --onchain`
5. Fund it: `bitcoin-cli sendtoaddress <addr> 0.01`
6. Mine a block: `bitcoin-cli generatetoaddress 1 <miner_addr>`
7. Check balance: `wavecli --network=regtest balance`
8. List VTXOs (once boarding completes):
   `wavecli --network=regtest ark vtxos list`

Steps 3, 4, and 7 need a `wavewalletrpc` daemon (step 2). Only step 8 works
against a default-tag build.

## Environment Variables

| Variable | Description |
|----------|-------------|
| `WAVED_WALLET_PASSWORD` | Wallet password for create/unlock |
| `WAVED_SEED_PASSPHRASE` | Optional aezeed seed passphrase for create |
| `WAVED_NETWORK` | Bitcoin network override |
| `WAVED_WALLET_TYPE` | Wallet backend type override |
| `WAVED_WALLET_ESPLORAURL` | Esplora URL override |
| `WAVED_DEBUGLEVEL` | Log verbosity override (the daemon flag is `--debuglevel`) |
| `WAVELENGTH_DEV` | Set to `1` to reveal the `ark` / `recovery` / `dev` subtrees in `wavecli --help` |

All daemon config flags can be set via env vars with the `WAVED_`
prefix, dots replaced by underscores (e.g., `WAVED_SERVER_HOST`).
`WAVELENGTH_DEV` is the one exception: it is a `wavecli` help-visibility
toggle, not daemon config.

## Troubleshooting

| Problem | Fix |
|---------|-----|
| `connection refused` | Daemon not running or wrong `--rpcserver` |
| `wallet not ready` | Run `wavecli unlock` first |
| `wallet already exists` | Wallet was already created; use `unlock` |
| `daemon was not built with -tags wavewalletrpc` | Rebuild the **daemon** with `make build-wavewalletrpc` (both tags); all eight top-level wallet verbs plus the `mcp` wallet tools need them. `getinfo`, `schema`, `ark.*`, `recovery.*` and `dev daemon *` do not, so a default-tag daemon is not a broken one |
| `--sweep-all requires --amt=0` | On `send --onchain`: pass `--sweep-all` for "drain wallet", or set `--amt N` |
| `--offchain and --onchain are mutually exclusive` | Pick one direction on `send` / `recv` |
| `GenSeed: lwwallet mode only` | Switch daemon to `--wallet.type=lwwallet` |
| `wallet password input required: ...` | Stdin is never read implicitly. Use `WAVED_WALLET_PASSWORD`, `--wallet-password-file`, or an explicit `--password-stdin` |
| `unknown flag: --server.localmailboxid` | Mailbox IDs are daemon-derived now; delete the flag |
| `unknown command "swap" for "wavecli"` | The `swap` subtree was retired; use the wallet verbs or `ark.*` |
| `unable to load TLS cert: .../data/mainnet/tls.cert: no such file...` | wavecli is on the default `--network=mainnet`. Pass `--network=regtest` |
| `read macaroon: .../data/mainnet/admin.macaroon: no such file...` | Same cause one step later. Pass `--network=regtest` or `--no-macaroons` |
| `error reading server preface: EOF` | A `--no-tls` client against a TLS listener. Drop `--no-tls` |
| `tls: first record does not look like a TLS handshake` | The reverse: TLS client against a `--rpc.notls` daemon. Add `--no-tls` |
| `grpc: the credentials require transport level security` | `--no-tls` without `--no-macaroons`; the two dev flags travel as a pair |
| `expected 1 macaroon, got 0` | `--no-macaroons` against a daemon that still enforces them. Drop `--no-macaroons`, or turn enforcement off with `--rpc.no-macaroons` on the daemon. Adding `--macaroonpath` alone does nothing: it is only read when macaroons are enabled |
