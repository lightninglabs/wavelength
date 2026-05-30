# vhtlcrecovery/coordinator

## Purpose

Joins durable vHTLC recovery rows with the generic `unroll` registry. Owns
the runtime handoff from a recovery intent to per-target unroll execution.
The parent `vhtlcrecovery` package is a pure data package (imported by `db`);
this child package is free to import `unroll` without creating an import cycle.

## Key Types

- `Service` — Thin stateless service; SQL owns recovery state and the unroll
  registry owns per-target execution workers. Methods: `ArmRecovery`,
  `EscalateRecovery`, `CancelRecovery`, `GetRecoveryStatus`,
  `ListRecoveryStatuses`, `RestoreNonTerminal`.
- `ServiceConfig` — Wiring struct: `Store` (SQL persistence), `Unroll`
  (admission/status surface), `Log`, and optional `TargetMaterializer`.
- `Store` interface — Durable persistence required by the recovery service:
  `ArmRecovery`, `GetRecovery`, `ListNonTerminalRecoveries`, `ListRecoveries`,
  `EscalateRecovery`, `CancelRecovery`, `CompleteRecovery`, `FailRecovery`.
  Implementations must use SQL transactions for every state mutation.
- `UnrollRegistry` interface — Narrow `unroll` control surface: `EnsureUnroll`
  and `GetStatus`. Narrower than the full actor ref so tests can model
  admission without spinning up the full unroll subsystem.
- `TargetMaterializer` interface — Domain adapter that prepares local VTXO
  and OOR artifact state before the recovery service admits the target to
  unroll. Implementations live in packages (like `darepod`) that know how
  to stitch vHTLC recovery rows back to local artifacts.
- `ActorUnrollRegistry` — Production adapter wrapping an
  `actor.ActorRef[unroll.RegistryMsg, unroll.RegistryResp]`.
- `RecoveryStatus` — Joined read-only view: durable `vhtlcrecovery.RecoveryJob`
  plus best-effort live unroll observations (`UnrollFound`, `UnrollActive`,
  `UnrollPhase`, `UnrollSweep`, `UnrollFailure`).

## Relationships

- **Depends on**: `vhtlcrecovery` (recovery row types), `unroll` (registry
  interfaces and request/response types), `baselib/actor` (ActorRef for the
  production registry adapter).
- **Depended on by**: `darepod` (wires the service and exposes its methods as
  gRPC handlers: `ArmVHTLCRecovery`, `EscalateVHTLCRecovery`,
  `CancelVHTLCRecovery`, `GetVHTLCRecoveryStatus`, `ListVHTLCRecoveries`).
- **Sends**:
  - → `unroll` registry (via `ActorUnrollRegistry.Ask`):
    `unroll.EnsureUnrollRequest`, `unroll.GetStatusRequest`
- **Receives**:
  - ← `darepod` RPC layer: `ArmRecovery`, `EscalateRecovery`,
    `CancelRecovery`, `GetRecoveryStatus`, `ListRecoveryStatuses`,
    `RestoreNonTerminal`

## Invariants

- SQL state is the single source of truth. The service keeps no durable
  in-memory state; restart is handled by `RestoreNonTerminal`.
- The SQL state transition (`EscalateRecovery`) happens BEFORE unroll
  admission. A crash between the two is recovered by `RestoreNonTerminal`
  reissuing admission for every non-terminal non-armed row.
- Armed jobs stay dormant through `RestoreNonTerminal`; only previously
  escalated jobs are re-admitted to unroll.
- `CancelRecovery` deliberately does NOT stop an already-admitted unroll
  worker. It marks the recovery row cancelled so later unroll terminal
  results are not applied to this row.
- Unroll policy mismatch (an existing unroll job for the same target
  carrying a different exit policy kind/ref) is a terminal failure; the
  recovery row is marked failed rather than silently overriding the
  existing job's policy.
- `reconcileLoaded` folds terminal unroll outcomes back into the recovery
  row on every `GetRecoveryStatus` / `ListRecoveryStatuses` call so the
  recovery table stays consistent without a separate background writer.

## Deep Docs

- [vhtlcrecovery/CLAUDE.md](../CLAUDE.md) — Pure recovery data types.
- [vhtlcrecovery/unrollpolicy/CLAUDE.md](../unrollpolicy/CLAUDE.md) —
  Exit spend policy adapter.
- [unroll/CLAUDE.md](../../unroll/CLAUDE.md) — Generic unroll registry.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
