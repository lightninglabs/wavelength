# sdk/walletdk/mobile

## Purpose

gomobile-safe facade over `sdk/walletdk` that flattens the wallet SDK into a
JSON bytes-in/bytes-out (plus a handful of scalar convenience calls) API, so
Android/iOS/WASM hosts without a Go runtime can drive an embedded `darepod`
wallet daemon through `gomobile bind` bindings.

## Key Types

- `Subscription` — gomobile-safe, pull-based handle over a wallet activity
  stream (`Next`/`Close`), used in place of a Go channel, which cannot cross
  the gomobile boundary.
- `mobileConfig` — flat, JSON-serializable subset of `walletdk.Config` (all
  scalars: strings, bools, `int64` seconds/amounts) that a JSON host can
  express; deliberately omits the reference-typed `DaemonConfig`/`LogWriter`
  fields.
- `lifecycleStatus` — four-state daemon lifecycle enum (`statusStopped` /
  `statusStarting` / `statusStarted` / `statusStopping`) backing the
  package-level singleton `state`, closing the race between an in-flight
  `Start` and a racing `Stop`.

## Relationships

- **Depends on**: `sdk/walletdk` (wraps `walletdk.Client`/`walletdk.Start`
  and re-exports its request/result DTOs as JSON payloads for
  gomobile-compatible bindings).
- **Depended on by**: `cmd/walletdk-wasm` (thin `syscall/js` adapter that
  reuses this same facade for WebAssembly hosts); gomobile build tooling —
  `sdk/walletdk/mobile/gen_bindings.sh` and the `make mobile` /
  `mobile-android` / `mobile-ios` targets run `gomobile bind` against this
  package to produce the Android `.aar` and iOS `.xcframework`;
  `.github/workflows/mobile-bindings.yml` compiles and tests it on every PR
  touching `sdk/walletdk/**`.

## Invariants

- Every file is gated behind `//go:build mobile && walletdkrpc &&
  swapruntime`; a plain `go build ./...` sees no buildable Go files in this
  package, so it only becomes real code inside a `gomobile bind` or wasm
  build.
- No `context.Context`, channels, maps, or non-`[]byte` slices cross the
  exported API, and unsigned integers are avoided too (`mobileConfig` uses
  `int64` seconds/amounts rather than `time.Duration`/`uint32`).
  `Subscription`'s pull-based `Next`/`Close` substitutes for a channel.
- Exported functions are free functions operating on the package-level
  singleton `state`, not methods on a host-visible handle — gomobile exposes
  free functions, so only one embedded daemon can run per process at a time;
  `Start` returns an error rather than booting a second daemon if called
  before a prior `Stop` completes.
- Every RPC verb follows the `(reqJSON []byte) ([]byte, error)` (or no-arg
  `() ([]byte, error)`) convention so hosts decode with
  `kotlinx.serialization`/`Codable` without a protobuf runtime; a few scalar
  hot paths (`ConfirmedBalanceSat`, `PendingInboundSat`, `WalletReady`,
  `IsRunning`) return plain scalars instead of JSON.
- Panics inside exported entry points (`Start`, `Subscription.Next`) are
  recovered into the returned `error` so they never cross the gomobile
  boundary as a process kill.
- `mobileConfig.validate` rejects negative durations/amounts and an
  oversized `wallet_recovery_window` before they reach the daemon: gomobile's
  signed-only integers could otherwise produce a negative `time.Duration`
  that panics a background ticker, or silently wrap on the `int64`→`uint32`
  conversion.

## Deep Docs

- [sdk/walletdk/CLAUDE.md](../CLAUDE.md) — Underlying wallet SDK this
  package wraps.
- [ARCHITECTURE.md](../../../ARCHITECTURE.md) — System-wide package map.
