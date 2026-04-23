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

## Relationships

- **Depends on**: `chainsource` (implements `ChainBackend`), `wallet` (implements `BoardingBackend`).
- **Depended on by**: `darepod` (alternative to LND-backed wallet).

## Invariants

- UTXO enumeration queries Esplora directly rather than btcwallet's internal UTXO set, because btcwallet does not credit-mark outputs for non-default key scopes (m/1017').
- `Stop()` explicitly closes btcwallet's internal database to prevent resource leaks.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
