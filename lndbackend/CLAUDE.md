# lndbackend

## Purpose

lndclient-backed implementations of wallet interfaces for connecting to
remote LND nodes: boarding UTXO/key management, remote signing (including
MuSig2) for the round actor, and proof-key derivation/signing.

## Key Types

- `BoardingBackend` — Wraps `lndclient.WalletKitClient` and `ChainKitClient`.
  Implements `wallet.BoardingBackend` and `wallet.OutputLeaser`.
  `GetTransaction` returns `*wallet.TxInfo`; `GetBlock` fetches raw blocks via
  `chainKit` for TxProof merkle inclusion. `ListUnspent` spans every wallet
  account including imported watch-only scripts; `ListUnspentDefaultAccount`
  restricts to the default account for CPFP fee-input selection (watch-only
  outputs are unsignable). `LeaseOutput`/`ReleaseOutput` forward to
  walletKit, casting `wallet.LockID` <-> `wtxmgr.LockID`.
- `ClientWallet` — Adapts lndclient's remote signer to `input.Signer` +
  MuSig2 (`round.ClientWallet`), so the round actor can sign VTXO tree
  branches and forfeit transactions via LND's remote signer without a local
  wallet. Uses a background context internally since `input.Signer` carries
  none; relies on the lndclient dial-option gRPC deadline instead.
- `ProofKeyBackend` — Implements `proofkeys.Backend` for LND-backed key
  derivation and Schnorr proof signing. Wraps `walletKit`/`signer` for
  `DeriveKey`, `DeriveNextKey`, and produces `indexer.SchnorrSigner`
  instances via `indexer.NewLNDSchnorrSigner`.

## Relationships

- **Depends on**: `wallet` (implements `BoardingBackend`/`OutputLeaser`),
  `proofkeys` (implements `Backend`), `indexer` (`SchnorrSigner`), `build`
  (context logger fallback).
- **Depended on by**: `waved` (LND-backed wallet mode, all three types),
  root `main` package (`lnd_boarding_wallet.go` back-compat alias), `systest`.

## Invariants

- `ClientWallet.signOutputRawWithLocator` always forwards the key locator
  when set (including family != 0, index == 0), working around an lndclient
  gap that otherwise breaks the family-6/index-0 identity signing path.
- CPFP fee-input selection must use `ListUnspentDefaultAccount`, not
  `ListUnspent`: offering a watch-only (imported script) output as a fee
  input makes the child PSBT unsignable.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
