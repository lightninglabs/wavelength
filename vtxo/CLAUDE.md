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
  `ForfeitVTXOActorAskTimeout`, `RefreshFeeQuoter`, `ExitOutcomeResolver`,
  `ReservationStore`, and `ForfeitParticipantSigner`. Confirmed exit-cost
  accounting is emitted by unroll after final sweep confirmation.
  `ForfeitVTXOActorAskTimeout` (default 5 s) bounds forfeit and refresh child
  asks so a blocked child actor cannot monopolize the manager until the outer
  RPC deadline. Zero uses the default; negative disables the timeout. Spend-path
  asks keep the caller's context. `ExitOutcomeResolver` is called at startup to
  reconcile VTXOs still persisted in `VTXOStatusUnilateralExit` with their
  terminal job outcome. `ReservationStore` is used at startup to sweep orphaned
  Spending VTXOs. `ForfeitParticipantSigner` is propagated to each spawned
  `VTXOActor` so custom-policy actors can collect non-operator signatures.
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
- `ForfeitParticipantSignRequest` — describes the exact forfeit transaction that
  a non-local participant must sign: VTXO descriptor, spend path, forfeit tx,
  connector outpoint/pkScript/amount, and server forfeit pkScript.
- `ForfeitParticipantSigner` — function type
  `func(ctx, *ForfeitParticipantSignRequest) ([]*types.ForfeitParticipantSig, error)`.
  Obtains keyed signatures from non-local participants for custom VTXO
  policies. Stored on `VTXOEnvironment` and propagated to each actor by the
  manager. Nil for standard single-signer VTXOs.
- `CustomForfeitInput` — alias for `actormsg.CustomForfeitInput`. Describes a
  caller-supplied VTXO that is not part of the wallet's live coin set but still
  needs a temporary VTXO actor to sign the round forfeit transaction once
  connector details are known. Carries amount, pkScript, policy template,
  client key, operator key, relative expiry, round lineage (RoundID,
  CommitmentTxID, BatchExpiry, CreatedHeight), and ancestry.
- `ActivateCustomForfeitInputsRequest/Response` — alias for `actormsg` types.
  Sent to the manager to start temporary `PendingForfeit` VTXO actors for
  custom inputs before registering a round intent. Inputs not already known to
  the wallet are persisted as synthetic signer rows; existing rows are overlaid
  without modification.
- `DropCustomForfeitInputsRequest/Response` — alias for `actormsg` types. Sent
  to the manager to remove custom `PendingForfeit` signer overlays when a round
  fails. Synthetic descriptors are deleted; non-synthetic (pre-existing) rows
  are restored to their original actor.
- `FilterOptions` / `FilterDescriptors` — VTXO filtering by expiry status, spend state, etc.
- `GetActiveVTXOCountRequest` / `GetActiveVTXOCountResponse` — Ask-message for querying active VTXO count from the manager.
- `ManagerMsg` / `ManagerResp` — Type aliases for `actormsg.VTXOManagerMsg` / `actormsg.VTXOManagerResp` (admission types live in `lib/actormsg` to avoid import cycles).
- `IncomingVTXOHandler` — Actor that consumes `arkrpc.IncomingVTXOEvent` push notifications, looks up the receive script in the owned-script store, builds a `Descriptor` (with tapscript derived via `lib/arkscript`), persists it via `VTXOSaver`, and tells the manager via `VTXOsMaterializedNotification`. Only `VTXO_EVENT_TYPE_CREATED` events are acted on; unknown event kinds and unowned scripts are silently ignored. Inputs are validated for outpoint shape, pkScript presence, and `int64`/`MaxSatoshi` value bounds before any DB write.
- `IncomingVTXOMsg` / `IncomingVTXOResp` — Actor envelope wrapping an `arkrpc.IncomingVTXOEvent` and the `any`-typed response.
- `IncomingVTXOServiceKey` — Well-known service key (`"incoming-vtxo-handler"`) used by `darepod` to register the actor and by `serverconn.EventRouter` to dispatch routed events.
- `OwnedReceiveScript` / `OwnedScriptLookup` — Read-only view of the owned receive scripts store used by the incoming handler. `LookupOwnedReceiveScript` returns `sql.ErrNoRows` for unknown scripts; the handler treats this as "not ours" and exits cleanly.
- `VTXOSaver` — Narrow persistence interface (`SaveVTXO(ctx, *Descriptor)`) the incoming handler uses; the production implementation is the `db` VTXO store, which serializes a missing tree path as an empty blob.
- `VTXOsMaterializedNotification` — Manager-facing notification carrying already-persisted descriptors; the manager spawns one actor per descriptor without performing another store write. Used by both the OOR receive path and the new incoming round VTXO handler.
- `LazyChainResolver` — Forwarding `TellOnlyRef[ExpiringNotification]` that buffers notifications until `Set()` wires the real chain-resolver target. Breaks the init-order dependency between the VTXO manager (which spawns `LazyChainResolver` at startup) and the unroll registry (which is wired after the VTXO manager starts). Buffered notifications are replayed in-order on `Set()`.
- `RefreshFeeQuoter` — Function type `func(ctx, amount btcutil.Amount, remainingBlocks uint32) btcutil.Amount`. Optional hook on `VTXOActorConfig`; invoked as an **advisory preview** before each auto-refresh emission to estimate the per-input operator fee for UX surfaces. Under the seal-time fee handshake (#270) the server is the binding fee authority — the quoter's return value is no longer attached to the wire intent. Nil quoter (legacy and test paths) yields `OperatorFee=0` on the harness-local `RefreshVTXORequest`, which has no effect on the round protocol.

## Relationships

- **Depends on**: `baselib/protofsm` (FSM engine), `baselib/actor` (actor system), `lib/tree` (tree paths), `lib/arkscript` (taproot construction and policy helpers in `IncomingVTXOHandler`), `lib/actormsg` (admission message types), `arkrpc` (`IncomingVTXOEvent`), `chainsource` (block epochs), `coinselect` (`LargestFirst` algorithm used by `selectAndReserveVTXOs`), `ledger` (`Sink` type for compatibility with manager wiring), `unroll` (via `ExitOutcomeResolver` callback wired by `darepod`).
- **Depended on by**: `round` (triggers forfeit requests), `oor` (incoming VTXOs), `wallet` (admission gating), `db` (persistence), `darepod` (wiring, owned-script adapters, incoming event route).
- **Sends**:
  - → `round` (via manager relay): `RelayToRoundMsg` wrapping `ForfeitSignatureSubmission`
  - → `db` (via outbox): `VTXOStatusUpdate`
  - → `vtxo` manager: `VTXOTerminatedNotification`, `RelayToRoundMsg`, `VTXOsMaterializedNotification` (from `IncomingVTXOHandler`)
  - → `ledger` actor: no direct messages; unroll emits confirmed
    `ExitCostMsg` after sweep confirmation
- **Receives**:
  - ← `round`: `ForfeitRequestEvent`, `ForfeitConfirmedEvent`, `ForfeitSignedEvent`, `ForfeitReleasedEvent`, `BlockEpochEvent`, `PendingForfeitEvent`, `SpendReserveEvent`, `SpendReleasedEvent`, `SpendCompletedEvent`, `ResumeVTXOEvent`
  - ← `wallet` (via `lib/actormsg`): `SelectAndReserveSpendRequest`, `ReleaseSpendRequest`, `CompleteSpendRequest`, `ReserveForfeitRequest`, `ReleaseForfeitRequest`, `SelectAndReserveForfeitRequest`, `ActivateCustomForfeitInputsRequest`, `DropCustomForfeitInputsRequest`
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
- **Startup sweep of orphaned Spending VTXOs.** When `ManagerConfig.ReservationStore` is set, `Start` calls `sweepOrphanedReservations` after all actors are recovered. A Spending VTXO with no reservation row in the durable index is provably orphaned (its spend session died before checkpointing) and is released back to `LiveState` via `SpendReleasedEvent`. The sweep aborts entirely if `ListReservedOutpoints` fails to avoid releasing VTXOs an in-flight spend still owns.
- **Atomic reservation cleanup.** `VTXOStore.UpdateVTXOStatusReleasingReservation` deletes the spending-reservation row in the same transaction as the VTXO status change when a VTXO leaves `SpendingState` (via `SpendReleasedEvent`, `SpendCompletedEvent`, or escalation to `UnilateralExitState`). This prevents the durable index from retaining stale rows that would mask a future orphan on the same outpoint.
- `ForceUnrollEvent` is accepted in `LiveState`, `PendingForfeitState`, `SpendingState`, and `ForfeitingState`: each transitions to `UnilateralExitState` and emits `ExpiringNotification` + `VTXOStatusUpdate{UnilateralExit}`. It does **not** emit `VTXOTerminatedNotification` on intent — `UnilateralExitState` is **non-terminal** (darepo-client#602), so the actor stays alive to observe the exit. Truly terminal states (`Spent`, `Forfeited`, `Failed`) self-loop; the manager maps that self-loop back to `ForceUnrollResponse{Accepted: false, Reason: "already terminal"}`. A re-unroll of a VTXO already in `UnilateralExitState` self-loops with no outbox; the `Unroll` RPC short-circuits it earlier via the persisted `VTXOStatusUnilateralExit` status.
- `UnilateralExitState` is **non-terminal** and observed, not fire-and-forget. The actor survives until the unroll job reports a terminal outcome via the manager's `ExitOutcomeNotification`: `ExitOutcomeRecoverable` (the unroll failed with no on-chain footprint) drives `ExitFailedEvent` → `LiveState` + `VTXOStatusUpdate{Live}`, while `ExitOutcomeConfirmed` (the exit confirmed on-chain) drives `ExitConfirmedEvent` → terminal `SpentState` + `VTXOTerminatedNotification` (the actor is reaped here, gated on a terminal on-chain event rather than the user's intent). When the actor is absent (e.g. a daemon restart, since exiting VTXOs are excluded from `ListLiveVTXOs` recovery) the manager re-materializes a live actor from the persisted descriptor (recover) or persists `VTXOStatusSpent` directly (confirm).
- `Manager.handleForceUnroll` uses `Ask` (not `Tell`) so FSM errors and self-loop no-ops surface as structured `ForceUnrollResponse{Accepted, Reason}` instead of a uniform `Accepted:true` that masks work that was never scheduled.
- Admission types (`SelectAndReserveSpendRequest`, `SelectAndReserveForfeitRequest`, `ReserveForfeitRequest`, etc.) are defined in `lib/actormsg` and re-exported as type aliases to avoid wallet → vtxo → round → wallet import cycles.
- `selectAndReserveVTXOs` is a shared helper parameterized by `reserveParams`
  that serves both the OOR spend and cooperative forfeit coin selection paths.
  It calls `coinselect.LargestFirst` for the selection algorithm (replacing the
  former private `selectLargestFirst` helper) and maps typed errors
  (`ErrChangeBelowMin`, `ErrSelectionShortfall`, `ErrNoCandidates`) onto
  locked-vs-shortfall liquidity diagnostics.
- **Detached spend reservations.** Spend reservations (`SpendReserveEvent`) are
  now delivered tell-style off the manager turn: the manager marks the outpoint
  in an in-memory reservation map, issues the Ask to the VTXO actor, and
  observes the child's future via `OnComplete` on a detached goroutine instead
  of blocking the selection turn. A failed reservation (candidate raced out of
  `LiveState`) hops back as a manager-internal message that drops the mark. The
  in-memory map gates both spend and forfeit admission ahead of the durable
  status, since a detached `VTXOStatusSpending` write may still be in flight
  when the next selection lists candidates. The forfeit path keeps its
  synchronous ask: round participation wants durable state settled before
  proceeding and is not on the payment hot path.
- **Self-heal of actorless live VTXOs during coin selection.** Because
  `VTXOsMaterializedNotification` is delivered asynchronously after the
  producing session's commit, a coin selection racing that window can find a
  committed Live DB row with no resident actor. The reserve loop now respawns
  the actor from the persisted descriptor on a map miss, on the manager
  goroutine where the actors map is owned. Respawn fails closed: a store miss
  or a no-longer-live row surfaces as a normal reservation error; no actor is
  registered for non-spendable liquidity.
- **Custom forfeit actor lifecycle.** Temporary `PendingForfeit` actors created
  for custom refresh inputs by `handleActivateCustomForfeitInputs` carry a
  `customForfeitSynthetic` flag. On `DropCustomForfeitInputs`: synthetic rows
  (no pre-existing wallet VTXO) are deleted from the store; non-synthetic rows
  (pre-existing wallet VTXOs overlaid as `PendingForfeit`) are rolled back to
  their prior state by respawning a base actor. The activation is atomic in the
  sense that any partial activation is rolled back if `ActivateCustomForfeitInputsRequest`
  fails midway.
- Standard (non-custom) forfeits use the local wallet's single-signer path and
  `Delivery.ShouldDeadLetter`; custom-policy actors invoke
  `ForfeitParticipantSigner` to collect additional keyed signatures before
  assembling the complete forfeit for submission.
- `IncomingVTXOHandler` only handles `VTXO_EVENT_TYPE_CREATED` events. Other event kinds, missing/short outpoints, empty pkScripts, oversized values (`> int64` or `> MaxSatoshi`), and tapscript derivation failures all return success without persisting — they cannot crash the actor or block the indexer push stream. Real DB lookup/save errors are surfaced.
- Incoming VTXOs are saved with `Status: VTXOStatusLive` and empty `Ancestry` (the round commitment tree is not pushed alongside the event); `db.VTXOPersistenceStore.descriptorToInsertParams` accepts an empty tree-path blob to support this.
- The `CommitmentTxID` on a materialized incoming VTXO comes from `IncomingVTXOEvent.CommitmentTxid`, which is the round commitment txid — **not** the leaf txid in the outpoint.
- Per-subsystem logging: `ManagerConfig.Log` provides an optional instance logger; falls back to `build.LoggerFromContext` (no global mutable loggers).

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
