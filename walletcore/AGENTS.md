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
- `OutputLeaser` — Interface for UTXO reservation during coin selection:
  `LeaseOutput(ctx, id, outpoint, expiry)` and
  `ReleaseOutput(ctx, id, outpoint)`. Implemented by `lwwallet.BoardingBackendAdapter`
  and the btcwallet adapters in `darepod`.
- `LockID [32]byte` — Caller-scoped identifier for output leases, stable
  across restarts. Each subsystem uses a distinct, hardcoded `LockID` so
  leases from one caller do not collide with another.
- `Utxo` — Simplified UTXO representation for coin selection: `Outpoint`,
  `PkScript`, `Amount`, `Confirmations`.

## Relationships

- **Depends on**: `build` (context logger extraction), `proofkeys` (implements Backend), `indexer` (SchnorrSigner interface).
- **Depended on by**: `lwwallet` (embeds Wallet + BoardingBackendBase), `btcwbackend` (embeds Wallet + BoardingBackendBase), `darepod` (proof key backend).

## Invariants

- Taproot scripts must be imported under `KeyScopeBIP0086` (not the custom chain key scope), because btcwallet's block processing skips credit tracking for non-default scopes (`chainntfns.go:IsDefaultScope` check).
- `ImportedAddrs` is in-memory only and must be repopulated from the DB on
  restart. `ImportTaprootScript` recovers from
  `waddrmgr.ErrDuplicateAddress` by resolving the existing address via
  `addressForTaprootScript`, repopulating the filter without a second
  import attempt — covers the case where btcwallet already persisted the
  script but the in-memory filter started empty.
- The user-supplied `Config.WalletPassword` is btcwallet's `PrivatePass`; the
  static `PublicWalletPassphrase` constant covers only public (watch-only)
  data. A nil `Config.Seed` opens an existing wallet database; a non-nil seed
  creates a new one, and backends must refuse a seed when a database already
  exists (btcwallet would silently ignore it).

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
