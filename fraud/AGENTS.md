# fraud

## Purpose

Passive recipient fraud watcher. Monitors ancestry spend notifications for
VTXOs the local client received via OOR transfers; when a watched ancestor
is spent unexpectedly (indicating a potential double-spend or fraud attempt
by the operator), the watcher triggers a unilateral exit via the unroll
registry.

## Key Types

- `WatcherActor` — Durable actor owning all spend watches for tracked VTXOs.
  Registered with the actor system under `ServiceKeyName`.
- `WatcherConfig` — Wiring: `ChainSource` (for passive spend watches),
  `UnrollRef` (to start unroll jobs on detected fraud), optional `Log`,
  `MailboxSize` override.
- `ServiceKey()` — Returns the typed actor service key for receptionist
  lookup.
- `Msg` / `Resp` — Sealed message and response surfaces.
- `TrackVTXOsRequest` / `TrackVTXOsResp` — Ask the watcher to arm passive
  ancestry spend watches for a list of VTXO descriptors.
- `UntrackRequest` / `UntrackResp` — Release watches for a VTXO that is no
  longer live (e.g., after a successful round refresh or cooperative leave).

## Relationships

- **Depends on**: `baselib/actor` (actor framework), `chainsource` (spend
  notifications), `unroll` (triggers unilateral exit on fraud detection),
  `vtxo` (VTXO descriptor type).
- **Depended on by**: `darepod` (starts and wires the fraud watcher during
  `initOORActor`; sends `TrackVTXOsRequest` after materializing incoming OOR
  VTXOs).
- **Sends**:
  - → `unroll` registry: `EnsureUnrollRequest` (triggered when a watched
    ancestor spend is observed and classified as fraudulent).
  - → `chainsource`: `RegisterSpendRequest` (ancestry spend subscriptions).
- **Receives**:
  - ← API (via `darepod`): `TrackVTXOsRequest`, `UntrackRequest`.
  - ← `chainsource`: spend notifications re-wrapped into internal messages.

## Invariants

- Watches are passive: the watcher subscribes to chainsource spend events on
  known VTXO ancestors and never initiates proactive chain scans.
- Unroll jobs are only started once per target outpoint; a second fraud
  notification for the same target is a no-op (the registry deduplicates).
- The default mailbox size (`defaultWatcherMailboxSize = 64`) is sized to
  absorb burst spend notifications during chain reorganisations without
  blocking the chainsource publisher.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
