# sdk/walletdk/mobile

## Purpose

gomobile-safe JSON facade over `sdk/walletdk`. Wraps the wallet SDK in a
flat API that respects gomobile's type restrictions (no `context.Context`,
no channels, no maps, no slices other than `[]byte`, no unsigned integers
crossing the boundary). Uses a JSON bytes-in / bytes-out convention so host
apps decode with their native JSON tools (kotlinx.serialization, Codable)
without a protobuf runtime. Also shared by `cmd/walletdk-wasm` as the
single source of truth for the browser bridge.

The entire package is gated on `//go:build mobile && walletdkrpc && swapruntime`.

## Key Types

- `lifecycleStatus` — four-state machine (`Stopped`, `Starting`, `Started`,
  `Stopping`) guarding the singleton daemon lifetime. Prevents a racing
  `Stop` from orphaning an in-progress `Start`.
- `state` — package-level singleton holding the live `*walletdk.Client`,
  call context, start/stop cancel functions, and a generation counter that
  lets a late-finishing boot detect it has been superseded.
- `Subscription` — pull-based subscription handle returned by `Subscribe`.
  `Next() ([]byte, error)` blocks until an event arrives or the
  subscription closes; `Close()` tears it down. Returned instead of a
  callback because Go channels cannot cross the gomobile boundary.

## Key Functions (API surface)

- `Start(cfgJSON string) error` / `Stop() error` — lifecycle. `Start` is
  synchronous (call off the main thread); `Stop` cancels in-flight calls
  and shuts down the embedded daemon.
- `IsRunning() bool` — returns `true` only in `statusStarted`.
- `GetInfo`, `CreateWallet`, `UnlockWallet`, `OpenWalletFromPasskey` —
  wallet lifecycle verbs. `OpenWalletFromPasskey` derives the seed from a
  WebAuthn PRF output in Go so the host never handles raw seed material.
- `Balance`, `Deposit`, `Receive`, `PrepareSend`, `SendPrepared`, `List`,
  `Exit`, `ExitStatus`, `Status`, `Subscribe` — core wallet verbs (JSON
  in / JSON out).
- `ConfirmedBalanceSat() (int64, error)`, `PendingInboundSat() (int64, error)`,
  `WalletReady() (bool, error)` — scalar convenience methods for the
  hottest widget paths.

## Relationships

- **Depends on**: `sdk/walletdk` (embedded daemon lifecycle and all typed
  wallet methods).
- **Depended on by**: `cmd/walletdk-wasm` (browser WASM bridge), gomobile
  bind output (Android/iOS).
- **Sends**: nothing (delegates in-process to `sdk/walletdk`).
- **Receives**: method calls from the host app (via gomobile or WASM).

## Invariants

- Singleton: only one daemon instance runs at a time. `Start` while
  `statusStarted` or `statusStarting` returns an error immediately.
- `Stop` racing an in-progress `Start` aborts the boot via `startCancel`
  rather than orphaning the daemon.
- The call context (`callCtx`) is cancelled by `Stop` so in-flight RPCs
  and `Subscription.Next` unwind promptly on shutdown.
- `Start` has a 90-second startup deadline; the daemon lifetime itself is
  owned by walletdk's internal `runCtx`, not the startup context.
- The package is source-compatible with `cmd/walletdk-wasm` — any new verb
  added here must also be wired into the WASM bridge switch.

## Deep Docs

- [sdk/walletdk/CLAUDE.md](../CLAUDE.md) — The underlying client this
  package wraps.
- [cmd/walletdk-wasm/CLAUDE.md](../../../cmd/walletdk-wasm/CLAUDE.md) —
  The browser bridge built on top of this facade.
- [ARCHITECTURE.md](../../../ARCHITECTURE.md) — System-wide package map.
