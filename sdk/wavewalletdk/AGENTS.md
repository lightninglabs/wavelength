# sdk/wavewalletdk

## Purpose

Wallet-shaped SDK facade for host apps that want a small, stable Go API over
an embedded `waved` client daemon. `Start` boots the daemon in-process,
dials it over a private `bufconn` gRPC transport, and exposes typed methods
that mirror the seven core CLI verbs (create, unlock, send, recv, list,
balance, exit) plus supporting subscribe/deposit/status.

Wallet methods are gated behind the `wavewalletrpc` build tag (transitively
requires `swapruntime`): stub builds compile, but wallet methods return
`ErrWalletRPCUnavailable` synchronously.

## Key Types

For field-level detail, use `go doc github.com/lightninglabs/wavelength/sdk/wavewalletdk.<Symbol>`.

- `Client` — concurrency-safe wallet handle owning the embedded daemon
  lifecycle, the private `bufconn` gRPC connection, and the
  waverpc/wavewalletrpc/swapclientrpc clients. `Stop`/`Close` are
  idempotent aliases.
- `Config` — embedded daemon + wallet facade config. Two usage modes:
  zero-value plus convenience fields (`DataDir`, `Network`,
  `ServerAddress`, …), or a caller-owned `DaemonConfig` plus only the
  convenience fields the host wants to override. Convenience booleans
  (`AllowMainnet`, `ServerInsecure`, `SwapServerInsecure`,
  `EagerRoundJoin`) are plain enable-only `bool`s — `true` forces the
  value, zero defers to `DaemonConfig` (no tri-state). To force a
  `false`, set `DaemonConfig` directly or use the matching `Option`.
- `DefaultConfig` — `Config` populated from `waved.DefaultConfig()`.
- `Start(ctx, cfg, opts...)` — boots the embedded daemon, dials it,
  waits for gRPC readiness, and returns a ready `*Client`. The daemon
  lifetime is owned by wavewalletdk's `runCtx`, not the caller's `ctx`, so
  a tight startup deadline cancels dialing only.
- `Connect(ctx, ConnectConfig)` — dials an already-running external
  daemon instead of embedding one. `ConnectConfig.Transport` selects
  `TransportGRPC` (default) or `TransportREST`; `Insecure`,
  `TLSCertPath`, and `MacaroonPath` configure auth. `Client.Stop`/
  `Close` on a `Connect`-built client releases only the transport, not
  a daemon runtime.
- `Option` — functional option accepted as variadic trailing args.
  Options apply **after** the `Config`/`DaemonConfig` merge and after
  `configureSwapRuntime` / `configureWalletRPC`, so they can override
  values seeded by `waved.DefaultConfig` or carried on a caller-owned
  `DaemonConfig`. First option: `WithEagerRoundJoinDisabled()` forces
  `daemonCfg.EagerRoundJoin = false`.
- DTOs (wrapper-owned, isolated from proto enums): `Info` / `ServerInfo`
  (including the advisory free-refresh window), `Balance`,
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
  `wavewalletrpc.Exit`, which queues cooperative leave by default and
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
- `GetExitPlanRequest`/`Result`, `ExitPlanEntry` — previews backing-wallet
  funding needed before `Exit` can start, per outpoint and aggregated.
- `ExitSummaryRequest`/`Result`, `ExitSummaryEntry` — wallet-wide
  portfolio of in-progress (non-terminal) exits plus aggregate totals.
- `SweepWalletRequest`/`Result`, `WalletSweepInput` — preview or
  broadcast a full backing-wallet sweep to one destination address.
- `CreditPreview`, `SendRail`, `SendQuoteStatus` — embedded in
  `PrepareSendResult`; describe whether a prepared send will draw on
  sat-native server credits and how complete the prepare-time quote is.
- `ErrWalletRPCUnavailable` — sentinel returned by every wallet method
  on builds without the `wavewalletrpc` tag.
- `ErrSwapRuntimeUnavailable` — back-compat alias for
  `ErrWalletRPCUnavailable`.

## RPC Methods (host-facing API)

| Method | Description |
|--------|-------------|
| `GetInfo` | Daemon readiness snapshot (version, network, identity, wallet/server readiness). |
| `CreateWallet` | Create or import the embedded wallet (auto-generates seed when mnemonic empty); proxies waverpc. |
| `UnlockWallet` | Unlock an existing wallet; proxies waverpc. |
| `Balance` | Flat balance (`confirmed_sat`, `pending_in_sat`, `pending_out_sat`). |
| `Deposit` | Allocate a fresh boarding address (`recv --onchain` from CLI). |
| `Receive` | Open a Lightning invoice receive (`recv --offchain`). Returns `{Invoice, Entry}`. |
| `PrepareSend` | Validate + quote an outbound payment; returns a single-use `SendIntentID`. |
| `SendPrepared` | Dispatch a prepared send (consumes `SendIntentID`). Returns `{Entry, ActualAmountSat}`. |
| `List` | Unified history view (Activity / VTXOs / Onchain) as a tagged-union `ListResult`. |
| `Exit` | Trigger cooperative leave or unilateral unroll for a VTXO. |
| `ExitStatus` | Query the phase of an exit job. |
| `ExitSummary` | Wallet-wide portfolio of in-progress exits plus aggregate totals. |
| `GetExitPlan` | Preview backing-wallet funding needed before `Exit` can start. |
| `SweepWallet` | Preview or broadcast a full backing-wallet sweep. |
| `Status` | Wallet readiness, balance, pending-entry count. |
| `Subscribe` | Stream wallet activity (`Entry`) updates. |
| `Stop` / `Close` | Shut down the embedded daemon, or release the transport for a `Connect`-built client. |
| `Wait` | Single shared channel yielding the daemon's terminal run error. |
| `GRPCConn` / `ArkRPC` / `SwapRPC` / `WalletRPC` / `BtcwalletRPC` / `BtcwalletVersionRPC` | Escape hatches to the underlying private gRPC conn and raw clients. |

## Relationships

- **Depends on**: `waved` (embedded daemon runtime), `waverpc`
  (wallet, balance, info, address RPCs + direct paths for
  `CreateWallet`/`UnlockWallet`), `rpc/wavewalletrpc` (unified wallet API
  the seven verbs target), `rpc/swapclientrpc` (raw-swap escape hatch),
  `rpc/restclient` (REST transport for `Connect`), `rpcauth` (macaroon /
  TLS-cert helpers for `Connect`), `swapclientserver` (registered as
  daemon-side swap subserver in `swapruntime` builds), `swapwallet`
  (daemon-side wavewalletrpc subserver in `wavewalletrpc` builds),
  `google.golang.org/grpc/test/bufconn`.
- **Depended on by**: host Go apps directly, and `sdk/wavewalletdk/mobile`
  (gomobile bridge consumed by `cmd/wavewalletdk-wasm` and React
  Native/mobile hosts).
- **Sends** → `waved` (in-process via bufconn): all daemon RPCs are
  routed across the private gRPC connection, not the daemon's public
  listener.
- **Receives** ← host application calls. wavewalletdk registers no RPC
  handlers; it only consumes them.

## Invariants

- `Client` is safe for concurrent use.
- Daemon lifetime is owned by wavewalletdk's `runCtx`, not the caller's
  `Start` ctx. A startup deadline cancels dialing, not the daemon.
  `Stop`/`Close` is the only correct termination path (the
  `//nolint:contextcheck` on `Start` guards this).
- `Start` blocks until either gRPC reports `Ready`, the daemon exits
  early with an error, or the caller's startup `ctx` cancels.
- A caller-supplied `DaemonConfig` is deep-copied via
  `cloneDaemonConfig` before mutation (`RPC.Listener` is replaced;
  `RPCServiceRegistrars` may be appended). New reference-typed fields
  added to `waved.Config` require matching clone logic.
- `Config.EagerRoundJoin` flips the embedded daemon's flag so
  confirmed deposits and cooperative-leave intents auto-trigger a
  round join without the host chasing the FSM forward. The
  wavewalletrpc-tagged embedded build already defaults this to `true`
  via `waved.DefaultConfig` (see `waved/config_wavewalletrpc.go`), so
  leaving the convenience field zero is correct for nearly every
  host. To force eager round-join OFF, pass
  `wavewalletdk.WithEagerRoundJoinDisabled()` to `Start`; it applies
  after the convenience merge and `configureWalletRPC` so the
  disable wins over the build-tag default and any `DaemonConfig`
  value.
- Secret-bearing slices (`SeedPassphrase`, `WalletPassword`,
  `Mnemonic`) are cloned at the SDK boundary via `bytes.Clone` /
  `append` before being handed to the RPC layer, so host apps can zero
  their copies on return without racing the marshaller.
- Wallet methods fail with `ErrWalletRPCUnavailable` synchronously at
  the wrapper boundary on builds without the `wavewalletrpc` tag, before
  any RPC is attempted. `ErrSwapRuntimeUnavailable` is an alias for
  source-level compatibility with older swap-only callers.
- `Entry.Kind`/`Entry.Status` / `Entry.Progress.Phase` /
  `Entry.Request.Type` / `ListResult.View` / `WalletVTXO.Status` /
  `OnchainTx.Kind` / `ExitJobStatus` are wrapper-owned lowercase
  strings (not proto enums). Projection lives in `convert.go`,
  intentionally decoupled from proto enum renumbering.
- `ListResult` is a discriminated union: read the variant named by
  `View` and treat the others as `nil`. Exhaustiveness is not
  enforced at compile time — switch on `View` rather than chaining
  nil checks.
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

- [docs/wavewalletdk_integration.md](../../docs/wavewalletdk_integration.md) —
  Integration flow, startup/config examples, swap accounting, host
  wrapper guidance.
- [docs/sdk_layered_architecture.md](../../docs/sdk_layered_architecture.md)
  — Layering rationale; wavewalletdk sits one layer above `sdk/ark` for
  wallet-shaped hosts.
- [sdk/ark/CLAUDE.md](../ark/CLAUDE.md) — Lower-level Ark SDK facade.
- [sdk/swaps/CLAUDE.md](../swaps/CLAUDE.md) — Swap FSM and durable
  session semantics. Reach the underlying swap RPC client via
  `Client.SwapRPC()` when needed.
- [swapwallet/CLAUDE.md](../../swapwallet/CLAUDE.md) — Daemon-side
  wavewalletrpc subserver.
- [swapclientserver/CLAUDE.md](../../swapclientserver/CLAUDE.md) —
  Daemon-side swap subserver (`-tags swapruntime`).
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
