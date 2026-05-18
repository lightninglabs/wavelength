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
- `LockID` — `[32]byte` caller-scoped UTXO lease identifier. Callers use distinct `LockID` values to prevent subsystems from interfering with each other's reservations.
- `OutputLeaser` — Interface for caller-scoped UTXO lease management: `LeaseOutput(ctx, id LockID, op wire.OutPoint, expiry time.Duration) (time.Time, error)` and `ReleaseOutput(ctx, id LockID, op wire.OutPoint) error`. The two-method shape matches `btcwallet` and `lndclient.WalletKit` so backends can delegate directly. Implemented by `btcwbackend`, `lndbackend`, and `lwwallet`; consumed by `txconfirm` and `wallet` for cross-subsystem UTXO reservation.
- `Utxo` — Confirmed wallet UTXO with outpoint, amount, and pkscript.

## Relationships

- **Depends on**: `build` (context logger extraction), `proofkeys` (implements Backend), `indexer` (SchnorrSigner interface).
- **Depended on by**: `lwwallet` (embeds Wallet + BoardingBackendBase), `btcwbackend` (embeds Wallet + BoardingBackendBase), `darepod` (proof key backend).

## Invariants

- Taproot scripts must be imported under `KeyScopeBIP0086` (not the custom chain key scope), because btcwallet's block processing skips credit tracking for non-default scopes (`chainntfns.go:IsDefaultScope` check).
- `ImportedAddrs` is in-memory only; repopulated on restart from the database by the wallet actor's `handleStartupRecovery`.
- `WalletPassphrase` is shared across all wallet backends for both `PrivatePass` and `PublicPass`.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
