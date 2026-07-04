# sdk/walletdk

## Purpose

Wallet-shaped SDK facade for host apps that want a small, stable Go API over
an embedded `darepod` client daemon. `Start` boots the daemon in-process,
dials it over a private `bufconn` gRPC transport, and exposes typed methods
that mirror the seven core CLI verbs (create, unlock, send, recv, list,
balance, exit) plus supporting subscribe/deposit/status.

Wallet methods are gated behind the `walletdkrpc` build tag (transitively
requires `swapruntime`): stub builds compile, but wallet methods return
`ErrWalletRPCUnavailable` synchronously.

## Key Types

For field-level detail, use `go doc github.com/lightninglabs/darepo-client/sdk/walletdk.<Symbol>`.

- `Client` — concurrency-safe wallet handle owning the embedded daemon
  lifecycle, the private `bufconn` gRPC connection, and the
  daemonrpc/walletdkrpc/swapclientrpc clients. `Stop`/`Close` are
  idempotent aliases.
- `Config` — embedded daemon + wallet facade config. Two usage modes:
  zero-value plus convenience fields (`DataDir`, `Network`,
  `ServerAddress`, …), or a caller-owned `DaemonConfig` plus only the
  convenience fields the host wants to override. Convenience booleans
  (`AllowMainnet`, `ServerInsecure`, `SwapServerInsecure`,
  `EagerRoundJoin`) are plain enable-only `bool`s — `true` forces the
  value, zero defers to `DaemonConfig` (no tri-state). To force a
  `false`, set `DaemonConfig` directly or use the matching `Option`.
- `DefaultConfig` — `Config` populated from `darepod.DefaultConfig()`.
- `Start(ctx, cfg, opts...)` — boots the embedded daemon, dials it,
  waits for gRPC readiness, and returns a ready `*Client`. The daemon
  lifetime is owned by walletdk's `runCtx`, not the caller's `ctx`, so
  a tight startup deadline cancels dialing only.
- `Option` — functional option accepted as variadic trailing args.
  Options apply **after** the `Config`/`DaemonConfig` merge and after
  `configureSwapRuntime` / `configureWalletRPC`, so they can override
  values seeded by `darepod.DefaultConfig` or carried on a caller-owned
  `DaemonConfig`. First option: `WithEagerRoundJoinDisabled()` forces
  `daemonCfg.EagerRoundJoin = false`.
- DTOs (wrapper-owned, isolated from proto enums): `Info`, `Balance`,
  `CreateWalletResult`, `UnlockWalletResult`, `ReceiveRequest`/`Result`
  (returns invoice + initial `Entry`), the two-step send DTOs
  `PrepareSendRequest`/`PrepareSendResult` and
  `SendPreparedRequest`/`SendResult` (`PrepareSend` quotes and returns a
  single-use `SendIntentID`; `SendPrepared` dispatches it and returns
  `Entry` + `ActualAmountSat`, which equals the requested amount for a
  bounded send and the swept total for sweep-all), `DepositRequest`/`Result` (boarding
  address + initial `Entry`), `ListRequest`, `ListResult` (tagged union
  on `View`, populates one of `Activity`/`VTXOs`/`Onchain`),
  `ActivityList`, `VTXOInventory`, `OnchainHistory`, `Entry`
  (optional `Progress *EntryProgress` and `Request *EntryRequest`
  sub-objects, both nil when absent; `Request` is a `Type`-tagged
  union over lightning/onchain/ark), `WalletVTXO`, `OnchainTx`.
- `ExitRequest` / `ExitResult` / `ExitStatusRequest` /
  `ExitStatusResult` / `ExitJobStatus` — exit DTOs. `ExitRequest`
  carries the target outpoint plus an optional on-chain `Destination`
  for cooperative leave; when omitted, the daemon generates an
  internal backing-wallet address. The SDK delegates exit policy to
  `walletdkrpc.Exit`, which queues cooperative leave by default and
  starts unilateral unroll only when `ForceUnrollAck` carries the
  daemon's exact acknowledgement string. `Destination` and
  `ForceUnrollAck` are mutually exclusive. `ExitResult.Cooperative`
  reports the path taken; `QueuedOutpoints` echoes cooperative
  selection; `Created`/`ActorID` describe a forced unilateral job.
  `CooperativeError` and `ExitPathUnilateralFallback` are retained for
  source compatibility with the old fallback result shape but are not
  populated by current behavior. Status strings are the wrapper-owned
  lowercase set
  (`pending`/`materializing`/`csv_pending`/`sweeping`/`completed`/`failed`/`unspecified`).
- `GetExitPlanRequest` / `GetExitPlanResult` / `ExitPlanEntry` —
  previews backing-wallet funding readiness for unilateral exit across
  a slice of VTXO outpoints (required confirmations, fee-UTXO counts,
  funding shortfall), aggregating per-outpoint plans into a total.
- `SweepWalletRequest` / `SweepWalletResult` / `WalletSweepInput` —
  previews (or, with `Broadcast`, executes) a sweep of confirmed
  backing-wallet funds to a caller-supplied destination address.
- `CreditPreview` (optional field on `PrepareSendResult`) — describes
  how a prepared invoice send will draw on sat-native server credit;
  `SendRail` gained `SendRailCredit`/`SendRailMixed` alongside the
  existing rails.
- `CreateWalletRequest`/`Result` gained a recovery path:
  `RecoverState`/`RecoveryWindow` on the request opt into scanning for
  existing chain state on import, and `Result.RecoveryRan` plus
  recovered-object counters (`RecoveredBoardingAddresses`,
  `RecoveredBoardingUTXOs`, `RecoveredVTXOs`,
  `RecoveredOORReceiveScripts`, `RecoveredOORRecipientEvents`) report
  what recovery found.
- `OpenWalletFromPasskey` / `OpenWalletResult` — derives a
  reproducible wallet seed and local DB password from a WebAuthn
  passkey's PRF output (HKDF-SHA256) and imports (fresh device) or
  unlocks (existing local wallet) accordingly; see `passkey.go`.
- `ErrWalletRPCUnavailable` — sentinel returned by every wallet method
  on builds without the `walletdkrpc` tag.
- `ErrSwapRuntimeUnavailable` — back-compat alias for
  `ErrWalletRPCUnavailable`.
- Failure-reason sentinels (`ErrInvalidDestination`,
  `ErrInvalidSendIntent`, `ErrAmountRequired`, `ErrAmountInvalid`,
  `ErrUnsupportedKind`, `ErrSwapBackendUnavailable`,
  `ErrAmountExceedsVTXOLimit`, `ErrBalanceLimitExceeded`) mirror the
  daemon's walletdkrpc rejection taxonomy. `errmap.go` reconstructs
  these `errors.Is`-able sentinels from a `google.rpc.ErrorInfo` detail
  on the gRPC status via a unary client interceptor installed on every
  wallet connection (embedded bufconn and external `Connect`).

## RPC Methods (host-facing API)

| Method | Description |
|--------|-------------|
| `GetInfo` | Daemon readiness snapshot (version, network, identity, wallet/server readiness). |
| `CreateWallet` | Create or import the embedded wallet (auto-generates seed when mnemonic empty); proxies daemonrpc. |
| `UnlockWallet` | Unlock an existing wallet; proxies daemonrpc. |
| `OpenWalletFromPasskey` | Derive a reproducible wallet from a WebAuthn passkey PRF output; imports (fresh device) or unlocks (existing local wallet). |
| `Balance` | Flat balance (`confirmed_sat`, `pending_in_sat`, `pending_out_sat`, plus sat-native credit fields). |
| `Deposit` | Allocate a fresh boarding address (`recv --onchain` from CLI). |
| `Receive` | Open a Lightning invoice receive (`recv --offchain`). Returns `{Invoice, Entry}`. |
| `PrepareSend` | Validate + quote an outbound payment; returns a single-use `SendIntentID`. |
| `SendPrepared` | Dispatch a prepared send (consumes `SendIntentID`). Returns `{Entry, ActualAmountSat}`. |
| `List` | Unified history view (Activity / VTXOs / Onchain) as a tagged-union `ListResult`. |
| `Exit` | Trigger cooperative leave or unilateral unroll for a VTXO. |
| `ExitStatus` | Query the phase of an exit job. |
| `GetExitPlan` | Preview backing-wallet funding readiness for unilateral exit across VTXO outpoints. |
| `SweepWallet` | Preview or broadcast a sweep of confirmed backing-wallet funds. |
| `Status` | Wallet readiness, balance, pending-entry count. |
| `Subscribe` | Stream wallet activity (`Entry`) updates. |
| `Stop` / `Close` | Shut down the embedded daemon, release the private transport. |
| `Wait` | Single shared channel yielding the daemon's terminal run error. |
| `GRPCConn` / `ArkRPC` / `SwapRPC` / `WalletRPC` | Escape hatches to the underlying private gRPC conn and raw clients. |

## Relationships

- **Depends on**: `darepod` (embedded daemon runtime), `daemonrpc`
  (wallet, balance, info, address RPCs + direct paths for
  `CreateWallet`/`UnlockWallet`), `rpc/walletdkrpc` (unified wallet API
  the seven verbs target), `rpc/swapclientrpc` (raw-swap escape hatch),
  `swapclientserver` (registered as daemon-side swap subserver in
  `swapruntime` builds), `swapwallet` (daemon-side walletdkrpc subserver
  in `walletdkrpc` builds), `rpcauth` (TLS cert / macaroon dial and HTTP
  options for external `Connect`), `lnd/aezeed` + `golang.org/x/crypto/hkdf`
  (passkey seed derivation), `google.golang.org/grpc/test/bufconn`.
- **Depended on by**: host Go apps, `sdk/walletdk/mobile` (gomobile-safe
  JSON bytes-in/bytes-out facade over this package), React Native / WASM
  bridges, and `cmd/walletdk-tui`.
- **Sends** → `darepod` (in-process via bufconn): all daemon RPCs are
  routed across the private gRPC connection, not the daemon's public
  listener.
- **Receives** ← host application calls. walletdk registers no RPC
  handlers; it only consumes them.

## Invariants

- `Client` is safe for concurrent use.
- Daemon lifetime is owned by walletdk's `runCtx`, not the caller's
  `Start` ctx. A startup deadline cancels dialing, not the daemon.
  `Stop`/`Close` is the only correct termination path (the
  `//nolint:contextcheck` on `Start` guards this).
- `Start` blocks until either gRPC reports `Ready`, the daemon exits
  early with an error, or the caller's startup `ctx` cancels.
- A caller-supplied `DaemonConfig` is deep-copied via
  `cloneDaemonConfig` before mutation (`RPC.Listener` is replaced;
  `RPCServiceRegistrars` may be appended). New reference-typed fields
  added to `darepod.Config` require matching clone logic.
- `Config.EagerRoundJoin` flips the embedded daemon's flag so
  confirmed deposits and cooperative-leave intents auto-trigger a
  round join without the host chasing the FSM forward. The
  walletdkrpc-tagged embedded build already defaults this to `true`
  via `darepod.DefaultConfig` (see `darepod/config_walletdkrpc.go`), so
  leaving the convenience field zero is correct for nearly every
  host. To force eager round-join OFF, pass
  `walletdk.WithEagerRoundJoinDisabled()` to `Start`; it applies
  after the convenience merge and `configureWalletRPC` so the
  disable wins over the build-tag default and any `DaemonConfig`
  value.
- Secret-bearing slices (`SeedPassphrase`, `WalletPassword`,
  `Mnemonic`) are cloned at the SDK boundary via `bytes.Clone` /
  `append` before being handed to the RPC layer, so host apps can zero
  their copies on return without racing the marshaller.
- Wallet methods fail with `ErrWalletRPCUnavailable` synchronously at
  the wrapper boundary on builds without the `walletdkrpc` tag, before
  any RPC is attempted. `ErrSwapRuntimeUnavailable` is an alias for
  source-level compatibility with older swap-only callers.
- Every wallet gRPC connection (embedded bufconn and external
  `Connect`) installs `errorReconstructInterceptor`, which rewrites a
  daemon rejection's `google.rpc.ErrorInfo` reason into the matching
  SDK sentinel from `errmap.go` before the error reaches the caller.
- `OpenWalletFromPasskey` requires a fixed, app-controlled WebAuthn PRF
  salt on every device and call for a given wallet: the entire seed is
  HKDF-derived from the PRF output, so a varying salt silently derives
  a different, unrecoverable wallet.
- `Entry.Kind`/`Entry.Status` / `Entry.Progress.Phase` /
  `Entry.Request.Type` / `Entry.FailureCode` / `ListResult.View` /
  `WalletVTXO.Status` / `OnchainTx.Kind` / `ExitJobStatus` are
  wrapper-owned lowercase strings (not proto enums). Projection lives
  in `convert.go`, intentionally decoupled from proto enum
  renumbering.
- `ListResult` is a discriminated union: read the variant named by
  `View` and treat the others as `nil`. Exhaustiveness is not
  enforced at compile time — switch on `View` rather than chaining
  nil checks.
- The Activity view paginates by opaque `ListRequest.Cursor` /
  `ActivityList.NextCursor`, not `ListRequest.Offset` (VTXOs/Onchain
  still use `Offset`). Check `ActivityList.HasMore` rather than
  `Total`, which counts only the current page.
- `Entry.Progress` and `Entry.Request` are optional pointers: both are
  `nil` when the daemon supplied no progress hint / persisted no
  request, so nil-check before dereferencing. `Entry.Request` is a
  discriminated union — read the variant named by `Type`
  (`lightning`/`onchain`/`ark`) and treat the other fields as zero,
  the same idiom as `ListResult.View`.
- `Wait()` is single-reader: same shared channel on every call. The
  channel delivers the daemon's terminal run error then closes; a
  closed channel reads as the zero error indefinitely.
- `Subscribe` returns an unbuffered updates channel so a slow consumer
  applies backpressure end-to-end; the errs channel is cap-1 for a
  single terminal error.
- New options should follow the "apply after merge" placement so
  override semantics stay consistent.

## Deep Docs

- [docs/walletdk_integration.md](../../docs/walletdk_integration.md) —
  Integration flow, startup/config examples, swap accounting, host
  wrapper guidance.
- [docs/sdk_layered_architecture.md](../../docs/sdk_layered_architecture.md)
  — Layering rationale; walletdk sits one layer above `sdk/ark` for
  wallet-shaped hosts.
- [sdk/ark/CLAUDE.md](../ark/CLAUDE.md) — Lower-level Ark SDK facade.
- [sdk/swaps/CLAUDE.md](../swaps/CLAUDE.md) — Swap FSM and durable
  session semantics. Reach the underlying swap RPC client via
  `Client.SwapRPC()` when needed.
- [swapwallet/CLAUDE.md](../../swapwallet/CLAUDE.md) — Daemon-side
  walletdkrpc subserver.
- [swapclientserver/CLAUDE.md](../../swapclientserver/CLAUDE.md) —
  Daemon-side swap subserver (`-tags swapruntime`).
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
