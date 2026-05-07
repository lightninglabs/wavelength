# lwwallet

## Purpose

Lightweight in-process wallet using btcwallet for HD key management and Esplora
(mempool.space) for chain monitoring. Self-contained without external LND.
Implements `wallet.BoardingBackend`, `input.Signer` + MuSig2, and
`chainsource.ChainBackend`.

## Key Types

- `BoardingBackendAdapter` — Implements `wallet.BoardingBackend` and `wallet.OutputLeaser`. Queries Esplora directly for UTXOs (bypasses btcwallet's UTXO tracking because btcwallet skips credit marking for non-default key scopes like m/1017'). `LeaseOutput`/`ReleaseOutput` forward to btcwallet's native lock table, casting `wallet.LockID` → `wtxmgr.LockID`.
- `GetTransaction` / `GetBlock` — Methods on `BoardingBackendAdapter` for fetching raw tx/block data from Esplora. `GetTransaction` returns `*wallet.TxInfo` (containing tx, block hash, and block height).
- `ChainBackend` — Implements `chainsource.ChainBackend` via Esplora polling.
  Constructor: `NewChainBackend(esplora, pollInterval, logger)`. Maintains
  registration maps (`confRegs`, `spendRegs`, `blockRegs`) protected by a
  mutex. A `poll()` loop checks for new blocks and processes pending
  confirmation/spend registrations on each tick.
- `EsploraClient` — HTTP REST client for the Esplora/mempool.space API.
  Constructor: `NewEsploraClient(baseURL, logger)`. Provides methods for tip
  height, block hash, tx status, raw tx/block, script UTXOs, outspend queries,
  fee estimates, transaction broadcast, and package submission.
- `EsploraChainService` — btcwallet `chain.Interface` adapter over
  `EsploraClient`, used to give btcwallet an Esplora-backed chain source for
  address-credit marking.
- `Wallet.FinalizePsbtDirect(packet)` — Signs and finalizes a PSBT via
  `BtcWallet.FinalizePsbt` under `DefaultAccountName`. The lwwallet
  equivalent of LND's `WalletKit.FinalizePsbt`, used by the darepod unroll
  sweep adapter (`lwUnrollWallet.FinalizePsbt`) since lwwallet has no gRPC
  surface.
- `Wallet.WaitForSync(ctx)` — Blocks until btcwallet's internal synced-to
  height catches the Esplora tip. Closes the race between the chain backend
  actor (which fires confirmations off the Esplora poll) and btcwallet's
  asynchronous block processing pipeline fed by `EsploraChainService`. Polls
  at 50ms until match or ctx cancellation. Without this gate,
  `ListUnspentWitness` can return stale results right after a confirmation
  event because the two pipelines poll Esplora independently.
- `TipPoller` — Central tip-polling goroutine: the single source of truth for
  the lwwallet chain tip. Polls Esplora at a configurable interval, walks
  `oldHeight+1 → newHeight` on each advance, resolves hash + header for each
  block, and fans out `*TipBlock` events to all active subscribers. Lets the
  `ChainBackend` and `EsploraChainService` share one Esplora call cadence.
- `TipBlock` — Block event emitted by `TipPoller`: `Height`, `Hash`, and
  `Header` (JSON Esplora block header) so subscribers avoid a second fetch.
- `TipSubscription` — Typed alias for `Subscription[*TipBlock]`; returned by
  `TipPoller.Subscribe`.
- `EventServer[T]` — Generic typed event server wrapping lnd's
  `subscribe.Server`. Handles broadcaster concurrency, per-client unbounded
  queues, and idempotent `Cancel`. Used internally by `TipPoller`.
- `Subscription[T]` — Typed subscription handle; exposes `Updates() <-chan T`
  and `Cancel()`. Owned by a translator goroutine that type-asserts
  `subscribe.Server`'s `interface{}` updates.
- `NewChainBackendWithPoller` — Constructor that accepts a pre-created
  `*TipPoller` so callers can share the poller across `ChainBackend` and
  `EsploraChainService`.
- Esplora LRU caches (`esplora_cache.go`) — Four LRU caches (by byte budget)
  keyed on content-hash for immutable Esplora data: `txCacheCapacity` (5 MiB
  for raw transactions), `rawBlockCacheCapacity` (20 MiB for raw blocks),
  `rawHeaderCacheCapacity` (1 MiB for raw block headers),
  `blockHeaderCacheCapacity` for decoded headers. Entries are admitted only
  after hash verification so a misbehaving Esplora endpoint cannot pin
  arbitrary data.

## Relationships

- **Depends on**: `chainsource` (implements `ChainBackend`), `wallet` (implements `BoardingBackend`).
- **Depended on by**: `darepod` (alternative to LND-backed wallet).

## Invariants

- UTXO enumeration queries Esplora directly rather than btcwallet's internal UTXO set, because btcwallet does not credit-mark outputs for non-default key scopes (m/1017').
- `Stop()` explicitly closes btcwallet's internal database to prevent resource leaks.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
