# mailbox/pb

## Purpose

Generated protobuf/gRPC stubs for `mailbox.v1.MailboxService` — the wire
format and edge API for the durable mailbox transport. Defines the
`Envelope` every client↔server message rides in, the RPC-overlay metadata
that lets unary request/response semantics be layered on top of an
asynchronous mailbox, and the `Send`/`Pull`/`AckUpTo` edge methods. Proto
source: `mailbox/pb/mailbox.proto`.

## Key Types

- `MailboxService` — The edge API: `Send` (push one envelope), `Pull`
  (long-poll for envelopes from `cursor`), `AckUpTo` (acknowledge delivery up
  to a cursor).
- `Envelope` — The durable unit sent via the mailbox edge. Carries
  `msg_id` (unique per send attempt), `idempotency_key` (stable per semantic
  operation, used for server-side dedupe), `sender`/`recipient` mailbox ids,
  a typed `body` (`google.protobuf.Any`), optional `rpc` overlay metadata,
  and a server-assigned `event_seq` that defines per-mailbox delivery order.
  Since baseline, also carries `ark_protocol_version` — the negotiated Ark
  protocol version (distinct from `protocol_version`, the mailbox transport
  version) bound to the runtime after the direct `GetInfo` bootstrap RPC;
  every envelope sent after negotiation carries it.
- `RpcMeta` — RPC-overlay metadata attached to an `Envelope` so unary
  request/response flows can be correlated over the mailbox: `Kind`
  (REQUEST/RESPONSE/EVENT), `service`/`method` (fully-qualified proto
  service+method name), `correlation_id`, `reply_to`.
- `Status` — Result of a mailbox edge operation: `ok`, `code`, `message`,
  and (for version-mismatch errors) `min_supported_protocol_version`,
  `server_protocol_version`, `supported_mailbox_versions`, and
  `supported_ark_versions` so a sender can surface actionable version
  guidance without parsing gRPC status details.
- `SendRequest`/`SendResponse`, `PullRequest`/`PullResponse`,
  `AckUpToRequest`/`AckUpToResponse` — Request/response pairs for the three
  edge methods. `PullResponse.next_cursor` is the highest `event_seq`
  returned + 1.
- `mailbox/pb/version.go` (hand-written) — `MailboxProtocolVersionV1`, the
  stable mailbox transport version constant. Deliberately a code constant
  (not operator configuration) because it is the stable bootstrap format
  every client must be able to decode.

## Relationships

- **Depends on**: `google.protobuf.Any` (envelope body), nothing else
  (generated proto types only).
- **Depended on by**: `mailbox/conn` (constructs/parses `Envelope`,
  `RpcMeta`, `Status`), `mailbox/rpc` (routes on `RpcMeta.service`/`method`),
  `serverconn` (client-side mailbox runtime: ingress dispatch, event router,
  version/compatibility negotiation, heartbeats), `darepod` (server-side
  mailbox edge, outbound clients), `swapclientserver` and `sdk/swaps` (mailbox
  message construction for swap flows), `rpc/restclient`
  (`MailboxServiceClient` REST wrapper).
- **Sends**: N/A — this package defines message shapes, not message flow. See
  `mailbox/CLAUDE.md` and `serverconn`/`darepod` for who actually calls
  `Send`/`Pull`/`AckUpTo`.
- **Receives**: N/A — see above.

## Invariants

- **Never edit generated code** — regenerate via `make rpc`.
- `msg_id` must differ on every send attempt (including retries);
  `idempotency_key` must stay stable across retries of the same semantic
  operation so the server can dedupe.
- `event_seq` is assigned by the mailbox edge, not the sender; it is the
  authoritative per-mailbox ordering key used as the `Pull`/`AckUpTo` cursor.
- `protocol_version` (mailbox transport) and `ark_protocol_version` (Ark
  protocol) are independent axes and must not be conflated: a mailbox
  transport upgrade and an Ark protocol version bump can each roll out on
  their own schedule.
- Field 6 of `Status` (`upgrade_url`) is `reserved`; never reuse that field
  number/name — an old peer emitting the removed field must not silently
  collide with a new field on the wire.

## Deep Docs

- [mailbox/CLAUDE.md](../CLAUDE.md) — Parent `mailbox` package: `conn`/`rpc`
  sub-package roles and the three-layer mailbox system.
- [docs/mailbox_architecture.md](../../docs/mailbox_architecture.md) — Full
  three-layer architecture (pb, rpc, conn).
- [docs/RPC_MAILBOX_CONTRACT.md](../../docs/RPC_MAILBOX_CONTRACT.md) —
  Envelope semantics and ack watermarks in detail.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
