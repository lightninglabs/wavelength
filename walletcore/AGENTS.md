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
- `LockID` — `[32]byte` caller-scoped output lease identifier. Lives in
  `walletcore` (not `wallet`) so packages like `txconfirm` that would create
  import cycles through `wallet` can depend on a single canonical type. Each
  subsystem derives a stable ID (e.g. `sha256("txconfirm")[:32]`). Re-exported
  as a type alias from `wallet`.
- `OutputLeaser` — Interface for wallet backends supporting output locking:
  `LeaseOutput(ctx, id, outpoint, expiry)` and `ReleaseOutput(ctx, id,
  outpoint)`. Shape matches btcwallet and lndclient WalletKit directly.
  Re-exported as a type alias from `wallet`.
- `Utxo` — Simplified UTXO representation (outpoint, pkScript, amount,
  confirmations) for boarding-UTXO detection and fee-input selection.

## Relationships

- **Depends on**: `build` (context logger extraction), `proofkeys` (implements Backend), `indexer` (SchnorrSigner interface).
- **Depended on by**: `lwwallet` (embeds Wallet + BoardingBackendBase),
  `btcwbackend` (embeds Wallet + BoardingBackendBase), `darepod` (proof key
  backend), `txconfirm` (LockID + OutputLeaser + Utxo without cycling through
  wallet), `wallet` (re-exports LockID and OutputLeaser as type aliases).

## Invariants

- Taproot scripts must be imported under `KeyScopeBIP0086` (not the custom chain key scope), because btcwallet's block processing skips credit tracking for non-default scopes (`chainntfns.go:IsDefaultScope` check).
- `ImportedAddrs` is in-memory only; repopulated on restart from the database by the wallet actor's `handleStartupRecovery`.
- `WalletPassphrase` is shared across all wallet backends for both `PrivatePass` and `PublicPass`.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
