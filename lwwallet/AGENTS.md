# lwwallet

## Purpose

Lightweight in-process wallet using btcwallet for HD key management and Esplora
(mempool.space) for chain monitoring. Self-contained without external LND.
Implements `wallet.BoardingBackend`, `input.Signer` + MuSig2, and
`chainsource.ChainBackend`.

## Relationships

- **Depends on**: `chainsource` (implements `ChainBackend`), `wallet` (implements `BoardingBackend`).
- **Depended on by**: `darepod` (alternative to LND-backed wallet).
