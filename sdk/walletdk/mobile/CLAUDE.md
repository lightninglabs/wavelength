# sdk/walletdk/mobile

## Purpose

Gomobile-safe facade over `sdk/walletdk` that provides a flat,
host-language-compatible API. Wraps the wallet SDK in types that respect
gomobile's type restrictions: no `context.Context`, no channels, no maps, no
slices other than `[]byte`, and no unsigned integers at the language boundary.
RPC verbs use a JSON bytes-in / bytes-out convention so host apps decode with
kotlinx.serialization or Swift Codable without needing a protobuf runtime.
`Start` is synchronous (must be called off the UI thread); the one streaming
verb returns a pull-based `Subscription` handle instead of a callback because
Go channels cannot cross the gomobile boundary.

Only compiles with `-tags "mobile walletdkrpc swapruntime"`. `stub.go`
provides an empty build so `go build ./...` succeeds without the tags.

## Key Types

- `Subscription` — pull-based streaming handle returned by `Subscribe`.
  `Next() ([]byte, error)` blocks until the next entry JSON is available or
  the stream ends (`io.EOF`). `Close() error` cancels the underlying stream.

## Lifecycle

The package manages a package-level singleton guarded by a mutex; at most one
Start/Stop cycle can be active simultaneously.

- `Start(cfgJSON string) error` — boots the embedded walletdk daemon;
  blocks until gRPC is ready or start fails.
- `Stop() error` — shuts down the daemon; idempotent.
- `IsRunning() bool` — true while the daemon is up.

## JSON RPC Verbs

All verbs take a JSON request body (`[]byte`, nil treated as zero request) and
return a JSON response body. Host apps marshal/unmarshal with their native JSON
library.

| Verb | Description |
|------|-------------|
| `GetInfo() ([]byte, error)` | Daemon readiness snapshot. |
| `Balance() ([]byte, error)` | Flat balance summary. |
| `Status() ([]byte, error)` | Wallet readiness, balance, pending-entry count. |
| `CreateWallet([]byte) ([]byte, error)` | Create or import wallet. |
| `UnlockWallet([]byte) ([]byte, error)` | Unlock existing wallet. |
| `Deposit([]byte) ([]byte, error)` | Allocate boarding address. |
| `Receive([]byte) ([]byte, error)` | Open Lightning receive. |
| `PrepareSend([]byte) ([]byte, error)` | Quote outbound payment; returns single-use intent ID. |
| `SendPrepared([]byte) ([]byte, error)` | Dispatch prepared send. |
| `List([]byte) ([]byte, error)` | Unified history view. |
| `Exit([]byte) ([]byte, error)` | Trigger cooperative or unilateral exit. |
| `ExitStatus([]byte) ([]byte, error)` | Query exit job phase. |
| `GetExitPlan([]byte) ([]byte, error)` | Preview unilateral-exit readiness. |
| `SweepWallet([]byte) ([]byte, error)` | Preview or broadcast backing-wallet sweep. |
| `Subscribe([]byte) (*Subscription, error)` | Stream wallet activity updates. |

## Scalar Convenience Methods

Hot UI paths that must avoid JSON decode overhead:

- `ConfirmedBalanceSat() (int64, error)`
- `PendingInboundSat() (int64, error)`
- `WalletReady() (bool, error)`

## Relationships

- **Depends on**: `sdk/walletdk` (all wallet operations delegate to the
  embedded `walletdk.Client`).
- **Depended on by**: `cmd/walletdk-wasm` (WASM bridge), iOS/Android gomobile
  targets, `cmd/walletdk-tui`.
- **Sends**: nothing (all calls delegate to `walletdk.Client`).
- **Receives** ← host application (gomobile or syscall/js): lifecycle and
  verb calls.

## Invariants

- Singleton lifecycle: concurrent `Start` calls while the daemon is already
  starting or running return an error; `Stop` during stopping is a no-op.
- All RPC verbs call `activeClient()` and return an error if the daemon is
  not running, so hosts never need to guard calls with `IsRunning`.
- Verb implementations use `callCtx` so a `Stop` cancels in-flight calls
  cleanly before tearing down the daemon.
- The `gen` counter guards against a racing Stop/Start: a goroutine that
  started under `gen N` detects the new `gen N+1` and exits rather than
  touching the fresh client.

## Deep Docs

- [sdk/walletdk/CLAUDE.md](../CLAUDE.md) — The underlying Go SDK this
  facade wraps.
- [cmd/walletdk-wasm/CLAUDE.md](../../../cmd/walletdk-wasm/CLAUDE.md) —
  WASM bridge that wraps this facade for browsers.
- [ARCHITECTURE.md](../../../ARCHITECTURE.md) — System-wide package map.
