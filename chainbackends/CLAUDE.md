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
  a typed sentinel at construction time, and parses any replacement-policy fee
  constraints out of it.
- `ReplacementFeeConstraints` — Structured fee information Bitcoin Core reports
  when a replacement child cannot evict a conflicting transaction:
  `ConflictingFee` (total fee the replacement must beat),
  `AdditionalFeeDeficit` (extra fee needed for the incremental relay fee), and
  `ConflictingFeeRateSatPerVByte` (integer part of the highest conflicting
  feerate). Reached via `(*PackageTxError).ReplacementConstraints()`; lets
  `unroll`/`txconfirm` reprice a rejected sweep to a concrete target instead of
  guessing a multiplier.
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
- Provides real-time notifications via lnd's chainntnfs package.
- `PackageSubmitter` is optional; package-capable backends return an error
  from `SubmitPackage` when no submitter is set. `waved` selects one at
  startup: an explicit `waved.Config.PackageSubmitter` wins (bitcoind flags
  inject `chainbackends/bitcoindrpc.PackageSubmitter`, and the itest harness
  sets the same field); otherwise, for an LND wallet it falls back to
  `chainbackends/lndsubmitter.New(lndSvc.WalletKit)` as the default.
- `LndClientChainNotifier` enforces a 15-second timeout on registration to
  prevent hanging under LND block load.
- Log messages use canonical txid strings (not reversed byte slices).
- `ReplacementFeeConstraints` fields are pointers and every one of them may be
  nil: the backend only reports the constraints its rejection path happens to
  mention, and the parse is best-effort over Bitcoin Core's diagnostic strings.
  Treat a nil field as "unknown", never as zero, and keep a fallback repricing
  path for the case where nothing was parsed.
- `ConflictingFeeRateSatPerVByte` is the *integer part* of the conflicting
  feerate. A replacement must pay at least one sat/vByte above it to be
  strictly greater; matching it exactly is still a rejection.
- **A `Canceled` status is only shutdown noise when the owning context is also
  done.** Round completion stops each VTXO's block subscription, and a block
  already in flight can race that cancellation, so `GetBlockHash` or the
  notifier error channel returns `Canceled` while the forwarder exits normally
  — paging an operator per terminated VTXO is pure noise.
  `isBlockEpochShutdownError(ctx, err)` demotes these to debug, but it returns
  false whenever `ctx.Err() == nil`. Do not simplify it to a bare
  `errors.Is(err, context.Canceled) || status.Code(err) == codes.Canceled`
  check: a `Canceled` arriving from a *live* subscription means the backend
  dropped it independently, which is actionable and must stay at warning. Every
  other notifier or block-hash failure remains a warning regardless of context
  state.

## Deep Docs

- [chainbackends/doc.go](doc.go) — Package overview.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
