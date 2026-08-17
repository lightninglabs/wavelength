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
  account including imported watch-only scripts; `ListUnspentWalletAccount`
  restricts to the backend's own account for CPFP fee-input selection
  (watch-only outputs are unsignable). The account defaults to
  `DefaultWalletAccount` (lnd's "default") and is overridden with the
  `WithAccount` option; `Account()`/`IsDefaultAccount()` expose it to PSBT
  builders. `LeaseOutput`/`ReleaseOutput` forward to walletKit, casting
  `wallet.LockID` <-> `wtxmgr.LockID`.
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
- CPFP fee-input selection must use `ListUnspentWalletAccount`, not
  `ListUnspent`: offering a watch-only (imported script) output as a fee
  input makes the child PSBT unsignable.
- Every lnd call that spends, derives change, or signs must use the same
  account (`Account()`). A custom account lives in one key scope, so lnd
  rejects an explicit `ChangeType` for it on the `Psbt`/`Raw` templates —
  guard that with `IsDefaultAccount()`.
- A custom account must be **taproot-scoped**, and not because of CPFP
  witness types: lnd resolves a custom account name *within the key scope
  the requested address type implies* (`keyScopeForAccountAddr`), and every
  address this daemon derives asks for taproot, so an account under any
  other scope fails address derivation outright with "account not found".
- Observation must stay unscoped. Imported boarding scripts live in lnd's
  watch-only account, which belongs to no wallet account, so an account
  filter hides them — and lnd treats an empty account name as *every*
  account but `"default"` as a real filter, so scoping an observation path
  breaks it even with no account configured.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
