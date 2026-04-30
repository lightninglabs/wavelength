# rpc/oorpb

## Purpose

Hand-written domain helpers for the OOR mailbox RPC package. Provides typed
request/response builders and parsers for the OOR submit/finalize/ack wire
protocol, layered on top of the generated proto stubs in this directory.

## Key Types

- `SigningDescriptor` — Minimal signing metadata for one checkpoint input.
  Fields: `Outpoint wire.OutPoint`, `VTXOPolicyTemplate []byte`,
  `SpendPath []byte`, `OwnerLeafPolicy []byte`. Converted to/from
  `OORSigningDescriptor` proto by `encodeSigningDescriptor` /
  `decodeSigningDescriptor`.
- `SubmitRejectedError` — Typed error returned by `ParseSubmitPackageResponse`
  when the operator rejected the submit. Fields: `Code OORRejectCode`,
  `Reason string`. Callers can route on `Code` (e.g. fall back to in-round
  payment for `OOR_REJECT_LINEAGE_TOO_LARGE`) without string-matching `Reason`.
- `ServiceName = "oorpb.OORMailboxService"` — Mailbox RPC service name used
  for submit/finalize request-response flows.
- `MethodSubmitPackage` / `MethodFinalizePackage` / `MethodIncomingAck` —
  Well-known mailbox method name constants.

## Key Functions (payloads.go)

- `NewSubmitPackageRequest` — Builds a `*SubmitPackageRequest` proto from
  domain types: Ark PSBT, checkpoint PSBTs, signing descriptors, recipients.
  Serializes PSBTs via `psbtutil.Serialize`.
- `ParseSubmitPackageRequest` — Inverse of `NewSubmitPackageRequest`; decodes
  the request back to domain types. Validates that every `OORSigningDescriptor`
  and `OORRecipientOutput` is non-nil.
- `NewSubmitPackageResponse` — Builds the success branch of
  `*SubmitPackageResponse` (carries session id and co-signed checkpoint PSBTs).
- `NewSubmitPackageRejection` — Builds the rejection branch of
  `*SubmitPackageResponse` (carries `OORRejectCode`, human-readable reason, and
  the session id echoed back so the client EventRouter can route the failure to
  the correct OOR FSM).
- `ParseSubmitPackageResponse` — Decodes the response oneof; returns
  `(sessionID, coSignedCheckpoints, nil)` on success or
  `(sessionID, nil, *SubmitRejectedError)` on rejection. The session id is
  decoded best-effort even from rejection branches — a malformed session id
  surfaces as a typed error with a zero hash rather than dropping the branch
  entirely.
- `NewFinalizePackageRequest` / `ParseFinalizePackageRequest` — Build/decode
  finalize request (session id + final checkpoint PSBTs).
- `NewFinalizePackageResponse` / `ParseFinalizePackageResponse` — Build/decode
  finalize response (echoed session id).

## Relationships

- **Depends on**: `lib/tx/oor` (`RecipientOutput`), `lib/tx/psbtutil` (PSBT
  serialization), `btcd/psbt`, `btcd/wire`.
- **Depended on by**: `oor` (uses builders/parsers for submit/finalize outbox
  events), `serverconn` (routes the mailbox methods), operator-side RPC
  handlers (use `ParseSubmitPackageRequest` / `NewSubmitPackageResponse`).

## Invariants

- **Never edit generated code** — the `.pb.go` and `*_mailboxrpc.pb.go` files
  in this directory are regenerated via `make rpc`. Only `payloads.go` and its
  test are hand-written.
- `SubmitRejectedError` carries `SessionId` echoed from the server rejection so
  the durable `EventRouter` dispatch path can route the failure to the correct
  OOR session FSM rather than stalling the ingress cursor on an undispatchable
  envelope. A zero session hash (from a malformed rejection) is non-routable and
  the FSM-side error path handles it gracefully.
- `encodeOutPoint` / `decodeOutPoint` enforce `chainhash.HashSize` (32 bytes)
  on all outpoint txids; short blobs are rejected with a descriptive error
  rather than a bounds panic.

## Deep Docs

- [rpc/CLAUDE.md](../CLAUDE.md) — Parent rpc package overview.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
