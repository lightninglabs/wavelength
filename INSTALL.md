# Installing wavelength

This document covers everything needed to get `waved` (the daemon) and
`wavecli` (the CLI) built, installed, and running. For day-to-day daemon
operation, configuration flags, and the full CLI reference, see
[`docs/daemon_cli_guide.md`](docs/daemon_cli_guide.md).

---

## Prerequisites

| Requirement | Version            | Notes                                                       |
|-------------|--------------------|-------------------------------------------------------------|
| Go          | **1.25.5** or later| The reference version (used by CI and release builds).      |
| Git         | any recent         | For cloning and submodule resolution.                       |
| make        | GNU make           | All build/test/lint targets are driven through the Makefile.|
| C toolchain | optional           | Only needed for `make unit-race` (requires `CGO_ENABLED=1`).|

Verify your toolchain:

```bash
go version       # expect: go1.25.5+
make --version
git --version
```

Make sure `$GOPATH/bin` (or `$(go env GOBIN)`) is on your `PATH` so the
installed binaries are reachable:

```bash
export PATH="$(go env GOPATH)/bin:$PATH"
```

---

## TL;DR: Install With Wallet RPC (Recommended)

```bash
git clone https://github.com/lightninglabs/wavelength.git
cd wavelength
make install-walletdkrpc
```

That single target builds and installs both `waved` and `wavecli` to
`$GOPATH/bin` with the optional `walletdkrpc` + `swapruntime` subsystems
enabled. After it completes you have access to:

- The top-level wallet verbs: `wavecli {create, unlock, balance, recv,
  send, activity, exit, mcp serve}`.
- The Lightning swap subsystem (in-process swap FSM).
- The full power-user surface: `wavecli ark *` and `wavecli dev *`.
- The MCP server for AI-agent integration.

Confirm the install:

```bash
which waved wavecli
waved   --version
wavecli --help
```

If `wavecli balance` reports `daemon was not built with -tags walletdkrpc`,
the binary on `PATH` came from a default build. Re-run
`make install-walletdkrpc` and ensure `$GOPATH/bin` precedes any older copy.

---

## Build Variants

`waved` ships with optional subsystems gated behind Go build tags. Pick
the variant that matches your needs.

| Variant                          | Tags                       | Binaries                | When to use                                            |
|----------------------------------|----------------------------|-------------------------|--------------------------------------------------------|
| Core (default)                   | _(none)_                   | `waved`, `wavecli`  | Headless Ark client; no swaps; power-user CLI only.    |
| With Lightning swaps             | `swapruntime`              | `waved`, `wavecli`  | Use Lightning-to-Ark / Ark-to-Lightning swaps.         |
| With wallet RPC (recommended)    | `walletdkrpc swapruntime`  | `waved`, `wavecli`  | Use the top-level wallet verbs and host-app SDK.       |

`walletdkrpc` is a strict superset of `swapruntime`; you cannot enable
`walletdkrpc` without `swapruntime` (the combination is enforced at compile
time).

### Local debug builds (output to `./bin/`)

```bash
make build                       # core
make build-swapruntime           # + swap subsystem
make build-walletdkrpc             # + walletdkrpc and swap subsystem  (recommended)
```

After any of these, the binaries are at:

- `./bin/waved`
- `./bin/wavecli`

### Install to `$GOPATH/bin`

```bash
make install                     # core
make install-swapruntime         # + swap subsystem
make install-walletdkrpc           # + walletdkrpc and swap subsystem  (recommended)
```

For more on what each tag turns on, see
[`docs/walletdkrpc_build.md`](docs/walletdkrpc_build.md).

---

## Step-By-Step From Source

If you want the long form (e.g. for CI or reproducible-build setups):

```bash
# 1. Clone.
git clone https://github.com/lightninglabs/wavelength.git
cd wavelength

# 2. Verify Go version (1.25.5+).
go version

# 3. Fetch and tidy dependencies. The repo uses a multi-module Go
#    workspace (see docs/go_workspace.md); this is normally automatic.
go mod download

# 4. Build the recommended (walletdkrpc) variant into ./bin.
make build-walletdkrpc

# 5. Or install the same variant to $GOPATH/bin.
make install-walletdkrpc

# 6. Confirm the binaries.
./bin/waved   --help
./bin/wavecli --help
```

---

## Backend Prerequisites

`waved` supports three wallet/chain backends, selected at runtime via
`--wallet.type`. Each has its own external dependencies.

### `lwwallet` (default, standalone)

Lightweight in-process wallet backed by an Esplora REST endpoint. No
external Bitcoin node or lnd required.

```bash
waved \
  --network=regtest \
  --wallet.type=lwwallet \
  --wallet.esploraurl=http://localhost:3000
```

For local development the easiest Esplora is [Nigiri](https://nigiri.vulpem.com/):

```bash
nigiri start                 # spins up bitcoind + Esplora on regtest
```

### `btcwallet` (Neutrino)

In-process btcwallet using Neutrino compact block filters. No external
node required, but initial sync downloads block/filter headers.

```bash
waved \
  --network=signet \
  --wallet.type=btcwallet \
  --wallet.feeurl=https://mempool.space/signet/api/v1/fees/recommended
```

The Ark and swap connections resolve from the configured public test network
unless explicitly overridden. See [`docs/signet.md`](docs/signet.md) for the
testnet3, testnet4, and signet gRPC and REST addresses.

### `lnd`

Uses an existing lnd node for signing and chain access. The node must be
reachable over gRPC with a TLS cert and admin macaroon.

```bash
waved \
  --wallet.type=lnd \
  --lnd.host=localhost:10009 \
  --lnd.tlspath=~/.lnd/tls.cert \
  --lnd.macaroonpath=~/.lnd/data/chain/bitcoin/regtest/admin.macaroon
```

---

## Initial Wallet Setup

After starting the daemon, the wallet must be created and unlocked before
any operation can proceed.

`wavecli` authenticates to the daemon over TLS with the daemon's admin
macaroon, both derived from `--datadir` / `--network` (defaults `~/.waved`
and `mainnet`). Match those to your daemon, or use `--no-tls --no-macaroons`
for a local plaintext daemon (a macaroon can't ride an unencrypted connection,
so `--no-tls` alone fails). Set it once via an alias — the commands below use
`da`:

```bash
# Pick the one that matches your daemon:
#   regtest, plaintext:
#     alias da='wavecli --no-tls --no-macaroons --network=regtest'
#   signet under ~/.waved-signet, TLS:
alias da='wavecli --network=signet --datadir=~/.waved-signet'
```

With a `walletdkrpc`-enabled build (`make install-walletdkrpc`):

```bash
# Create a wallet (prints the seed mnemonic on stderr; write it down!).
WAVED_WALLET_PASSWORD=your_password da create

# Unlock the wallet after every restart.
WAVED_WALLET_PASSWORD=your_password da unlock
```

To skip manual unlock entirely, pass `--wallet.password_file=/path/to/file`
to `waved` at startup; the daemon will auto-unlock from the file.

Without `walletdkrpc`, the only supported path is the password-file auto-unlock
above. The `create` / `unlock` CLI commands are not present in the default
build. Full password-handling rules:
[`docs/daemon_cli_guide.md`](docs/daemon_cli_guide.md#password-handling).

---

## Verifying the Install

Using the `da` alias from the previous section:

```bash
# 1. Daemon answers basic status.
da getinfo

# 2. (walletdkrpc only) wallet verbs work.
da balance
da activity

# VTXO inventory lives under the ark subtree (available in every build).
da ark vtxos list

# 3. Schema dump (useful for tooling and AI agents).
da schema
```

If you see `daemon was not built with -tags walletdkrpc` for the wallet
verbs, your `waved` binary is the default (untagged) build. Reinstall
with `make install-walletdkrpc`.

---

## Updating

Pull the latest source and reinstall the same variant you previously used:

```bash
git pull --rebase
make install-walletdkrpc       # or whichever variant you run
```

---

## Uninstalling

```bash
rm "$(go env GOPATH)/bin/waved"
rm "$(go env GOPATH)/bin/wavecli"
# (Optional) wipe daemon state. This destroys the wallet seed!
# rm -rf ~/.waved
```

The wallet key material lives in the wallet database under
`~/.waved/<network>/`, encrypted with your wallet password.
Deleting `~/.waved` is irreversible without the recorded mnemonic.

---

## Troubleshooting

| Symptom                                                | Fix                                                                                  |
|--------------------------------------------------------|--------------------------------------------------------------------------------------|
| `daemon was not built with -tags walletdkrpc`            | Reinstall with `make install-walletdkrpc`.                                             |
| `connection refused` on `wavecli`                    | Daemon not running, or wrong `--rpcserver` address.                                  |
| `wallet not ready`                                     | Run `wavecli unlock` (walletdkrpc), or restart `waved` with `--wallet.password_file`. |
| `wallet already exists`                                | Use `wavecli unlock` instead of `create`.                                          |
| `read macaroon: ... no such file`                      | CLI is looking under the wrong data dir/network; pass `--datadir` / `--network` to match the daemon (or `--macaroonpath`). |
| `credentials require transport level security`         | A macaroon can't ride a plaintext connection; use TLS, or add `--no-macaroons` alongside `--no-tls`. |
| TLS / x509 errors against the daemon                   | Point `--datadir` / `--network` at the daemon's cert, pass `--tlscertpath`, or use `--no-tls --no-macaroons` on regtest. |
| `go: module ... requires go 1.25.5` or similar         | Upgrade to Go 1.25.5+ (see Prerequisites).                                           |
| `make: command not found` / build fails inside Docker  | The `lint` and `rpc` targets need Docker; the `*-local` variants do not.             |

Deeper troubleshooting (per-flag and per-backend) is in
[`docs/daemon_cli_guide.md`](docs/daemon_cli_guide.md#troubleshooting).

---

## Next Steps

- **Run the daemon:** [`docs/daemon_cli_guide.md`](docs/daemon_cli_guide.md)
- **Build-tag deep dive:** [`docs/walletdkrpc_build.md`](docs/walletdkrpc_build.md)
- **Embed the daemon in a host app:** [`docs/walletdk_integration.md`](docs/walletdk_integration.md)
- **Codebase map:** [`ARCHITECTURE.md`](ARCHITECTURE.md)
- **Contribute:** style guide and pre-commit checklist in
  [`docs/development_guidelines.md`](docs/development_guidelines.md) and
  [`docs/testing-guide.md`](docs/testing-guide.md).
