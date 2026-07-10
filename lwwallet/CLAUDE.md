# lwwallet

## Purpose

Lightweight in-process Bitcoin wallet backed by LND's btcwallet and an
Esplora/mempool.space chain backend. Self-contained without an external LND
node. Implements `wallet.BoardingBackend`, `input.Signer` + MuSig2, and
`chainsource.ChainBackend`. Shares HD key management, signing, and boarding
base logic with the neutrino-backed `btcwbackend` sibling via the extracted
`walletcore` package.

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
- `BoardingBackendAdapter` — Embeds `walletcore.BoardingBackendBase` for
  shared key derivation/script import; implements `wallet.BoardingBackend`
  and `wallet.OutputLeaser`. Queries Esplora directly for UTXOs (bypasses
  btcwallet's UTXO tracking because btcwallet skips credit marking for
  non-default key scopes like m/1017'). `LeaseOutput`/`ReleaseOutput` forward
  to btcwallet's native lock table.
- `Wallet` — Embeds `walletcore.Wallet` for shared btcwallet operations, adding
  the Esplora chain source. `WaitForSync(ctx)` blocks until btcwallet's
  internal height catches the Esplora tip, closing the race between the chain
  backend actor and btcwallet's asynchronous block processing pipeline (polls
  at 50ms). `FinalizePsbtDirect(packet)` signs and finalizes a PSBT via
  `BtcWallet.FinalizePsbt` under `DefaultAccountName`; used by the darepod
  unroll sweep adapter since lwwallet has no gRPC surface.

## Relationships

- **Depends on**: `walletcore` (shared HD key mgmt, signing, boarding base —
  also used by `btcwbackend`), `chainsource` (implements `ChainBackend`),
  `wallet` (implements `BoardingBackend`), `chainbackends` (typed
  `PackageTxError` for package-relay results).
- **Depended on by**: `darepod` (alternative to LND-backed wallet), `sdk`
  (embedded-wallet config references).

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

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
