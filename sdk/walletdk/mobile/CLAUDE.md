# sdk/walletdk/mobile

## Purpose

Gomobile-safe facade over `sdk/walletdk`: a flat, JSON-bytes-in/bytes-out API
(plus a few scalar convenience methods) that respects gomobile's type
restrictions, so iOS/Android hosts — and the `cmd/walletdk-wasm` browser
bridge — can drive the embedded wallet daemon without a protobuf runtime or
callback interfaces.

## Key Types

- `Start(cfgJSON string) error` / `Stop() error` — singleton lifecycle for
  the package-level embedded daemon; gomobile exposes free functions, so the
  live client lives in a package-global `state`, not a host-carried handle.
  Both are safe to call from any thread and race-guarded via an explicit
  four-state machine (`statusStopped/Starting/Started/Stopping`).
- `Subscription` — pull-based handle (`Next`/`Close`) over a wallet activity
  stream, standing in for the Go channel that cannot cross the gomobile
  boundary; maps to a Kotlin `Flow` or Swift `AsyncStream`.
- Verb functions in `wallet.go` (`GetInfo`, `CreateWallet`, `UnlockWallet`,
  `Balance`, `Deposit`, `Receive`, `PrepareSend`, `SendPrepared`, `List`,
  `Exit`, `ExitStatus`, `ExitSummary`, `GetExitPlan`, `SweepWallet`,
  `Status`, `Subscribe`) — each decodes a JSON request into the matching
  `walletdk` DTO, calls the singleton `*walletdk.Client`, and marshals the
  `walletdk` result back to JSON.
- `OpenWalletFromPasskey` (`passkey.go`) — imports/unlocks the wallet from a
  hex-encoded WebAuthn PRF assertion output; the PRF→seed derivation lives
  here in Go so the wasm and gomobile bindings share one source of truth and
  the browser never handles raw seed material.
- `mobileConfig` (`config.go`) — flat, JSON-serializable subset of
  `walletdk.Config`; `parseConfig`/`applyMobileConfig` overlay only the
  fields a host actually set onto `walletdk.DefaultConfig()`, mirroring
  walletdk's own enable-only convenience-merge semantics.
- Scalar conveniences (`convenience.go`): `ConfirmedBalanceSat`,
  `PendingInboundSat`, `WalletReady`, `IsRunning` — avoid round-tripping JSON
  for the hottest UI paths.

## Relationships

- **Depends on**: `sdk/walletdk` (the wrapped Go SDK; every verb here
  proxies a `*walletdk.Client` method and reuses its request/response DTOs).
- **Depended on by**: `cmd/walletdk-wasm` (dispatches every browser
  `walletdkCall` verb to this package), and out-of-repo iOS/Android hosts
  that consume the gomobile-generated `.xcframework`/`.aar` built by
  `gen_bindings.sh`.

## Invariants

- Every hand-written file except `doc.go` is gated behind `//go:build mobile
  && walletdkrpc && swapruntime`. Without those tags the package compiles
  down to just `doc.go`'s package comment (there is no separate `stub.go`
  here, unlike `cmd/walletdk-wasm`) — `go build ./...` sees an effectively
  empty package, and the real API only exists in a `gomobile bind` output.
- Gomobile type restrictions are load-bearing, not stylistic: no
  `context.Context`, no channels, no maps, no slices other than `[]byte`, no
  unsigned integers may cross an exported function signature. `Subscription`
  and the JSON-bytes convention exist specifically to route around this.
- `Start` is synchronous and singleton-guarded: a second `Start` before
  `Stop` returns an error rather than booting a second daemon. A `Stop` that
  races an in-progress `Start` cancels the boot via `startCancel`/`gen`, and
  that `Start` then tears down any client it produced instead of publishing
  it — do not "simplify" the status/gen bookkeeping in `mobile.go` without
  preserving that race guard.
- `activeClient` hands out the wrapper-owned `callCtx`; callers must not
  retain it past the call, since `Stop` cancels it to unblock any in-flight
  `Subscription.Next`.
- `gen_bindings.sh` is a separate build-artifact step (invokes `gomobile
  bind` to produce the Android `.aar` / iOS `.xcframework`) — it does not
  generate any of the `.go` files in this package, which are all
  hand-written and reviewed normally.
- New host-facing capabilities should be added as a new verb/function here
  mirroring an existing `walletdk.Client` method, then wired into
  `cmd/walletdk-wasm`'s `walletCall` switch — keep the two in sync so the
  wasm bridge never drifts from the mobile bindings.

## Deep Docs

- [ARCHITECTURE.md](../../../ARCHITECTURE.md) — System-wide package map.
