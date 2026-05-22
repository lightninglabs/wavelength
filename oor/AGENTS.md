# oor

## Purpose

Server-side out-of-round (OOR) transfer coordinator FSM. Manages direct VTXO
transfers between clients outside round periods: input locking, co-signing,
finalization, and recipient notification.

## Key Concepts

Use `go doc oor.<Symbol>` for signatures. Highlights:

- **`TransferCoordinatorActor` (alias `Actor`)** — Durable FSM driver
  implementing `ActorBehavior[OORDurableMsg, ActorResp]` directly. Each
  `ActorMsg` implements `TLVMessage` itself, so the durable mailbox
  serializes without an envelope layer. `StopAndWait(ctx)` for graceful
  shutdown.
- **Authoritative locking order (#215)**: FSM is
  `AwaitingSubmitValidationState` (owner-proof + rebuild + caps)
  → `AwaitingInputsLockState` → `ValidatedState`. Pre-lock validation
  failure cannot emit `UnlockInputsReq`, eliminating the phantom-unlock
  race that the old lock-then-validate order produced.
- **Recoverable finalize** — After Ark co-sign (the point-of-no-return),
  any finalize failure stores the reason in
  `CoSignedState.LastFinalizeFailureReason` and transitions back to
  `CoSignedState` (not `FailedState`), so clients can re-submit. The old
  `FailedState` transition on finalize errors is gone.
- **SIGHASH_DEFAULT enforcement** — `arkCoSignSigHashType = txscript.SigHashDefault`
  is package-level. `arkSigningLeaf` rejects PSBTs whose existing taproot
  signature records use a different sighash, blocking signature-reuse
  attacks via sighash weakening.
- **Tap-tree binding** — `verifyCheckpointTapTreeBindsToPkScript` (in
  `checkpoint_sweep.go`) rebuilds the expected output key from the
  encoded tap tree (must be exactly two leaves) and rejects a
  substituted/truncated tree that would produce an unspendable sweep.
- **Submit validation** — `validateSubmitOwnerProofs` (runs in
  `handleSubmit` before `LockInputsReq`) verifies each checkpoint uses
  the standard collaborative owner leaf and carries a valid Schnorr
  signature for it. `validateRecipientOutputsMatchArk` binds each
  recipient's optional `VTXOPolicyTemplate` to the on-chain pkScript
  via `template.PkScript() == recipient.PkScript`, closing the
  policy-template poisoning window before persistence (since the
  template gates indexer query auth).
- **Lineage vBytes cap** — `DriverCfg.MaxOORLineageVBytes` (default
  `DefaultMaxOORLineageVBytes = 25,000 vB`) bounds the cumulative chain
  weight to claim a produced VTXO unilaterally. Computed by
  `LineageVBytesEstimator` (production:
  `indexer.EstimateOORLineageVBytes`) inside one `Store.ExecReadTx` so
  parallel submits with overlapping lineage see a consistent ancestry
  graph. Exceeded submits emit `SubmitFailedEvent{Code:
  RejectCodeLineageTooLarge}`. The check runs **after** rebuild +
  owner-proof but **before** `LockInputsReq` — a cap rejection cannot
  trigger a phantom unlock. `ErrLineageWeightInternal` distinguishes
  internal failures (missing parents, etc.) so clients don't treat them
  as typed reject codes.
- **Typed rejections** — `SubmitOORResponse` carries either
  `CoSignedCheckpointPSBTs` (success) or `Rejection *SubmitOORRejection`
  (`Code RejectCode` + human `Reason`). `ToProto` emits the typed proto
  branch when `Rejection != nil`. `RejectCode` constants:
  `RejectCodeUnspecified`, `RejectCodeLineageTooLarge`.
- **Correlation keys** — `SubmitOORResponse.CorrelationKey()` and
  `FinalizeOORResponse.CorrelationKey()` both return
  `oorSessionCorrelationKey(clientID, sessionID)` (format
  `"<clientID>/oor/<sessionID>"`) so submit/finalize responses for the
  same session stay FIFO-ordered in the durable mailbox.
- **Request-size caps** (`enforceSubmit/FinalizeRequestLimits`): 64
  checkpoint PSBTs, 64 signing descriptors, 64 recipients per submit;
  64 checkpoint PSBTs per finalize; 64 KiB per PSBT blob. Enforced at
  the top of the handlers before expensive work.
- **Self-contained signing metadata** — `VTXOSigningDescriptor` carries
  `(outpoint, VTXOPolicyTemplate, SpendPath, OwnerLeafPolicy)`, all
  `arkscript`-encoded; replaces the old `(OwnerKey, ExitDelay)` shape.
  `encodeSigningDescriptor` refuses blobs that would fail decoding.
  Policy helpers (`policy_helpers.go`): `decodeDescriptorPolicyTemplate`,
  `decodeDescriptorSpendPath`, `validateSpendPathAgainstPolicy`,
  `resolveSpendPathLeaf`.
- **Persistence interfaces** — `SessionStore` is the base interface;
  `CoSignedAtomicStore` and `FinalizeAtomicStore` are optional extensions
  that fold transitions + lock/spent updates + recipient materialization
  into one transaction. `DBSessionStore` implements all three so the root
  package can wire it as `batchwatcher.CheckpointLookup`. The in-memory
  test path uses sequential `MarkSpent` / `Create` and is only acceptable
  when no session store is configured (it loses the late-failure window
  the atomic path closes).
  `SessionStore.LoadCheckpointTxByInput` returns the broadcastable
  finalized checkpoint tx that spent a given input (used by fraud
  response in batchwatcher). `CheckpointSweepInfo` is the narrow
  projection (`InputOutpoint`, `CheckpointTx`, `CheckpointOutputIndex`,
  `CheckpointOutput`, `TapTreeEncoded`) returned by
  `LoadCheckpointSweepInfoByInput`.
- **Checkpoint extraction** — `extractCheckpointTx` (in
  `checkpoint_extract.go`) handles both `FinalScriptWitness` (custom
  spends like vHTLC) and the standard tapscript path (reconstructs
  witness from `TaprootScriptSpendSig` + `TaprootLeafScript`, including
  pkScript binding verification).
- **Side-effect plumbing** — `RecipientNotifier` is best-effort
  recipient push (implemented by indexer). `ActorConfig.LedgerRef` is
  optional; when set, the actor `Tell`s
  `ledger.OORFinalizedMsg{SessionID}` post-finalize with zero amounts
  (ledger handler skips the fee leg until OOR fees ship — preserves the
  audit trail in the meantime).
- **Multi-tree ancestry** — Cross-commitment multi-input now produces one
  ancestry fragment per distinct commitment tx. Same-commitment
  multi-input still merges via `tryResolveCombinedRoundPath`. The
  graceful-degrade `mixedSingularLineage` path is removed —
  `len(AncestryPaths) >= 1` or hard error.

## Relationships

- **Depends on**: `clientconn` (response push), `db` (session persistence,
  atomic store), `vtxo` (locking + record materialization), `ledger`
  (optional).
- **Depended on by**: root `darepo` (wires `server_oor.go`), `indexer`
  (OOR event queries + `RecipientNotifier`).
- **Messages**:
  - Submit / finalize ← `clientconn` via `AddEnvelopeRoute` (ClientID from
    `env.Sender`).
  - `SubmitOORResponse` / `FinalizeOORResponse` → originating client via
    `ClientsConn.Tell()` (wrapped in `SendServerEventRequest`).
  - `RecipientNotifier.NotifyRecipientEvent()` → indexer.
  - `OORFinalizedMsg` → `ledger` via `LedgerRef`.
  - Session reads/writes → `db` (atomic finalize+materialize when
    `FinalizeAtomicStore` is wired).

## Invariants

- VTXO owner-proof passes before the server acquires locks.
- Co-signing is atomic — all inputs or none.
- Ark PSBTs are co-signed **before** OOR packages persist (sign-then-persist
  ordering fix).
- Recipients notified only after finalization persists.
- Failed transfers release all VTXO locks + clean up the session map entry.
- DB-backed finalize is single-transaction: session transition + mark
  inputs spent + materialize recipient outputs. Materialized recipient
  records are computed **before** any mutation so metadata lookup errors
  fail fast.
- `FinalCheckpointPSBTs` are threaded through FSM states so they survive
  restart for re-notification of `AwaitingRecipientsNotify` sessions.
- `VTXOSigningDescriptor.VTXOPolicyTemplate` and `SpendPath` are both
  required.
- `validateRecipientOutputsMatchArk` must pass before session recipients
  are stored.
- Recipients from `SubmitOORRequest` are propagated into
  `FinalizeReq.Recipients` in `askAndDrive` so the client doesn't
  re-send them.
- Outcomes are instrumented via metrics actor
  (`OORTransferStartedMsg`/`OORTransferCompletedMsg`).

## Deep Docs

- [docs/authoritative_locking.md](../docs/authoritative_locking.md) —
  Server-side locking model.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide map.
