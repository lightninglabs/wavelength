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
- `LockID` — `[32]byte` caller-scoped output lease identifier. A stable
  value shared by all callers that need to cross-subsystem coordinate
  UTXO reservations (e.g. `txconfirmLockID` in `txconfirm`). Leases
  round-trip across restart by casting directly to `wtxmgr.LockID`.
- `Utxo` — Simplified confirmed UTXO representation used for fee-input
  selection: `Outpoint wire.OutPoint`, `PkScript []byte`,
  `Amount btcutil.Amount`, `Confirmations int32`.
- `OutputLeaser` — Interface for cross-subsystem UTXO reservation:
  `LeaseOutput(ctx, outpoint, lockID LockID, expiry time.Time) error`
  and `ReleaseOutput(ctx, outpoint, lockID LockID) error`. Implemented
  by all three backing wallet backends (`btcwbackend`, `lndbackend`,
  `lwwallet`) and consumed by `txconfirm.CPFPBroadcaster` for fee-input
  lock coordination.

## Relationships

- **Depends on**: `build` (context logger extraction), `proofkeys` (implements Backend), `indexer` (SchnorrSigner interface).
- **Depended on by**: `lwwallet` (embeds Wallet + BoardingBackendBase), `btcwbackend` (embeds Wallet + BoardingBackendBase), `darepod` (proof key backend).

## Invariants

- Taproot scripts must be imported under `KeyScopeBIP0086` (not the custom chain key scope), because btcwallet's block processing skips credit tracking for non-default scopes (`chainntfns.go:IsDefaultScope` check).
- `ImportedAddrs` is in-memory only; repopulated on restart from the database by the wallet actor's `handleStartupRecovery`.
- `WalletPassphrase` is shared across all wallet backends for both `PrivatePass` and `PublicPass`.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
