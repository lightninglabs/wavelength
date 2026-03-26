# lwwallet

## Purpose

Lightweight in-process wallet using btcwallet for HD key management and Esplora
(mempool.space) for chain monitoring. Self-contained without external LND.
Implements `wallet.BoardingBackend`, `input.Signer` + MuSig2, and
`chainsource.ChainBackend`.

## Key Types

- `BoardingBackendAdapter` — Implements `wallet.BoardingBackend`. Queries Esplora directly for UTXOs (bypasses btcwallet's UTXO tracking because btcwallet skips credit marking for non-default key scopes like m/1017').
- `GetTransaction` / `GetBlock` — Methods on `BoardingBackendAdapter` for fetching raw tx/block data from Esplora, used for `TxProof` construction.

## Relationships

- **Depends on**: `chainsource` (implements `ChainBackend`), `wallet` (implements `BoardingBackend`).
- **Depended on by**: `darepod` (alternative to LND-backed wallet).

## Invariants

- UTXO enumeration queries Esplora directly rather than btcwallet's internal UTXO set, because btcwallet does not credit-mark outputs for non-default key scopes (m/1017').

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
