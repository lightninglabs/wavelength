# lib/recovery

## Purpose

Pure, immutable proof graph plus per-session planning state for unilateral
exit / recovery of a VTXO target outpoint. The package exposes the data model
(proof, session, durable state) and a TLV codec for crash-safe persistence.
It is deliberately I/O-free: broadcast orchestration, chain queries, and retry
scheduling live downstream in `unrollplan` and `unroll`.

## Key Types

- `Proof` — Immutable recovery graph: target outpoint, csv delay, topologically
  layered transaction nodes, parent/child adjacencies, reachability-checked.
- `Node` / `NodeKind` — One recovery transaction and its role (tree /
  checkpoint / ark).
- `Session` — Mutable planning object driven by caller-reported observations
  (`MarkBroadcasted`, `MarkConfirmed`, `MarkFailed`). Goroutine-safe via
  `sync.RWMutex`.
- `Snapshot` / `SessionStatus` — Caller-facing view of session progress at a
  block height, including CSV maturity and ready/blocked frontiers.
- `SessionState` — Durable caller-owned state suitable for TLV persistence.
  Optional fields use `fn.Option` instead of nilable pointers.
- `ComputeMaturityHeight` — Overflow-safe `targetConfirmHeight + csvDelay`
  helper shared with `unrollplan`.
- `Proof.RootExternalInputs() []wire.OutPoint` — The outpoints consumed by root
  transactions that no node in the proof produces: the external funding inputs
  the whole recovery graph hangs off. For a round-direct VTXO this is the
  batch/commitment output the tree root spends; for an OOR-chained or
  multi-input fan-in VTXO it is every distinct commitment output rooting a
  local lineage fragment. Deduplicated and sorted by hash then index, so the
  same node set always yields identical output regardless of map order.

## Relationships

- **Depends on**: `lib/arkscript` (AnchorPkScript detection on nodes),
  `lib/tree` (generic BFS `Queue[T]` for iterative ancestor traversal),
  `github.com/lightningnetwork/lnd/fn/v2` (Option type),
  `github.com/lightningnetwork/lnd/tlv` (state / proof codec).
- **Depended on by**: `unrollplan` (pure planning layer; re-uses `Proof`,
  `Node`, `ComputeMaturityHeight`), `unroll` (actor/FSM that drives broadcast
  and persists `SessionState` via the TLV codec), `waved` (RPC wiring).

## Invariants

- `csvDelay` is a raw block count (not a BIP-68-encoded sequence) and is
  capped at `MaxCSVDelay` (65535, the BIP-68 height-mode limit).
- `len(nodes)` is capped at `MaxProofNodes` to bound the cost of graph
  validation against adversarial inputs.
- Every node in a `Proof` is reachable (via parents) from the target outpoint;
  unreachable nodes fail construction.
- Parent/child reachability traversal uses an iterative BFS (`tree.Queue`), so
  a deeply-adversarial graph cannot blow the goroutine stack.
- Every `MarkConfirmed` call requires prior `MarkBroadcasted`, all parents
  confirmed, a non-negative height, and refuses re-confirmation at a
  different height. A same-height re-confirmation is idempotent.
- `MarkFailed` refuses to overwrite an existing terminal failure so the root
  cause survives across a restart.
- `Session` methods are safe for concurrent use under `RWMutex`; internal
  helpers assume the caller already holds the lock.
- The TLV codec is canonical (sorted by raw hash bytes) and carries an
  explicit version byte; version mismatch is a hard decode error.
- `RootExternalInputs` names exactly the outpoints a competing party (an
  operator sweeping an expired batch, a fraud spend) can consume out from under
  the exit. A confirmed foreign spend of any one of them makes every dependent
  root — and therefore the whole proof — permanently unbroadcastable, so
  callers arm spend watches on them and fail the exit terminally instead of
  spinning on a tree that can never confirm.
- `parseHash` via `chainhash.NewHashFromStr` is intentionally absent: raw
  32-byte hashes are encoded directly to avoid the short-form / zero-pad
  attack surface that JSON shipping with `chainhash.Hash.String()` would open.

## Deep Docs

- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
- [lib/CLAUDE.md](../CLAUDE.md) — Parent lib package overview.
