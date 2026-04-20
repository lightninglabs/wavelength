# lndbackend

## Purpose

`BoardingBackend` and `ProofKeyBackend` implementations wrapping lndclient's
WalletKitClient for key derivation, taproot script import, UTXO enumeration,
and proof key signing via LND.

## Key Types

- `BoardingBackend` — Struct holding `walletKit lndclient.WalletKitClient` and
  `chainKit lndclient.ChainKitClient`. Implements `wallet.BoardingBackend`.
  `GetTransaction` returns `*wallet.TxInfo`; `GetBlock` fetches raw blocks via
  `chainKit`. `WalletKit()` exposes the underlying `lndclient.WalletKitClient`
  for callers that need operations not covered by the `BoardingBackend`
  interface.
- `ProofKeyBackend` — Implements `proofkeys.Backend` for LND-backed key
  derivation and Schnorr proof signing. Wraps `walletKit` for `DeriveKey`,
  `DeriveNextKey`, and produces `indexer.SchnorrSigner` instances.

## Relationships

- **Depends on**: `wallet` (implements `BoardingBackend`), `proofkeys`
  (implements `Backend`), `indexer` (SchnorrSigner interface).
- **Depended on by**: `darepod` (LND-backed wallet mode).

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
