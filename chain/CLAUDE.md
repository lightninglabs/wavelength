# chain

## Purpose

Bitcoind/Bitcoin Core RPC utilities, including `BitcoindRPCClient` wrapping btcd
rpcclient with extended methods like `SubmitPackage` for v3 transaction package
relay.

## Key Types

- `BitcoindRPCClient` — Wraps an `rpcclient.Client` and adds `SubmitPackage`,
  a v3 parents+child package-relay call not yet exposed by the standard btcd
  RPC client.

## Relationships

- **Depends on**: `btcd/rpcclient`, `btcd/btcjson`, `btcd/wire` (RPC plumbing
  only; no other repo packages).
- **Depended on by**: `harness` (test environment's `BitcoindClient` helper).

## Invariants

- `SubmitPackage` requires at least one parent transaction and a non-nil
  child; it errors immediately otherwise rather than forwarding a malformed
  request to bitcoind.
- `maxFeeRateBTCPerVByte` is converted to BTC/kvB before being sent, matching
  bitcoind's `submitpackage` RPC parameter units.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
