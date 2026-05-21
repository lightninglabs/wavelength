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
  `SubmitOORResponse` carries either `CoSignedCheckpointPSBTs` (success) or a
  non-nil `Rejection *SubmitOORRejection` (failure); `ToProto` emits the
  typed proto rejection branch when `Rejection != nil`.
- `SubmitOORRejection` — Typed rejection carrier embedded in
  `SubmitOORResponse.Rejection`. Holds `Code RejectCode` (maps to the
  proto `OOR_REJECT_LINEAGE_TOO_LARGE` enum value) and a human-readable
  `Reason` for logs/UX.
- `RejectCode` — Typed uint8 discriminator for submit failures. Constants:
  `RejectCodeUnspecified` (zero, generic rejection) and
  `RejectCodeLineageTooLarge` (lineage vbytes cap exceeded). Clients route on
  this code (e.g. fall back to in-round payment) without string-matching.
- `DefaultMaxOORLineageVBytes` — Default operator cap (25,000 vB) on the
  cumulative on-chain virtual bytes required to claim a produced VTXO
  unilaterally. Used as the config default in `Config.MaxOORLineageVBytes`.
- `ErrLineageWeightExceeded` — Sentinel returned by the lineage cap check when
  the cumulative input lineage exceeds the operator's cap; surfaced as
  `SubmitFailedEvent{Code: RejectCodeLineageTooLarge}`.
- `ErrLineageWeightInternal` — Sentinel distinguishing operator cap rejections
  from internal computation failures (e.g., missing parent rows). Clients must
  not interpret internal failures as typed reject codes.
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
- `validateSubmitOwnerProofs(ark, checkpoints, descs, policy)` — Verifies
  that each checkpoint consumed by the Ark package uses the standard
  collaborative owner leaf and carries a valid Schnorr owner signature for
  that leaf. Rebuild validation confirms descriptors match authoritative VTXO
  records; this function adds the possession proof — the submitter must
  demonstrate control of the claimed owner key before the server acquires a
  shared lock. Runs inside `handleSubmit` before `LockInputsReq` is emitted.

- `CheckpointSweepInfo` — Narrow projection of OOR persistence data needed
  by the fraud responder to reconstruct a checkpoint timeout sweep:
  `InputOutpoint`, `CheckpointTx`, `CheckpointOutputIndex`, `CheckpointOutput`,
  and `TapTreeEncoded`. Returned by
  `DBSessionStore.LoadCheckpointSweepInfoByInput`.
- `extractCheckpointTx` (in `checkpoint_extract.go`) — Extracts a broadcastable
  `*wire.MsgTx` from a finalized OOR checkpoint PSBT. Handles both
  `FinalScriptWitness` (custom spends such as vHTLC) and the standard
  collaborative tapscript path (reconstructs witness from
  `TaprootScriptSpendSig` + `TaprootLeafScript`, including pkScript binding
  verification).
- `ActorConfig.LedgerRef` — Optional `fn.Option[actor.TellOnlyRef[ledger.LedgerMsg]]`
  wired by the root package. When set, the actor sends
  `ledger.OORFinalizedMsg{SessionID}` to the ledger actor after each OOR
  finalization (fire-and-forget). Currently carries zero input/output amounts
  because the OOR pipeline has not yet threaded value through the finalize
  event; the ledger handler skips the fee leg when `fee = input - output`
  is zero, so this is a no-op on accounting but ensures the audit trail
  captures every finalized session for future fee schedule activation.
- `CoSignedState.LastFinalizeFailureReason string` — Stores the reason from
  the most recent failed finalize attempt. Introduced so that when finalization
  fails after the Ark package has already been co-signed (the point-of-no-return),
  the FSM transitions back to `CoSignedState` rather than `FailedState`. This
  keeps the session retryable: the client can re-submit a `FinalizeOORRequest`
  without losing the co-signed checkpoints. The old path that moved to
  `FailedState` on any finalize error is removed.
- `arkCoSignSigHashType = txscript.SigHashDefault` — Package-level constant
  enforcing that all Ark co-sign operations use `SIGHASH_DEFAULT` exclusively.
  `arkSigningLeaf` now validates that every existing taproot signature record
  uses this sighash; a mismatch returns a hard error, preventing the operator
  from co-signing with a weakened or non-default sighash that could enable
  signature reuse attacks.
- `verifyCheckpointTapTreeBindsToPkScript` — New validation in
  `checkpoint_sweep.go` that reconstructs the expected taproot output key from
  the checkpoint's encoded tap tree and verifies it matches the checkpoint
  output's pkScript. Rejects checkpoints whose tap tree was substituted or
  tampered with between submission and sweep.
- `SubmitOORResponse.CorrelationKey()` / `FinalizeOORResponse.CorrelationKey()`
  — Return `oorSessionCorrelationKey(clientID, sessionID)` (format:
  `"<clientID>/oor/<sessionID>"`). Ensures submit and finalize responses for
  the same session are claim-ordered in the durable mailbox regardless of
  transient send failures.

## Relationships

- **Depends on**: `clientconn` (response push via `ClientsConn`), `db` (OOR
  session persistence, `FinalizeAtomicStore`), `vtxo` (VTXO locking and
  record materialization during transfers), `ledger` (optional
  `OORFinalizedMsg` notification via `LedgerRef`).
- **Depended on by**: root `darepo` (wiring in `server_oor.go`), `indexer`
  (OOR event queries, `RecipientNotifier` implementation).
- **Messages to/from**:
  - Receives submit/finalize requests <- `clientconn` via `AddEnvelopeRoute`
    (fire-and-forget Tell from clients; ClientID extracted from `env.Sender`).
  - Pushes `SubmitOORResponse`/`FinalizeOORResponse` -> originating client via
    `ClientsConn.Tell()` (wrapped in `SendServerEventRequest`).
  - Calls `RecipientNotifier.NotifyRecipientEvent()` -> indexer layer for
    best-effort recipient push after finalization.
  - Sends `OORFinalizedMsg` -> `ledger` (fire-and-forget via `LedgerRef`
    after finalization; zero-value amounts until OOR fee pipeline is wired).
  - Reads/writes OOR session state -> `db` (including the atomic
    finalize+materialize path when `FinalizeAtomicStore` is implemented).

## Invariants

- **Lineage-vbytes cap enforced before VTXO lock.** When
  `DriverCfg.MaxOORLineageVBytes > 0` the cumulative on-chain virtual
  bytes required to claim the produced VTXO unilaterally is computed
  by the configured `LineageVBytesEstimator` (production wiring uses
  `indexer.EstimateOORLineageVBytes`) and compared against the cap.
  Submits that exceed produce `SubmitFailedEvent{Code: RejectCodeLineageTooLarge}`
  and the FSM transitions to `FailedState` carrying the same code. The
  check runs **after** rebuild + owner-proof validation but **before**
  `LockInputsReq`, so a cap rejection cannot trigger a phantom
  unlock. The cap arithmetic walk runs inside a single
  `Store.ExecReadTx` snapshot so per-call store queries see a
  consistent ancestry graph; this eliminates intra-cap inconsistency
  between two parallel submits whose lineages overlap.
- **Cross-round multi-input produces multi-tree ancestry.** Same-commitment
  multi-input continues to merge into one spanning subtree via
  `tryResolveCombinedRoundPath`; cross-commitment multi-input now
  produces one ancestry fragment per distinct contributing commitment
  tx. The `mixedSingularLineage` graceful-degrade path is
  removed — the indexer lineage resolver returns `len(AncestryPaths) >= 1`
  or hard-errors.

- **Submit validation precedes VTXO locking.** The authoritative locking
  model (PR #215) reordered the FSM: the sequence is now
  `AwaitingSubmitValidationState` (owner-proof + rebuild + limit checks)
  → `AwaitingInputsLockState` → `ValidatedState`. Validation failure before
  the lock is acquired never triggers an `UnlockInputsReq`, preventing a
  phantom unlock race. The old order (lock → validate) is gone.
- VTXO inputs must pass owner-proof validation before the server acquires
  locks (enforces possession before commitment).
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
- **OOR FSM remains in `CoSignedState` after finalize failures past the
  point-of-no-return.** Once the Ark package is co-signed, any finalize
  failure stores the failure reason in `LastFinalizeFailureReason` and
  transitions back to `CoSignedState` (not `FailedState`). This allows the
  client to re-submit a finalize request. The old `FailedState` transition
  on finalize errors is removed — the FSM is now fully recoverable from
  transient finalize failures.
- **Ark co-sign MUST use `SIGHASH_DEFAULT`.** `arkSigningLeaf` rejects any
  PSBT where an existing taproot signature record uses a sighash other than
  `SIGHASH_DEFAULT`. Callers must not mutate the sighash on inputs before
  passing PSBTs to the co-signer.
- **Checkpoint tap-tree must bind to the checkpoint output's pkScript.**
  `verifyCheckpointTapTreeBindsToPkScript` is called during checkpoint sweep
  construction. It requires exactly two tap leaves and verifies the assembled
  taproot output key matches the output's pkScript, preventing a substituted
  or truncated tap tree from producing a sweep that never confirms.

## Deep Docs

- [docs/authoritative_locking.md](../docs/authoritative_locking.md) — Server-side locking model: ownership rules, FSM ordering, recovery invariants.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
