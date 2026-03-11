# lndbackend

## Purpose

`BoardingBackend` implementation wrapping lndclient's WalletKitClient for key
derivation, taproot script import, and UTXO enumeration via LND.

## Relationships

- **Depends on**: `wallet` (implements `BoardingBackend`).
- **Depended on by**: `darepod` (LND-backed wallet mode).
