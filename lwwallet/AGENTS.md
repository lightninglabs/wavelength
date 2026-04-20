# lwwallet

## Purpose

Lightweight in-process wallet using btcwallet for HD key management and Esplora
(mempool.space) for chain monitoring. Self-contained without external LND.
Implements `wallet.BoardingBackend`, `input.Signer` + MuSig2, and
`chainsource.ChainBackend`.

## Key Types

- `BoardingBackendAdapter` — Implements `wallet.BoardingBackend`. Queries Esplora directly for UTXOs (bypasses btcwallet's UTXO tracking because btcwallet skips credit marking for non-default key scopes like m/1017').
- `GetTransaction` / `GetBlock` — Methods on `BoardingBackendAdapter` for fetching raw tx/block data from Esplora. `GetTransaction` returns `*wallet.TxInfo` (containing tx, block hash, and block height).
- `ChainBackend.SubmitPackage` — Implements `chainsource.ChainBackend.SubmitPackage` by serializing transactions and POSTing to the Esplora `/txs/package` endpoint. Parents are sent in dependency order with the fee-paying child last.
- `EsploraClient.SubmitPackage` — Low-level HTTP call to `/txs/package`. Parses the `submitPackageResponse` JSON body and converts per-tx errors to a Go error. Empty response body (non-JSON Esplora variants) is treated as success.

## Relationships

- **Depends on**: `chainsource` (implements `ChainBackend`), `wallet` (implements `BoardingBackend`).
- **Depended on by**: `darepod` (alternative to LND-backed wallet).

## Invariants

- UTXO enumeration queries Esplora directly rather than btcwallet's internal UTXO set, because btcwallet does not credit-mark outputs for non-default key scopes (m/1017').
- `Stop()` explicitly closes btcwallet's internal database to prevent resource leaks.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
