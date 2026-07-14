# rpc/oorpb

## Purpose

Generated gRPC/mailbox-RPC stubs for the OOR (out-of-round transfer)
client/server wire protocol (`oorwire.proto`), plus hand-written helpers that
convert between the proto wire types and domain types (`psbt.Packet`,
`chainhash.Hash`, `oortx.RecipientOutput`).

## Key Types

- `SubmitPackageRequest` / `SubmitPackageResponse` — wire request/response for
  submitting an Ark package and checkpoint PSBTs for co-signing.
- `FinalizePackageRequest` / `FinalizePackageResponse` — wire request/response
  for submitting finalized checkpoint PSBTs.
- `SigningDescriptor` — domain-side signing metadata (outpoint, VTXO policy
  template, spend path, owner-leaf policy) for one checkpoint input; encoded
  to/from `OORSigningDescriptor` on the wire.
- `FlowVersion` — permanent per-session OOR choreography version
  (`FlowVersionV1` is the only value understood today); validated with
  `ValidateFlowVersion`.
- `SubmitRejectedError` — typed error carrying `OORRejectCode` + reason,
  returned by `ParseSubmitPackageResponse` on a rejection branch.
- `OORMailboxServiceMailboxClient` / `RegisterOORMailboxServiceMailboxServer`
  — durable-mailbox transport bindings (`mailbox/rpc.Router`/`RPCClient`),
  alongside the standard grpc client/server interfaces.

## Relationships

- **Depends on**: `lib/tx/oor` (RecipientOutput domain type), `lib/tx/psbtutil`
  (PSBT serialize/parse), `mailbox/rpc` (durable mailbox transport used by the
  generated `*Mailbox*` client/server).
- **Depended on by**: `oor` (session actor, outbox messages, errors — the OOR
  client/server FSM), `db` (`oor_session_registry_store.go` persists
  `FlowVersion`), `waved` (server wiring), `systest` (end-to-end OOR tests).

## Invariants

- `FlowVersion` is zero-indexed so the Go zero value and an omitted wire field
  both read as `FlowVersionV1`; never renumber existing version constants.
- `ValidateFlowVersion` must reject any version this build does not
  understand — fail closed on unknown values arriving from a counterparty.
- `co_signed_ark_psbt` in `SubmitPackageSuccess` is additive: treat empty
  bytes as "operator hasn't upgraded yet," not a parse error, to keep rolling
  upgrades working.
- Regenerate wire stubs from `oorwire.proto` via `make rpc`; hand-edit only
  `payloads.go` and `version.go`, never the generated `*.pb.go` files.

## Deep Docs

- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map
