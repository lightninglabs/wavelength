# lwwallet

## Purpose

Lightweight in-process Bitcoin wallet backed by LND's btcwallet for HD key
management and a shared Esplora/mempool.space chain backend. Self-contained
without an external LND node. Implements `wallet.BoardingBackend`,
`input.Signer` + MuSig2, and `chainsource.ChainBackend`.

## Key Types

- `TipPoller` — Single source of truth for the chain tip. One goroutine polls
  Esplora at a configurable interval; when the tip advances it walks each new
  height, fetches hash + header, and fans out `TipBlock` events to all
  subscribers via the embedded `EventServer`. It also detects chain
  reorganizations — both same-height hash drift (a block at height N replaced
  by a different block at the same height) and deeper reorgs (the first new
  height's `PrevBlock` header field does not point at the cached tip hash) —
  and fans `ReorgEvent` updates out on a sibling `EventServer`. A unified
  `ChainEvent` stream (reachable via `SubscribeChain` /
  `BestBlockAndSubscribeChain`) delivers both event types in producer
  order on a single channel for consumers that require strict
  reorg-before-replacement-tip ordering. A bounded height→hash history
  map (`historySize` entries, default `DefaultHashHistorySize=100`)
  is seeded back to `tip - historySize + 1` at `Start` so reorg
  walk-back can resolve old-chain hashes from the cache rather than
  terminating early on the first uncached height. Constructors:
  `NewTipPoller(esplora, pollInterval, logger)` (default history) and
  `NewTipPollerWithConfig(esplora, pollInterval, historySize, logger)`.
  Key methods: `Start()`, `Stop()`, `BestBlock()`, `Subscribe()`,
  `SubscribeReorgs()`, `SubscribeChain()`, `BestBlockAndSubscribe()`,
  `BestBlockAndSubscribeAll()`, and `BestBlockAndSubscribeChain()`.
- `TipBlock` — Event emitted per new block: `Height`, `Hash`, and the
  `*esploraBlock` header (pre-fetched so subscribers avoid a second Esplora
  round-trip).
- `TipSubscription` — Typed alias `Subscription[*TipBlock]` returned by
  `TipPoller.Subscribe`. Cancel via `Cancel()`.
- `ReorgEvent` — Event emitted when the poller observes that one or more
  previously broadcast blocks are no longer on the canonical chain. Carries
  `ForkHeight`, `Disconnected []chainhash.Hash` (ascending), and
  `Connected []*TipBlock` (ascending). The poller delivers `ReorgEvent`
  BEFORE fanning the connected blocks out on the standard `TipBlock` stream
  so consumers can mark registrations dirty before the re-confirmation
  arrives.
- `ReorgSubscription` — Typed alias `Subscription[*ReorgEvent]` returned by
  `TipPoller.SubscribeReorgs`.
- `EventServer[T]` — Generic wrapper around LND's `subscribe.Server` that
  delivers typed events. `Subscribe()` returns a `Subscription[T]` that
  converts untyped `interface{}` updates to `T` on a per-subscriber goroutine.
- `Subscription[T]` — Typed subscription handle with `Updates() <-chan T`,
  `Quit() <-chan struct{}`, and idempotent `Cancel()`.
- `ChainBackend` — Implements `chainsource.ChainBackend` by subscribing to
  the shared `TipPoller`'s unified `ChainEvent` stream. A single
  handler goroutine processes reorg and tip events in producer order:
  on each `ReorgEvent` it walks active conf/spend registrations whose
  last delivered positive event names a disconnected block hash, fires
  their `Reorged` channel, resets state to `stateWatching`, and
  re-checks status against the new chain; on each `TipBlock` it
  dispatches block-epoch notifications and runs the broad re-check.
  Single-goroutine dispatch guarantees a `ReorgEvent` is fully
  processed before any post-reorg block-epoch can drive chainsource
  finality synthesis on the replacement chain. The `Done` channel on
  each returned registration is allocated but never written by the
  backend — the chainsource `ConfActor` / `SpendActor` synthesizes
  `Done` at its configured `FinalityDepth` using block epochs the
  backend already delivers. Constructor:
  `NewChainBackend(esplora, pollInterval, logger)` (owns its own
  TipPoller) or `NewChainBackendWithPoller(esplora, tipPoller, logger)`
  (shares an externally managed poller).
- `EsploraClient` — HTTP REST client for the Esplora/mempool.space API.
  Constructor: `NewEsploraClient(baseURL, logger)`. Hash-addressed responses
  (transactions, blocks, headers) are cached in LRU caches bounded by
  cumulative serialized byte size (see `esplora_cache.go`). Mutable live data
  (tip height, UTXOs, fee estimates) is never cached. Cache integrity: every
  response is verified to hash to the requested key before insertion.
- `EsploraChainService` — `chain.Interface` adapter over `EsploraClient`,
  driven by a shared `TipPoller`. Feeds btcwallet's internal address-credit
  pipeline. Subscribes to the unified `ChainEvent` stream via
  `BestBlockAndSubscribeChain`; on each `ReorgEvent` it emits
  `chain.BlockDisconnected` notifications for the disconnected hashes
  (newest height first), then processes the replacement chain's tip
  events as `chain.BlockConnected`. The unified stream guarantees
  `BlockDisconnected` lands on btcwallet's notification queue before
  any `BlockConnected` for the new chain — without this, btcwallet's
  `disconnectBlock` path would refuse the rollback because the cached
  hash at the affected height would already be overwritten.
  Constructor: `NewEsploraChainService(esplora, tipPoller, logger)`.
- `BoardingBackendAdapter` — Implements `wallet.BoardingBackend` and
  `wallet.OutputLeaser`. Queries Esplora directly for UTXOs (bypasses
  btcwallet's UTXO tracking because btcwallet skips credit marking for
  non-default key scopes like m/1017'). `LeaseOutput`/`ReleaseOutput` forward
  to btcwallet's native lock table.
- `Wallet.WaitForSync(ctx)` — Blocks until btcwallet's internal height catches
  the Esplora tip, closing the race between the chain backend actor and
  btcwallet's asynchronous block processing pipeline. Polls at 50ms.
- `Wallet.FinalizePsbtDirect(packet)` — Signs and finalizes a PSBT via
  `BtcWallet.FinalizePsbt` under `DefaultAccountName`. Used by the darepod
  unroll sweep adapter since lwwallet has no gRPC surface.

## Relationships

- **Depends on**: `chainsource` (implements `ChainBackend`), `wallet`
  (implements `BoardingBackend`).
- **Depended on by**: `darepod` (alternative to LND-backed wallet).

## Invariants

- Exactly one `TipPoller` goroutine drives both `EsploraChainService` and
  `ChainBackend`; neither polls Esplora independently.
- `BestBlockAndSubscribe` / `BestBlockAndSubscribeAll` holds `TipPoller.mu`
  across `{Subscribe + tip-read}` while the poll loop holds it across
  `{update tip + SendUpdate}`, ensuring no tip event is missed or duplicated
  on subscribe.
- Same-height reorgs ARE detected: each poll cycle compares the live hash at
  the cached tip height against the cached hash and emits a `ReorgEvent` on
  mismatch. The previous limitation ("same-height reorgs invisible until the
  chain advances") is closed.
- Conf/spend registrations are multi-shot reorg-aware: they are not deleted
  after the first positive event, the returned `Reorged` channel fires when
  a previously delivered confirmation/spend is reorged out, and a fresh
  `Confirmed`/`Spend` may fire on the new canonical chain. `Done` is
  synthesized at the chainsource actor layer from block epochs, not by the
  backend.
- Reorg detection for registrations whose last delivered positive event
  references a block older than the poller's seeded hash history falls
  back to a canonical re-query of the live chain state. The fast path
  (cached block hash in `ReorgEvent.Disconnected`) covers in-window
  reorgs; the canonical re-query covers deeper reorgs where the cached
  hash was pruned from `recentHashes` or the registration delivered
  against a block below the seeded window on a fresh poller. Both paths
  fire `Reorged` followed by a fresh `Confirmed`/`Spend` if the chain
  still carries the watched event under a different anchor.
- The tip poller aborts the current poll cycle on raw-block-header fetch
  failure rather than optimistically advancing. The raw 80-byte header
  is the only carrier of `PrevBlock`, which is the continuity check that
  catches a reorg crossing the boundary at the old tip; falling through
  on a transient fetch flake would permanently hide that reorg by
  caching the new tip hash without ever emitting a `ReorgEvent`. The
  next poll tick retries.
- The poller exposes a unified `ChainEvent` stream (`SubscribeChain` /
  `BestBlockAndSubscribeChain`) that delivers reorg and tip updates on
  a single producer-ordered channel. `ChainBackend` and
  `EsploraChainService` both subscribe to this stream rather than the
  separate `events` / `reorgs` streams so a `ReorgEvent` is observed
  before the subsequent `TipBlock` events for the replacement chain.
  Strict ordering is required: btcwallet's `disconnectBlock` rejects
  rollback if the cached hash at that height has been overwritten by a
  stale `BlockConnected`, and chainsource finality synthesis depends on
  registrations being reset before block-epoch driven re-checks run on
  the replacement chain.
- The poller seeds `recentHashes` at `Start` by walking back
  `historySize - 1` heights from the initial tip. Without the seed a
  reorg whose disconnected range extends below the seeded tip but
  within `historySize` would terminate walk-back at the first
  uncached height, producing a `ReorgEvent` whose `Disconnected` list
  carries only the tip-boundary hash and starving the chain.Interface
  adapter of the per-height `BlockDisconnected` events btcwallet needs
  to roll back its sync state.
- LRU caches only hold immutable, hash-addressed data; a verified hash prevents
  a compromised Esplora endpoint from injecting arbitrary cache entries.
- UTXO enumeration queries Esplora directly rather than btcwallet's internal
  UTXO set, because btcwallet does not credit-mark non-default scope outputs.
- `Stop()` explicitly closes btcwallet's internal database to prevent resource
  leaks.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
