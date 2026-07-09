# lndsubmitter

## Purpose

Implements `chainbackends.PackageSubmitter` by relaying v3/TRUC CPFP packages
through lnd's own `WalletKit.SubmitPackage` RPC. Lets a darepod running the
lnd wallet backend broadcast zero-fee unilateral-exit packages without a
separate bitcoind RPC or Esplora endpoint.

## Key Types

- `Submitter` — Relays a parents-first, child-last package to lnd's
  `WalletKit.SubmitPackage` RPC and maps the lndclient-native result back to
  `btcjson.SubmitPackageResult`. Constructed via `New(walletKit)`.
- `walletKitSubmitter` — Unexported interface narrowing
  `lndclient.WalletKitClient` down to just `SubmitPackage`, so tests can fake
  it without a full lndclient mock.

## Relationships

- **Depends on**: `lndclient` (WalletKit RPC client and
  `SubmitPackageResult`), `lnd/lnwallet/chainfee` (`SatPerVByte` for the
  max-fee-rate ceiling), `btcd/btcjson` (result type returned to callers),
  `btcd/wire` (`MsgTx`).
- **Depended on by**: `darepod` (`darepod/server.go` constructs
  `lndsubmitter.New(lndSvc.WalletKit)` and wires it in as the
  `chainbackends.PackageSubmitter` when the daemon runs the lnd wallet
  backend, instead of the bitcoind-direct `chainbackends/bitcoindrpc`
  submitter).

## Invariants

- `SubmitPackage` rejects a nil child or any nil parent up front with a typed
  error, rather than letting lndclient/wire serialization panic on a nil
  pointer deep in the call stack.
- The optional `maxFeeRate` ceiling arrives as BTC/kvB (bitcoind's
  `maxfeerate` shape, per the `chainbackends.PackageSubmitter` contract) and
  must be converted to sat/vByte for lnd's RPC by rounding to the nearest
  integer, not truncating — truncation would silently make the ceiling
  stricter than the caller asked for (e.g. 12.5 sat/vByte → 12).
- `mapResult` only sets a `TxResults` entry's `Error` field when lnd reported
  a non-empty rejection reason; an empty string means the tx was accepted.

## Deep Docs

- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
