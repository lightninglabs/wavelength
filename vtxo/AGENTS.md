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
  ActorSystem, ExpiryConfig, RoundActor ref, ChainResolver ref, optional
  `Log`, optional `LedgerSink fn.Option[ledger.Sink]`, and
  `ForfeitVTXOActorAskTimeout time.Duration`. The manager propagates the sink
  into each spawned `VTXOActor` so per-VTXO handlers can fire-and-forget
  `ExitCostMsg` emissions. `ForfeitVTXOActorAskTimeout` bounds Ask calls on
  forfeit/refresh admission paths so a blocked child actor cannot monopolize
  the shared manager; zero uses a 5-second default, negative disables the
  timeout.
- `VTXOActorConfig.LedgerSink` — Per-VTXO actor field plumbed from the manager. The `emitExitCost` helper is wired onto the unilateral-exit transition but is currently a no-op pending chain resolver integration: the actor cannot determine the on-chain miner fee until the chain resolver reports the confirmed exit-spend transaction. The emission site exists so a single future change in the chain resolver wiring enables it without touching the FSM transition logic.
- `VTXOEvent` — Inbound events (BlockEpochEvent, ForfeitRequest, ForfeitConfirmed, SpendReserveEvent, SpendCompletedEvent, etc.).
- `VTXOOutMsg` — Outbound messages (ForfeitRequest, ExpiringNotify, StatusUpdate, Terminated).
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

- **Depends on**: `baselib/protofsm` (FSM engine), `baselib/actor` (actor system), `lib/tree` (tree paths), `lib/arkscript` (taproot construction and policy helpers in `IncomingVTXOHandler`), `lib/actormsg` (admission message types), `arkrpc` (`IncomingVTXOEvent`), `chainsource` (block epochs), `ledger` (`Sink` + `ExitCostMsg` for planned exit cost emission).
- **Depended on by**: `round` (triggers forfeit requests), `oor` (incoming VTXOs), `wallet` (admission gating), `db` (persistence), `darepod` (wiring, owned-script adapters, incoming event route).
- **Sends**:
  - → `round` (via manager relay): `RelayToRoundMsg` wrapping `ForfeitSignatureSubmission`
  - → `db` (via outbox): `VTXOStatusUpdate`
  - → `vtxo` manager: `VTXOTerminatedNotification`, `RelayToRoundMsg`, `VTXOsMaterializedNotification` (from `IncomingVTXOHandler`)
  - → `ledger` actor (via `ledger.Sink` Tell): `ExitCostMsg` planned; currently a no-op emission pending chain-resolver fee propagation
- **Receives**:
  - ← `round`: `ForfeitRequestEvent`, `ForfeitConfirmedEvent`, `ForfeitSignedEvent`, `ForfeitReleasedEvent`, `BlockEpochEvent`, `PendingForfeitEvent`, `SpendReserveEvent`, `SpendReleasedEvent`, `SpendCompletedEvent`, `ResumeVTXOEvent`
  - ← `wallet` (via `lib/actormsg`): `SelectAndReserveSpendRequest`, `ReleaseSpendRequest`, `CompleteSpendRequest`, `ReserveForfeitRequest`, `ReleaseForfeitRequest`, `SelectAndReserveForfeitRequest`
  - ← `chainsource` (via Manager): `BlockEpochEvent`
  - ← `serverconn` (via `EventRouter` route `MethodIncomingVTXO`): `IncomingVTXOMsg` (wrapping `arkrpc.IncomingVTXOEvent`), routed to `IncomingVTXOHandler`

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
- `ForceUnrollEvent` is accepted in `LiveState`, `PendingForfeitState`, `SpendingState`, and `ForfeitingState`: each transitions to `UnilateralExitState` and emits the same `ExpiringNotification` / `VTXOStatusUpdate` / `VTXOTerminatedNotification` outbox shape. Terminal states (`UnilateralExit`, `Spent`, `Forfeited`, `Failed`) self-loop; the manager maps that self-loop back to `ForceUnrollResponse{Accepted: false, Reason: "already terminal"}` so the caller sees a distinct outcome from "no such VTXO".
- `Manager.handleForceUnroll` uses `Ask` (not `Tell`) so FSM errors and self-loop no-ops surface as structured `ForceUnrollResponse{Accepted, Reason}` instead of a uniform `Accepted:true` that masks work that was never scheduled.
- Admission types (`SelectAndReserveSpendRequest`, `SelectAndReserveForfeitRequest`, `ReserveForfeitRequest`, etc.) are defined in `lib/actormsg` and re-exported as type aliases to avoid wallet → vtxo → round → wallet import cycles.
- `selectAndReserveVTXOs` is a shared helper parameterized by `reserveParams` that serves both the OOR spend and cooperative forfeit coin selection paths, avoiding code duplication.
- `IncomingVTXOHandler` only handles `VTXO_EVENT_TYPE_CREATED` events. Other event kinds, missing/short outpoints, empty pkScripts, oversized values (`> int64` or `> MaxSatoshi`), and tapscript derivation failures all return success without persisting — they cannot crash the actor or block the indexer push stream. Real DB lookup/save errors are surfaced.
- Incoming VTXOs are saved with `Status: VTXOStatusLive` and no `TreePath` (the round commitment tree is not pushed alongside the event); `db.VTXOPersistenceStore.descriptorToInsertParams` accepts an empty tree-path blob to support this.
- The `CommitmentTxID` on a materialized incoming VTXO comes from `IncomingVTXOEvent.CommitmentTxid`, which is the round commitment txid — **not** the leaf txid in the outpoint.
- Per-subsystem logging: `ManagerConfig.Log` provides an optional instance logger; falls back to `build.LoggerFromContext` (no global mutable loggers).

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
