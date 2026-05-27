# Installing darepo-client

This document covers everything needed to get `darepod` (the daemon) and
`darepocli` (the CLI) built, installed, and running. For day-to-day daemon
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
git clone https://github.com/lightninglabs/darepo-client.git
cd darepo-client
make install-walletrpc
```

That single target builds and installs both `darepod` and `darepocli` to
`$GOPATH/bin` with the optional `walletrpc` + `swapruntime` subsystems
enabled. After it completes you have access to:

- The top-level wallet verbs: `darepocli {create, unlock, balance, recv,
  send, activity, exit, mcp serve}`.
- The Lightning swap subsystem (in-process swap FSM).
- The full power-user surface: `darepocli ark *` and `darepocli dev *`.
- The MCP server for AI-agent integration.

Confirm the install:

```bash
which darepod darepocli
darepod   --version
darepocli --help
```

If `darepocli balance` reports `daemon was not built with -tags walletrpc`,
the binary on `PATH` came from a default build. Re-run
`make install-walletrpc` and ensure `$GOPATH/bin` precedes any older copy.

---

## Build Variants

`darepod` ships with optional subsystems gated behind Go build tags. Pick
the variant that matches your needs.

| Variant                          | Tags                       | Binaries                | When to use                                            |
|----------------------------------|----------------------------|-------------------------|--------------------------------------------------------|
| Core (default)                   | _(none)_                   | `darepod`, `darepocli`  | Headless Ark client; no swaps; power-user CLI only.    |
| With Lightning swaps             | `swapruntime`              | `darepod`, `darepocli`  | Use Lightning-to-Ark / Ark-to-Lightning swaps.         |
| With wallet RPC (recommended)    | `walletrpc swapruntime`    | `darepod`, `darepocli`  | Use the top-level wallet verbs and host-app SDK.       |

`walletrpc` is a strict superset of `swapruntime`; you cannot enable
`walletrpc` without `swapruntime` (the combination is enforced at compile
time).

### Local debug builds (output to `./bin/`)

```bash
make build                       # core
make build-swapruntime           # + swap subsystem
make build-walletrpc             # + walletrpc and swap subsystem  (recommended)
```

After any of these, the binaries are at:

- `./bin/darepod`
- `./bin/darepocli`

### Install to `$GOPATH/bin`

```bash
make install                     # core
make install-swapruntime         # + swap subsystem
make install-walletrpc           # + walletrpc and swap subsystem  (recommended)
```

For more on what each tag turns on, see
[`docs/walletrpc_build.md`](docs/walletrpc_build.md).

---

## Step-By-Step From Source

If you want the long form (e.g. for CI or reproducible-build setups):

```bash
# 1. Clone.
git clone https://github.com/lightninglabs/darepo-client.git
cd darepo-client

# 2. Verify Go version (1.25.5+).
go version

# 3. Fetch and tidy dependencies. The repo uses a multi-module Go
#    workspace (see docs/go_workspace.md); this is normally automatic.
go mod download

# 4. Build the recommended (walletrpc) variant into ./bin.
make build-walletrpc

# 5. Or install the same variant to $GOPATH/bin.
make install-walletrpc

# 6. Confirm the binaries.
./bin/darepod   --help
./bin/darepocli --help
```

---

## Backend Prerequisites

`darepod` supports three wallet/chain backends, selected at runtime via
`--wallet.type`. Each has its own external dependencies.

### `lwwallet` (default, standalone)

Lightweight in-process wallet backed by an Esplora REST endpoint. No
external Bitcoin node or lnd required.

```bash
darepod \
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
darepod \
  --network=signet \
  --wallet.type=btcwallet \
  --wallet.feeurl=https://mempool.space/signet/api/v1/fees/recommended
```

### `lnd`

Uses an existing lnd node for signing and chain access. The node must be
reachable over gRPC with a TLS cert and admin macaroon.

```bash
darepod \
  --wallet.type=lnd \
  --lnd.host=localhost:10009 \
  --lnd.tlspath=~/.lnd/tls.cert \
  --lnd.macaroonpath=~/.lnd/data/chain/bitcoin/regtest/admin.macaroon
```

---

## Initial Wallet Setup

After starting the daemon, the wallet must be created and unlocked before
any operation can proceed.

With a `walletrpc`-enabled build (`make install-walletrpc`):

```bash
# Create a wallet (prints the seed mnemonic on stderr; write it down!).
DAREPOD_WALLET_PASSWORD=your_password darepocli create --no-tls

# Unlock the wallet after every restart.
DAREPOD_WALLET_PASSWORD=your_password darepocli unlock --no-tls
```

To skip manual unlock entirely, pass `--wallet.password_file=/path/to/file`
to `darepod` at startup; the daemon will auto-unlock from the file.

Without `walletrpc`, the only supported path is the password-file auto-unlock
above. The `create` / `unlock` CLI commands are not present in the default
build. Full password-handling rules:
[`docs/daemon_cli_guide.md`](docs/daemon_cli_guide.md#password-handling).

---

## Verifying the Install

```bash
# 1. Daemon answers basic status.
darepocli getinfo --no-tls

# 2. (walletrpc only) wallet verbs work.
darepocli balance --no-tls
darepocli activity --no-tls

# VTXO inventory lives under the ark subtree (available in every build).
darepocli ark vtxos list --no-tls

# 3. Schema dump (useful for tooling and AI agents).
darepocli schema --no-tls
```

If you see `daemon was not built with -tags walletrpc` for the wallet
verbs, your `darepod` binary is the default (untagged) build. Reinstall
with `make install-walletrpc`.

---

## Updating

Pull the latest source and reinstall the same variant you previously used:

```bash
git pull --rebase
make install-walletrpc       # or whichever variant you run
```

---

## Uninstalling

```bash
rm "$(go env GOPATH)/bin/darepod"
rm "$(go env GOPATH)/bin/darepocli"
# (Optional) wipe daemon state. This destroys the wallet seed!
# rm -rf ~/.darepod
```

The wallet seed is stored encrypted under `~/.darepod/<network>/wallet_seed.enc`.
Deleting `~/.darepod` is irreversible without the recorded mnemonic.

---

## Troubleshooting

| Symptom                                                | Fix                                                                                  |
|--------------------------------------------------------|--------------------------------------------------------------------------------------|
| `daemon was not built with -tags walletrpc`            | Reinstall with `make install-walletrpc`.                                             |
| `connection refused` on `darepocli`                    | Daemon not running, or wrong `--rpcserver` address.                                  |
| `wallet not ready`                                     | Run `darepocli unlock` (walletrpc), or restart `darepod` with `--wallet.password_file`. |
| `wallet already exists`                                | Use `darepocli unlock` instead of `create`.                                          |
| TLS / x509 errors against the daemon                   | Use `--no-tls` on regtest, or pass `--tlscertpath` to `darepocli`.                   |
| `go: module ... requires go 1.25.5` or similar         | Upgrade to Go 1.25.5+ (see Prerequisites).                                           |
| `make: command not found` / build fails inside Docker  | The `lint` and `rpc` targets need Docker; the `*-local` variants do not.             |

Deeper troubleshooting (per-flag and per-backend) is in
[`docs/daemon_cli_guide.md`](docs/daemon_cli_guide.md#troubleshooting).

---

## Next Steps

- **Run the daemon:** [`docs/daemon_cli_guide.md`](docs/daemon_cli_guide.md)
- **Build-tag deep dive:** [`docs/walletrpc_build.md`](docs/walletrpc_build.md)
- **Embed the daemon in a host app:** [`docs/walletdk_integration.md`](docs/walletdk_integration.md)
- **Codebase map:** [`ARCHITECTURE.md`](ARCHITECTURE.md)
- **Contribute:** style guide and pre-commit checklist in
  [`docs/development_guidelines.md`](docs/development_guidelines.md) and
  [`docs/testing-guide.md`](docs/testing-guide.md).
