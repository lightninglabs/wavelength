# rpc/oorpb

## Purpose

Generated protobuf/gRPC/mailbox-RPC stubs for OOR (out-of-round) VTXO
transfer wire messages, plus hand-written `payloads.go` containing typed
constructors and parsers that convert between OOR domain types (PSBTs,
signing descriptors, recipient outputs) and their proto wire shapes, and the
mailbox method name constants used to route client↔server OOR
submit/finalize/ack flows. Proto source: `rpc/oorpb/oorwire.proto`.

## Key Types

All `*.pb.go` files (`oorwire.pb.go`, `oorwire_grpc.pb.go`,
`oorwire_mailboxrpc.pb.go`) are generated — never edit directly; regenerate
with `make rpc`. The manually-maintained `payloads.go` (covered by
`payloads_test.go` and `payloads_property_test.go`) defines:

- `ServiceName` — mailbox RPC service name (`"oorpb.OORMailboxService"`)
  used for client/server submit/finalize/ack event routing.
- `MethodSubmitPackage`, `MethodFinalizePackage`, `MethodIncomingAck` —
  mailbox method name constants.
- `SigningDescriptor` — minimal signing metadata (outpoint, VTXO policy
  template, spend path, owner-leaf policy) the server OOR actor needs to
  co-sign checkpoint inputs.
- `NewSubmitPackageRequest` / `ParseSubmitPackageRequest` — convert between
  domain PSBTs + signing descriptors + recipient outputs and the wire
  `SubmitPackageRequest`.
- `NewSubmitPackageResponse` / `NewSubmitPackageRejection` /
  `ParseSubmitPackageResponse` — build/parse the success or rejection
  branch of `SubmitPackageResponse`; parsing a rejection branch returns a
  typed `*SubmitRejectedError`.
- `SubmitRejectedError` — typed rejection error (`Code`, `Reason`) so
  callers route on cause (e.g. `OOR_REJECT_LINEAGE_TOO_LARGE`) via
  `errors.As` instead of string-matching.
- `NewFinalizePackageRequest` / `ParseFinalizePackageRequest` /
  `NewFinalizePackageResponse` / `ParseFinalizePackageResponse` — typed
  wire conversion for the finalize RPC.
- Unexported wire↔domain helpers (`encodeSigningDescriptor`,
  `decodeSigningDescriptor`, `encodeOutPoint`, `decodeOutPoint`,
  `decodeSessionID`, `encodePSBTSlice`, `decodePSBTSlice`) shared by the
  request/response builders above.

## Relationships

- **Depends on**: `lib/tx/oor` (`RecipientOutput`), `lib/tx/psbtutil`
  (PSBT serialize/parse helpers).
- **Depended on by**: `oor` (session FSM and outbox handlers build/parse
  submit and finalize payloads), `darepod` (server-side OOR wiring),
  `systest`.

## Invariants

- **Never edit generated code** — regenerate via `make rpc`.
- `SubmitPackageResponse.Success.co_signed_ark_psbt` is an additive field:
  empty bytes mean an unupgraded operator, not a parse error, so mixed-
  version client/operator pairs keep working during a rolling upgrade.
- Rejection responses always echo `session_id` so the client-side
  `EventRouter` can route the failure to the correct OOR session FSM
  instead of stalling the ingress cursor on an undispatchable envelope.

## Deep Docs

- [rpc/CLAUDE.md](../CLAUDE.md) — Parent rpc package overview.
- [oor/CLAUDE.md](../../oor/CLAUDE.md) — Client-side OOR session FSM that
  builds and consumes these payloads.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
