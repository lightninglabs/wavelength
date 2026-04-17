# oor

## Purpose

Server-side out-of-round (OOR) transfer coordinator FSM. Manages direct VTXO
transfers between clients outside of round periods, handling input locking,
co-signing, finalization, and recipient notification.

## Key Types

- `TransferCoordinatorActor` (alias `Actor`) — Durable OOR transfer coordinator
  with FSM state persistence and `ClientsConn` push for response delivery.
  Implements `ActorBehavior[OORDurableMsg, ActorResp]` directly (no intermediate
  behavior wrapper). Each `ActorMsg` type implements `TLVMessage` directly
  (TLVType, Encode, Decode), so the durable mailbox serializes messages without
  an intermediate envelope layer. Exposes `StopAndWait(ctx)` for graceful
  shutdown.
- `OORDurableMsg` — Message constraint for the durable actor mailbox; embeds
  `actor.TLVMessage` so both application messages and framework restart messages
  satisfy it.
- `SessionID` — OOR transfer session identifier (derived from ArkTxid).
- `State` — Sealed interface for FSM states (Idle through Finalized/Failed).
- `Event` — Inbound events (SubmitRequest, FinalizeRequest, etc.).
- `OutboxEvent` — Outbound side effects (notify recipients, persist state).
- `SubmitOORRequest` / `FinalizeOORRequest` — Primary actor messages implementing
  `TLVMessage` directly (dispatched via `AddEnvelopeRoute` from `server_oor.go`).
- `SubmitOORResponse` / `FinalizeOORResponse` — Response types implementing
  `clientconn.ClientMessage` for push delivery via `ClientsConn.Tell()`.
- `InProcessOutboxDriver` — Reusable outbox handler for the OOR FSM session
  lifecycle (lock, validate, co-sign, finalize, notify). On finalize it
  computes the materialized recipient `vtxo.Record` set first (so metadata
  lookup failures fail fast before any mutation), then delegates to either
  the atomic DB path or the in-memory test path.
- `SessionStore` — Interface for durable OOR session persistence. Includes
  `UpsertCoSigned`, `ApplyFinalize`, `MarkNotified`, `GetSessionState`,
  `LoadActiveSessions`, `LoadFinalizedPackage`, and
  `LoadCheckpointTxByInput` (newly added: returns the broadcastable
  finalized checkpoint tx that spent a given input, for fraud-response
  wiring in batchwatcher).
- `DBSessionStore` — Concrete DB-backed `SessionStore` implementation in
  `session_store_db.go`. Implements all `SessionStore`, `CoSignedAtomicStore`,
  and `FinalizeAtomicStore` methods. Exported so the root package can hold a
  typed reference (`Server.oorSessionStore`) and wire it as
  `batchwatcher.CheckpointLookup` before the batch watcher actor is spawned.
- `CoSignedAtomicStore` — Optional session-store extension that applies the
  co-signed transition and input locking in one transaction.
- `FinalizeAtomicStore` — Optional session-store extension (implemented by
  `DBSessionStore`) that applies the finalized checkpoint set, marks
  consumed inputs spent, and materializes recipient outputs in one
  transaction. Required when both a `vtxo.Store` and a session store are
  configured.
- `RecipientNotifier` — Interface for best-effort recipient notification after
  durable event persistence; implemented by the indexer layer.
- `RecipientEventStore` — Persists per-recipient notification cursors and
  payloads.
- `VTXOSigningDescriptor` — Per-input signing metadata (outpoint,
  `VTXOPolicyTemplate`, `SpendPath`, `OwnerLeafPolicy`) threaded through the
  FSM for checkpoint construction. Replaces the old `(OwnerKey, ExitDelay)`
  shape; all fields are serialized bytes using the `arkscript` encoding.
- `enforceSubmitRequestLimits` / `enforceFinalizeRequestLimits` — Request-size
  caps applied before expensive validation: max 64 checkpoint PSBTs, 64 signing
  descriptors, 64 recipient outputs per submit; max 64 checkpoint PSBTs per
  finalize; max 64 KiB per PSBT blob.
- Policy helpers in `policy_helpers.go`: `decodeDescriptorPolicyTemplate`,
  `decodeDescriptorSpendPath`, `validateSpendPathAgainstPolicy`,
  `resolveSpendPathLeaf` — decode and bind policy templates to spend paths;
  `resolveSpendPathLeaf` returns the matched AST node for downstream AST-level
  operator key checks.
- `validateRecipientOutputsMatchArk` — Binds each recipient's optional
  `VTXOPolicyTemplate` to its on-chain pkScript by checking
  `template.PkScript() == recipient.PkScript`. Prevents policy-template
  poisoning (attaching a template whose participant set is unrelated to the
  output), since the persisted template is what indexer query-auth consults.

## Relationships

- **Depends on**: `clientconn` (response push via `ClientsConn`), `db` (OOR
  session persistence, `FinalizeAtomicStore`), `vtxo` (VTXO locking and
  record materialization during transfers).
- **Depended on by**: root `darepo` (wiring in `server_oor.go`), `indexer`
  (OOR event queries, `RecipientNotifier` implementation).
- **Messages to/from**:
  - Receives submit/finalize requests <- `clientconn` via `AddEnvelopeRoute`
    (fire-and-forget Tell from clients; ClientID extracted from `env.Sender`).
  - Pushes `SubmitOORResponse`/`FinalizeOORResponse` -> originating client via
    `ClientsConn.Tell()` (wrapped in `SendServerEventRequest`).
  - Calls `RecipientNotifier.NotifyRecipientEvent()` -> indexer layer for
    best-effort recipient push after finalization.
  - Reads/writes OOR session state -> `db` (including the atomic
    finalize+materialize path when `FinalizeAtomicStore` is implemented).

## Invariants

- VTXO inputs must be locked before validation proceeds (prevents
  double-spend).
- Co-signing happens atomically: either all inputs are co-signed or none.
- Ark PSBTs are co-signed before persisting OOR packages (ordering fix: sign
  then persist, not persist then sign).
- Recipients are notified only after finalization is persisted.
- Failed transfers must release all VTXO locks and clean up the session map
  entry to prevent leaks.
- Finalize must apply the session transition, mark consumed inputs spent, and
  materialize recipient outputs in a single transaction when a DB-backed
  store is configured. The in-memory test path uses sequential
  `MarkSpent`/`Create` calls and is only acceptable when no session store is
  configured. This closes the late-failure window where inputs could be
  marked spent before recipient outputs and `awaiting_notify` were durable.
- Materialized recipient records are computed **before** any mutation in the
  finalize path so metadata lookup or validation errors fail fast.
- `FinalCheckpointPSBTs` are threaded through FSM states so they survive
  restart and are available for re-notification of `AwaitingRecipientsNotify`
  sessions.
- Self-contained VTXO spend metadata (outpoint, `VTXOPolicyTemplate`,
  `SpendPath`, `OwnerLeafPolicy`) is persisted alongside OOR packages for
  checkpoint construction. The old `(OwnerKey, ExitDelay)` descriptor shape
  is replaced by policy bytes.
- `VTXOSigningDescriptor.VTXOPolicyTemplate` and `SpendPath` must both be
  non-empty; `encodeSigningDescriptor` refuses blobs that would fail decoding.
- Request-size caps are enforced at the top of `handleSubmit`/`handleFinalize`
  before any expensive work; a well-behaved client never trips these bounds.
- `validateRecipientOutputsMatchArk` must succeed before session recipients are
  stored; it checks both pkScript/value and the policy-template-to-pkScript
  binding to close the read-access poisoning window.
- Recipients captured from `SubmitOORRequest` are propagated into
  `FinalizeReq.Recipients` during the `askAndDrive` outbox loop so the
  finalize path has full recipient metadata without requiring the client to
  re-send it.
- OOR transfer outcomes are instrumented via metrics actor events
  (`OORTransferStartedMsg`/`OORTransferCompletedMsg`).
- Structured logging emits at every key lifecycle event (submit, co-sign,
  finalize, restore, lock/unlock, validation, notification).

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
