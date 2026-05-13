# walletcore

## Purpose

Shared btcwallet wrapping used by both lwwallet (Esplora-backed) and btcwbackend
(neutrino-backed) wallet implementations. Extracts common HD key management,
signing, address generation, and balance operations that delegate to
btcwallet.BtcWallet regardless of the underlying chain source.

## Key Types

- `Wallet` — Core wallet struct embedding `input.Signer`. Provides key derivation, P2TR address generation, balance queries, and UTXO listing. Also implements `proofkeys.Backend` (via `ProofSigner` method). Chain-specific implementations embed this to satisfy `round.ClientWallet`.
- `BoardingBackendBase` — Shared boarding functionality: taproot script import under BIP86 scope, imported address tracking for UTXO filtering, and HD key derivation. Chain-specific adapters embed this and add `ListUnspent`, `GetTransaction`, `GetBlock`.
- `Config` — Base configuration (seed, chain params, recovery window, DB dir, logger) shared by all btcwallet-backed wallet backends.
- `LockID` — `[32]byte` caller-scoped output lease identifier. Lives here (not in `wallet`) so packages that would import-cycle with `wallet` — notably `txconfirm` — can use a single canonical type. `wallet.LockID` is a type alias pointing here.
- `Utxo` — Simplified UTXO representation (outpoint, pkscript, amount, confirmations) returned by `ListUnspent`. Lives here for the same import-cycle reason as `LockID`. `wallet.Utxo` is a type alias.
- `OutputLeaser` — Interface for UTXO output leasing (`LeaseOutput` / `ReleaseOutput`). Lives here so `txconfirm` can depend on it without going through `wallet`. `wallet.OutputLeaser` is a type alias.

## Relationships

- **Depends on**: `build` (context logger extraction), `proofkeys` (implements Backend), `indexer` (SchnorrSigner interface).
- **Depended on by**: `lwwallet` (embeds Wallet + BoardingBackendBase), `btcwbackend` (embeds Wallet + BoardingBackendBase), `darepod` (proof key backend), `wallet` (re-exports LockID / Utxo / OutputLeaser as type aliases), `txconfirm` (uses LockID / Utxo / OutputLeaser directly to avoid import cycle with wallet).

## Invariants

- Taproot scripts must be imported under `KeyScopeBIP0086` (not the custom chain key scope), because btcwallet's block processing skips credit tracking for non-default scopes (`chainntfns.go:IsDefaultScope` check).
- `ImportedAddrs` is in-memory only; repopulated on restart from the database by the wallet actor's `handleStartupRecovery`.
- `WalletPassphrase` is shared across all wallet backends for both `PrivatePass` and `PublicPass`.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
