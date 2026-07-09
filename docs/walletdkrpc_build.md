# walletdkrpc build mode

`walletdkrpc` is an optional daemon-side subserver enabled by the
`walletdkrpc` build tag. It exposes a flat, swap-vocabulary-free wallet API on
top of the running `darepod` daemon and composes the existing swap subsystem,
the daemon-managed signer, the
cooperative-leave RPC (`LeaveVTXOs`), and the unified ledger surface into one
small RPC service. `WalletService` exposes the seven core user verbs (`Create`,
`Unlock`, `Send`, `Recv`, `List`, `Balance`, and `Exit`) plus supporting
methods (`PrepareSend`, `Deposit`, `Status`, `GetExitPlan`, `SweepWallet`,
`ExitStatus`, `ExitSummary`, `SubscribeWallet`, and `InspectActivity`).

This document covers how to build and install the daemon and CLI with the
wallet RPC subserver enabled, and what surfaces become available once the
binaries are tagged.

## Build tags

The wallet RPC subserver lives behind paired build tags:

- `walletdkrpc` — registers the wallet RPC gRPC service in the daemon and
  gives the top-level `darepocli` wallet verbs (`balance`, `recv`,
  `send`, `activity`, `create`, `unlock`, `exit`, `wallet-sweep`) and the
  `mcp` server's wallet tools live backing instead of an `Unimplemented`
  stub.
- `swapruntime` — the underlying swap subsystem the wallet RPC layer
  composes against. Required transitively: building with `walletdkrpc` but
  without `swapruntime` is a deliberate compile error.

Default builds (no tags) include neither the swap nor wallet RPC subsystems,
so the daemon stays light for hosts that only need plain Ark RPCs.

## Make targets

| Target | What it does |
|--------|-------------|
| `make build` | Debug build, neither tag. Default. |
| `make build-swapruntime` | Debug build with `-tags swapruntime`. Adds the swap subsystem. |
| `make build-walletdkrpc` | Debug build with `-tags "walletdkrpc swapruntime"`. Adds both swap and wallet RPC. |
| `make install` | `go install` with the default tag set. |
| `make install-swapruntime` | `go install` with `-tags swapruntime`. |
| `make install-walletdkrpc` | `go install` with `-tags "walletdkrpc swapruntime"`. |

The `walletdkrpc` targets are supersets of the `swapruntime` targets: building
with `walletdkrpc` always pulls `swapruntime` in transitively.

### Quick reference

```bash
# Local debug build with the wallet RPC surface enabled.
make build-walletdkrpc

# Or install to $GOPATH/bin.
make install-walletdkrpc
```

Both binaries land in the usual locations:

- `bin/darepod` (or `$GOPATH/bin/darepod`) — daemon
- `bin/darepocli` (or `$GOPATH/bin/darepocli`) — CLI

## What gets enabled

When the daemon is started from a `walletdkrpc`-tagged build:

- The daemon registers the `walletdkrpc.WalletService` gRPC service on its
  existing public listener. No separate port.
- The `swapwallet` package owns the full swap lifecycle in-process: it runs
  a synchronous resume-on-startup sweep before the gRPC server accepts
  calls, enforces a wallet-level deadline watcher that transitions stuck
  entries to FAILED, and runs a monitor loop that fans normalized updates
  to `SubscribeWallet` subscribers.
- The CLI exposes top-level wallet verbs: `send`, `recv`, `activity`,
  `balance`, `create`, `unlock`, `exit`, `wallet-sweep`, and `mcp serve`.
  Raw transaction / onchain
  history is available via `ark listtransactions`, the live VTXO set via
  `ark vtxos list`, and boarding-timeout sweep records via `ark sweep list`.
  Subscriptions are available from the
  `walletdkrpc.WalletService.SubscribeWallet` RPC.
- The `sdk/walletdk` facade can route through wallet RPC instead of the
  raw swap RPCs.

When the daemon is built without `walletdkrpc`, the gRPC subserver is replaced
by a stub that returns `Unimplemented` on every method, and the top-level
wallet verbs return the same error so scripts depending on them fail fast
rather than appearing to succeed. Power-user equivalents stay available
under the `ark *` (e.g. `ark vtxos list`, `ark send oor`) and
`dev daemon *` (raw RPC) subtrees.

## Configuration

The wallet RPC subserver reads four optional knobs from the standard
`darepod` config under the `swapwallet` section (see
[`sample-darepod.conf`](../sample-darepod.conf) for the canonical list):

| Key | Default | Purpose |
|-----|---------|---------|
| `swapwallet.deadline` | `30m` | Wallet-level timeout applied to every PENDING entry. The runtime overlays the entry as FAILED with `failure_reason="timed_out"` past this duration. |
| `swapwallet.defaultlistlimit` | `100` | Default page size for `List` and the initial snapshot of `SubscribeWallet`. |
| `swapwallet.maxlistlimit` | `1000` | Per-call hard cap on List page size; larger requests are clamped. |
| `swapwallet.subscribebuffer` | `32` | Per-subscriber channel buffer for `SubscribeWallet`. Slow consumers drop updates and reconcile via `List` on reconnect. |

The sample config uses the struct zero-defaults (`0s`, `0`) so the package
fallback values stay in one place; see `swapwallet/deps.go` for the
authoritative constants.

## Verification

```bash
# Build the daemon with walletdkrpc enabled.
make build-walletdkrpc

# Confirm the top-level wallet verbs appear in `darepocli --help`
# (balance, recv, send, activity, create, unlock, mcp).
./bin/darepocli --help

# After starting the daemon and creating/unlocking a wallet:
./bin/darepocli balance
./bin/darepocli ark vtxos list
```

If `darepocli balance` returns `daemon was not built with -tags walletdkrpc`,
the binary was built without the tag. Re-run `make build-walletdkrpc` or
`make install-walletdkrpc`.

## Related docs

- [`docs/swap_background_execution.md`](swap_background_execution.md) — the
  underlying `swapruntime` subserver that wallet RPC composes against.
- [`docs/walletdk_integration.md`](walletdk_integration.md) — the
  `sdk/walletdk` host-facing facade.
- [`swapwallet/doc.go`](../swapwallet/doc.go) — package-level overview of
  the daemon-side runtime, including v1 limitations on canonical-id
  stability and onchain sweep semantics.
