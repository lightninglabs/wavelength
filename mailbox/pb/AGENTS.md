# mailbox/pb

## Purpose

Generated protobuf/gRPC stubs for the mailbox wire protocol. Defines the
`Envelope` format and `MailboxService` edge API used by all durable
client-server transport in this project.

## Key Types

- `Envelope` — Durable transport unit. Fields: `ProtocolVersion`, `MsgId`
  (unique per retry), `IdempotencyKey` (stable across retries for server
  deduplication), `Sender`, `Recipient`, `Body` (google.protobuf.Any),
  `Rpc` (RpcMeta overlay), `EventSeq` (server-assigned monotonic cursor),
  `Headers`, `Checksum`.
- `RpcMeta` — RPC overlay: `Kind` (REQUEST/RESPONSE/EVENT), `Service`,
  `Method`, `CorrelationId`, `ReplyTo`.
- `Header` — Key-value pair for optional envelope headers.
- `MailboxService` — Edge API with three methods:
  - `Send(Envelope) → Envelope` — Deliver an envelope to a recipient.
  - `Pull(PullRequest) → PullResponse` — Long-poll for new envelopes
    (default 5 s timeout).
  - `AckUpTo(AckRequest) → AckResponse` — Acknowledge all envelopes up
    to and including a given `EventSeq` cursor.

## Relationships

- **Depends on**: nothing (pure proto-generated types, google.protobuf.Any).
- **Depended on by**: `serverconn` (constructs envelopes, drives Pull/AckUpTo),
  `darepod` (server-side mailbox), `sdk/swaps` (swap mailbox pull).

## Invariants

- **Never edit generated code** — regenerate via `make rpc`.
- `MsgId` differs on each retry; `IdempotencyKey` is stable across retries —
  do not confuse the two.
- `EventSeq` is server-assigned; clients treat it as an opaque monotonic
  cursor for `AckUpTo`.
- Proto source: `mailbox/pb/mailbox.proto`.

## Deep Docs

- [mailbox/CLAUDE.md](../CLAUDE.md) — Parent mailbox package overview.
- [docs/mailbox_architecture.md](../../docs/mailbox_architecture.md) — Full
  three-layer mailbox architecture.
- [docs/RPC_MAILBOX_CONTRACT.md](../../docs/RPC_MAILBOX_CONTRACT.md) —
  Envelope semantics and ack watermark contract.
