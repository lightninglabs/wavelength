# mailbox/pb

## Purpose

Generated protobuf/gRPC/REST-gateway stubs for the `mailbox.v1.MailboxService`
wire format (`mailbox.proto`): `Envelope`, `RpcMeta`, and the
Send/Pull/AckUpTo RPCs. Generated via `make rpc`
(`scripts/gen_protos_docker.sh`); do not edit the `*.pb.go` files by hand.

## Key Types

- `Envelope` — wire message: `msg_id`, `idempotency_key`, `sender`,
  `recipient`, `body`, `RpcMeta`, `event_seq`, headers, `protocol_version`
  (mailbox transport version), and `ark_protocol_version` (the Ark protocol
  version negotiated via the direct GetInfo bootstrap RPC — distinct from
  `protocol_version`; every envelope sent after negotiation carries it).
- `RpcMeta` / `RpcMeta_Kind` — RPC overlay (`REQUEST`/`RESPONSE`/`EVENT`) and
  correlation metadata.
- `Status` — result of a mailbox edge operation: `ok`, `code`, `message`,
  `min_supported_protocol_version` / `server_protocol_version` (populated for
  upgrade-required errors), and `supported_mailbox_versions` /
  `supported_ark_versions` (populated for permanent version errors so the
  sender can surface actionable guidance without parsing gRPC status
  details). Field 6 (`upgrade_url`) was removed and is `reserved`, not
  reused. `mailbox/conn.StatusError` wraps this type for classification.
- `MailboxServiceClient` / `MailboxServiceServer` — generated client/server
  interfaces for `Send`, `Pull`, `AckUpTo`.
- `MailboxProtocolVersionV1` (`version.go`, hand-maintained, not generated) —
  stable mailbox transport version constant; a breaking transport change gets
  a new endpoint/proto package, not a bump of this constant.

## Relationships

- **Depends on**: none beyond `google.golang.org/protobuf` and
  `grpc-gateway` runtime.
- **Depended on by**: `mailbox/conn`, `serverconn`, `darepod`.

## Invariants

- `*.pb.go`, `*_grpc.pb.go`, `*.pb.gw.go` are generated — regenerate via
  `make rpc`, never edit manually. Only `version.go` is hand-maintained.
- New fields must be additive (proto field-number append-only) so older
  envelopes decode cleanly under newer generated code; see
  `version_compat_test.go` for the compatibility contract this protects.
- A removed field's tag and name must be marked `reserved` (see `Status`
  field 6, formerly `upgrade_url`), never dropped silently and never
  reused by a later field — a future field reusing the tag would collide
  on the wire with a peer still emitting the old value.

## Deep Docs

- [mailbox/CLAUDE.md](../CLAUDE.md) — Parent package (pb/rpc/conn) overview.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
