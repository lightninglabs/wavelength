# rpc/oorpb

## Purpose

Generated protobuf/gRPC stubs for the out-of-round (OOR) transfer mailbox
protocol. Carries Ark PSBTs, signing descriptors, recipient outputs, and
server co-signed checkpoint responses between client and server for OOR
transfers.

## Key Types

- `OORMailboxService` — Two mailbox methods:
  - `SubmitPackage` — Client submits an OOR package (inputs, recipients,
    policies, fee rate, session/idempotency keys). Server responds with
    co-signed checkpoint PSBTs and a server-assigned session ID.
  - `FinalizePackage` — Client submits finalized checkpoint PSBTs;
    server confirms the session.
- `SubmitPackageRequest` — Carries: `ArkPsbt`, `SigningDescriptors`,
  `RecipientOutputs`, `FeeRateSatPerVbyte`, `SessionId` (optional resume),
  `IsDryRun`, `IdempotencyKey`.
- `SubmitPackageResponse` — Carries: `CheckpointPsbts` (one per recipient),
  `SessionId` (server-assigned).
- `OORSigningDescriptor` — Per-input signing metadata: `Outpoint`,
  `VtxoPolicyTemplate`, `SpendPath`, `OwnerLeafPolicy`.
- `OORRecipientOutput` — Recipient output spec: `PkScript`, `VhtlcPolicy`,
  `AmountSat`.
- `OOROutPoint` — Protobuf representation of a Bitcoin outpoint (txid bytes
  + vout index).
- `SubmitRejectedError` — Server-side rejection with a rejection code
  (e.g., `OOR_REJECT_LINEAGE_TOO_LARGE`).
- `FinalizePackageRequest/Response` — Finalize the OOR session after
  client fully signs checkpoint PSBTs.

## Relationships

- **Depends on**: nothing (pure proto-generated types).
- **Depended on by**: `oor` (OOR FSM uses these types for message marshaling),
  `darepod` (OOR orchestration), `systest` (fake operator mailbox).

## Invariants

- **Never edit generated code** — regenerate via `make rpc`.
- Proto source: `rpc/oorpb/oorwire.proto`.
- `IdempotencyKey` is stable across retries; `SessionId` is the server's
  durable identifier assigned on first successful submit.

## Deep Docs

- [oor/CLAUDE.md](../../oor/CLAUDE.md) — OOR FSM package using these types.
- [rpc/CLAUDE.md](../CLAUDE.md) — Parent rpc package overview.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
