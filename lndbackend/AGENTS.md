# lndbackend

## Purpose

LND chain backend integration providing chain source queries (fee estimation,
block/conf/spend notifications) and wallet controller operations (key
derivation, signing, UTXO management) for the server.

## Key Types

- `ChainSource` — Chain backend implementation backed by LND RPCs.
- `LndWalletController` — Wallet operations (signing, key management) via LND.
- `NewLndHeaderVerifier` — Returns a `proof.HeaderVerifier` that validates block headers against LND's chain backend via `ChainKit.GetBlockHash`. Used for TxProof SPV validation of boarding inputs.

## Relationships

- **Depends on**: `rounds` (chain query interfaces).
- **Depended on by**: root `darepo` (wiring as concrete backend).

## Invariants

- LND connection must be established and healthy before round operations begin.
- Wallet operations must use the correct key scope for Ark-specific derivations.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
