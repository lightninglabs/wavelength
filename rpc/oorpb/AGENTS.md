# rpc/oorpb

## Purpose

OOR (out-of-round) transfer protocol message definitions and hand-written
type helpers for the client-side submit/finalize/ack RPC flows. The package
contains generated proto stubs plus `payloads.go`, which provides typed Go
constructors and parsers so callers never manipulate raw proto bytes directly.

## Key Types

- `SigningDescriptor` — Minimal signing metadata for one OOR transfer input:
  `Outpoint`, `VTXOPolicyTemplate`, `SpendPath`, `OwnerLeafPolicy`. Used by
  both submit-request construction (client) and server co-signing.
- `SubmitRejectedError` — Typed rejection error returned by
  `ParseSubmitPackageResponse`. Carries `Code OORRejectCode` and `Reason`
  string so callers can route on the cause (e.g.
  `OOR_REJECT_LINEAGE_TOO_LARGE`) without string-matching.
- `ServiceName`, `MethodSubmitPackage`, `MethodFinalizePackage`,
  `MethodIncomingAck` — Canonical mailbox RPC service and method name
  constants for the OOR request-response flows.

## Constructors and Parsers (payloads.go)

- `NewSubmitPackageRequest` — Builds a `*SubmitPackageRequest` from typed Go
  domain objects (`*psbt.Packet`, `[]*psbt.Packet`, `[]SigningDescriptor`,
  `[]oortx.RecipientOutput`).
- `ParseSubmitPackageRequest` — Decodes a `*SubmitPackageRequest` back to
  domain types. Returns an error on nil or malformed input.
- `NewSubmitPackageResponse` — Builds the success branch of
  `*SubmitPackageResponse`.
- `NewSubmitPackageRejection` — Builds the rejection branch; echoes
  `sessionID` so the client EventRouter can route the failure to the correct
  OOR session FSM.
- `ParseSubmitPackageResponse` — Decodes the success branch; returns
  `*SubmitRejectedError` (via `errors.As`) for the rejection branch.
- `NewFinalizePackageRequest` / `ParseFinalizePackageRequest` —
  Finalize-phase typed encode/decode helpers.
- `NewFinalizePackageResponse` / `ParseFinalizePackageResponse` —
  Finalize-phase response helpers.

## Relationships

- **Depends on**: `lib/tx/oor` (`RecipientOutput`), `lib/tx/psbtutil`
  (PSBT encode/decode), standard `btcd` types.
- **Depended on by**: `oor` (client-side submit/finalize flows),
  `swapclientserver` (server-side OOR co-signing).

## Invariants

- `payloads.go` is hand-written; the remaining `*.pb.go` files are generated
  from `oorwire.proto` — never edit generated files directly.
- `NewSubmitPackageRejection` echoes the session ID in the rejection proto so
  the client-side EventRouter can route `SubmitRejectedError` to the correct
  OOR session FSM rather than stalling the ingress cursor.
- `ParseSubmitPackageResponse` returns a typed `*SubmitRejectedError`; use
  `errors.As` to recover `Code` and `Reason` without string-matching.

## Deep Docs

- [rpc/CLAUDE.md](../CLAUDE.md) — Parent package overview.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
