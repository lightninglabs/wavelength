# lwwallet

## Purpose

Lightweight in-process Bitcoin wallet backed by LND's btcwallet for HD key
management and a shared Esplora/mempool.space chain backend. Self-contained
without an external LND node. Implements `wallet.BoardingBackend`,
`input.Signer` + MuSig2, and `chainsource.ChainBackend`. Builds natively
(bbolt-backed wallet DB) and for the browser (`js`/`wasm`, OPFS-backed SQLite
wallet DB) from the same `Wallet`/`Config` surface.

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
- `WalletExists(cfg)` — Reports whether a wallet database already exists for
  `cfg.ChainParams`/`RecoveryWindow`/`DBDir`, so callers can pick the create
  (seed) or open (password-only) path before calling `New`. `ErrWalletNotFound`
  and `ErrWalletExists` are the sentinel errors `New` returns when the
  supplied seed and on-disk state disagree.
- `newWalletLoaderOptions(cfg)` / `walletExists(cfg)` — Build-tag-gated pair
  giving `New` its `btcwallet.LoaderOption`s and existence probe.
  `walletdb_native.go` (`!js || !wasm`) opens the local bbolt DB via
  `LoaderWithLocalWalletDB`. `walletdb_wasm.go` (`js && wasm`) opens an
  OPFS-backed SQLite `walletdb.DB` (via `internal/sqlbase` +
  `go-wasmsqlite`) and adopts it with `LoaderWithExternalWalletDB`,
  deriving a stable per-`DBDir` OPFS file name.

## Relationships

- **Depends on**: `chainsource` (implements `ChainBackend`), `wallet`
  (implements `BoardingBackend`).
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
- `New` refuses to proceed unless the seed and on-disk database state agree:
  a nil `Config.Seed` requires an existing database (else `ErrWalletNotFound`)
  and a non-nil seed requires none exists yet (else `ErrWalletExists`).
  btcwallet itself silently generates a random seed or silently ignores a
  supplied one in these cases, which is unacceptable for a funds-bearing
  wallet.
- If `BtcWallet.Start` fails after `New` has already opened the wallet
  database, the rollback path closes it again; otherwise a retried unlock
  (e.g. after a wrong passphrase) deadlocks on the database's exclusive lock
  (bbolt flock natively, OPFS EXCLUSIVE locking in browser builds).

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
