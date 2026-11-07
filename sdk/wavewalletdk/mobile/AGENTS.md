# sdk/wavewalletdk/mobile

## Purpose

Gomobile-safe facade over `sdk/wavewalletdk` for Android/iOS host apps. It flattens
the wallet SDK into free functions using only gomobile-legal types (no
`context.Context`, channels, maps, or non-`[]byte` slices), with JSON
bytes-in/bytes-out RPC verbs plus a few scalar convenience methods. Built only
via `gomobile bind` under the `mobile`, `wavewalletrpc`, and `swapruntime` build
tags; without those tags this package has no exported API at all (there is no
committed stub source in this directory, unlike `cmd/wavewalletdk-wasm/stub.go`).
The exported functions are a binding ABI for typed host SDKs, not the
application-facing wallet API.

## Key Types

- `Start(cfgJSON string) error` / `Stop() error` — singleton lifecycle for the
  embedded daemon; a four-state machine (`stopped`/`starting`/`started`/
  `stopping`) plus a generation counter guards a `Stop` racing an in-progress
  `Start`.
- `StartExternalSeedWallet(reqJSON []byte) ([]byte, error)` — starts at the
  final data directory, imports or unlocks 16 bytes of already-derived entropy,
  and returns the marshalled open result through the same lifecycle.
- `Subscription` — pull-based handle (`Next`/`Close`) over a wallet activity
  stream, replacing the callback interfaces gomobile would otherwise require.
- `mobileConfig` — unexported flat JSON config decoded by `parseConfig`/
  `applyMobileConfig` into a `wavewalletdk.Config`; validated (non-negative
  durations/counts, `uint32`-safe recovery window, `int32`-safe
  `max_payment_cltv`) before merging onto `wavewalletdk.DefaultConfig()`.
  Scalars are value-typed except `MaxPaymentCLTV *int64`, whose pointer
  distinguishes an omitted key from an explicit JSON zero.
- `mobileStartOptions(cfg)` — unexported bridge from an explicit zero-valued
  mobile setting to the `wavewalletdk.Option` that overrides an enable-only
  SDK convenience field. Its result is passed to `wavewalletdk.Start` and
  `wavewalletdk.StartExternalSeedWalletWithContexts`; today it emits
  `WithMaxPaymentCLTVDisabled()` and nothing else.
- `mobileReceiveRequest` — unexported wire DTO carrying the SDK's current
  `AmountSat`/`Memo` fields plus the mobile-only `TimeoutSeconds`. It is
  projected onto `wavewalletdk.ReceiveRequest` after deadline validation.
- RPC verbs (`GetInfo`, `CreateWallet`, `UnlockWallet`, `Balance`, `Deposit`,
  `Receive`, `PrepareSend`, `SendPrepared`, `List`, `Exit`, `ExitStatus`,
  `ExitSummary`, `GetExitPlan`, `SweepWallet`, `Status`, `Subscribe`,
  `OpenWalletFromPasskey`) — each dereferences the singleton `wavewalletdk.Client`
  via `activeClient()`, decodes a JSON request into the matching
  `wavewalletdk.*Request`, and marshals the `wavewalletdk.*Result` response.
  `Receive` is the exception: it decodes `mobileReceiveRequest` first so the
  binding can own its request deadline without changing the SDK DTO.
- Scalar conveniences (`ConfirmedBalanceSat`, `PendingInboundSat`,
  `WalletReady`, `IsRunning`) — avoid a JSON round trip for hot-path reads;
  `IsRunning` never blocks on an RPC.

## Relationships

- **Depends on**: `sdk/wavewalletdk` (wraps `wavewalletdk.Client`/`Config`/DTOs
  directly; this package owns no wallet logic of its own).
- **Depended on by**: nothing in-tree; consumed externally as a `gomobile
  bind` output (Android `.aar` / iOS `.xcframework`) built by
  `gen_bindings.sh`.

## Invariants

- All package state lives in the unexported singleton `state`; a second
  `Start` before `Stop` returns an error instead of booting a second daemon.
- `StartExternalSeedWallet` shares the same singleton guard.
- `StartExternalSeedWallet` accepts nested `config` and standard-base64
  `seed_entropy`. `config.data_dir` is the final wallet directory; this package
  derives no account, seed, or path namespace.
- `Stop` cancels startup or the running daemon and waits up to five seconds.
  A timeout retains the stopping generation and its client. New Start calls
  reject until actual cleanup ends; repeated Stop calls re-wait that owner.
- `startEmbedded` separates its daemon-readiness deadline from the lifecycle
  context used to open or recover the wallet. `Stop` cancels both.
- gomobile cannot carry a caller `context.Context`, so this package owns
  per-call deadlines. Repeatable reads (`GetInfo`, `Balance`, `List`,
  `ExitStatus`, `ExitSummary`, `GetExitPlan`, `Status`, and the scalar
  balance/readiness helpers) use `readContext`'s 10-second deadline. `Receive`
  uses `TimeoutSeconds`, defaults to 20 seconds when zero or omitted, and
  rejects negative values or values above five minutes.
- A `Receive` canceled by its binding-owned deadline or the parent lifecycle is
  returned with the stable `receiveUncertainErrorPrefix`. The result is
  uncertain: the host must reconcile Activity before deliberately requesting
  another invoice.
- Every bounded call context derives from the daemon-lifetime parent returned
  by `activeClient`, so `Stop` still cancels in-flight calls immediately.
- Startup through `startEmbedded` and `Subscription.Next` recover panics into
  a returned `error`; those are the entry points documented to survive a panic
  without crossing the gomobile boundary and killing the host process.
- `Subscribe`'s updates/close path is driven by a context derived from the
  wrapper-owned call context, not the caller's; `Stop` cancelling that context
  is what unblocks an in-flight `Subscription.Next`.
- `mobileConfig` validation must reject negative durations/counts before they
  reach `wavewalletdk.Config`, since a negative `WalletPollIntervalSeconds` in
  particular panics the lwwallet tip poller's ticker in a background
  goroutine after startup.
- `wavewalletdk.Config` convenience scalars are enable-only: zero defers to
  the daemon default rather than forcing zero. A mobile zero that must win
  needs both a pointer field in `mobileConfig` and a case in
  `mobileStartOptions`. For `max_payment_cltv`, omission keeps the 480-block
  swap default while explicit `0` disables the payment reserve.

## Deep Docs

- [docs/wavewalletdk_mobile.md](../../../docs/wavewalletdk_mobile.md) — Binding
  ABI, JSON field casing, per-call deadlines, and host lifecycle rules.
- [sdk/wavewalletdk/CLAUDE.md](../CLAUDE.md) — Wrapped SDK; see for full DTO and
  RPC method detail.
- [ARCHITECTURE.md](../../../ARCHITECTURE.md) — System-wide package map.

## Notification source registration

`SetNotificationSubscription` forwards the facade DTO's optional `source`
selector. `swap_notification_issuer` configures the separately trusted swap
source public identity. No push payload supplies a URL or issuer pin. The
binding call retains its lifecycle-derived ten-second deadline. See
[notification sources](../../../docs/notification-sources.md).

## Bounded canonical activity reads

Finite embedded activity batches join event positions to current canonical
rows. A captured ceiling bounds replay; the host persists explicit NextCursor
and deduplicates by canonical identity. Capturing a cursor does not reconcile
sources. Both direct SDK callbacks join shutdown before releasing the store.
See [finite activity reads](../../../docs/canonical-activity-batches.md).

## Enrolled invoice receipts

ReadLightningReceipts accepts the SDK typed-source request and uses a
cooperative ten-second context. It performs no wallet unlock or host window.
See [receipt sources](../../../docs/lightning-receipt-source.md).

## Warm receipt windows

Host mode transitions retain the daemon and join ingress ownership. Restricted
pumps validate full batches before waiter delivery; request provenance comes
from send-time registration. Targeted resume only re-arms existing receives.
See [warm invoice recovery](../../../docs/mobile-receipt-window.md).

## Bounded warm maintenance

The host maintenance window samples current-tip expiry for at most 64 persisted
VTXO IDs, drives existing owners outside the manager turn, retains a finite
keyset cursor across windows, then pumps native durable round work. It shares
the receipt controller's mode lease, cancellation fence and final-access join.
It performs no startup repair and produces no payment proof. See
[background windows](../../../docs/mobile-receipt-window.md).
