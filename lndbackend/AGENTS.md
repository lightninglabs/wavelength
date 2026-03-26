# lndbackend

## Purpose

`BoardingBackend` implementation wrapping lndclient's WalletKitClient for key
derivation, taproot script import, and UTXO enumeration via LND.

## Key Types

- `BoardingBackend` — Struct holding `walletKit lndclient.WalletKitClient` and `chainKit lndclient.ChainKitClient`. Implements `wallet.BoardingBackend`.
- `GetTransaction` / `GetBlock` — Methods using `chainKit` for raw tx/block fetching (for `TxProof` construction, added by vtxo-owner-cosigner-split).

## Relationships

- **Depends on**: `wallet` (implements `BoardingBackend`).
- **Depended on by**: `darepod` (LND-backed wallet mode).

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
