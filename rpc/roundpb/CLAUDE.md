# rpc/roundpb

## Purpose

Generated protobuf/gRPC stubs for the round protocol, plus hand-written
`service.go` containing the canonical mailbox method name constants used
for routing client↔server round messages through the durable transport
layer.

## Key Types

All `*.pb.go` files are generated — never edit directly; regenerate with
`make rpc`. The manually-maintained `service.go` defines:

- `ServiceName` — Fully-qualified protobuf service name
  (`"round.v1.RoundService"`) used for mailbox event routing.
- Server→client push method names: `MethodJoinAck`, `MethodBatchInfo`,
  `MethodAwaitingInputSigs`, `MethodAggNonces`, `MethodAggSigs`,
  `MethodRoundFailed`, `MethodError`, `MethodJoinRoundQuote`.
- Client→server method names: `MethodJoinRound`, `MethodAcceptQuote`,
  `MethodRejectQuote`, `MethodSubmitNonces`, `MethodSubmitPartialSigs`,
  `MethodSubmitForfeitSigs` (boarding input sigs),
  `MethodSubmitVTXOForfeitSigs` (VTXO forfeit sigs).

`MethodSubmitForfeitSigs` and `MethodSubmitVTXOForfeitSigs` are distinct
wire methods for two different payload types; see `round/CLAUDE.md` for
the `SubmitForfeitSigRequest` vs `SubmitVTXOForfeitSigsToServer`
distinction.

## Relationships

- **Depends on**: nothing (generated proto types only).
- **Depended on by**: `round` (outbox routing, `FromProto` helpers),
  `serverconn` (mailbox method dispatch), `darepod` (proto conversion).

## Invariants

- **Never edit generated code** — regenerate via `make rpc`.
- Method name constants in `service.go` must match the proto service
  definition; mismatches silently drop events at the mailbox router.

## Deep Docs

- [rpc/CLAUDE.md](../CLAUDE.md) — Parent rpc package overview.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
