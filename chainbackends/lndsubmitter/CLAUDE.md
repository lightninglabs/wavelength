# chainbackends/lndsubmitter

## Purpose

Implements `chainbackends.PackageSubmitter` on top of lnd's
`WalletKit.SubmitPackage` RPC. It lets an lnd-backed waved relay its
zero-fee unilateral-exit v3/TRUC packages through lnd's own chain
connection, so no separate bitcoind RPC or Esplora endpoint is required
for CPFP fee bumping.

## Key Types

- `Submitter` — Relays parent+child packages through lnd's WalletKit.
  Constructed via `New(walletKit)`.
- `New(walletKit)` — Builds a `Submitter` from anything satisfying the
  package's narrow `walletKitSubmitter` interface (the subset of
  `lndclient.WalletKitClient` used here); `lndclient.WalletKitClient`
  satisfies it directly, and the narrow surface keeps the submitter easy
  to fake in tests.

## Relationships

- **Depends on**: `chainbackends` (implements its `PackageSubmitter`
  interface and returns its `btcjson.SubmitPackageResult` shape),
  `lndclient` (`WalletKitClient.SubmitPackage`, `SubmitPackageResult`).
- **Depended on by**: `waved` (`server.go` wires `lndsubmitter.New` as
  the default `PackageSubmitter` for `WalletTypeLnd` whenever
  `cfg.PackageSubmitter` is not explicitly injected, i.e. an explicit
  bitcoind-backed submitter always takes precedence).

## Invariants

- `chainbackends.PackageSubmitter.SubmitPackage`'s `maxFeeRate` is a
  `*float64` in BTC/kvB (bitcoind's `maxfeerate` shape); lnd's RPC wants
  an integer sat/vByte. The conversion rounds to the nearest sat/vByte
  rather than truncating — truncation would silently lower the ceiling
  (e.g. 12.5 -> 12), making it stricter than the caller asked for. A nil
  `maxFeeRate` passes through unchanged as the node default.
- `parents` must be topologically sorted (unconfirmed parents first) with
  `child` last; `SubmitPackage` assembles them in that order before
  calling lnd.
- Nil `child` or any nil entry in `parents` is rejected up front with an
  error instead of being forwarded, since a nil `*wire.MsgTx` would
  otherwise panic deep in lndclient/wire serialization.
- `mapResult` treats an empty `TxResults[wtxid].Err` string as acceptance;
  only a non-empty reject reason is surfaced as `Error` on the mapped
  `btcjson.SubmitPackageTxResult`.

## Deep Docs

- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
