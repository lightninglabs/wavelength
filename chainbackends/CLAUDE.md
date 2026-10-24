# chainbackends

## Purpose

Concrete implementations of the `chainsource.ChainBackend` interface. Provides
`LNDBackend` wrapping lnd's chainntnfs for real-time chain notifications, fee
estimation, and optional v3 package relay via a pluggable `PackageSubmitter`.

## Key Types

- `LNDBackend` — Full-node backend wrapping lnd's chain notification and fee
  estimation interfaces. Accepts an optional `PackageSubmitter` for v3 CPFP
  package relay (set via `SetPackageSubmitter`).
- `TxBroadcaster` — Interface over transaction broadcasting (wraps
  lndclient.WalletKitClient or in-process lnd).
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
  timeout and forwarding from lndclient's authenticated raw chainrpc client.
  Block events retain the upstream hash; no `GetBlockHash` lookup is needed.
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
  a typed sentinel at construction time, and eagerly parses any
  replacement-policy fee floors out of the reason string.
- `ReplacementFeeConstraints` — Structured fee floors Bitcoin Core reports when
  a replacement child cannot evict a conflicting transaction:
  `ConflictingFee` (total fee the replacement must beat),
  `AdditionalFeeDeficit` (extra fee needed for the incremental relay fee), and
  `ConflictingFeeRateSatPerVByte` (integer part of the highest conflicting
  feerate; a replacement must pay at least one sat/vByte above it). Retrieved
  via `PackageTxError.ReplacementConstraints()`, which returns a deep copy.
- `WalkPackageTxErrors(err, fn)` — Walks both `Unwrap() error` and
  `Unwrap() []error` shapes to invoke `fn` for every `*PackageTxError` in a
  joined error tree. Use this instead of `errors.As` when all per-tx entries
  must be inspected (e.g. to distinguish parent-known vs. child-fee
  classification).

## Relationships

- **Depends on**: `chainsource` (implements `ChainBackend` interface),
  `chainfees` (fee estimator types).
- **Depended on by**: `waved` (instantiates `LNDBackend` and wires a
  `PackageSubmitter` from operator config), `systest` (constructs
  `LNDBackend` via lndclient for system tests), `btcwbackend` / `lwwallet` /
  `txconfirm` (reuse `PackageSubmitter`, `PackageTxError`, and
  `WalkPackageTxErrors` to classify per-tx package-relay results).

## Invariants

- `LNDBackend` requires an lnd instance (local or remote via lndclient).
- Remote `BestBlock` queries use `ChainKit.GetBestBlock` on every read.
  Notification processing can trail the live chain; RPC errors propagate
  without falling back to that tip. In-process backends retain the notifier
  path. Confirmation and spend subscriptions use chainntnfs.
- `PackageSubmitter` is optional; package-capable backends return an error
  from `SubmitPackage` when no submitter is set. `waved` selects one at
  startup: an explicit `waved.Config.PackageSubmitter` wins (bitcoind flags
  inject `chainbackends/bitcoindrpc.PackageSubmitter`, and the itest harness
  sets the same field); otherwise, for an LND wallet it falls back to
  `chainbackends/lndsubmitter.New(lndSvc.WalletKit)` as the default.
- `LndClientChainNotifier` enforces a 15-second timeout on registration to
  prevent hanging under LND block load.
- Log messages use canonical txid strings (not reversed byte slices).
- **`ReplacementFeeConstraints` fields are individually optional and
  best-effort.** They are scraped from Bitcoin Core's human-readable
  replacement-policy diagnostics, so a nil field means "this backend did not
  report that constraint", never "the constraint is zero". The total-fee and
  incremental-relay messages hold across Core 28–31; the conflicting-feerate
  message only appears on Core 28–30, since cluster mempool replaced that
  check. Callers must degrade gracefully when a field is absent rather than
  treating the parse as authoritative, and a Core release that rewords these
  strings silently yields nil — never a wrong number.
- `RegisterBlocks` shares one lazy upstream subscription across all local
  subscribers, including confirmation/spend finality watches. The last
  cancellation or backend `Stop` joins upstream cleanup before another stream
  can start. Individual caller contexts only own their local queues.
- New block subscribers receive the latest observed tip. Live events keep
  upstream order without height-only deduplication. Reconnect uses the last
  observed hash/height cursor for lnd's connected-block catch-up. Explicit
  disconnect signals remain on confirmation/spend notification streams.
- Each subscriber has a queue of 10 epochs. Overflow closes only that queue;
  it never silently drops an individual event or stalls healthy subscribers.
  Chainsource actors reconnect closed registrations; consumers needing a full
  historical view must reconcile after an overflow or fresh registration.
- The chainntnfs registration API has no caller context. Remote stream setup
  is bounded to 15 seconds, so shutdown racing establishment can wait that
  long. Established stream cancellation joins its forwarding goroutine.
- `BestBlock` remains a live ChainKit query for remote connections, independent
  of the shared stream's cached notification tip.
- A `Canceled` stream error is shutdown noise only when the owning context is
  cancelled. `isBlockEpochShutdownError` keeps independent backend cancellation
  actionable; live stream errors remain warnings before the shared reconnect.

## Deep Docs

- [chainbackends/doc.go](doc.go) — Package overview.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
