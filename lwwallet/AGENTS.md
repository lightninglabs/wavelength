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
  subscribers via the embedded `EventServer`. Multiple downstream chain
  watchers share one poller cadence instead of polling independently.
  Constructor: `NewTipPoller(esplora, pollInterval, logger)`. Key methods:
  `Start()`, `Stop()`, `BestBlock()`, `Subscribe()`,
  `BestBlockAndSubscribe()` (atomic tip-read + subscribe to avoid missed
  events).
- `TipBlock` — Event emitted per new block: `Height`, `Hash`, and the
  `*esploraBlock` header (pre-fetched so subscribers avoid a second Esplora
  round-trip).
- `TipSubscription` — Typed alias `Subscription[*TipBlock]` returned by
  `TipPoller.Subscribe`. Cancel via `Cancel()`.
- `EventServer[T]` — Generic wrapper around LND's `subscribe.Server` that
  delivers typed events. `Subscribe()` returns a `Subscription[T]` that
  converts untyped `interface{}` updates to `T` on a per-subscriber goroutine.
- `Subscription[T]` — Typed subscription handle with `Updates() <-chan T`,
  `Quit() <-chan struct{}`, and idempotent `Cancel()`.
- `ChainBackend` — Implements `chainsource.ChainBackend` by subscribing to a
  shared `TipPoller`. On each `TipBlock` event it dispatches block epoch
  notifications and re-checks pending confirmation/spend registrations.
  Constructor: `NewChainBackend(esplora, pollInterval, logger)` (owns its own
  TipPoller) or `NewChainBackendWithPoller(esplora, tipPoller, logger)` (shares
  an externally managed poller).
- `EsploraClient` — HTTP REST client for the Esplora/mempool.space API.
  Constructor: `NewEsploraClient(baseURL, logger)`. Hash-addressed responses
  (transactions, blocks, headers) are cached in LRU caches bounded by
  cumulative serialized byte size (see `esplora_cache.go`). Mutable live data
  (tip height, UTXOs, fee estimates) is never cached. Cache integrity: every
  response is verified to hash to the requested key before insertion.
- `EsploraChainService` — `chain.Interface` adapter over `EsploraClient`,
  driven by a shared `TipPoller`. Feeds btcwallet's internal address-credit
  pipeline. Constructor: `NewEsploraChainService(esplora, tipPoller, logger)`.
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
- `Config.Birthday` — Seed creation time, forwarded to
  `btcwallet.Config.Birthday` so recovery rescans start from the seed's
  birthday instead of genesis.
- `newWalletLoaderOptions(cfg)` — Build-tag-split constructor for
  `btcwallet.LoaderOption`s, called from `Wallet.New`. See
  `walletdb_native.go`/`walletdb_wasm.go` below for the two implementations.

## Relationships

- **Depends on**: `chainsource` (implements `ChainBackend`), `wallet`
  (implements `BoardingBackend`). `walletdb_wasm.go` (browser build only,
  `js && wasm`) additionally depends on `internal/sqlbase` and
  `go-wasmsqlite` for an OPFS-backed SQLite walletdb; the native build
  (`walletdb_native.go`, `!js || !wasm`) has no such dependency and stores
  btcwallet's database as local bbolt files under `Config.DBDir`.
- **Depended on by**: `darepod` (alternative to LND-backed wallet).

## Invariants

- Exactly one `TipPoller` goroutine drives both `EsploraChainService` and
  `ChainBackend`; neither polls Esplora independently.
- `BestBlockAndSubscribe` holds `TipPoller.mu` across `{Subscribe + tip-read}`
  while the poll loop holds it across `{update tip + SendUpdate}`, ensuring no
  tip event is missed or duplicated on subscribe.
- Same-height reorgs are invisible until the chain advances to the next height
  (known limitation; acceptable for confirmation-target use cases).
- LRU caches only hold immutable, hash-addressed data; a verified hash prevents
  a compromised Esplora endpoint from injecting arbitrary cache entries.
- UTXO enumeration queries Esplora directly rather than btcwallet's internal
  UTXO set, because btcwallet does not credit-mark non-default scope outputs.
- `Stop()` explicitly closes btcwallet's internal database to prevent resource
  leaks.
- `newWalletLoaderOptions` is the only place that should choose between
  bbolt (native) and OPFS SQLite (wasm) storage; `Wallet.New` must stay
  build-tag agnostic and call it rather than constructing
  `btcwallet.LoaderOption`s directly.
- `walletdb_wasm.go`'s DB filename is derived from a hash of the normalized
  `Config.DBDir`, not the literal path, since OPFS is a flat origin-local
  namespace; callers must not rely on inspecting the OPFS filename to
  recover the original `DBDir`.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
