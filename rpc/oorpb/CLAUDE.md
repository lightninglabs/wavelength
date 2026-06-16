# rpc/oorpb

## Purpose

OOR (out-of-round) transfer gRPC stubs and handwritten payload helpers.
Generated `*.pb.go` files define the proto message/service types; the
handwritten `payloads.go` provides typed constructor and parser functions
so callers never manipulate raw proto bytes directly.

## Key Types

All `*.pb.go` files are generated — never edit directly; regenerate with
`make rpc`.

The manually-maintained `payloads.go` defines:

- `ServiceName` / `MethodSubmitPackage` / `MethodFinalizePackage` /
  `MethodIncomingAck` — Mailbox RPC service and method name constants
  used for OOR submit/finalize request-response routing.
- `SigningDescriptor` — Domain type carrying the minimal signing
  metadata the server OOR actor needs to co-sign checkpoint inputs:
  `Outpoint wire.OutPoint`, `VTXOPolicyTemplate`, `SpendPath`, and
  `OwnerLeafPolicy` (all serialized arkscript bytes).
- `NewSubmitPackageRequest` / `ParseSubmitPackageRequest` —
  Build/decode a `SubmitPackageRequest` from/to domain types
  (`*psbt.Packet` Ark tx, `[]*psbt.Packet` checkpoints,
  `[]SigningDescriptor`, `[]oortx.RecipientOutput`).
- `NewSubmitPackageResponse` / `ParseSubmitPackageResponse` —
  Build/decode the success branch of a `SubmitPackageResponse`.
  `ParseSubmitPackageResponse` returns `*SubmitRejectedError` when the
  response carries a rejection branch so callers can route on the typed
  `OORRejectCode` without string-matching.
- `NewSubmitPackageRejection` — Build the rejection branch of a
  `SubmitPackageResponse` with a typed `OORRejectCode` and session id.
- `SubmitRejectedError` — Error type wrapping `Code OORRejectCode` and
  `Reason string`. Returned by `ParseSubmitPackageResponse`; callers
  use `errors.As` to inspect the code.
- `NewFinalizePackageRequest` / `ParseFinalizePackageRequest` —
  Build/decode a `FinalizePackageRequest` (session id +
  `[]*psbt.Packet` final checkpoints).
- `NewFinalizePackageResponse` / `ParseFinalizePackageResponse` —
  Build/decode a `FinalizePackageResponse` (session id echo).

## Relationships

- **Depends on**: `lib/tx/oor` (`RecipientOutput`),
  `lib/tx/psbtutil` (PSBT serialization), `btcd/wire` (outpoints),
  `btcd/btcutil/psbt`.
- **Depended on by**: `oor` (builds submit/finalize requests and
  decodes responses), `serverconn` (mailbox method dispatch using the
  method name constants), `darepod` (registers event routes and
  classifies rejections via `SubmitRejectedError`).

## Invariants

- **Never edit generated code** — regenerate via `make rpc`.
- `ParseSubmitPackageResponse` tolerates a missing `CoSignedArkPsbt`
  (empty bytes) in the success branch so clients can talk to older
  operators during a rolling upgrade; recovery flows surface the
  absence at the recovery boundary, not on every submit.
- The rejection branch echoes `session_id` so the ingress dispatcher
  can route the failure to the correct OOR session FSM instead of
  stalling the cursor on an undispatchable envelope.

## Deep Docs

- [rpc/CLAUDE.md](../CLAUDE.md) — Parent rpc package overview.
- [oor/CLAUDE.md](../../oor/CLAUDE.md) — OOR actor using these helpers.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
