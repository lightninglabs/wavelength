# sdk/walletdk/mobile

## Purpose

Gomobile-safe facade over `sdk/walletdk` for Android/iOS host apps. It flattens
the wallet SDK into free functions using only gomobile-legal types (no
`context.Context`, channels, maps, or non-`[]byte` slices), with JSON
bytes-in/bytes-out RPC verbs plus a few scalar convenience methods. Built only
via `gomobile bind` under the `mobile`, `walletdkrpc`, and `swapruntime` build
tags; without those tags this package has no exported API at all (there is no
committed stub source in this directory, unlike `cmd/walletdk-wasm/stub.go`).

## Key Types

- `Start(cfgJSON string) error` / `Stop() error` — singleton lifecycle for the
  embedded daemon; a four-state machine (`stopped`/`starting`/`started`/
  `stopping`) plus a generation counter guards a `Stop` racing an in-progress
  `Start`.
- `Subscription` — pull-based handle (`Next`/`Close`) over a wallet activity
  stream, replacing the callback interfaces gomobile would otherwise require.
- `mobileConfig` — unexported flat JSON config decoded by `parseConfig`/
  `applyMobileConfig` into a `walletdk.Config`; validated (non-negative
  durations/counts, `uint32`-safe recovery window) before merging onto
  `walletdk.DefaultConfig()`.
- RPC verbs (`GetInfo`, `CreateWallet`, `UnlockWallet`, `Balance`, `Deposit`,
  `Receive`, `PrepareSend`, `SendPrepared`, `List`, `Exit`, `ExitStatus`,
  `ExitSummary`, `GetExitPlan`, `SweepWallet`, `Status`, `Subscribe`,
  `OpenWalletFromPasskey`) — each dereferences the singleton `walletdk.Client`
  via `activeClient()`, decodes a JSON request into the matching
  `walletdk.*Request`, and marshals the `walletdk.*Result` response.
- Scalar conveniences (`ConfirmedBalanceSat`, `PendingInboundSat`,
  `WalletReady`, `IsRunning`) — avoid a JSON round trip for hot-path reads;
  `IsRunning` never blocks on an RPC.

## Relationships

- **Depends on**: `sdk/walletdk` (wraps `walletdk.Client`/`Config`/DTOs
  directly; this package owns no wallet logic of its own).
- **Depended on by**: nothing in-tree; consumed externally as a `gomobile
  bind` output (Android `.aar` / iOS `.xcframework`) built by
  `gen_bindings.sh`.

## Invariants

- All package state lives in the unexported singleton `state`; a second
  `Start` before `Stop` returns an error instead of booting a second daemon.
- `Stop` is idempotent and always resets the singleton so a subsequent `Start`
  can succeed (e.g. after OS suspend/resume).
- Only `Start` (mobile.go:98) and `Subscription.Next` (wallet.go:365-371)
  recover panics into a returned `error`; those are the two entry points
  documented to survive a panic without crossing the gomobile boundary and
  killing the host process.
- `Subscribe`'s updates/close path is driven by a context derived from the
  wrapper-owned call context, not the caller's; `Stop` cancelling that context
  is what unblocks an in-flight `Subscription.Next`.
- `mobileConfig` validation must reject negative durations/counts before they
  reach `walletdk.Config`, since a negative `WalletPollIntervalSeconds` in
  particular panics the lwwallet tip poller's ticker in a background
  goroutine after startup.

## Deep Docs

- [sdk/walletdk/CLAUDE.md](../CLAUDE.md) — Wrapped SDK; see for full DTO and
  RPC method detail.
- [ARCHITECTURE.md](../../../ARCHITECTURE.md) — System-wide package map.
