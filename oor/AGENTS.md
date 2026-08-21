# oor

## Purpose

Client-side out-of-round (OOR) VTXO transfer coordination: lets a client send
VTXOs to one or more recipients without waiting for a normal round, while
keeping transaction construction deterministic and resume semantics
crash-safe. Built on `baselib/protofsm`: I/O is modeled as outbox requests
that a durable actor executes and feeds back as events.

## Key Types

For field-level detail, use `go doc github.com/lightninglabs/wavelength/oor.<Symbol>`.

- `OORSessionActor` — one durable actor per OOR session (outgoing or
  incoming). Its `driveOutbox` switch handles every outbox event inline
  (signs Ark/checkpoint PSBTs, enqueues transport to `serverconn`,
  materializes incoming VTXOs, schedules retries) on the Read/Commit turn;
  there is no separate signing actor.
- `OORRegistryActor` — durable coordinator registered under the OOR service
  key. Routes messages to the owning session's child, lazily spawns
  children, dedups outgoing transfers by idempotency key, reaps terminated
  children, and respawns/resumes non-terminal sessions on boot.
- `Session` / `ReceiveSession` — outgoing/incoming FSM state containers;
  `OutgoingSnapshot` / `IncomingSnapshot` are their durable, versioned
  serializations.
- `OutboxHandler` / `LocalPersistenceOutboxHandler` — handles the local
  persistence outbox events (mark-inputs-spent, incoming metadata query,
  VTXO materialization, ack); everything else is handled inline by the
  session actor.
- `ReceiveLimits` / `DefaultReceiveLimits` — defense-in-depth bounds on
  incoming receive (`MaxCheckpoints`, `MaxVTXOMatches`, `MaxMailboxItems`,
  `MaxMailboxScriptBytes`, `MaxConcurrentIncomingSessions`).
- `TransferInput` (`transfer_inputs.go`) — one selected spend input: the
  VTXO descriptor, its policy template, optional `TaprootAssetRoot`,
  owner leaf, custom spend path, and the two asset markers
  `OperatorFunded` / `TaprootAssetRoundCreated` (see the Taproot Asset
  section below). `TransferInputSnapshot`
  (`transfer_input_snapshot.go`) is its durable form.
- `TaprootAssetOORIntent` / `TaprootAssetOORPrepareRequest` /
  `TaprootAssetOORPreparation` / `TaprootAssetOORPreparer`
  (`taproot_asset_preparer.go`) — the declarative asset-transfer contract
  and its driver seam. This package owns validation and carrier
  arithmetic only; the concrete driver is `tapassets.Preparer`, which
  imports `oor` (never the reverse).
- `TaprootAssetCarrierPlan` — the satoshi layout an asset transfer must
  produce: `AssetChange`, `SenderChange`, `OperatorChange`. Computed by
  `TaprootAssetOORPrepareRequest.CarrierAllocation()`.
- `TaprootAssetOORPreparationResumer` / `TaprootAssetOORResumeRequest` /
  `TaprootAssetOORResume` — restart adoption of a journaled preparation:
  returns the outpoint set and lease the pre-crash attempt committed to.
- `OORCarrierLease` (`carrier_lease.go`) — an operator carrier-float VTXO
  leased for one transfer: `Outpoint`, `Value`, `PolicyTemplate`,
  `PkScript`, `ExpiresAtUnix`.
- `MaxTaprootAssetInputs = 8` — hard cap on asset VTXOs merged into one
  transfer.

## Relationships

- **Depends on**: `baselib/protofsm` (FSM), `baselib/actor` (durable actor
  framework), `serverconn` (submit/finalize/query transport), `vtxo`
  (materialization + status), `ledger` (`Sink`, accounting emission),
  `timeout` (`TimeoutActor` retry scheduling), `lib/arkscript` (checkpoint
  policy, collab tapleaf, `ComposeWithSiblingRoot` for composed asset
  scripts), `lib/tx/oor` (`RecipientOutput`, asset transfer container),
  `arkrpc` (indexer response types), `lnd/input` (signer interface for
  inline Ark/checkpoint signing).
- **Depended on by**: `waved` (spawns the registry, wires config, drives
  RPCs and event routing), `tapassets` (implements
  `TaprootAssetOORPreparer` against these types).
- **Messages to/from**: Sends `SendSubmitPackageRequest` /
  `SendFinalizePackageRequest` / `SendIncomingAckRequest` and durable query
  requests (`QueryIncomingTransferRequest`, `QueryIncomingMetadataRequest`)
  -> `serverconn`; `MaterializeIncomingVTXOsRequest` -> wallet/VTXO store;
  `VTXOSentMsg`/`VTXOReceivedMsg` -> `ledger` (when `LedgerSink` is set).
  Receives `SubmitAcceptedEvent` / `FinalizeAcceptedEvent` /
  `ResolveIncomingTransferRequest` <- `serverconn` event router;
  `StartTransferRequest` / `DriveEventRequest` / `ListSessionsRequest` <-
  `waved` RPC layer.

## Taproot Asset Transfers

An asset-bearing VTXO pays to `taproot(internal_key,
tapBranch(sorted(policy_root, taproot_asset_root)))`. The asset root is a
sibling of the ordinary Ark policy root, so every Ark spend path needs
that root as its final control-block sibling —
`EffectiveSpendPath` and `defaultVTXOPolicyTemplate`
(`transfer_inputs.go`) extend the collab control block, and
`validateTaprootAssetPkScript` re-derives the composed script and
requires byte equality before signing.

### One transfer, N inputs

`TaprootAssetOORIntent.InputVTXOOutpoints` is an **ordered set**, spine
first. Up to `MaxTaprootAssetInputs = 8` asset VTXOs are merged into
**one** atomic transfer: N asset checkpoints feed a single merged ark
transition, never several sequential transfers. The spine is transition
input 0 because tap-sdk emits the merged transition with `PrevOut` set
to vPacket input 0. `TaprootAssetOORIntent.Validate` rejects duplicate
outpoints, more than 8, and a caller-supplied `ProofFile` alongside more
than one input (a multi-input transfer resolves each base proof itself).
`TaprootAssetOORPrepareRequest.Validate` separately bounds the package
count at `oortx.MaxTaprootAssetCheckpointPackages = 64`.

### The two input markers

`TransferInput` carries two booleans that the rest of the subsystem
switches on. Both describe *whose satoshis* an input is, which is why
they change signing and change-output arithmetic rather than script
construction.

- `OperatorFunded` — the input is an operator carrier-float VTXO leased
  through `arkrpc.LeaseOORCarrier`. The local wallet holds no key for
  it, so **every** local signing and normalization site must skip it:
  `SignCheckpointPSBTs` (`checkpoint_sign.go`),
  `signArkPSBTInput` (`ark_sign.go`), and
  `NormalizeCheckpointOwnerLeaves` (`transitions.go`) — the last one
  because normalizing would overwrite the float's lease-supplied owner
  leaf with a locally derived one. `WalletInputOutpoints` and
  `queueVTXOSent` also exclude it, so the float is neither reserved as
  wallet liquidity nor booked as value sent. The operator's own
  signatures on both legs are still verified
  (`validateOperatorCheckpointSignatures`).
- `TaprootAssetRoundCreated` — the asset leaf came out of a round
  (boarded or refreshed), so its Bitcoin carrier is the sender's own
  money and returns as plain sender change. An OOR-created leaf rides an
  operator-funded carrier instead, which is *reclaimed* into the
  operator's change when the leaf is spent. `BuildTransferInputs`
  (`waved/wallet_ops.go`) derives it as
  `desc.TaprootAssetRef != "" && len(desc.TaprootAssetSealedPackage) > 0`
  — only a round-created leaf stores a sealed package.

### Carrier allocation and reclaim

`TaprootAssetOORPrepareRequest.CarrierAllocation()` is the single place
the satoshi layout is decided, with `leafCount =
Intent.NewAssetLeafCount()` (2 on a partial send, 1 otherwise) and
`floors = OutputFloor * leafCount`:

```
recipient leaf   = OutputFloor                          (always)
AssetChange      = OutputFloor                          (partial send only)
SenderChange     = Σ carriers of ROUND-created inputs   (omitted when 0)
OperatorChange   = Lease.Value - floors
                     + Σ carriers of OOR-created inputs (reclaim)
```

`splitAssetInputCarriers` performs that split. A lease worth less than
`floors` is refused outright. `tapassets.Preparer.plannedRecipients`
materializes the outputs in that order — receiver, asset change, sender
change, operator change — and `TaprootAssetOORPreparation.Validate`
re-checks all of it: exactly one output paying `Lease.PkScript` for
`OperatorChange`, a sender-change output iff `SenderChange > 0`, every
new asset leaf exactly at `OutputFloor`, and asset units conserved
against `Intent.AssetAmount`.

The practical consequence: **a sender needs no Bitcoin at all for an
asset OOR send.** Bitcoin in Ark is for round fees.

## Invariants

- Checkpoint collab output is 2-of-2
  (`arkscript.MultiSigCollabTapLeaf(clientKey, operatorKey)`), never
  single-sig; resumed custom-spend inputs are re-verified against the VTXO
  pkScript before signing.
- Point-of-no-return is server co-signing of the checkpoint transaction(s):
  after that, the client must resume with byte-identical co-signed PSBTs
  (deterministic construction), not re-derive them.
- Signing is inline and durable-by-construction: the session actor signs
  within its Read/Commit turn, so the signed transport outbox commits in the
  same transaction as the FSM advance.
- Transport sends (submit/finalize/ack) are delivered into `serverconn`'s
  durable mailbox inside the OOR commit transaction; the actual wire send
  happens later on serverconn's own egress turn and is retried there — OOR
  does not run a separate outbox publisher for transport.
- Incoming receive never performs a synchronous unary RPC inside the durable
  actor's DB transaction; both phase-1 hint resolution and phase-2
  authoritative metadata lookup go through durable `serverconn` query
  messages and return as fresh events.
- Point-of-no-return moves **earlier** for an asset transfer:
  `prePONRInputOutpoints` returns nil whenever
  `TaprootAssetTransfer != nil`, because the tapd commit already
  happened and releasing the inputs would strand a committed transition.
  Ordinary Bitcoin transfers still release their inputs up to server
  co-signing.
- `TaprootAssetTransfer` rides byte-identically through
  `AwaitingArkSignatures` → `AwaitingSubmitAccepted` →
  `AwaitingCheckpointSignatures` → `AwaitingFinalizeAccepted` and across
  submit retries; the asset path enters the FSM through
  `evt.PreparedSubmit` instead of `buildSubmitPackage`, since the package
  was built (and journaled) by the preparer before the session started.
  Restore re-validates it against the persisted checkpoint count.
- `TaprootAssetRoundCreated` is deliberately **not** in the snapshot TLV.
  It is re-derived per request by `BuildTransferInputs` from the
  descriptor's sealed package, and carrier arithmetic runs at the RPC
  edge before the point of no return, so nothing after a restart needs
  it. Adding a resume path that recomputes `CarrierAllocation()` would
  have to re-derive it the same way, never read it back from a snapshot.
- Snapshots are versioned per direction (`OutgoingSnapshot.Version = 8`,
  `IncomingSnapshot.Version = 1`); restore rejects a zero version. Outgoing
  v5 adds the `FirstRejectUnixNanos` record (bounded transient submit-reject
  retry window); a pre-v5 snapshot decodes it to 0 (a fresh window). v8
  adds `transferInputOperatorFundedRecordType = 21` inside each transfer
  input record, so a resumed session never re-attempts local signing on a
  leased float; the field is an optional appended TLV, so older snapshots
  still decode. TLV record numbers are per-record-block, so 21 is also in
  use by unrelated blocks (`eventPayloadRetryAfterNanosRecordType`,
  `snapshotIdempotencyKeyRecordType`,
  `incomingMetadataMatchAssetRootRecordType`) — read the block, not the
  number.
- `StartTransferRequest.IdempotencyKey` dedup relies on a partial UNIQUE
  index on `oor_session_registry` (at most one live-or-completed row per
  key); a failed session never blocks a keyed retry.
- `MaxConcurrentIncomingSessions` (default 1024) is enforced in the
  registry's `ensureChild` choke point, the only path that makes a session
  resident, so every admission path (RPC, routed message, boot restore)
  shares the same bound.
- Witness/script decode bounds mirror consensus limits:
  `maxConditionWitnessItems = 64` items of at most 520 bytes each (Bitcoin's
  `MAX_SCRIPT_ELEMENT_SIZE`), enforced on both encode and decode.
- Terminal rows (completed and failed) are retained in
  `oor_session_registry` for status/diagnostics; reaping only removes the
  in-memory child, never the row.
- Outgoing finalize ordering: input-spend completion runs inline with no OOR
  writer transaction held, because its write commits in the VTXO manager's
  own transaction; awaiting that second writer under a held OOR writer lock
  would deadlock the single SQLite/Postgres writer.
- The registry's detached-continuation wait on a spawned child (`OnComplete`)
  is bounded solely by wrapping `DetachedAsk.CallerCtx` in
  `context.WithTimeout(detachedWaitTimeout)` (5m); the phantom-reap guard
  keys off that wrapped context's error, not the raw caller ctx, so a
  timed-out wait is treated as a benign hang-up and never reaps a
  still-signing session.
- Idempotency-key dedup on Postgres is a commit-race, not a pre-check: losing
  children collide on the partial UNIQUE index in `commitAck`, roll back, and
  redeliver; the redelivered `resolveKeyDedup` then sees the winner's
  committed row and consumes cleanly as `Existing`.
- `handleStartTransfer` answers `Existing: true` for a resident outgoing
  child only after confirming a durable row via `GetSession`; a row-less
  phantom (pending async reap via `SessionTerminalNotification`) is dropped
  synchronously and falls through to a fresh admission instead of wedging a
  same-input retry.
- A late duplicate server push for a terminal, already-reaped session routes
  through the registry; `handleDriveEvent` acks it as an idempotent no-op
  (`sessionIsTerminal`) rather than erroring, since only a genuinely-unknown
  session should Nack.
- Incoming resolve/metadata retries give up terminally once their persisted
  attempt counts reach `maxResolveRetries` / `maxMetadataRetries` (20),
  freeing the session's concurrency slot instead of pinning a child forever
  on operator silence.
- Incoming ancestor packages are capped at `maxAncestorPackages = 64`
  checkpoints, and indexer-supplied `tree_depth` is cross-checked against the
  reconstructed path via `arkrpc.ValidateAncestryPathDepth`.
- Server-side lineage-cap rejection surfaces as a typed `*ErrLineageTooLarge`
  via `ClassifySubmitError`, so wallet callers can switch on the cause
  without depending on the `oorpb` proto type.

## Deep Docs

- [oor/doc.go](doc.go) — Package overview.
- [docs/oor_subsystem.md](../docs/oor_subsystem.md) — Per-session actor
  design in full.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
