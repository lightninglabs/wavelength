# mailbox/pb

## Purpose

Generated protobuf/gRPC wire types for the mailbox transport (`Envelope`,
`RpcMeta`, `Send`/`Pull`/`AckUpTo`), plus a hand-written constant that pins
the stable mailbox transport version every client must be able to decode.

## Key Types

All `*.pb.go` files are generated — never edit directly; regenerate with
`make rpc`. The manually-maintained `version.go` defines:

- `MailboxProtocolVersionV1` — The stable mailbox transport version (`1`),
  covering envelope framing, `RpcMeta` routing, `Send`/`Pull`/`AckUpTo`
  behavior, and cursor/ack/durable-replay semantics. It is a code constant,
  not operator configuration, because v1 is the bootstrap endpoint every
  client must decode; a breaking mailbox transport must ship on a new
  endpoint/proto package (e.g. `mailbox.v2`) rather than reuse this value.

This is distinct from `Envelope.ArkProtocolVersion`, a separately-versioned
field carried inside the same envelope for the higher-level Ark protocol —
see `version_compat_test.go` for the additive-compatibility guarantees that
keep old-shape envelopes decoding cleanly as new version fields are added.

## Relationships

- **Depends on**: nothing (generated proto types plus one constant).
- **Depended on by**: `serverconn` (constructs `Envelope`/`RpcMeta`,
  drives `Send`/`Pull`/`AckUpTo`, stamps `MailboxProtocolVersionV1`),
  `mailbox/conn` (`ResponseRegistry`, `WrappedProto`, status errors wrap
  `Status`/`Envelope`), `darepod` (server-side mailbox edge), `rpc/restclient`,
  `sdk/swaps`, `swapclientserver` (mailbox-backed clients).

## Invariants

- **Never edit generated code** — regenerate via `make rpc`.
- `MailboxProtocolVersionV1` must never change value; a new mailbox
  transport version is a new constant on a new endpoint, not a bump of
  this one.
- New wire fields (e.g. `ArkProtocolVersion`, the `SupportedMailboxVersions`
  / `SupportedArkVersions` lists on `Status`) must be additive so that
  peers running older code decode them as zero/empty rather than failing —
  see `version_compat_test.go` for the round-trip proofs this depends on.

## Deep Docs

- [mailbox/CLAUDE.md](../CLAUDE.md) — Parent mailbox package overview.
- [docs/mailbox_architecture.md](../../docs/mailbox_architecture.md) — Three-layer mailbox architecture.
- [docs/RPC_MAILBOX_CONTRACT.md](../../docs/RPC_MAILBOX_CONTRACT.md) — Envelope semantics and ack watermarks.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
