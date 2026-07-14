# proofkeys

## Purpose

Interface package defining wallet-managed key operations for daemon-owned
receive scripts and indexer proof generation. Acts as the abstraction boundary
between wallet backends and proof key derivation.

## Key Types

- `Backend` — Interface defining the wallet key derivation contract:
  `DeriveKey`, `DeriveNextKey`, and `ProofSigner`. Implemented by
  `walletcore.Wallet` and `lndbackend.ProofKeyBackend`.

## Relationships

- **Depends on**: `indexer` (SchnorrSigner interface for proof signing).
- **Depended on by**: `walletcore` (implements Backend), `lndbackend`
  (implements Backend via ProofKeyBackend), `waved` (consumes Backend for
  indexer proof operations).

## Invariants

- `Backend` must provide deterministic key derivation for the same locator
  inputs.
- `ProofSigner` binds to the exact key descriptor and produces Schnorr
  signatures for indexer proof-of-control.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
