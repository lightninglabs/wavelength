# lndbackend

## Purpose

`BoardingBackend` and `ProofKeyBackend` implementations wrapping lndclient's
WalletKitClient for key derivation, taproot script import, UTXO enumeration,
and proof key signing via LND.

## Key Types

- `BoardingBackend` — Struct holding `walletKit lndclient.WalletKitClient` and
  `chainKit lndclient.ChainKitClient`. Implements `wallet.BoardingBackend` and
  `wallet.OutputLeaser`. `GetTransaction` returns `*wallet.TxInfo`; `GetBlock`
  fetches raw blocks via `chainKit`. Exposes `WalletKit()
  lndclient.WalletKitClient` for callers that need operations beyond the
  `BoardingBackend` interface (e.g., building the `LndClientTxBroadcaster` in
  `chainbackends`). `LeaseOutput`/`ReleaseOutput` forward to walletKit, casting
  `wallet.LockID` → `wtxmgr.LockID`. `ListUnspent` spans every account
  including imported watch-only scripts; `ListUnspentDefaultAccount` restricts
  results to LND's default account for callers that need outputs the wallet
  can unilaterally sign.
- `ProofKeyBackend` — Implements `proofkeys.Backend` for LND-backed key
  derivation and Schnorr proof signing. Wraps `walletKit` for `DeriveKey`,
  `DeriveNextKey`, and produces `indexer.SchnorrSigner` instances.

## Relationships

- **Depends on**: `wallet` (implements `BoardingBackend`), `proofkeys`
  (implements `Backend`), `indexer` (SchnorrSigner interface).
- **Depended on by**: `darepod` (LND-backed wallet mode).

## Invariants

- CPFP fee-input selection must use `ListUnspentDefaultAccount`, not
  `ListUnspent`. `ListUnspent` includes imported watch-only outputs (e.g.
  boarding/exit scripts from `ImportTaprootScript`) that LND tracks but cannot
  unilaterally sign; offering one as a fee input makes the child PSBT
  unsignable and the fee bump fails with "PSBT is not finalizable".

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
