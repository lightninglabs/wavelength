# rpc/roundpb

## Purpose

Generated protobuf/gRPC stubs for the round protocol, plus hand-written
`service.go` containing the canonical mailbox method name constants used
for routing client↔server round messages through the durable transport
layer.

## Key Types

All `*.pb.go` files are generated — never edit directly; regenerate with
`make rpc`. The manually-maintained `service.go` defines:

`ClientBatchInfo` carries five per-round cryptographic keys (fields 5-9)
alongside existing `vtxo_tree_paths` and `connector_leaf_map`, enabling
operator key rotation without breaking in-flight round agreements:

- `TreeCosignKey` (bytes, field 5) — VTXO-tree MuSig2 cosigner key for
  this round; derived fresh per round, independent of the persistent
  identity key.
- `ConnectorOperatorKey` (bytes, field 6) — operator key used to build this
  round's connector tree; clients reconstruct connector outpoints from this
  key rather than global operator terms.
- `SweepKey` (bytes, field 7) — operator sweep key for this round's
  VTXO-tree sweep leaf.
- `SweepDelay` (uint32, field 8) — batch-wide absolute-timelock in blocks
  for the VTXO-tree sweep leaf; clients use this for batch expiry
  calculations, not global operator terms.
- `ForfeitKey` (bytes, field 9) — dedicated forfeit penalty key for this
  round; clients derive the forfeit-tx penalty output via BIP-86 key-spend
  to this key.

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
