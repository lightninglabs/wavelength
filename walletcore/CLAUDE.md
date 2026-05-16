# walletcore

## Purpose

Shared btcwallet wrapping and cycle-breaking shared types used by both
lwwallet (Esplora-backed) and btcwbackend (neutrino-backed) wallet
implementations. Extracts common HD key management, signing, address
generation, balance operations, and the canonical `LockID` / `Utxo` /
`OutputLeaser` types that would otherwise create import cycles between
`wallet`, `txconfirm`, and backend packages.

## Key Types

- `Wallet` — Core wallet struct embedding `input.Signer`. Provides key
  derivation, P2TR address generation, balance queries, and UTXO listing.
  Also implements `proofkeys.Backend` (via `ProofSigner` method).
  Chain-specific implementations embed this to satisfy `round.ClientWallet`.
- `BoardingBackendBase` — Shared boarding functionality: taproot script
  import under BIP86 scope, imported address tracking for UTXO filtering,
  and HD key derivation. Chain-specific adapters embed this and add
  `ListUnspent`, `GetTransaction`, `GetBlock`.
- `Config` — Base configuration (seed, chain params, recovery window, DB
  dir, logger) shared by all btcwallet-backed wallet backends.
- `LockID` — `[32]byte` caller-scoped identifier for wallet output leases.
  Lives here (not in `wallet`) so packages that would otherwise import-cycle
  with `wallet` (notably `txconfirm`) can reference a single shared type.
  `wallet.LockID` is a type alias for this.
- `Utxo` — Simplified unspent output view (Outpoint, PkScript, Amount,
  Confirmations) used for boarding UTXO detection and fee-input selection.
  `wallet.Utxo` is a type alias for this.
- `OutputLeaser` — Interface for UTXO output leasing (`LeaseOutput` /
  `ReleaseOutput`). Lives here to break the `wallet` ↔ `txconfirm` import
  cycle. `wallet.OutputLeaser` is a type alias for this.

## Relationships

- **Depends on**: `build` (context logger extraction), `proofkeys` (implements
  Backend), `indexer` (SchnorrSigner interface).
- **Depended on by**: `lwwallet` (embeds Wallet + BoardingBackendBase),
  `btcwbackend` (embeds Wallet + BoardingBackendBase), `darepod` (proof key
  backend), `txconfirm` (uses `LockID`, `Utxo`, `OutputLeaser` to avoid
  importing `wallet`), `wallet` (re-exports via type aliases).

## Invariants

- Taproot scripts must be imported under `KeyScopeBIP0086` (not the custom
  chain key scope), because btcwallet's block processing skips credit
  tracking for non-default scopes (`chainntfns.go:IsDefaultScope` check).
- `ImportedAddrs` is in-memory only; repopulated on restart from the database
  by the wallet actor's `handleStartupRecovery`.
- `WalletPassphrase` is shared across all wallet backends for both
  `PrivatePass` and `PublicPass`.
- `LockID`, `Utxo`, and `OutputLeaser` are the **canonical** declarations;
  `wallet.LockID`, `wallet.Utxo`, and `wallet.OutputLeaser` are type aliases
  pointing here. Code that would cycle through `wallet` must import
  `walletcore` directly.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
