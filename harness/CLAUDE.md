# harness

## Purpose

Docker-based Bitcoin/LND integration test environment. Manages bitcoind and LND
containers with network isolation for end-to-end testing. Also provides
`BitcoindPackageSubmitter` for V3 package relay in integration tests.

## Key Types

- `BitcoindPackageSubmitter` — Implements `chainbackends.PackageSubmitter` via
  direct JSON-RPC to bitcoind (no LND intermediary). Serializes parent+child
  transactions to hex and calls `submitpackage` with `maxfeerate=0`. Used by
  the itest harness to provide package relay without requiring LND support.

## Key Constants and Functions

- `BitcoindRPCUser` = `"admin1"`, `BitcoindRPCPass` = `"123"` — Public
  constants for bitcoind container credentials used by test harness setup and
  `BitcoindPackageSubmitter`.
- `GetLNDClientConn(host, cert, macaroon) (*grpc.ClientConn, error)` — Creates
  an authenticated mTLS gRPC connection to an LND node. Used by
  `FundOperatorLND` and `SetupChannelBetween`.
- `numInitialBlocks` = 106, `defaultTimeout` = 30s, `pollInterval` = 200ms.
- Coinbase maturity: 100 blocks + 6-block buffer.

## Relationships

- **Depends on**: `chainbackends` (`PackageSubmitter` interface), `chain`
  (bitcoind RPC), `lndbackend` (LND integration).
- **Depended on by**: `systest` (system-level tests).

## Invariants

- `BitcoindPackageSubmitter` submits packages with `maxfeerate=0` to bypass
  per-transaction fee rate limits — only the child's fee is evaluated.
- Package submission returns an error if any individual transaction in the
  response carries a non-nil error field.
