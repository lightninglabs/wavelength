# vtxo

## Purpose

Per-VTXO lifecycle management using a state machine that monitors expiry,
coordinates refresh (forfeit + new issuance), coordinates forfeit
signing, and tracks cooperative and unilateral spending paths. The
Manager actor is the single admission gate for all VTXO operations. The
VTXO FSM models lifecycle phases only, not business intent like refresh
versus leave. The package also hosts the `IncomingVTXOHandler` actor,
which materializes round-produced VTXOs from indexer push notifications
when the local wallet owns the receive script.

## Key Types

- `VTXOState` — Sealed interface for all states (Live, Spending, Spent, PendingForfeit, Forfeiting, Forfeited, UnilateralExit, Failed).
- `Descriptor` — Complete VTXO metadata: `Outpoint`, `Amount`, `PkScript`, `OwnerKey` (keychain.KeyDescriptor), `OperatorKey`, `TapScript`, `TreePath`, `RoundID`, `CommitmentTxID`, `BatchExpiry`, `RelativeExpiry`, `TreeDepth`, `ChainDepth` (OOR hop count), `CreatedHeight`, `Status`.
- `Manager` — Actor managing per-VTXO FSM instances, lifecycle, and admission gating. Configured via `ManagerConfig`.
- `ManagerConfig` — Configuration holding Store, Wallet, ChainSource,
  ActorSystem, ChainParams, ExpiryConfig, RoundActor ref, ChainResolver ref,
  optional `Log`, optional `LedgerSink fn.Option[ledger.Sink]`,
  `ForfeitVTXOActorAskTimeout`, `RefreshFeeQuoter`, `ExitOutcomeResolver`, and
  `ReservationStore`. Confirmed exit-cost accounting is emitted by unroll
  after final sweep confirmation. `ForfeitVTXOActorAskTimeout`
  (default 5 s) bounds forfeit and refresh child asks so a blocked child actor
  cannot monopolize the manager until the outer RPC deadline. Zero uses the
  default; negative disables the timeout. Spend-path asks keep the caller's
  context. `ExitOutcomeResolver` is called at startup to reconcile VTXOs still
  persisted in `VTXOStatusUnilateralExit` with their terminal job outcome.
  `ReservationStore` is used at startup to sweep orphaned Spending VTXOs.
- `ExitOutcomeResolution` — Terminal result for an exiting VTXO: `Outcome`
  (`ExitOutcomeRecoverable` or `ExitOutcomeConfirmed`) and `Reason`.
- `ExitOutcomeResolver` — Function type
  `func(ctx, wire.OutPoint) (fn.Option[ExitOutcomeResolution], error)`.
  Returns `None` when the job has no terminal result yet.
- `SpendingReservationStore` — Narrow interface the VTXO manager uses for its
  startup orphan sweep: `ListReservedOutpoints(ctx) ([]wire.OutPoint, error)`.
  Intentionally small to avoid coupling vtxo to the concrete db type or oor.
- `VTXOActorConfig.LedgerSink` — Per-VTXO actor field plumbed from the
  manager. The VTXO actor cannot determine the confirmed on-chain miner fee,
  so its `emitExitCost` helper is intentionally empty; unroll emits
  `ExitCostMsg` after the final sweep confirms.
- `VTXOEvent` — Inbound events (BlockEpochEvent, ForfeitRequest, ForfeitConfirmed, SpendReserveEvent, SpendCompletedEvent, etc.).
- `VTXOOutMsg` — Outbound messages (ForfeitRequest, ExpiringNotify, StatusUpdate, Terminated).
- `FilterOptions` / `FilterDescriptors` — VTXO filtering by expiry status, spend state, etc.
- `GetActiveVTXOCountRequest` / `GetActiveVTXOCountResponse` — Ask-message for querying active VTXO count from the manager.
- `ManagerMsg` / `ManagerResp` — Type aliases for `actormsg.VTXOManagerMsg` / `actormsg.VTXOManagerResp` (admission types live in `lib/actormsg` to avoid import cycles).
- `CustomForfeitInput` / `ActivateCustomForfeitInputsRequest` / `ActivateCustomForfeitInputsResponse` / `DropCustomForfeitInputsRequest` / `DropCustomForfeitInputsResponse` — Aliases for the canonical `lib/actormsg` types. Let a caller-supplied VTXO outside the wallet's live coin set (custom policy, keys, ancestry) get a temporary `PendingForfeit` VTXO actor that can sign the exact round forfeit tx before a round intent registers. Activation persists a synthetic descriptor only when no durable row already exists; if one does, activation overlays the normal actor without touching the row so drop/rollback can restore it from storage untouched.
- `ForfeitParticipantSigner` — Function type `func(ctx, *ForfeitParticipantSignRequest) ([]*types.ForfeitParticipantSig, error)` on `VTXOEnvironment`/`VTXOActorConfig`/`ManagerConfig`, propagated manager → actor → environment. Called from `LiveState`/`PendingForfeitState`'s `handleForfeitRequest` only when the incoming `ForfeitRequestEvent` carries a custom `ForfeitSpend`; obtains keyed schnorr signatures from non-local participants for custom VTXO spend policies. Called after connector assignment so the signature binds the exact forfeit tx, and independently verified against the computed sighash before being attached to the outbound `ForfeitSignatureSubmission.ParticipantVTXOSigs`.
- `ForfeitParticipantSignRequest` — Carries the exact forfeit tx, VTXO, spend path, and connector/forfeit-output details a `ForfeitParticipantSigner` must sign over.
- `IncomingVTXOHandler` — Actor that consumes `arkrpc.IncomingVTXOEvent` push notifications, looks up the receive script in the owned-script store, builds a `Descriptor` (with tapscript derived via `lib/arkscript`), persists it via `VTXOSaver`, and tells the manager via `VTXOsMaterializedNotification`. Only `VTXO_EVENT_TYPE_CREATED` events are acted on; unknown event kinds and unowned scripts are silently ignored. Inputs are validated for outpoint shape, pkScript presence, and `int64`/`MaxSatoshi` value bounds before any DB write.
- `IncomingVTXOMsg` / `IncomingVTXOResp` — Actor envelope wrapping an `arkrpc.IncomingVTXOEvent` and the `any`-typed response.
- `IncomingVTXOServiceKey` — Well-known service key (`"incoming-vtxo-handler"`) used by `darepod` to register the actor and by `serverconn.EventRouter` to dispatch routed events.
- `OwnedReceiveScript` / `OwnedScriptLookup` — Read-only view of the owned receive scripts store used by the incoming handler. `LookupOwnedReceiveScript` returns `sql.ErrNoRows` for unknown scripts; the handler treats this as "not ours" and exits cleanly.
- `VTXOSaver` — Narrow persistence interface (`SaveVTXO(ctx, *Descriptor)`) the incoming handler uses; the production implementation is the `db` VTXO store, which serializes a missing tree path as an empty blob.
- `IncomingVTXOHandlerConfig.MetricsSink` — Optional `fn.Option[metrics.Sink]`. The handler Tells `metrics.OORTransferReceivedMsg{Status: "materialized"|"failed"}` at the terminal outcome of an owned incoming VTXO (persisted, or a relevant receive that could not be persisted); best-effort, a Tell failure only logs at debug and never fails the receive. Pre-ownership ignore paths (non-CREATED events, malformed pushes, unowned scripts) emit nothing.
- `SelectedVTXO` — Alias for `actormsg.SelectedVTXO`, the lightweight `(Outpoint, Amount, PkScript)` projection returned by `VTXOStore.ListSelectionCandidatesByStatus` and used by `selectAndReserveVTXOs`'s largest-first pass (via `coinselect.LargestFirst`) without decoding full descriptors on the per-payment hot path.
- `VTXOsMaterializedNotification` — Manager-facing notification carrying already-persisted descriptors; the manager spawns one actor per descriptor without performing another store write. Used by both the OOR receive path and the new incoming round VTXO handler.
- `LazyChainResolver` — Forwarding `TellOnlyRef[ExpiringNotification]` that buffers notifications until `Set()` wires the real chain-resolver target. Breaks the init-order dependency between the VTXO manager (which spawns `LazyChainResolver` at startup) and the unroll registry (which is wired after the VTXO manager starts). Buffered notifications are replayed in-order on `Set()`.
- `RefreshFeeQuoter` — Function type `func(ctx, amount btcutil.Amount, remainingBlocks uint32) btcutil.Amount`. Optional hook on `VTXOActorConfig`; invoked as an **advisory preview** before each auto-refresh emission to estimate the per-input operator fee for UX surfaces. Under the seal-time fee handshake (#270) the server is the binding fee authority — the quoter's return value is no longer attached to the wire intent. Nil quoter (legacy and test paths) yields `OperatorFee=0` on the harness-local `RefreshVTXORequest`, which has no effect on the round protocol.

## Relationships

- **Depends on**: `baselib/protofsm` (FSM engine), `baselib/actor` (actor system), `lib/tree` (tree paths), `lib/arkscript` (taproot construction and policy helpers in `IncomingVTXOHandler`), `lib/actormsg` (admission message types, including custom forfeit input activation), `lib/types` (`ForfeitParticipantSig`), `coinselect` (`LargestFirst` shared coin-selection algorithm), `arkrpc` (`IncomingVTXOEvent`), `chainsource` (block epochs), `ledger` (`Sink` type for compatibility with manager wiring), `metrics` (`Sink`, `OORTransferReceivedMsg` from `IncomingVTXOHandler`), `unroll` (via `ExitOutcomeResolver` callback wired by `darepod`).
- **Depended on by**: `round` (triggers forfeit requests), `oor` (incoming VTXOs), `wallet` (admission gating), `db` (persistence), `darepod` (wiring, owned-script adapters, incoming event route).
- **Sends**:
  - → `round` (via manager relay): `RelayToRoundMsg` wrapping `ForfeitSignatureSubmission` (now carrying optional `ParticipantVTXOSigs []*types.ForfeitParticipantSig` for custom spend policies)
  - → `db` (via outbox): `VTXOStatusUpdate`
  - → `vtxo` manager: `VTXOTerminatedNotification`, `RelayToRoundMsg`, `VTXOsMaterializedNotification` (from `IncomingVTXOHandler`), `spendReservationFailedMsg` (manager-internal hop-back from the detached spend-reserve watcher)
  - → `metrics` actor: `OORTransferReceivedMsg` (from `IncomingVTXOHandler`, fire-and-forget via `MetricsSink`)
  - → `ledger` actor: no direct messages; unroll emits confirmed
    `ExitCostMsg` after sweep confirmation
- **Receives**:
  - ← `round`: `ForfeitRequestEvent`, `ForfeitConfirmedEvent`, `ForfeitSignedEvent`, `ForfeitReleasedEvent`, `BlockEpochEvent`, `PendingForfeitEvent`, `SpendReserveEvent`, `SpendReleasedEvent`, `SpendCompletedEvent`, `ResumeVTXOEvent`
  - ← `wallet` (via `lib/actormsg`): `SelectAndReserveSpendRequest`, `ReleaseSpendRequest`, `CompleteSpendRequest`, `ReserveForfeitRequest`, `ReleaseForfeitRequest`, `SelectAndReserveForfeitRequest`
  - ← `chainsource` (via Manager): `BlockEpochEvent`
  - ← `serverconn` (via `EventRouter` route `MethodIncomingVTXO`): `IncomingVTXOMsg` (wrapping `arkrpc.IncomingVTXOEvent`), routed to `IncomingVTXOHandler`
  - ← `unroll` (via `RegistryConfig.VTXOExitObserver`, forwarded by `darepod`): `ExitOutcomeNotification` — terminal exit job result forwarded to reconcile VTXO lifecycle after an unroll completes or fails cleanly

## Multi-Tree Ancestry

- `Descriptor.Ancestry []Ancestry` replaces the singular
  `Descriptor.TreePath` field. Round-direct and same-commitment OOR
  VTXOs carry a length-1 slice; cross-commitment multi-input OOR VTXOs
  carry one entry per distinct contributing commitment tx.
- Each `Ancestry` carries `TreePath`, `CommitmentTxID`, `InputIndices`
  (Ark tx input indices the fragment serves), and `TreeDepth`.
- `Descriptor.MaxTreeDepth()` returns `max(TreeDepth)` across the
  ancestry slice for callers that need worst-case unilateral-exit
  timing (e.g. `expiry.go`).
- The DB persistence layer stores ancestry rows in the
  `vtxo_ancestry_paths` side table (migration 000009) keyed by VTXO
  outpoint; routine queries (`ListUnspentVTXOs`, `GetVTXO`) skip the
  ancestry join and only load it when the unroller resolves an exit.

## Invariants

- VTXO actor state is the single source of truth for availability.
- Forfeit transaction is not broadcast until the connector output's round confirms (atomic replacement).
- Refresh is auto-triggered at configurable height before expiry.
- Once ForfeitedState is reached, the old VTXO is unspendable; the new VTXO is available only after round confirmation.
- SpendingState is persisted as VTXOStatusSpending and survives restarts.
- OOR completion transitions VTXOs to SpentState through the VTXO actor FSM, not by direct store writes.
- A VTXO in SpendingState cannot be admitted for cooperative consumption, and vice versa.
- The `ExpiringNotification` Tell to the chain resolver is sent outside the
  FSM transition context via a detached goroutine (using `context.WithoutCancel`
  on the actor turn context). This prevents a slow or blocking chain resolver
  from stalling the VTXO actor's turn and delays the notification delivery
  past the FSM transition without affecting the transition outcome.
- **Startup reconcile of unilateral-exit VTXOs.** When `ManagerConfig.ExitOutcomeResolver` is set, `Start` calls `reconcileUnilateralExits` after recovering actors. For each VTXO in `VTXOStatusUnilateralExit`, it resolves the terminal outcome: `ExitOutcomeRecoverable` (no on-chain footprint) rolls the VTXO back to `LiveState` and spawns a fresh actor; `ExitOutcomeConfirmed` retires it to `SpentState`. `None` (job still running) is left untouched.
- **Startup sweep of orphaned Spending VTXOs (bidirectional).** When `ManagerConfig.ReservationStore` is set, `Start` calls `sweepOrphanedReservations` after all actors are recovered. Forward direction: a Spending VTXO with no reservation row in the durable index is provably orphaned (its spend session died before checkpointing) and is released back to `LiveState` via `SpendReleasedEvent`. Reverse direction: a reservation row whose VTXO is still `Live` means the owning session checkpointed its reservation but the detached Spending status write never landed before shutdown; the sweep re-marks the in-memory reservation and re-drives `SpendReserveEvent` so a restarted daemon cannot re-select an input an in-flight session still owns. The sweep aborts entirely if `ListReservedOutpoints` fails to avoid releasing VTXOs an in-flight spend still owns.
- **In-memory spend-reservation admission gate.** `Manager.reserved` (outpoint → monotonic epoch, manager-goroutine-owned) closes the window between a spend reservation being handed to a child actor and that actor's durable `Spending` status write landing. `selectAndReserveVTXOs`'s detached (spend) path marks the outpoint via `markReserved` and issues the reserve `Ask` without awaiting it (`detachedReserve` observes the future via `OnComplete` on a detached goroutine bounded by `detachedReserveTimeout`, 30s); a failure hops back to the manager goroutine as `spendReservationFailedMsg` carrying the observed epoch, and the mark is dropped only if the epoch still matches (`dropReservedEpoch`) so a stale failure cannot un-gate a newer reservation on the same outpoint (ABA). A watch timeout re-confirms the durable status before reporting failure, since the child's write may simply still be in flight. Coin-selection candidates already holding a mark are filtered out before largest-first selection runs. `handleReserveForfeit` also refuses any outpoint currently marked, returning `ErrVTXOLiquidityLocked`, since the child's FSM would otherwise still read `LiveState` and accept a conflicting forfeit reservation.
- **Actor self-heal on selection.** If `selectAndReserveVTXOs` selects an outpoint with a committed `Live` row but no resident actor (the materialized-VTXO notification is delivered asynchronously after commit, or was lost to a crash), `respawnActorFromStore` spawns the actor from the persisted descriptor instead of failing the payment; the store row is the source of truth.
- **Custom forfeit input activation.** `ActivateCustomForfeitInputsRequest`/`DropCustomForfeitInputsRequest` (see `lib/actormsg/CLAUDE.md`) start and tear down temporary `PendingForfeit` VTXO actors for caller-supplied `CustomForfeitInput`s ahead of round intent registration. `Manager.customForfeitSynthetic` tracks, per outpoint, whether the descriptor was created solely for the temporary signer (rollback/drop deletes it) or overlays a pre-existing durable row (rollback/drop only stops the actor and respawns the ordinary one from storage via `respawnCustomForfeitBaseActor`, leaving the row untouched since other tables such as OOR package bindings may reference it). Activation fails the whole batch and rolls back already-activated inputs if any input conflicts with an existing non-custom actor or a differently-keyed active custom signer.
- **Atomic reservation cleanup.** `VTXOStore.UpdateVTXOStatusReleasingReservation` deletes the spending-reservation row in the same transaction as the VTXO status change when a VTXO leaves `SpendingState` (via `SpendReleasedEvent`, `SpendCompletedEvent`, or escalation to `UnilateralExitState`). This prevents the durable index from retaining stale rows that would mask a future orphan on the same outpoint.
- `ForceUnrollEvent` is accepted in `LiveState`, `PendingForfeitState`, `SpendingState`, and `ForfeitingState`: each transitions to `UnilateralExitState` and emits `ExpiringNotification` + `VTXOStatusUpdate{UnilateralExit}`. It does **not** emit `VTXOTerminatedNotification` on intent — `UnilateralExitState` is **non-terminal** (darepo-client#602), so the actor stays alive to observe the exit. Truly terminal states (`Spent`, `Forfeited`, `Failed`) self-loop; the manager maps that self-loop back to `ForceUnrollResponse{Accepted: false, Reason: "already terminal"}`. A re-unroll of a VTXO already in `UnilateralExitState` self-loops with no outbox; the `Unroll` RPC short-circuits it earlier via the persisted `VTXOStatusUnilateralExit` status.
- `UnilateralExitState` is **non-terminal** and observed, not fire-and-forget. The actor survives until the unroll job reports a terminal outcome via the manager's `ExitOutcomeNotification`: `ExitOutcomeRecoverable` (the unroll failed with no on-chain footprint) drives `ExitFailedEvent` → `LiveState` + `VTXOStatusUpdate{Live}`, while `ExitOutcomeConfirmed` (the exit confirmed on-chain) drives `ExitConfirmedEvent` → terminal `SpentState` + `VTXOTerminatedNotification` (the actor is reaped here, gated on a terminal on-chain event rather than the user's intent). When the actor is absent (e.g. a daemon restart, since exiting VTXOs are excluded from `ListLiveVTXOs` recovery) the manager re-materializes a live actor from the persisted descriptor (recover) or persists `VTXOStatusSpent` directly (confirm).
- `Manager.handleForceUnroll` uses `Ask` (not `Tell`) so FSM errors and self-loop no-ops surface as structured `ForceUnrollResponse{Accepted, Reason}` instead of a uniform `Accepted:true` that masks work that was never scheduled.
- Admission types (`SelectAndReserveSpendRequest`, `SelectAndReserveForfeitRequest`, `ReserveForfeitRequest`, etc.) are defined in `lib/actormsg` and re-exported as type aliases to avoid wallet → vtxo → round → wallet import cycles.
- `selectAndReserveVTXOs` is a shared helper parameterized by `reserveParams` that serves both the OOR spend and cooperative forfeit coin selection paths, avoiding code duplication. It lists candidates via `VTXOStore.ListSelectionCandidatesByStatus` (the lightweight `SelectedVTXO` projection) and runs `coinselect.LargestFirst` rather than decoding full descriptors or sorting in-package.
- `ForfeitingState` also accepts `ForfeitReleasedEvent` (not just `PendingForfeitState`): a pre-signing round failure can land here when this VTXO already replied with its forfeit signature and advanced out of `PendingForfeitState` before the round failed during `ForfeitSignaturesCollecting`. The release is still safe because no forfeit signature reaches the operator until the round's success edge calls `SubmitVTXOForfeitSigsToServer`; the VTXO returns to `LiveState` + `VTXOStatusUpdate{Live}` rather than staying wedged in `ForfeitingState`.
- `VTXOStatusUpdate.ReleaseSpendReservation`, when true, instructs the persistence layer to delete the durable spending-reservation row in the same transaction as the status change. Set on the `SpendingState` → `UnilateralExitState` critical-expiry and non-terminal-exit-recovery edges so a stale reservation row cannot outlive the spend and cause the startup sweep to re-reserve a VTXO that has already recovered to `LiveState`.
- `ForfeitSignatureSubmission.ParticipantVTXOSigs []*types.ForfeitParticipantSig`, forwarded verbatim into `round.ForfeitSignatureResponse`, carries keyed non-operator signatures collected via `ForfeitParticipantSigner` for custom spend paths requiring multiple client-side participants; empty/nil when the forfeit spend is the standard client+operator path.
- `IncomingVTXOHandler` only handles `VTXO_EVENT_TYPE_CREATED` events. Other event kinds, missing/short outpoints, empty pkScripts, oversized values (`> int64` or `> MaxSatoshi`), and tapscript derivation failures all return success without persisting — they cannot crash the actor or block the indexer push stream. Real DB lookup/save errors are surfaced.
- Incoming VTXOs are saved with `Status: VTXOStatusLive` and empty `Ancestry` (the round commitment tree is not pushed alongside the event); `db.VTXOPersistenceStore.descriptorToInsertParams` accepts an empty tree-path blob to support this.
- The `CommitmentTxID` on a materialized incoming VTXO comes from `IncomingVTXOEvent.CommitmentTxid`, which is the round commitment txid — **not** the leaf txid in the outpoint.
- Per-subsystem logging: `ManagerConfig.Log` provides an optional instance logger; falls back to `build.LoggerFromContext` (no global mutable loggers).

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
