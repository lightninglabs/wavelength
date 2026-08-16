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
- Start-block discovery (`start_block.go`) — resolves where a new or
  restored wallet begins scanning from the Esplora address index instead of
  from the seed birthday, and writes it into `waddrmgr` as a *verified*
  birthday block before `BtcWallet.Start()`. Entry point:
  `Wallet.stampStartBlock`. Left to itself btcwallet binary searches block
  timestamps (`locateBirthdayBlock`), which bottoms out at genesis for any
  birthday older than the chain and then walks every block to the tip.
  Discovery instead derives each active key scope's receive/change scripts
  through btcwallet's own scoped key managers, probes `/scripthash/:h` under
  the configured gap limit, and takes the earliest confirmed height.
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
  `BtcWallet.FinalizePsbt` under `DefaultAccountName`; used by the waved
  unroll sweep adapter since lwwallet has no gRPC surface.

## Relationships

- **Depends on**: `walletcore` (shared HD key mgmt, signing, boarding base —
  also used by `btcwbackend`), `chainsource` (implements `ChainBackend`),
  `wallet` (implements `BoardingBackend`), `chainbackends` (typed
  `PackageTxError` for package-relay results).
- **Depended on by**: `waved` (alternative to LND-backed wallet), `sdk`
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
- `scriptHashHex` hex-encodes the SHA256 digest in its natural byte order.
  Esplora's REST API differs from the Electrum wire protocol here, which
  reverses it, and a wrong order fails silently because the API answers an
  unknown script hash with an empty result rather than an error.
- Start-block discovery only ever stamps a wallet that has no birthday block
  at all, and never moves the sync cursor backwards. Once btcwallet has
  recorded a birthday block it owns the cursor.
- A discovery failure is non-fatal: the wallet still starts and btcwallet
  falls back to its own timestamp search, just slowly. Discovery refuses to
  stamp rather than stamping on partial information, because a short walk
  finds fewer used scripts, which raises the height it reports and moves the
  stamp toward the tip: the direction that loses funds.
- Start-block discovery trusts the Esplora index. That holds for a single
  self-consistent instance, but a load-balanced public endpoint can serve a
  current tip and a still-syncing script index from different backends, and
  partial history is indistinguishable from an unused script. A restore in
  that window stamps above its own funds permanently, since the stamp is
  written verified. Point `EsploraURL` at an endpoint you control.
- `FilterBlocks` and `Rescan` also trust successful script-history responses
  to be complete. Errors, truncated pagination, and block-identity conflicts
  fall back to full block scans, but an empty response is indistinguishable
  from a script with no activity. If the tip is current while the script index
  lags, btcwallet can advance `PutSyncedTo` past unindexed activity
  permanently. Avoid load-balanced public endpoints; point `EsploraURL` at a
  self-consistent instance you control.
- UTXO enumeration queries Esplora directly rather than btcwallet's internal
  UTXO set, because btcwallet does not credit-mark non-default scope outputs.
- `Stop()` explicitly closes btcwallet's internal database to prevent resource
  leaks.
- **Under `js && wasm`, btcwallet's SQL walletdb follows the host**
  (`walletdb_wasm.go`, `internal/sqlbase`). `internal/wasmhost.SQLiteVFS()`
  picks `nodefs` or `opfs`: under Node the store is `wallet.db` inside the
  caller's `dbDir` (the directory already distinguishes one wallet from
  another, and a real path is findable on disk), while a browser maps `dbDir`
  to a stable origin-local OPFS name via `wasmWalletDBFileName`. The DSN sets
  `require_persistent=true` because the wallet's seed and key state have no
  second copy anywhere — not starting is strictly better than an in-memory
  substitute. `journal_mode=WAL` survives only because of
  `locking_mode=EXCLUSIVE`: no wasm VFS implements `xShmMap`, so this is
  SQLite's WAL mode for hosts without shared memory, which keeps the WAL index
  on the heap and requires an already-exclusive connection. The journal mode
  travels as its own DSN key rather than in the pragma list, because only that
  route is checked against the mode SQLite actually ended up in — so dropping
  the exclusive lock fails the open instead of silently moving the wallet onto
  the rollback journal.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
