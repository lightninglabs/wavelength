# sdk/walletdk

## Purpose

Wallet-shaped SDK facade for host apps that want a small, stable Go API over
an embedded `darepod` client daemon. `Start` boots the daemon in-process,
dials it over a private `bufconn` gRPC transport, and exposes typed methods
for onboarding, balances, Lightning-to-Ark receives, Ark-to-Lightning sends,
and swap accounting. Daemon-owned swap send and receive are gated behind the
`swapruntime` build tag so default builds avoid the swap executor's dependency
graph.

## Key Types

- `Client` — Concurrency-safe wallet handle. Owns the embedded daemon
  lifecycle, the private `bufconn` gRPC connection, and the daemon-owned RPC
  clients (daemon, swap, wallet). `Stop`/`Close` are aliases; both are
  idempotent.
- `Config` — Embedded daemon + wallet facade config. Two usage modes:
  zero-value plus convenience fields (`DataDir`, `Network`, `ServerAddress`,
  …), or a caller-owned `DaemonConfig` plus only the convenience fields the
  host wants to override. The convenience booleans (`AllowMainnet`,
  `ServerInsecure`, `SwapServerInsecure`) are `fn.Option[bool]`: `fn.None`
  defers to `DaemonConfig`, `fn.Some(v)` forces that value.
- `ConnectConfig` — Configuration for `Connect` (remote daemon mode); holds
  `Address` and optional `DialOptions`.
- `Connect(ctx, ConnectConfig)` — Returns a `*Client` connected to an
  external daemon (no embedded daemon). Complement to `Start` for remote
  usage.
- `DefaultConfig` — Returns a walletdk `Config` populated from
  `darepod.DefaultConfig()`. Convenience starting point for hosts.
- `Start` — Boots the embedded daemon, dials it, waits for gRPC readiness,
  and returns a ready-to-use `*Client`. Detaches the daemon lifetime from the
  caller's `ctx` so a tight startup deadline does not kill the daemon.
- `WalletState` — Enum (`WalletStateNone`, `WalletStateLocked`,
  `WalletStateReady`) mirroring the daemon's tri-state for host UI surfaces.
- `Info` — Includes `WalletState` for the new tri-state wallet readiness
  field surfaced via `GetInfo`.
- `Balance` / `DepositRequest` / `DepositResult` — Onchain deposit address
  and balance DTOs.
- `ReceiveRequest` / `ReceiveResult` / `SendRequest` / `SendResult` —
  Wrapper-owned swap-start DTOs (swap-runtime only).
- `ListRequest` / `ListResult` / `Entry` / `EntryKind` / `EntryStatus` —
  Unified wallet history page request/response DTOs used by `List` and
  `Subscribe` (walletrpc only).
- `Status` — Unified balance + pending-entry summary DTO used by `Status`
  (walletrpc only).
- `SubscribeRequest` — Configuration for the `Subscribe` streaming call;
  optional `ExistingEntries bool` to emit current rows first.
- `SwapSummary` — Wrapper-owned durable swap view used for `ListSwaps`,
  `GetSwap`, `ResumeSwap`, and `SubscribeSwaps`. Stable lowercase `State`
  string is owned by `convert.go`, intentionally decoupled from the proto
  enum's renumbering.
- `SwapDirection` (`"pay"`, `"receive"`) — Public direction enum for resume
  requests and summary inspection.
- `ErrSwapRuntimeUnavailable` — Sentinel returned by `Receive`, `Send`,
  `ListSwaps`, `GetSwap`, `ResumeSwap`, and `SubscribeSwaps` when the package
  is built without the `swapruntime` tag.
- `WalletRPC()` — Returns the private `walletrpc.WalletServiceClient` for
  advanced callers.

## RPC Methods (host-facing API)

| Method | Description |
|--------|-------------|
| `GetInfo` | Daemon readiness snapshot: version, network, identity, wallet/server readiness, `WalletState` |
| `CreateWallet` | Create or import the embedded daemon wallet (auto-generates seed when mnemonic empty) |
| `UnlockWallet` | Unlock an existing embedded daemon wallet |
| `Balance` | Confirmed/unconfirmed boarding, VTXO, and on-chain balance buckets |
| `Deposit` | Allocate a fresh boarding address (walletrpc only) |
| `Receive` | Start a Lightning-to-Ark receive swap (swapruntime only) |
| `Send` | Start an Ark-to-Lightning payment or onchain send (walletrpc only) |
| `List` | Page through unified wallet history — swap, OOR, boarding, exit (walletrpc only) |
| `Status` | Unified balance + pending-entry summary (walletrpc only) |
| `Subscribe` | Stream `Entry` updates; optionally emit existing rows first (walletrpc only) |
| `ListSwaps` | List persisted daemon-owned swap summaries (swapruntime only) |
| `GetSwap` | Fetch one persisted swap by hex payment hash (swapruntime only) |
| `ResumeSwap` | Wake one pending persisted swap worker (swapruntime only) |
| `SubscribeSwaps` | Stream swap summary updates; optionally include existing rows (swapruntime only) |
| `Stop` / `Close` | Shut down the embedded daemon and release the private transport |
| `Wait` | Channel yielding the daemon's terminal run error (multi-reader via context.AfterFunc) |
| `GRPCConn` / `ArkRPC` / `SwapRPC` / `WalletRPC` | Escape hatches for the underlying gRPC connection and raw RPC clients |

## Relationships

- **Depends on**: `darepod` (embedded daemon runtime, default config,
  validation), `daemonrpc` (wallet, balance, info, address RPCs),
  `rpc/swapclientrpc` (daemon-owned swap RPCs), `rpc/walletrpc`
  (daemon-owned wallet RPCs), `swapclientserver` (`swapruntime` build only —
  registers the swap subserver via `RPCServiceRegistrars`), `swapwallet`
  (`walletrpc` + `swapruntime` build only — registers the wallet subserver),
  `google.golang.org/grpc/test/bufconn` (in-process transport).
- **Depended on by**: host Go apps, gomobile / React Native / WASM bridges,
  and `cmd/walletdk-tui` (Bubble Tea manual-test TUI; tracked in a sibling
  PR).
- **Sends**:
  - → `darepod` (in-process via bufconn): all daemon RPCs listed above are
    routed across the private gRPC connection rather than the daemon's
    public listener.
- **Receives**:
  - ← API: host application calls (`CreateWallet`, `Receive`, `Send`,
    `SubscribeSwaps`, …). walletdk does not register any RPC handlers itself;
    it only consumes them.

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
- Convenience booleans in `Config` are tri-state `fn.Option[bool]`: `fn.None`
  defers to `DaemonConfig`, `fn.Some(true)` / `fn.Some(false)` forces that
  value. There is no enable-only ambiguity.
- Secret-bearing slices (`SeedPassphrase`, `WalletPassword`, `Mnemonic`) are
  cloned at the SDK boundary via `bytes.Clone` / `append` before being handed
  to the daemon RPC layer so host apps can zero their own copies on return
  without racing the RPC marshaller.
- Swap-touching methods (`Receive`, `Send`, `ListSwaps`, `GetSwap`,
  `ResumeSwap`, `SubscribeSwaps`) fail with `ErrSwapRuntimeUnavailable` on
  builds that omit the `swapruntime` tag. The error is returned synchronously
  at the wrapper boundary, before any RPC is attempted.
- `SwapSummary.State` is a wrapper-owned lowercase string, not the generated
  proto enum. The explicit `SWAP_STATE_UNSPECIFIED` enum value maps to
  `"unspecified"`; unknown future enum values fall back to the lowercased
  proto name (minus the `SWAP_STATE_` prefix) so a new state surfaces in
  host UIs rather than being silently erased.
- `Wait()` is multi-reader: each call returns a fresh channel that delivers
  the same terminal error and then closes. Backed by `context.AfterFunc` on
  the runtime context, so no per-call goroutine is leaked.
- `SubscribeSwaps` returns an unbuffered updates channel so a slow consumer
  applies backpressure end-to-end; the errs channel is cap-1 for a single
  terminal error.
- Wallet-RPC methods (`Deposit`, `Send`, `List`, `Status`, `Subscribe`)
  return `ErrWalletRPCUnavailable` synchronously when the daemon was not
  compiled with both `walletrpc` and `swapruntime` tags. This is checked via
  `requireWalletRPC()` before any gRPC is attempted.
- `Connect` (remote mode) sets `canWallet` based on whether the daemon's
  `GetInfo` response advertises a `WalletService` capability.

## Deep Docs

- [docs/walletdk_integration.md](../../docs/walletdk_integration.md) —
  Integration flow, startup/config examples, swap accounting, and host
  wrapper guidance.
- [docs/sdk_layered_architecture.md](../../docs/sdk_layered_architecture.md) —
  Layering rationale: `sdk/ark` facade, remote vs. embedded modes,
  `sdk/swaps` future direction (walletdk sits one layer above `sdk/ark` for
  wallet-shaped hosts).
- [sdk/ark/CLAUDE.md](../ark/CLAUDE.md) — Lower-level Ark SDK facade.
- [sdk/swaps/CLAUDE.md](../swaps/CLAUDE.md) — Swap FSM and durable session
  semantics surfaced through walletdk's swap methods.
- [swapclientserver/CLAUDE.md](../../swapclientserver/CLAUDE.md) — Daemon-side
  swap subserver registered when walletdk is built with `-tags swapruntime`.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
