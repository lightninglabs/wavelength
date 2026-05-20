# sdk/walletdk

## Purpose

Wallet-shaped SDK facade for host apps that want a small, stable Go API over
an embedded `darepod` client daemon. `Start` boots the daemon in-process,
dials it over a private `bufconn` gRPC transport, and exposes typed methods
that mirror the seven core CLI verbs (create, unlock, send, recv, list,
balance, exit) plus supporting subscribe/deposit/status. walletdk is the
highest-level layer in the stack; it wraps `walletrpc.WalletService` (the
unified wallet API on the daemon side) with mobile-/JS-bridge-friendly
DTOs.

Wallet methods are gated behind the `walletrpc` build tag (which
transitively requires `swapruntime`): stub builds compile, but the wallet
methods return `ErrWalletRPCUnavailable` synchronously.

## Key Types

- `Client` — Concurrency-safe wallet handle. Owns the embedded daemon
  lifecycle, the private `bufconn` gRPC connection, and the daemonrpc /
  walletrpc / swapclientrpc clients. `Stop`/`Close` are aliases; both are
  idempotent.
- `Config` — Embedded daemon + wallet facade config. Two usage modes:
  zero-value plus convenience fields (`DataDir`, `Network`,
  `ServerAddress`, …), or a caller-owned `DaemonConfig` plus only the
  convenience fields the host wants to override. The convenience
  booleans (`AllowMainnet`, `ServerInsecure`, `SwapServerInsecure`) are
  plain `bool` enable-only overrides: set `true` to force the value or
  leave at the zero value to defer to `DaemonConfig`.
- `DefaultConfig` — Returns a walletdk `Config` populated from
  `darepod.DefaultConfig()`. Convenience starting point for hosts.
- `Start` — Boots the embedded daemon, dials it, waits for gRPC
  readiness, and returns a ready-to-use `*Client`. Detaches the daemon
  lifetime from the caller's `ctx` so a tight startup deadline does not
  kill the daemon.
- `Info` / `Balance` / `CreateWalletResult` / `UnlockWalletResult` —
  Wrapper-owned wallet DTOs. Mobile and JS bridges see these, not
  protobuf types.
- `ReceiveRequest` / `ReceiveResult` / `SendRequest` / `SendResult` —
  Wrapper-owned payment DTOs. Receive returns a BOLT-11 invoice plus the
  initial `Entry`; Send returns the initial `Entry` plus the actual
  outflow (`ActualAmountSat`) which may exceed the requested amount for
  onchain whole-VTXO sweeps.
- `DepositRequest` / `DepositResult` — Wrapper-owned DTOs for the
  onchain receive path (`recv --onchain` from the CLI). `DepositResult`
  carries the fresh boarding address plus the initial `Entry`.
- `ListRequest` / `ListResult` / `ListView` — View-tagged history query.
  `ListResult` is a tagged union (`View` + one of `Activity`, `VTXOs`,
  `Onchain`) — callers switch on `View` to pick the right field.
- `ActivityList` / `VTXOInventory` / `OnchainHistory` — Per-view typed
  result shapes; one is populated on each `ListResult`.
- `Entry` — Flat activity row used by `ActivityList`, `ReceiveResult`,
  `SendResult`, `DepositResult`, and `Subscribe`. `Kind` / `Status` are
  wrapper-owned lowercase strings (`EntryKind`, `EntryStatus`),
  intentionally decoupled from the proto enum.
- `WalletVTXO` / `OnchainTx` — Per-row shapes for the VTXOS and ONCHAIN
  views of `List`.
- `ExitRequest` / `ExitResult` / `ExitStatusRequest` /
  `ExitStatusResult` / `ExitJobStatus` — Exit DTOs. `ExitRequest`
  carries the target outpoint plus an optional `Destination`
  on-chain address: when set, `Exit` first attempts a cooperative
  leave (`daemonrpc.LeaveVTXOs`) so the VTXO is unwound via the
  next assembling round with the leave output landing on the
  caller-supplied address; the SDK transparently falls back to
  `walletrpc.Exit` (unilateral unroll) when the cooperative attempt
  fails for any reason. `ExitResult.Cooperative` reports which path
  the daemon took, `QueuedOutpoints` carries the cooperative
  selection echo, `Created` / `ActorID` describe the unilateral
  fallback job, and `CooperativeError` surfaces the original
  cooperative failure when the SDK fell back. The status string is
  the wrapper-owned lowercase set
  (`pending`/`materializing`/`csv_pending`/`sweeping`/`completed`/`failed`/
  `unspecified`), decoupled from `walletrpc.ExitJobStatus`.
- `ErrWalletRPCUnavailable` — Sentinel returned by every wallet method
  when the daemon was not built with the `walletrpc` tag.
- `ErrSwapRuntimeUnavailable` — Backwards-compat alias for
  `ErrWalletRPCUnavailable`; kept so older callers compile after the
  walletrpc rename.

## RPC Methods (host-facing API)

| Method | Description |
|--------|-------------|
| `GetInfo` | Daemon readiness snapshot: version, network, identity, wallet/server readiness |
| `CreateWallet` | Create or import the embedded daemon wallet (auto-generates seed when mnemonic empty); proxies daemonrpc directly |
| `UnlockWallet` | Unlock an existing embedded daemon wallet; proxies daemonrpc directly |
| `Balance` | Flat wallet balance: confirmed_sat, pending_in_sat, pending_out_sat (walletrpc) |
| `Deposit` | Allocate a fresh boarding address (walletrpc; `recv --onchain` from the CLI) |
| `Receive` | Open a Lightning invoice receive (walletrpc; `recv --offchain` from the CLI). Returns `ReceiveResult{Invoice, Entry}` |
| `Send` | Outbound payment by invoice or onchain address (walletrpc). Returns `SendResult{Entry, ActualAmountSat}` |
| `List` | Unified wallet view by `ListView` (Activity / VTXOs / Onchain); tagged-union `ListResult` per view (walletrpc) |
| `Exit` | Trigger a unilateral exit / unroll for a VTXO outpoint (walletrpc; proxies daemonrpc.Unroll) |
| `ExitStatus` | Query the phase of an exit job (walletrpc; proxies daemonrpc.GetUnrollStatus) |
| `Status` | Wallet readiness, balance, pending-entry count (walletrpc) |
| `Subscribe` | Stream wallet activity (`Entry`) updates; optionally include existing rows (walletrpc) |
| `Stop` / `Close` | Shut down the embedded daemon and release the private transport |
| `Wait` | Single shared channel yielding the daemon's terminal run error |
| `GRPCConn` / `ArkRPC` / `SwapRPC` / `WalletRPC` | Escape hatches exposing the underlying private gRPC connection and raw RPC clients |

## Relationships

- **Depends on**: `darepod` (embedded daemon runtime, default config,
  validation), `daemonrpc` (wallet, balance, info, address RPCs, plus
  daemonrpc-direct paths for `CreateWallet`/`UnlockWallet`),
  `rpc/walletrpc` (the unified wallet API the seven verbs target),
  `rpc/swapclientrpc` (escape hatch for raw swap RPCs),
  `swapclientserver` (`swapruntime` build only — registers the
  daemon-side swap subserver via `RPCServiceRegistrars`),
  `swapwallet` (`walletrpc` build only — registers the daemon-side
  wallet RPC subserver),
  `google.golang.org/grpc/test/bufconn` (in-process transport).
- **Depended on by**: host Go apps, gomobile / React Native / WASM bridges,
  and `cmd/walletdk-tui` (Bubble Tea manual-test TUI; tracked in a sibling
  PR).
- **Sends**:
  - → `darepod` (in-process via bufconn): all daemon RPCs listed above
    are routed across the private gRPC connection rather than the
    daemon's public listener.
- **Receives**:
  - ← API: host application calls (`CreateWallet`, `Receive`, `Send`,
    `Subscribe`, `Exit`, …). walletdk does not register any RPC
    handlers itself; it only consumes them.

## Invariants

- `Client` is safe for concurrent use.
- The embedded daemon's lifetime is owned by walletdk's `runCtx`, not by the
  caller's `Start` context. A startup deadline cancels dialing, not the
  daemon. `Stop`/`Close` is the only correct way to terminate the runtime
  (the `//nolint:contextcheck` on `Start` guards this).
- `Start` does not return until either gRPC reports `Ready` against the
  embedded daemon, the daemon exits early with an error, or the caller's
  startup `ctx` is cancelled.
- A caller-supplied `DaemonConfig` is deep-copied via `cloneDaemonConfig`
  before walletdk mutates it (`RPC.Listener` is replaced and
  `RPCServiceRegistrars` may be appended to). New reference-typed fields
  added to `darepod.Config` require matching clone logic.
- Convenience booleans in `Config` are plain `bool` enable-only
  overrides: `true` forces the value, the zero value defers to
  `DaemonConfig`. There is no tri-state distinction between "not set"
  and "explicitly false" — set `DaemonConfig` directly when the host
  needs to force a `false`.
- `Config.EagerRoundJoin` flips the embedded daemon's
  `darepod.Config.EagerRoundJoin` so confirmed deposits and
  cooperative-leave intents auto-trigger a round join without the
  host having to call `daemonrpc.Board` or chase the round FSM
  forward separately. Default false defers to whatever the supplied
  `DaemonConfig` says; wallet-shaped hosts that expect single-RPC
  user interactions (`recv --onchain` → boarded VTXO, `exit` →
  on-chain leave) should set this true. The flag is the only
  walletdk-side signal that controls cooperative round-joining
  cadence; the destination resolution + cooperative-vs-unilateral
  policy for the `Exit` verb itself is still driven by the
  walletdk method body.
- Secret-bearing slices (`SeedPassphrase`, `WalletPassword`, `Mnemonic`) are
  cloned at the SDK boundary via `bytes.Clone` / `append` before being handed
  to the daemon RPC layer so host apps can zero their own copies on return
  without racing the RPC marshaller.
- Wallet methods (`Balance`, `Deposit`, `Receive`, `Send`, `List`,
  `Exit`, `ExitStatus`, `Status`, `Subscribe`) fail with
  `ErrWalletRPCUnavailable` on builds that omit the `walletrpc` build
  tag. The error is returned synchronously at the wrapper boundary,
  before any RPC is attempted. `ErrSwapRuntimeUnavailable` is an alias
  for source-level compatibility with older swap-only callers.
- `Entry.Kind` / `Entry.Status` / `ListResult.View` /
  `WalletVTXO.Status` / `OnchainTx.Kind` / `ExitJobStatus` are
  wrapper-owned lowercase strings, not proto enums. The projection
  layer lives in `convert.go`, intentionally decoupled from proto enum
  renumbering.
- `ListResult` is a discriminated union: read the populated variant
  named by `View` and treat the others as `nil`. The SDK does not
  enforce exhaustiveness at compile time, so consumers should switch on
  `View` rather than chaining nil checks.
- `Wait()` is single-reader: it returns the same shared channel on
  every call. The channel delivers the daemon's terminal run error and
  then closes; a closed channel reads as the zero error indefinitely.
- `Subscribe` returns an unbuffered updates channel so a slow consumer
  applies backpressure end-to-end; the errs channel is cap-1 for a
  single terminal error.

## Deep Docs

- [docs/walletdk_integration.md](../../docs/walletdk_integration.md) —
  Integration flow, startup/config examples, swap accounting, and host
  wrapper guidance.
- [docs/sdk_layered_architecture.md](../../docs/sdk_layered_architecture.md) —
  Layering rationale: `sdk/ark` facade, remote vs. embedded modes,
  `sdk/swaps` future direction (walletdk sits one layer above `sdk/ark` for
  wallet-shaped hosts).
- [sdk/ark/CLAUDE.md](../ark/CLAUDE.md) — Lower-level Ark SDK facade.
- [sdk/swaps/CLAUDE.md](../swaps/CLAUDE.md) — Swap FSM and durable
  session semantics. walletdk no longer exposes these directly; reach
  the underlying swap RPC client via `Client.SwapRPC()` when needed.
- [swapwallet/CLAUDE.md](../../swapwallet/CLAUDE.md) — Daemon-side
  walletrpc subserver that the SDK's wallet methods target.
- [swapclientserver/CLAUDE.md](../../swapclientserver/CLAUDE.md) —
  Daemon-side swap subserver registered when walletdk is built with
  `-tags swapruntime`.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
