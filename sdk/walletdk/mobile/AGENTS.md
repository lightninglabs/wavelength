# sdk/walletdk/mobile

## Purpose

Gomobile-safe binding facade over `sdk/walletdk.Client` for iOS/Android hosts.
It exposes a package-level singleton daemon lifecycle plus a JSON bytes-in /
bytes-out RPC surface (with a few scalar convenience functions) so a gomobile
`bind` output needs no protobuf runtime and stays within gomobile's type
restrictions (no `context.Context`, no channels, no maps, no slices other than
`[]byte`, no unsigned integers crossing the boundary).

## Key Types

- `Start(cfgJSON string) error` — boots the embedded `darepod` wallet daemon
  from a JSON `mobileConfig` and blocks until the private gRPC transport is
  serving. Singleton-guarded: a second `Start` before `Stop` errors. Must be
  called off the main thread; a panic in the boot path is recovered into the
  returned error. A `Stop` that races an in-progress `Start` cancels the boot
  via the stored `startCancel`; `Start` then tears down any client it produced
  instead of publishing it (see the four-state `lifecycleStatus` machine —
  `statusStopped`/`statusStarting`/`statusStarted`/`statusStopping` — and the
  `gen` counter that lets a late-finishing `Start` tell it no longer owns the
  starting state).
- `Stop() error` — idempotent teardown; cancels the wrapper `callCtx` (unblocks
  any in-flight `Subscription.Next`) before stopping the underlying
  `walletdk.Client`, then resets the singleton so a host can `Start` again
  (e.g. after the OS suspends/resumes the app).
- `IsRunning() bool` — non-blocking lifecycle probe (`lifecycleActive`); true
  from the moment `Start` begins through the whole boot window until `Stop`
  completes. Does not imply RPCs will succeed — use `WalletReady`/`GetInfo`
  for that.
- RPC verbs, JSON bytes-in / bytes-out (`mobile.go`, `wallet.go`,
  `passkey.go`), each a thin wrapper around the matching `walletdk.Client`
  method via the package-level `activeClient()`: `GetInfo`, `CreateWallet`,
  `UnlockWallet`, `Balance`, `Deposit`, `Receive`, `PrepareSend`,
  `SendPrepared`, `List`, `Exit`, `ExitStatus`, `GetExitPlan`, `SweepWallet`,
  `Status`, `OpenWalletFromPasskey`. Request/response types are the same
  `sdk/walletdk` DTOs (e.g. `walletdk.CreateWalletRequest`/`Result`),
  marshaled/unmarshaled with the package-local `marshal`/`decode` helpers.
- `ConfirmedBalanceSat() (int64, error)`, `PendingInboundSat() (int64, error)`,
  `WalletReady() (bool, error)` (`convenience.go`) — scalar "hot path"
  shortcuts so a host does not need a JSON decoder for the most common
  single-value reads.
- `Subscription` (`mobile.go`) — pull-based handle over a wallet activity
  stream, returned by `Subscribe(reqJSON []byte) (*Subscription, error)`.
  `Next() ([]byte, error)` blocks for the next `walletdk.Entry` as JSON and
  returns `io.EOF` on a clean stream end (including a self-initiated `Close`
  or a `Stop`-triggered cancellation); `Close() error` cancels the derived
  context and unblocks any in-flight `Next`. This replaces the Go channel
  pair (`updates`/`errs`) that `walletdk.Client.Subscribe` returns, since a
  channel cannot cross the gomobile boundary.
- `mobileConfig` (`config.go`, unexported) — flat, JSON-serializable subset of
  `walletdk.Config`; `parseConfig` decodes it (empty string → `DefaultConfig`)
  and `applyMobileConfig` overlays only host-set fields onto the default,
  mirroring `walletdk.Config`'s own enable-only / non-empty convenience merge.
  `mobileConfig.validate()` rejects negative durations/counts and a
  `wallet_recovery_window` that would overflow the `uint32` narrowing, turning
  malformed JSON into a clean startup error instead of, e.g., a panicking
  `time.NewTicker` deep in a background goroutine.
- `OpenWalletFromPasskey(reqJSON []byte) ([]byte, error)` (`passkey.go`) —
  decodes a `{prfOutput: <hex>}` JSON object (camelCase key matches the
  browser/mobile bridge convention), hex-decodes the WebAuthn PRF assertion
  output, and delegates to `walletdk.Client.OpenWalletFromPasskey`. The
  PRF→seed derivation itself stays in `sdk/walletdk` so this binding and the
  wasm binding share one source of truth and raw seed material never touches
  browser/platform code beyond the hex string.

## Relationships

- **Depends on**: `sdk/walletdk` (`Client`, `Config`/`DefaultConfig`, all
  request/response DTOs, sentinel errors reconstructed by `errmap.go`).
- **Depended on by**: gomobile-generated iOS (`.xcframework`) and Android
  (`.aar`) bindings built by `gen_bindings.sh`, and the host mobile apps that
  link them. Nothing in this repo imports `sdk/walletdk/mobile` as a Go
  package — it is a bind target, not a library dependency.

## Invariants

- Every file is gated behind `//go:build mobile && walletdkrpc && swapruntime`.
  There is currently no unguarded stub in this package: without all three
  tags, `sdk/walletdk/mobile` has no buildable Go files at all, and only
  `gomobile bind` (via `gen_bindings.sh`, which passes exactly these tags)
  is expected to compile it. Plain `go build ./...` does not build this
  package's real implementation.
- The package exposes free functions plus a package-level singleton
  (`state`), not a handle type, because gomobile callers get one embedded
  daemon per process; do not add a second concurrent daemon lifecycle here —
  extend `walletdk.Client` directly for multi-instance use cases.
  All access to `state` is guarded by `state.mu`.
- No exported function may take a `context.Context`, return more than
  `(T, error)`, use a map, use a slice other than `[]byte`, or use variadic
  or generic parameters — gomobile bind rejects signatures that violate
  this. `activeClient()` supplies the wrapper-owned `callCtx` in place of a
  per-call context; a caller cannot pass its own context or a per-call
  timeout.
- Async/streaming operations use callback-free, pull-based polling
  (`Subscription.Next`) instead of Go channels, since channels cannot cross
  the gomobile boundary. Callers must run the poll loop off the UI/main
  thread and call `Close` to unblock a pending `Next` during teardown.
  `Subscription.ctx` is derived from the wrapper's `callCtx`, so a `Stop`
  also unblocks any live subscription even if the host never calls `Close`.
- Errors surface as plain Go `error` values across the gomobile boundary
  (gomobile maps them to a thrown exception per platform); this package does
  not itself re-map error causes to stable string codes. It relies entirely
  on `sdk/walletdk`'s `errmap.go` interceptor, which already reconstructs
  `errors.Is`-able sentinels (`walletdk.ErrInvalidDestination`, etc.) before
  the error reaches this package's wrappers. A host that needs to branch on
  failure cause should match on the sentinel's `Error()` string or a
  substring, since gomobile does not expose Go's `errors.Is` to Kotlin/Swift.
- `Start` must be called off the main thread — it can block for the full
  embedded daemon boot (bounded by `startTimeout`, 90s) — and only once
  live at a time; a concurrent `Start` while already `statusStarting` or
  `statusStarted` returns an error rather than booting a second daemon.
- A panic inside `Start` or `Subscription.Next` is recovered into a returned
  `error` so it never crosses the gomobile boundary as a process kill.
- `mobileConfig` intentionally omits `walletdk.Config`'s reference-typed
  fields (`DaemonConfig *darepod.Config`, `LogWriter io.Writer`) because they
  cannot cross a JSON/gomobile boundary; a host that needs fine-grained
  daemon knobs must use the Go SDK directly, not this package.
- `OpenWalletFromPasskey`'s hex-encoded `prfOutput` inherits
  `sdk/walletdk.OpenWalletFromPasskey`'s determinism requirement: the PRF
  ceremony's salt must be fixed and identical across devices/calls for a
  given wallet, or the derived seed changes and funds become unrecoverable.

## Deep Docs

- [sdk/walletdk/CLAUDE.md](../CLAUDE.md) — Underlying wallet SDK facade.
- [ARCHITECTURE.md](../../../ARCHITECTURE.md) — System-wide package map.
