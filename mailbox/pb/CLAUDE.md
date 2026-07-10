# mailbox/pb

## Purpose

Generated protobuf/gRPC/REST-gateway stubs for the `mailbox.v1.MailboxService`
wire format (`mailbox.proto`): `Envelope`, `RpcMeta`, and the
Send/Pull/AckUpTo RPCs. Generated via `make rpc`
(`scripts/gen_protos_docker.sh`); do not edit the `*.pb.go` files by hand.

## Key Types

- `Envelope` — wire message: `msg_id`, `idempotency_key`, `sender`,
  `recipient`, `body`, `RpcMeta`, `event_seq`, headers.
- `RpcMeta` / `RpcMeta_Kind` — RPC overlay (`REQUEST`/`RESPONSE`/`EVENT`) and
  correlation metadata.
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

## Deep Docs

- [mailbox/CLAUDE.md](../CLAUDE.md) — Parent package (pb/rpc/conn) overview.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
