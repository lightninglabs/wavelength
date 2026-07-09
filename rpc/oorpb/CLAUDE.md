# rpc/oorpb

## Purpose

Generated protobuf/gRPC/mailbox-RPC stubs for the out-of-round (OOR) transfer
protocol, plus hand-written helpers that convert between the wire messages
and domain types (`psbt.Packet`, `wire.OutPoint`, `chainhash.Hash`) and pin
the per-session OOR flow version.

## Key Types

All `*.pb.go` files are generated — never edit directly; regenerate with
`make rpc`. The manually-maintained `payloads.go` and `version.go` define:

- `SigningDescriptor` — Domain-side signing metadata (outpoint, VTXO policy
  template, spend path, owner-leaf policy) for one checkpoint input; mirrors
  the generated `OORSigningDescriptor` wire type.
- `NewSubmitPackageRequest` / `ParseSubmitPackageRequest` — Build/decode a
  `SubmitPackageRequest`, serializing/parsing the Ark and checkpoint PSBTs
  and signing descriptors between domain types and wire bytes.
- `NewSubmitPackageResponse` / `NewSubmitPackageRejection` /
  `ParseSubmitPackageResponse` — Build the success or rejection branch of
  `SubmitPackageResponse` and decode either branch back out; a rejection
  decodes into a typed `*SubmitRejectedError`.
- `SubmitRejectedError` — Typed error (`Code OORRejectCode`, `Reason string`)
  returned by `ParseSubmitPackageResponse` so callers route on `Code`
  (e.g. fall back to in-round payment) instead of string-matching `Reason`.
- `NewFinalizePackageRequest` / `ParseFinalizePackageRequest`,
  `NewFinalizePackageResponse` / `ParseFinalizePackageResponse` — Build/decode
  the finalize-package request/response pair.
- `FlowVersion` / `FlowVersionV1` — Permanent per-session choreography
  version stamped on the submit request and persisted with the session, so
  client and operator never drift on how a given OOR transfer was conducted.
  Zero-indexed: `FlowVersionV1` is the Go zero value, so an unstamped field
  reads as V1 with no normalization step.
- `ValidateFlowVersion` — Ingress guard that fails closed on any flow version
  this build does not understand (i.e. anything past the latest known
  version), applied where a version arrives from the other party.

## Relationships

- **Depends on**: `mailbox/rpc` (`rpc.Router`, `rpc.RPCClient` for the
  generated `OORMailboxServiceMailbox{Client,Server}`), `lib/tx/oor`
  (`oortx.RecipientOutput`), `lib/tx/psbtutil` (PSBT serialize/parse).
- **Depended on by**: `oor` (session actor, outbox messages, errors —
  the primary consumer of the typed constructors/parsers and
  `ValidateFlowVersion`), `db` (`oor_session_registry_store.go` persists
  `FlowVersion`), `darepod` (server-side OOR mailbox wiring).

## Invariants

- **Never edit generated code** — regenerate via `make rpc`.
- New wire fields on `SubmitPackageResponse`/`SubmitPackageRequest` must be
  additive: `ParseSubmitPackageResponse` already treats an absent
  `CoSignedArkPsbt` as "operator not yet upgraded" rather than a parse
  error, and new fields must preserve that rolling-upgrade compatibility.
- `FlowVersion` values are permanent once assigned to a session; a build
  must reject (`ValidateFlowVersion`) any version it does not implement
  rather than guess at unknown choreography rules.
- Rejection and success payloads both echo `session_id` so the client-side
  `EventRouter`/session FSM can route the response without stalling the
  ingress cursor on an undispatchable envelope.

## Deep Docs

- [rpc/CLAUDE.md](../CLAUDE.md) — Parent rpc package overview.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
