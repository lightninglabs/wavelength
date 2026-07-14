# rpc/roundpb

## Purpose

Generated protobuf/gRPC stubs for the round protocol, plus hand-written
support code: `service.go` (mailbox method name constants), `convert.go`
(proto <-> Go domain-type conversions, including the security-sensitive
`TreeFromProto` VTXO-tree deserializer), and `version.go` (the round flow
version guard).

## Key Types

All `*.pb.go` files are generated — never edit directly; regenerate with
`make rpc`. The hand-written files define:

- `ServiceName` — Fully-qualified protobuf service name
  (`"round.v1.RoundService"`) used for mailbox event routing.
- Server→client push method names: `MethodJoinAck`, `MethodBatchInfo`,
  `MethodAwaitingInputSigs`, `MethodAggNonces`, `MethodAggSigs`,
  `MethodRoundFailed`, `MethodError`, `MethodJoinRoundQuote`.
- Client→server method names: `MethodJoinRound`, `MethodAcceptQuote`,
  `MethodRejectQuote`, `MethodSubmitNonces`, `MethodSubmitPartialSigs`,
  `MethodSubmitForfeitSigs` (boarding input sigs),
  `MethodSubmitVTXOForfeitSigs` (VTXO forfeit sigs).
- `TreeFromProto` / `TreeToProto` — Convert between `*VTXOTree` proto and
  `lib/tree.Tree`; `TreeFromProto` takes `WithMaxTreeNodes` to bound the
  deserialized node count (`DefaultMaxTreeNodes` = 50,000).
- `OutpointFromProto`/`ToProto`, `TxOutFromProto`/`ToProto`,
  `PSBTFromBytes`/`ToBytes`, `MsgTxFromBytes`/`ToBytes`,
  `SchnorrSigFromBytes`/`ToBytes` — wire/proto ⇄ Go conversions for the
  round protocol's payload types.
- `FlowVersion` / `FlowVersionV1` / `ValidateFlowVersion` — the per-round
  choreography version stamped by the operator and validated by the
  client; fails closed on any version this build does not understand.

`MethodSubmitForfeitSigs` and `MethodSubmitVTXOForfeitSigs` are distinct
wire methods for two different payload types; see `round/CLAUDE.md` for
the `SubmitForfeitSigRequest` vs `SubmitVTXOForfeitSigsToServer`
distinction.

## Relationships

- **Depends on**: `lib/tree`, `lib/types` (conversion targets in
  `convert.go`); otherwise generated proto types only.
- **Depended on by**: `round` (outbox routing, proto conversions, flow
  version), `db` (persisting round/VTXO proto blobs), `waved` (proto
  conversion, flow version).

## Invariants

- **Never edit generated code** — regenerate via `make rpc`.
- Method name constants in `service.go` must match the proto service
  definition; mismatches silently drop events at the mailbox router.
- `TreeFromProto` enforces a pre-order invariant (child index > parent
  index) and bounds child/output indices; this is what prevents a
  malicious server from encoding cycles or out-of-range references in a
  `VTXOTree` and DoS-ing tree traversal. Do not relax these checks.
- `ValidateFlowVersion` must reject any `FlowVersion` other than the
  versions this build implements (currently only `FlowVersionV1`); never
  make it permissive by default.

## Deep Docs

- [rpc/CLAUDE.md](../CLAUDE.md) — Parent rpc package overview.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
