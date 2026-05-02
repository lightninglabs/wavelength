# rpc/oorpb

## Purpose

Generated protobuf/gRPC stubs for the OOR (out-of-round) transfer protocol,
plus hand-written `payloads.go` containing typed builder/parser helpers and
the canonical mailbox method name constants used for routing OOR
client↔server messages through the durable transport layer.

## Key Types

All `*.pb.go` files are generated — never edit directly; regenerate with
`make rpc`. The manually-maintained `payloads.go` defines:

- `ServiceName` — Fully-qualified mailbox service name
  (`"oorpb.OORMailboxService"`) used for mailbox event routing.
- `MethodSubmitPackage`, `MethodFinalizePackage`, `MethodIncomingAck` —
  RPC method name constants for the three OOR wire flows.
- `SigningDescriptor` — Minimal co-signing metadata (outpoint, policy
  template, spend path, owner-leaf policy) passed from client to server.
- `SubmitRejectedError` — Typed error returned by `ParseSubmitPackageResponse`
  when the server issued a rejection; callers route on `Code` without
  string-matching `Reason`.

## Relationships

- **Depends on**: `lib/tx/oor` (RecipientOutput), `lib/tx/psbtutil`
  (PSBT encode/decode helpers), `btcd/wire`, `btcd/btcutil/psbt`.
- **Depended on by**: `oor` (builds/parses OOR protocol messages),
  `serverconn` (mailbox method dispatch by ServiceName/Method constants).

## Invariants

- **Never edit generated code** — regenerate via `make rpc`.
- Method name constants in `payloads.go` must match the proto service
  definition; mismatches silently drop events at the mailbox router.
- `SubmitPackageResponse` carries either a `Success` or `Rejection` branch;
  `ParseSubmitPackageResponse` returns `*SubmitRejectedError` for the
  rejection case — callers must not treat a non-nil error as an I/O failure.

## Deep Docs

- [rpc/CLAUDE.md](../CLAUDE.md) — Parent rpc package overview.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
