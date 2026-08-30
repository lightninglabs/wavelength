# chainbackends

## Purpose

Concrete implementations of the `chainsource.ChainBackend` interface. Provides
`LNDBackend` wrapping lnd's chainntnfs for real-time chain notifications, fee
estimation, and optional v3 package relay via a pluggable `PackageSubmitter`.

## Key Types

- `LNDBackend` — Full-node backend wrapping lnd's chain notification and fee
  estimation interfaces. Accepts an optional `PackageSubmitter` for v3 CPFP
  package relay (set via `SetPackageSubmitter`). Also implements
  `chainsource.TxRemover` via `RemoveTx`.
- `TxBroadcaster` — Interface over transaction broadcasting (wraps
  lndclient.WalletKitClient or in-process lnd). Carries
  `PublishTransaction(ctx, tx, label)` and `RemoveTransaction(ctx, txid)`;
  the latter abandons a transaction (and its descendants) so the wallet stops
  rebroadcasting it from its unconfirmed queue (wavelength#609).
- `PackageSubmitter` — Optional interface for v3 package relay:
  `SubmitPackage(ctx, parents, child, maxFeeRate)`. Used by backends that need
  a direct bitcoind path for atomic parent+child submission; absent in
  environments that do not support package relay.
- `LndClientTxBroadcaster` — Implements `TxBroadcaster` using
  `lndclient.WalletKitClient`.
- `LndClientFeeEstimator` — Type alias for
  `chainfees.WalletKitEstimator`, backed by `lndclient.WalletKitClient` with
  a 15-second per-call timeout and last-good fallback semantics.
- `LndClientChainNotifier` / `LndClientChainNotifierConfig` — Implements
  `chainntnfs.ChainNotifier` using lndclient. Uses a 15-second registration
  timeout and goroutine-based forwarding to bridge lndclient's height-only
  block events to the full `chainntnfs` interface.
- `LNDBackendFromLndClientConfig` — Config struct for building an `LNDBackend`
  from lndclient services (notifier, wallet kit, chain kit).
- `NewLNDBackendFromLndClient(cfg)` — Factory constructing a full `LNDBackend`
  from an `LNDBackendFromLndClientConfig`.
- `PackageTxError` — Per-tx result error from a `SubmitPackage` response.
  Carries `Wtxid`, `Txid`, and raw `Reason`; unwraps to the mapped
  `rpcclient`-sentinel (via `rpcclient.MapRPCErr`) so callers can use
  `errors.Is` against typed sentinels (e.g. `rpcclient.ErrTxAlreadyKnown`,
  `rpcclient.ErrInsufficientFee`) instead of substring-matching reject
  strings.
- `NewPackageTxError(wtxid, txid, reason)` — Eagerly maps the reject reason to
  a typed sentinel at construction time.
- `WalkPackageTxErrors(err, fn)` — Walks both `Unwrap() error` and
  `Unwrap() []error` shapes to invoke `fn` for every `*PackageTxError` in a
  joined error tree. Use this instead of `errors.As` when all per-tx entries
  must be inspected (e.g. to distinguish parent-known vs. child-fee
  classification).
- `reorgSignalBufferSize = 8` (unexported, `lnd.go`) — depth of the buffered
  `Reorged` channel handed to each conf/spend registration. Reorg signals are
  coalescing (the consumer re-queries chain state), so the buffer only needs
  to be deep enough to keep the forwarder from stalling on a multi-block
  reorg burst, not to record every event.

## Reorg & Finality Forwarding

`RegisterConf` / `RegisterSpend` bridge lnd's notifier events into the
reorg-aware `chainsource` registration lifecycle. lnd signals a reorg by
firing `ConfirmationEvent.NegativeConf` (conf) or `SpendEvent.Reorg` (spend);
a single per-registration forwarder goroutine translates those into the
backend-agnostic `Reorged` channel while also forwarding `Confirmed`/`Spend`
and `Done`.

Because Go's `select` picks randomly among ready channels, the forwarder
routes every lifecycle fact through `chainsource.PositiveDoneOrder`
(`ObservePositive` / `ObserveReorg` / `ObserveDone`) so the causal
positive-before-`Done` contract survives the transport boundary. Delivery uses
`forwardValue`, which drops the send if the registration context was cancelled
first.

## Relationships

- **Depends on**: `chainsource` (implements `ChainBackend` interface),
  `chainfees` (fee estimator types).
- **Depended on by**: `waved` (instantiates `LNDBackend` and wires a
  `PackageSubmitter` from operator config), `systest` (constructs
  `LNDBackend` via lndclient for system tests), `btcwbackend` / `lwwallet` /
  `txconfirm` (reuse `PackageSubmitter`, `PackageTxError`, and
  `WalkPackageTxErrors` to classify per-tx package-relay results).
- **Sends → `chainsource`** (as registration channel payloads):
  `BlockRegistration`, `ConfRegistration`, `SpendRegistration` — each
  carrying `Confirmed`/`Spend`, `Reorged`, and `Done`.
- **Receives ← `chainsource`** (via the actor, on behalf of `unroll`):
  `RemoveTxRequest` → `LNDBackend.RemoveTx`. Backends without a
  rebroadcasting wallet do not implement `TxRemover` and the request is a
  no-op for them.

## Invariants

- `LNDBackend` requires an lnd instance (local or remote via lndclient).
- Provides real-time notifications via lnd's chainntnfs package.
- `PackageSubmitter` is optional; package-capable backends return an error
  from `SubmitPackage` when no submitter is set. `waved` selects one at
  startup: an explicit `waved.Config.PackageSubmitter` wins (bitcoind flags
  inject `chainbackends/bitcoindrpc.PackageSubmitter`, and the itest harness
  sets the same field); otherwise, for an LND wallet it falls back to
  `chainbackends/lndsubmitter.New(lndSvc.WalletKit)` as the default.
- `LndClientChainNotifier` enforces a 15-second timeout on registration to
  prevent hanging under LND block load (`lndRegistrationTimeout`).
- One forwarder goroutine per conf/spend registration owns ordering. Never
  emit `Confirmed`/`Spend`, `Reorged`, and `Done` from separate goroutines —
  the `PositiveDoneOrder` state machine is what makes the ordering contract
  hold, and it is not safe for concurrent use.
- A `Canceled` status from lnd is only demoted to `Debug` when the owning
  registration context is *also* done (`isBlockEpochShutdownError`). Round
  completion stops each VTXO's block subscription, so an in-flight block can
  lose that race; that is expected shutdown, not an operator-actionable event.
  A `Canceled` status from a still-live subscription — and every other
  notifier or block-hash failure — stays at `Warn`.
- Log messages use canonical txid strings (not reversed byte slices).

## Deep Docs

- [chainbackends/doc.go](doc.go) — Package overview.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
