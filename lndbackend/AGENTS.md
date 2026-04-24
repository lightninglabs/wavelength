# lndbackend

## Purpose

LND chain backend integration providing chain source queries (fee estimation,
block/conf/spend notifications) and wallet controller operations (key
derivation, signing, UTXO management) for the server.

## Key Types

- `ChainSource` — Chain backend implementation backed by LND RPCs.
- `LndWalletController` — Wallet operations (signing, key management) via LND.
  Uses `SignOutputRawKeyLocator` for all signing operations (key-locator-based
  signing rather than raw private key export).
- `NewLndHeaderVerifier` — Returns a `proof.HeaderVerifier` that validates block
  headers against LND's chain backend via `ChainKit.GetBlockHash`. Used for
  TxProof SPV validation of boarding inputs.
- `WalletKitEstimator` — `chainfee.Estimator` implementation that proxies every
  `EstimateFeePerKW` call to the backing `lndclient.WalletKitClient` so both
  the `EstimateFee` quote surface and `validateOperatorFee` see live mempool
  rates. On any WalletKit error falls back to the last successfully observed
  rate (clamped to `chainfee.FeePerKwFloor`) rather than the floor itself, to
  avoid silently re-opening the operator silent-absorption hole. Constructor:
  `NewWalletKitEstimator(walletKit, log)`. Test-injectable timeout variant:
  `NewWalletKitEstimatorWithTimeout(walletKit, log, timeout)`.

## Relationships

- **Depends on**: `rounds` (chain query interfaces).
- **Depended on by**: root `darepo` (wiring as concrete backend). The
  `batchsweeper` package depends on the external `input.Signer` interface
  from `lnd`; `lndbackend` satisfies that interface only through root-package
  wiring, not a direct import.

## Invariants

- LND connection must be established and healthy before round operations begin.
- All signing uses `SignOutputRawKeyLocator` with `KeyLocator` references; no
  raw private key export from LND.
- Wallet operations must use the correct key scope for Ark-specific derivations.
- `WalletKitEstimator` never panics on a nil `walletKit`; `NewWalletKitEstimator`
  returns nil when given a nil client, so the caller (root `setupFeesSubsystem`)
  gates on non-nil and falls back to the static floor estimator.
- `WalletKitEstimator` error fallback uses the last successful rate, not the
  floor, to keep the fee floor anchored to recent reality during transient LND
  outages. Only the very first call before any success falls to `FeePerKwFloor`.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
