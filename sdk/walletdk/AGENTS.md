# sdk/walletdk

## Purpose

Wallet-shaped SDK facade for host apps that want a small, stable Go API over
an embedded `darepod` client daemon. `Start` boots the daemon in-process,
dials it over a private `bufconn` gRPC transport, and exposes typed methods
that mirror the seven core CLI verbs (create, unlock, send, recv, list,
balance, exit) plus supporting subscribe/deposit/status/exit-plan/sweep.

Wallet methods are gated behind the `walletdkrpc` build tag (transitively
requires `swapruntime`): stub builds compile, but wallet methods return
`ErrWalletRPCUnavailable` synchronously.

The embedded (`Start`) path builds under `js`/`wasm` (no more `!js` guard on
`embedded.go`/`embedded_config.go`/`swapruntime*.go`/`walletdkrpc*.go`). Only
the out-of-process `Connect` path stays native-only: `connect_grpc_js.go`
stubs `connectGRPC` on `js` builds to return an error directing callers at a
REST transport, since gRPC-over-TCP isn't available in the browser.

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
- DTOs (wrapper-owned, isolated from proto enums): `Info`, `Balance`
  (adds `CreditAvailableSat`/`CreditReservedSat` for sat-native server
  credit alongside `ConfirmedSat`/`PendingInSat`/`PendingOutSat`),
  `CreateWalletResult` (also carries `RecoveryRan` and per-kind
  `Recovered*` counters — `RecoveredBoardingAddresses`,
  `RecoveredBoardingUTXOs`, `RecoveredVTXOs`,
  `RecoveredOORReceiveScripts`, `RecoveredOORRecipientEvents` — set when
  `CreateWalletRequest.RecoverState` requested a rescan), `UnlockWalletResult`,
  `ReceiveRequest`/`Result` (returns invoice + initial `Entry`), the
  two-step send DTOs `PrepareSendRequest`/`PrepareSendResult` (result
  adds an optional `CreditPreview` — `MustUseCredit`, `CreditAppliedSat`,
  `CreditShortfallSat`, `CreditTopupSat`, `ArkFundingSat` — describing how
  server credit will fund the send) and `SendPreparedRequest`/`SendResult`
  (`PrepareSend` quotes and returns a single-use `SendIntentID`;
  `SendPrepared` dispatches it and returns `Entry` + `ActualAmountSat`,
  which equals the requested amount for a bounded send and the swept
  total for sweep-all), `DepositRequest`/`Result` (boarding
  address + initial `Entry`), `ListRequest`, `ListResult` (tagged union
  on `View`, populates one of `Activity`/`VTXOs`/`Onchain`),
  `ActivityList`, `VTXOInventory`, `OnchainHistory`, `Entry`
  (optional `Progress *EntryProgress` and `Request *EntryRequest`
  sub-objects, both nil when absent; `Request` is a `Type`-tagged
  union over lightning/onchain/ark), `WalletVTXO`, `OnchainTx`.
- `SendRail` gained `SendRailCredit` and `SendRailMixed` alongside the
  original `SendRailInArk`/`SendRailLightning`/`SendRailOnchain`, so a
  prepared send can now be quoted as funded purely from server credit or
  from a credit+Ark blend.
- `OpenWalletResult` — return type of `OpenWalletFromPasskey`.
  `Imported` is `true` when a new local wallet was created from the
  passkey-derived seed (fresh device) and `false` when an existing local
  wallet was unlocked instead; `Mnemonic` is populated only on import
  (for backup display), `IdentityPubKey` always.
- `GetExitPlanRequest`/`GetExitPlanResult`/`ExitPlanEntry` — preview DTOs
  for `GetExitPlan`. `ExitPlanEntry` reports, per outpoint, the backing
  wallet's funding address, required/usable fee-UTXO counts,
  recommended/shortfall funding amounts, whether an exit job already
  exists (`ExitJobFound`, `ExitStatus`, `SweepTxid`, `LastError`), and a
  per-outpoint `Err` string on failure. `GetExitPlanResult` aggregates
  `CanStart` and total funding numbers across all previewed outpoints.
- `SweepWalletRequest`/`SweepWalletResult`/`WalletSweepInput` — preview
  or broadcast DTOs for `SweepWallet`. `Broadcast=false` previews input
  selection and fee only; `Broadcast=true` also returns `Txid`.
  `FailureReason` is set instead of an error for expected preview
  failures (e.g. no spendable backing-wallet UTXOs).
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
- `ErrWalletRPCUnavailable` — sentinel returned by every wallet method
  on builds without the `walletdkrpc` tag.
- `ErrSwapRuntimeUnavailable` — back-compat alias for
  `ErrWalletRPCUnavailable`.
- Rejection sentinels (`errors.go`), reconstructed by the error-mapping
  layer below so callers can `errors.Is` instead of matching gRPC status
  strings: `ErrInvalidDestination`, `ErrInvalidSendIntent`,
  `ErrAmountRequired`, `ErrAmountInvalid`, `ErrUnsupportedKind`,
  `ErrSwapBackendUnavailable`, `ErrAmountExceedsVTXOLimit`,
  `ErrBalanceLimitExceeded`. Each mirrors one `walletdkrpc.Reason*`
  constant from the daemon's failure taxonomy.

## RPC Methods (host-facing API)

| Method | Description |
|--------|-------------|
| `GetInfo` | Daemon readiness snapshot (version, network, identity, wallet/server readiness). |
| `CreateWallet` | Create or import the embedded wallet (auto-generates seed when mnemonic empty); proxies daemonrpc. |
| `UnlockWallet` | Unlock an existing wallet; proxies daemonrpc. |
| `Balance` | Flat balance (`confirmed_sat`, `pending_in_sat`, `pending_out_sat`). |
| `Deposit` | Allocate a fresh boarding address (`recv --onchain` from CLI). |
| `Receive` | Open a Lightning invoice receive (`recv --offchain`). Returns `{Invoice, Entry}`. |
| `PrepareSend` | Validate + quote an outbound payment; returns a single-use `SendIntentID`. |
| `SendPrepared` | Dispatch a prepared send (consumes `SendIntentID`). Returns `{Entry, ActualAmountSat}`. |
| `List` | Unified history view (Activity / VTXOs / Onchain) as a tagged-union `ListResult`. |
| `Exit` | Trigger cooperative leave or unilateral unroll for a VTXO. |
| `ExitStatus` | Query the phase of an exit job. |
| `GetExitPlan` | Preview backing-wallet funding readiness for unilateral exit across a slice of VTXOs. |
| `SweepWallet` | Preview or broadcast a sweep of confirmed backing-wallet funds to a destination address. |
| `OpenWalletFromPasskey` | Derive a wallet deterministically from a passkey PRF output; imports on a fresh device, unlocks otherwise. |
| `Status` | Wallet readiness, balance, pending-entry count. |
| `Subscribe` | Stream wallet activity (`Entry`) updates. |
| `Stop` / `Close` | Shut down the embedded daemon, release the private transport. |
| `Wait` | Single shared channel yielding the daemon's terminal run error. |
| `GRPCConn` / `ArkRPC` / `SwapRPC` / `WalletRPC` | Escape hatches to the underlying private gRPC conn and raw clients. |

## Relationships

- **Depends on**: `darepod` (embedded daemon runtime), `daemonrpc`
  (wallet, balance, info, address RPCs + direct paths for
  `CreateWallet`/`UnlockWallet`), `rpc/walletdkrpc` (unified wallet API
  the seven verbs target, plus its exported `Reason*` failure-taxonomy
  constants and `FailureDomain` consumed by `errmap.go`),
  `rpc/swapclientrpc` (raw-swap escape hatch), `swapclientserver`
  (registered as daemon-side swap subserver in `swapruntime` builds),
  `swapwallet` (daemon-side walletdkrpc subserver in `walletdkrpc`
  builds, also supplies `swapwallet.ErrorMappingInterceptor` wired in
  `configureWalletRPC` so the embedded daemon tags rejections the same
  way a standalone daemon does), `google.golang.org/grpc/test/bufconn`,
  `github.com/lightningnetwork/lnd/aezeed` + `golang.org/x/crypto/hkdf`
  (passkey seed derivation), `google.golang.org/genproto/.../errdetails`
  (`ErrorInfo` status detail read by `errmap.go`).
- **Depended on by**: host Go apps, `sdk/walletdk/mobile` (gomobile
  bindings; also has its own passkey pass-through), React Native / WASM
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
  `RPC.Gateway.Enabled` is forced `false` since the embedded path talks
  bufconn, not the public HTTP gateway; `RPCServiceRegistrars` and
  `UnaryServerInterceptors` are appended; `OOR` and `FeeEstimation`
  (including its nested `MempoolSpace`) are copied by value into fresh
  pointers). New reference-typed fields added to `darepod.Config`
  require matching clone logic — this has already caused two follow-up
  additions (`OOR`, then `FeeEstimation`/`MempoolSpace`), so check this
  function whenever `darepod.Config` gains a pointer/slice field.
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
- Every walletdk client connection (embedded `Start` and remote
  `Connect`) installs `errorReconstructInterceptor`
  (`grpc.WithChainUnaryInterceptor`, `connect_grpc.go`/`embedded.go`).
  It inspects a failed RPC's `status.Details()` for a
  `google.rpc.ErrorInfo` whose `Domain` is `walletdkrpc.FailureDomain`,
  and if the `Reason` matches a known `walletdkrpc.Reason*` constant,
  wraps the SDK sentinel (`fmt.Errorf("%w: %w", sentinel, err)`) around
  the original status error so both `errors.Is(err, ErrX)` and
  `status.FromError(err)` keep working. Unrecognized reasons/details
  pass the original error through unchanged — adding a new rejection
  reason requires updating `reasonToSentinel` in `errmap.go` (and the
  matching sentinel in `errors.go`) or callers cannot `errors.Is` it.
- `OpenWalletFromPasskey` derives the entire wallet seed via
  `HKDF-SHA256(passkeyPRFOutput)` with domain-separated info strings
  (`hkdfSeedInfo` for the 16-byte aezeed entropy, `hkdfDBKeyInfo` for the
  local DB password) — there is no WebAuthn ceremony in this package;
  the caller must run the platform WebAuthn PRF evaluation with a fixed,
  app-controlled salt that is identical on every device/call for a given
  wallet, or the same passkey derives a different seed and the wallet
  becomes unrecoverable. Inputs shorter than 32 bytes are rejected.
  Behavior branches on `Info.WalletState`: `WalletStateNone` imports a
  fresh wallet from the derived entropy, `WalletStateLocked` unlocks
  with the derived DB password, and `WalletStateReady`/`WalletStateSyncing`
  return an error (no way to confirm an already-open wallet matches the
  presented passkey).

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
</content>
</invoke>
