# daemonrpc

## Purpose

Daemon gRPC API definitions for wallet operations and round queries. Proto
source: `daemonrpc/daemon.proto`.

## Key Types

- `ServerInfo` — Operator terms snapshot: `OperatorPubkey`,
  `BoardingExitDelay`, `VtxoExitDelay`, `DustLimit`, `FeeRate`,
  `MinBoardingAmount`, `MaxBoardingAmount`, `MinConfirmations`,
  `MinOperatorFee`. Note: `ForfeitScript`, `SweepKey`, and `SweepDelay`
  (field numbers 4-6) were removed; these values are now delivered
  per-round in `round.v1.ClientBatchInfo`.
- `SendOORRequest` — OOR transfer request. `Recipients` is a repeated
  `Output` field supporting multiple recipients in a single transfer.
  `DryRun`, `CustomInputs`, and `IdempotencyKey` remain unchanged.
- `SendOORResponse` — OOR transfer response. `RecipientOutpoints` is a
  repeated string (one per requested recipient, in order). Individual
  entries may be empty if the daemon cannot resolve the outpoint from
  the finalized Ark transaction.

## Relationships

- **Depends on**: nothing (proto definitions).
- **Depended on by**: `darepod` (implements services), `cmd/darepocli`
  (uses generated clients), `sdk/ark` (SDK facade).

## Invariants

- **Never edit generated code** — regenerate via `make rpc`.
